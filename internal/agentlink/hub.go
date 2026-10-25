package agentlink

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"hmoob-dock/internal/protocol"
	"hmoob-dock/internal/store"
)

// The limits and timeouts of a connection of an agent.
const (
	// HeartbeatInterval is the time the agent sends a heartbeat.
	HeartbeatInterval = 10 * time.Second
	// HeartbeatTimeout is the time the control端 waits for a heartbeat
	// before it considers the node lost. The node is lost when the
	// control端 did not receive a heartbeat in that time, not when the
	// connection broke.
	HeartbeatTimeout = 60 * time.Second
	// InventoryTimeout is the time after which an inventory is too old to
	// answer a question about the node. A deployment check must not work
	// with an outdated inventory.
	InventoryTimeout = 60 * time.Second
	// handshakeTimeout bounds the hello of the agent.
	handshakeTimeout = 10 * time.Second
	// controlBuffer is the number of control frames that wait for the
	// write loop of a connection.
	controlBuffer = 16
	// monitorBuffer is the number of monitoring frames that wait for the
	// write loop of a connection.
	monitorBuffer = 64
	// inboundBuffer is the number of frames of the agent that wait for the
	// subscribers of the hub.
	inboundBuffer = 64
	// snapshotTTL is the time an incomplete snapshot is kept.
	snapshotTTL = 60 * time.Second
)

// The errors of the hub. The HTTP layer turns them into status codes and
// messages.
var (
	// ErrNodeNotConnected reports a node without a live connection.
	ErrNodeNotConnected = errors.New("agentlink: the node is not connected")
	// ErrNoInventory reports a node that did not send a complete
	// inventory.
	ErrNoInventory = errors.New("agentlink: the node did not send a complete inventory")
	// ErrStaleInventory reports an inventory that is too old to be used.
	ErrStaleInventory = errors.New("agentlink: the inventory of the node is outdated")
	// ErrNoManager reports a Swarm cluster without an available manager.
	ErrNoManager = errors.New("agentlink: no manager of the cluster is connected")
	// ErrIdentityMismatch reports an agent that claims another identity.
	ErrIdentityMismatch = errors.New("agentlink: the identity of the agent does not match the node")
	// ErrUnknownMode reports a mode the hub does not know.
	ErrUnknownMode = errors.New("agentlink: the mode is unknown")
	// ErrQueueFull reports a connection whose buffer is full.
	ErrQueueFull = errors.New("agentlink: the connection of the node is too slow")
	// ErrNoHandshake reports a connection without a hello of the agent.
	ErrNoHandshake = errors.New("agentlink: the agent did not send a hello")
	// ErrConnectionEnded reports that a connection ended. The reason of
	// the end is not interesting for the control端: the agent opens a
	// new connection.
	ErrConnectionEnded = errors.New("agentlink: the connection ended")
)

// Status is the live state of a node.
type Status struct {
	// Connected reports whether the agent of the node is connected and
	// sends heartbeats.
	Connected bool
	// LastSeen is the time the control端 received the last heartbeat. It
	// is the zero time while the node never sent one.
	LastSeen time.Time
	// ContainerCount is the number of containers of the last complete
	// inventory. It is zero while the node sent no inventory.
	ContainerCount int
	// HasInventory reports whether a complete inventory was received.
	HasInventory bool
	// Identity is the identity of the node.
	Identity protocol.NodeIdentity
}

// subscriber is a callback that receives the frames of the agents.
type subscriber struct {
	kind string
	fn   func(context.Context, string, protocol.Envelope) error
}

// Hub keeps the live connections of the agents.
//
// Every connection has its own read loop and its own write loop, so a slow
// agent never blocks the other ones. The frames the control端 sends wait in
// a bounded buffer, and a buffer that is full refuses the frame instead of
// holding the caller.
type Hub struct {
	db  *sql.DB
	now func() time.Time

	// mu guards the connections and the subscribers.
	mu sync.RWMutex
	// connections holds the live connection of every node.
	connections map[string]*connection
	// subscribers holds the callbacks per message type.
	subscribers map[string][]subscriber
}

// NewHub builds the hub of the agent connections.
func NewHub(db *sql.DB) *Hub {
	return &Hub{
		db:          db,
		now:         time.Now,
		connections: make(map[string]*connection),
		subscribers: make(map[string][]subscriber),
	}
}

// Subscribe registers a callback that receives the frames of the given
// message type. The callback runs in the dispatcher of the connection, so a
// slow callback delays the frames of its own connection only.
func (h *Hub) Subscribe(kind string, fn func(context.Context, string, protocol.Envelope) error) {
	if fn == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.subscribers[kind] = append(h.subscribers[kind], subscriber{kind: kind, fn: fn})
}

// Send hands a frame to the agent of a node.
//
// The frame waits in the buffer of the connection. A buffer that is full
// means that the agent does not read fast enough: the frame is refused
// instead of blocking the caller, so a slow agent never holds the other
// nodes.
func (h *Hub) Send(ctx context.Context, nodeID string, message protocol.Envelope) error {
	node, ok := h.connection(nodeID)
	if !ok {
		return ErrNodeNotConnected
	}
	frame, err := protocol.Encode(message)
	if err != nil {
		return err
	}
	select {
	case node.outbound(message.Type) <- frame:
		return nil
	default:
		return ErrQueueFull
	}
}

// Connected reports whether the agent of the node is connected.
func (h *Hub) Connected(nodeID string) bool {
	status, ok := h.Status(nodeID)
	return ok && status.Connected
}

// Status returns the live state of a node.
func (h *Hub) Status(nodeID string) (Status, bool) {
	node, ok := h.connection(nodeID)
	if !ok {
		return Status{}, false
	}
	return node.status(h.now()), true
}

// Disconnect closes the connection of a node. The agent cannot keep talking
// to the control端, but the containers of the node keep running.
func (h *Hub) Disconnect(nodeID string) {
	node, ok := h.connection(nodeID)
	if !ok {
		return
	}
	node.close()
}

// ReadInventory returns the inventory of a node.
//
// In the Compose mode the target is a node and its own inventory is read.
// In the Swarm mode the target is a cluster and the inventory of a manager
// of the cluster is read, because a worker cannot answer for the cluster.
//
// A node that is not connected and an inventory that is too old are
// errors: a deployment check must not work with the state of a node that
// nobody observes any more.
func (h *Hub) ReadInventory(ctx context.Context, mode, targetID string) (protocol.Inventory, error) {
	target := strings.TrimSpace(targetID)
	if target == "" {
		return protocol.Inventory{}, ErrNodeNotConnected
	}

	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "compose":
		node, ok := h.connection(target)
		if !ok {
			return protocol.Inventory{}, ErrNodeNotConnected
		}
		return node.currentInventory(h.now())
	case "swarm":
		node, ok := h.swarmManager(target)
		if !ok {
			return protocol.Inventory{}, ErrNoManager
		}
		return node.currentInventory(h.now())
	default:
		return protocol.Inventory{}, ErrUnknownMode
	}
}

// swarmManager returns the connection of a manager of the given cluster.
func (h *Hub) swarmManager(clusterID string) (*connection, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for _, node := range h.connections {
		if node.identity.SwarmID != clusterID {
			continue
		}
		if node.identity.Role != protocol.RoleManager {
			continue
		}
		if !node.status(h.now()).Connected {
			continue
		}
		return node, true
	}
	return nil, false
}

// connection returns the live connection of a node.
func (h *Hub) connection(nodeID string) (*connection, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	node, ok := h.connections[nodeID]
	return node, ok
}

// Serve runs the connection of a node until it ends.
//
// The agent opens the conversation with a hello that names its identity.
// The control端 compares the identity with the enrolled node: an agent
// that claims another engine is refused, so the source of a report is
// always the credential that authenticated the connection.
func (h *Hub) Serve(ctx context.Context, nodeID string, conn *websocket.Conn) error {
	conn.SetReadLimit(int64(protocol.MaxMessageSize))

	identity, err := h.readHello(ctx, conn)
	if err != nil {
		_ = conn.Close(websocket.StatusPolicyViolation, "the hello of the agent is missing")
		return err
	}

	record, err := h.record(ctx, nodeID)
	if err != nil {
		_ = conn.Close(websocket.StatusPolicyViolation, "the node is unknown")
		return err
	}
	if record.EngineID != identity.EngineID {
		_ = conn.Close(websocket.StatusPolicyViolation, "the identity does not match the node")
		return fmt.Errorf("%w: the node %s belongs to another engine", ErrIdentityMismatch, nodeID)
	}

	node := newConnection(h, nodeID, conn, identity)
	// The hello proves that the agent is alive.
	node.markSeen()
	h.register(node)
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	defer node.stop()
	defer h.unregister(node)

	go node.writeLoop(ctx)
	go node.dispatchLoop(ctx)

	return node.readLoop(ctx)
}

// readHello reads the hello of the agent and returns the identity it
// names.
func (h *Hub) readHello(ctx context.Context, conn *websocket.Conn) (protocol.NodeIdentity, error) {
	handshakeCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()

	_, data, err := conn.Read(handshakeCtx)
	if err != nil {
		return protocol.NodeIdentity{}, fmt.Errorf("%w: %v", ErrNoHandshake, err)
	}
	envelope, err := protocol.Decode(data)
	if err != nil {
		return protocol.NodeIdentity{}, err
	}
	if err := envelope.Validate(); err != nil {
		return protocol.NodeIdentity{}, err
	}
	if envelope.Type != protocol.TypeHello {
		return protocol.NodeIdentity{}, fmt.Errorf("%w: the first message is a %s", ErrNoHandshake, envelope.Type)
	}

	var hello protocol.Hello
	if err := json.Unmarshal(envelope.Payload, &hello); err != nil {
		return protocol.NodeIdentity{}, fmt.Errorf("agentlink: decode the hello: %w", err)
	}
	if strings.TrimSpace(hello.Identity.EngineID) == "" {
		return protocol.NodeIdentity{}, fmt.Errorf("%w: the hello names no engine", ErrIdentityMismatch)
	}
	return hello.Identity, nil
}

// record reads the enrolled record of a node.
func (h *Hub) record(ctx context.Context, nodeID string) (Node, error) {
	var node Node
	var createdAt int64
	var revokedAt sql.NullInt64
	err := h.db.QueryRowContext(ctx,
		`SELECT id, engine_id, swarm_id, swarm_node_id, role, hostname, created_at, revoked_at
		 FROM nodes WHERE id = ?`, nodeID).
		Scan(&node.ID, &node.EngineID, &node.SwarmID, &node.SwarmNodeID, &node.Role, &node.Hostname,
			&createdAt, &revokedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Node{}, ErrUnknownNode
	case err != nil:
		return Node{}, fmt.Errorf("agentlink: read the node: %w", err)
	}
	if revokedAt.Valid {
		return Node{}, ErrNodeRevoked
	}
	node.CreatedAt = store.TimeFromMillis(createdAt)
	return node, nil
}

// register makes the connection the live one of its node. A connection
// that opens while another one is still open replaces it: only the newest
// connection of a node is used.
func (h *Hub) register(node *connection) {
	h.mu.Lock()
	previous, ok := h.connections[node.nodeID]
	h.connections[node.nodeID] = node
	h.mu.Unlock()

	if ok && previous != node {
		previous.close()
	}
}

// unregister removes the connection from the hub when it ends.
func (h *Hub) unregister(node *connection) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if current, ok := h.connections[node.nodeID]; ok && current == node {
		delete(h.connections, node.nodeID)
	}
}

// notify hands a frame of an agent to the subscribers of its type.
func (h *Hub) notify(ctx context.Context, nodeID string, envelope protocol.Envelope) {
	h.mu.RLock()
	subscribers := make([]subscriber, len(h.subscribers[envelope.Type]))
	copy(subscribers, h.subscribers[envelope.Type])
	h.mu.RUnlock()

	for _, sub := range subscribers {
		if err := sub.fn(ctx, nodeID, envelope); err != nil {
			log.Printf("control: the subscriber of the %s frames failed: %v", sub.kind, err)
		}
	}
}

// connection is the live connection of the agent of a node.
type connection struct {
	hub    *Hub
	nodeID string
	conn   *websocket.Conn

	// control carries the frames that command the agent.
	control chan []byte
	// monitor carries the frames that report the state of a node.
	monitor chan []byte
	// inbound carries the frames of the agent to the subscribers.
	inbound chan protocol.Envelope

	// done closes when the connection ends.
	done chan struct{}
	// stopOnce guards the closing of the workers.
	stopOnce sync.Once
	// closeOnce guards the closing of the connection.
	closeOnce sync.Once
	// dropOnce guards the report of the dropped frames.
	dropOnce sync.Once

	// mu guards the state of the node.
	mu sync.RWMutex
	// identity is the identity the agent named in its hello.
	identity protocol.NodeIdentity
	// lastSeen is the time the control端 received the last sign of life
	// of the agent.
	lastSeen time.Time
	// inventory is the last complete inventory of the node.
	inventory *protocol.Inventory
	// inventoryAt is the time the last complete inventory arrived.
	inventoryAt time.Time
	// pending holds the snapshots that did not arrive completely.
	pending map[string]*pendingSnapshot
}

// pendingSnapshot holds the batches of a snapshot that did not arrive
// completely.
type pendingSnapshot struct {
	// count is the number of batches the snapshot has.
	count int
	// startedAt is the time the first batch of the snapshot arrived.
	startedAt time.Time
	// batches holds the batches of the snapshot by their index.
	batches map[int]protocol.InventoryBatch
}

// newConnection builds the connection of the agent of a node.
func newConnection(hub *Hub, nodeID string, conn *websocket.Conn, identity protocol.NodeIdentity) *connection {
	return &connection{
		hub:      hub,
		nodeID:   nodeID,
		conn:     conn,
		control:  make(chan []byte, controlBuffer),
		monitor:  make(chan []byte, monitorBuffer),
		inbound:  make(chan protocol.Envelope, inboundBuffer),
		done:     make(chan struct{}),
		identity: identity,
		pending:  make(map[string]*pendingSnapshot),
	}
}

// outbound returns the buffer that holds the frames of the given type. The
// control frames and the monitoring frames wait in their own buffer, so a
// flood of monitoring frames cannot hold back a command.
func (c *connection) outbound(messageType string) chan []byte {
	if isMonitoringType(messageType) {
		return c.monitor
	}
	return c.control
}

// isMonitoringType reports whether a frame of the given type carries the
// state of a node instead of a command.
func isMonitoringType(messageType string) bool {
	switch messageType {
	case protocol.TypeHeartbeat, protocol.TypeInventory:
		return true
	default:
		return false
	}
}

// readLoop reads the frames of the agent until the connection ends.
func (c *connection) readLoop(ctx context.Context) error {
	for {
		messageType, data, err := c.conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrConnectionEnded, err)
		}
		if messageType != websocket.MessageText {
			_ = c.conn.Close(websocket.StatusUnsupportedData, "the protocol uses text frames")
			return errors.New("agentlink: the agent sent a frame that is not text")
		}
		envelope, err := protocol.Decode(data)
		if err != nil {
			_ = c.conn.Close(websocket.StatusProtocolError, "the frame is not a valid message")
			return err
		}
		if err := envelope.Validate(); err != nil {
			_ = c.conn.Close(websocket.StatusProtocolError, "the message is not accepted")
			return err
		}
		if err := c.accept(envelope); err != nil {
			_ = c.conn.Close(websocket.StatusProtocolError, "the message is not accepted")
			return err
		}
		c.dispatch(envelope)
	}
}

// accept applies a frame of the agent to the state of the node.
func (c *connection) accept(envelope protocol.Envelope) error {
	switch envelope.Type {
	case protocol.TypeHello:
		// The conversation starts with a hello. A second one would let an
		// agent change its identity while it is connected.
		return fmt.Errorf("%w: the agent sent a second hello", ErrIdentityMismatch)
	case protocol.TypeHeartbeat:
		c.markSeen()
		return nil
	case protocol.TypeInventory:
		return c.acceptInventory(envelope)
	default:
		return fmt.Errorf("agentlink: the message type %s is not accepted", envelope.Type)
	}
}

// markSeen remembers that the agent is alive.
func (c *connection) markSeen() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastSeen = c.hub.now()
}

// acceptInventory takes a batch of an inventory.
//
// The snapshot is complete when every batch of it arrived. Until then the
// node keeps the inventory it had, so a truncated snapshot never looks
// like a complete one.
func (c *connection) acceptInventory(envelope protocol.Envelope) error {
	var batch protocol.InventoryBatch
	if err := json.Unmarshal(envelope.Payload, &batch); err != nil {
		return fmt.Errorf("agentlink: decode the inventory batch: %w", err)
	}
	if strings.TrimSpace(batch.SnapshotID) == "" {
		return errors.New("agentlink: the inventory batch names no snapshot")
	}
	if batch.BatchCount <= 0 || batch.BatchIndex < 0 || batch.BatchIndex >= batch.BatchCount {
		return errors.New("agentlink: the inventory batch is malformed")
	}
	if batch.Identity.EngineID != c.identity.EngineID {
		return fmt.Errorf("%w: the inventory names another engine", ErrIdentityMismatch)
	}

	now := c.hub.now()
	c.mu.Lock()
	defer c.mu.Unlock()

	c.pruneSnapshots(now)
	pending, known := c.pending[batch.SnapshotID]
	if !known {
		pending = &pendingSnapshot{
			count:     batch.BatchCount,
			startedAt: now,
			batches:   make(map[int]protocol.InventoryBatch),
		}
		c.pending[batch.SnapshotID] = pending
	}
	if pending.count != batch.BatchCount {
		return errors.New("agentlink: the batches of a snapshot disagree about their number")
	}
	pending.batches[batch.BatchIndex] = batch
	if len(pending.batches) < pending.count {
		return nil
	}

	containers := make([]protocol.Container, 0, len(pending.batches))
	var collectedAt time.Time
	for index := 0; index < pending.count; index++ {
		part := pending.batches[index]
		containers = append(containers, part.Containers...)
		if collectedAt.IsZero() {
			collectedAt = part.CollectedAt
		}
	}
	c.inventory = &protocol.Inventory{
		Identity:    pending.batches[0].Identity,
		Containers:  containers,
		CollectedAt: collectedAt,
	}
	c.inventoryAt = now
	delete(c.pending, batch.SnapshotID)
	return nil
}

// pruneSnapshots drops the snapshots that did not arrive completely.
func (c *connection) pruneSnapshots(now time.Time) {
	for id, pending := range c.pending {
		if now.Sub(pending.startedAt) > snapshotTTL {
			delete(c.pending, id)
		}
	}
}

// dispatch hands a frame of the agent to the subscribers.
//
// A buffer that is full means that the subscribers do not keep up. The
// frame is dropped then, so the connection of this node stays alive and
// the other nodes are not held back.
func (c *connection) dispatch(envelope protocol.Envelope) {
	select {
	case c.inbound <- envelope:
		return
	case <-c.done:
		return
	default:
		c.dropOnce.Do(func() {
			log.Printf("control: the connection of the node %s is too slow, frames are dropped", c.nodeID)
		})
	}
}

// writeLoop writes the frames of the control端 to the agent. The connection
// has exactly one write loop, so the frames never interleave.
func (c *connection) writeLoop(ctx context.Context) {
	for {
		frame, ok := c.nextFrame(ctx)
		if !ok {
			return
		}
		if err := c.conn.Write(ctx, websocket.MessageText, frame); err != nil {
			c.close()
			return
		}
	}
}

// nextFrame waits for the next frame of the control端. A control frame wins
// over a monitoring frame, so a flood of monitoring frames cannot hold back
// a command.
func (c *connection) nextFrame(ctx context.Context) ([]byte, bool) {
	select {
	case frame := <-c.control:
		return frame, true
	default:
	}
	select {
	case <-c.done:
		return nil, false
	case <-ctx.Done():
		return nil, false
	case frame := <-c.control:
		return frame, true
	case frame := <-c.monitor:
		return frame, true
	}
}

// dispatchLoop hands the frames of the agent to the subscribers.
func (c *connection) dispatchLoop(ctx context.Context) {
	for {
		select {
		case <-c.done:
			return
		case envelope := <-c.inbound:
			c.hub.notify(ctx, c.nodeID, envelope)
		}
	}
}

// status returns the live state of the node.
//
// The node counts as connected while the control端 received a sign of life
// in the heartbeat window. A node whose agent stopped sending heartbeats is
// lost, even while its connection is still open.
func (c *connection) status(now time.Time) Status {
	c.mu.RLock()
	defer c.mu.RUnlock()

	status := Status{
		Connected:    !c.lastSeen.IsZero() && now.Sub(c.lastSeen) <= HeartbeatTimeout,
		LastSeen:     c.lastSeen,
		HasInventory: c.inventory != nil,
		Identity:     c.identity,
	}
	if c.inventory != nil {
		status.ContainerCount = len(c.inventory.Containers)
	}
	return status
}

// inventory returns the last complete inventory of the node. An inventory
// that is too old is an error: a deployment check must not work with the
// state of a node that nobody observes any more.
func (c *connection) currentInventory(now time.Time) (protocol.Inventory, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.inventory == nil {
		return protocol.Inventory{}, ErrNoInventory
	}
	if now.Sub(c.inventoryAt) > InventoryTimeout {
		return protocol.Inventory{}, ErrStaleInventory
	}
	return *c.inventory, nil
}

// stop ends the workers of the connection. The connection itself stays
// open until it is closed.
func (c *connection) stop() {
	c.stopOnce.Do(func() {
		close(c.done)
	})
}

// close ends the connection of the node.
func (c *connection) close() {
	c.stop()
	c.closeOnce.Do(func() {
		_ = c.conn.CloseNow()
	})
}
