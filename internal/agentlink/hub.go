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
	// InboundControlBuffer is the number of control frames of an agent
	// that wait for the subscribers of the hub.
	//
	// The control frames of an agent are the reports of its tasks. They
	// are never dropped: a queue that is full refuses the frame and the
	// connection of the node.
	InboundControlBuffer = 64
	// InboundMonitorBuffer is the number of monitoring frames of an agent
	// that wait for the subscribers of the hub.
	//
	// The monitoring frames of an agent are the fragments of the logs and
	// the state of its node. They are dropped when their queue is full,
	// because they may be fetched again.
	InboundMonitorBuffer = 64
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
	// AgentVersion is the version of the agent the hello of the
	// connection reported. It is empty while the agent did not report
	// one.
	AgentVersion string
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
//
// The frames of an agent wait in two queues as well: the reports of the
// tasks of the agent wait in the control queue and the fragments of the logs
// wait in the monitoring queue. The control queue is served first and is
// never filled by dropping a frame: a report that may not be delivered ends
// the connection of the node instead.
type Hub struct {
	db  *sql.DB
	now func() time.Time

	// mu guards the connections and the subscribers.
	mu sync.RWMutex
	// connections holds the live connection of every node.
	connections map[string]*connection
	// subscribers holds the callbacks per message type.
	subscribers map[string][]subscriber
	// onConnect holds the callbacks that run when a node connects.
	onConnect []func(string)
	// onDisconnect holds the callbacks that run when the live connection of
	// a node ends.
	onDisconnect []func(string)

	// pendingMu guards the requests that wait for the result of a node.
	// It is a mutex of its own, so a request that starts or ends never
	// holds the fan-out of the frames.
	pendingMu sync.Mutex
	// pending holds the requests by the ID the node carries back.
	pending map[string]*pendingRequest
}

// NewHub builds the hub of the agent connections.
func NewHub(db *sql.DB) *Hub {
	return &Hub{
		db:          db,
		now:         time.Now,
		connections: make(map[string]*connection),
		subscribers: make(map[string][]subscriber),
		pending:     make(map[string]*pendingRequest),
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

// OnConnect registers a callback that runs when the agent of a node opens a
// connection.
//
// The callback runs in the goroutine of the connection, so it has to be
// quick: a slow callback delays the frames of its own connection only.
func (h *Hub) OnConnect(fn func(nodeID string)) {
	if fn == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onConnect = append(h.onConnect, fn)
}

// OnDisconnect registers a callback that runs when the live connection of a
// node ends. A connection that is replaced by a newer one of the same node
// does not run the callback, because the node did not disappear.
//
// The callback runs in the goroutine of the connection, so it has to be
// quick: a slow callback delays the end of its own connection only.
func (h *Hub) OnDisconnect(fn func(nodeID string)) {
	if fn == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onDisconnect = append(h.onDisconnect, fn)
}

// connected tells the callbacks that the agent of a node is connected.
//
// A node that comes back may have lost the reports of its tasks, so the
// control端 asks it about the tasks it holds.
func (h *Hub) connected(nodeID string) {
	h.mu.RLock()
	callbacks := make([]func(string), len(h.onConnect))
	copy(callbacks, h.onConnect)
	h.mu.RUnlock()

	for _, callback := range callbacks {
		callback(nodeID)
	}
}

// disconnected tells the callbacks that the live connection of a node
// ended.
func (h *Hub) disconnected(nodeID string) {
	h.mu.RLock()
	callbacks := make([]func(string), len(h.onDisconnect))
	copy(callbacks, h.onDisconnect)
	h.mu.RUnlock()

	for _, callback := range callbacks {
		callback(nodeID)
	}
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

// RecentContainer reports whether the recent inventory of a node holds a
// container.
//
// The second return value reports whether the node has a recent inventory
// at all. Without one the first value says nothing: a node that is away
// cannot prove the ownership of a container.
func (h *Hub) RecentContainer(nodeID, containerID string) (bool, bool) {
	node, ok := h.connection(nodeID)
	if !ok {
		return false, false
	}
	inventory, err := node.currentInventory(h.now())
	if err != nil {
		return false, false
	}
	for _, container := range inventory.Containers {
		if container.ID == containerID {
			return true, true
		}
	}
	return false, true
}

// swarmManager returns the connection of a manager of the given cluster.
//
// The manager is decided by the enrolled record of a node, not by the hello
// of its agent: only a node that was enrolled with the role of a manager
// can answer for a cluster.
func (h *Hub) swarmManager(clusterID string) (*connection, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for _, node := range h.connections {
		if node.record.SwarmID != clusterID {
			continue
		}
		if node.record.Role != protocol.RoleManager {
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
//
// The role and the cluster of a node belong to its enrollment. An agent
// that names another role or another cluster is refused as well, because a
// self reported role proves nothing: only the enrollment of the node does.
func (h *Hub) Serve(ctx context.Context, nodeID string, conn *websocket.Conn) error {
	conn.SetReadLimit(int64(protocol.MaxMessageSize))

	identity, agentVersion, err := h.readHello(ctx, conn)
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
	// The role and the cluster of the record are the values the control端
	// compares and stores, so a stray space cannot make two values differ.
	record.SwarmID = strings.TrimSpace(record.SwarmID)
	record.Role = strings.TrimSpace(record.Role)
	if !sameRoleAndCluster(identity, record) {
		_ = conn.Close(websocket.StatusPolicyViolation, "the role of the agent does not match the node")
		return fmt.Errorf("%w: the node %s does not hold the claimed role", ErrIdentityMismatch, nodeID)
	}
	// The role and the cluster of the connection are the ones of the
	// enrollment. The hello does not get to change them.
	identity.SwarmID = record.SwarmID
	identity.Role = record.Role

	node := newConnection(h, nodeID, conn, identity, record, agentVersion)
	// The hello proves that the agent is alive.
	node.markSeen()
	h.register(node)
	h.connected(nodeID)
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	defer node.stop()
	defer func() {
		// A connection that a newer connection replaced is not a lost
		// node, so only the connection that was still live reports the
		// end.
		if h.unregister(node) {
			h.disconnected(nodeID)
		}
	}()

	go node.writeLoop(ctx)
	go node.dispatchLoop(ctx)

	return node.readLoop(ctx)
}

// sameRoleAndCluster reports whether the agent and the enrolled record of
// its node agree about the role of the node and about the cluster it
// belongs to.
//
// The two values decide who may answer for a cluster, so a difference
// between them is a violation of the policy and not a detail.
func sameRoleAndCluster(identity protocol.NodeIdentity, record Node) bool {
	return strings.TrimSpace(identity.Role) == record.Role &&
		strings.TrimSpace(identity.SwarmID) == record.SwarmID
}

// readHello reads the hello of the agent and returns the identity it
// names.
func (h *Hub) readHello(ctx context.Context, conn *websocket.Conn) (protocol.NodeIdentity, string, error) {
	handshakeCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()

	_, data, err := conn.Read(handshakeCtx)
	if err != nil {
		return protocol.NodeIdentity{}, "", fmt.Errorf("%w: %v", ErrNoHandshake, err)
	}
	envelope, err := protocol.Decode(data)
	if err != nil {
		return protocol.NodeIdentity{}, "", err
	}
	if err := envelope.Validate(); err != nil {
		return protocol.NodeIdentity{}, "", err
	}
	if envelope.Type != protocol.TypeHello {
		return protocol.NodeIdentity{}, "", fmt.Errorf("%w: the first message is a %s", ErrNoHandshake, envelope.Type)
	}

	var hello protocol.Hello
	if err := json.Unmarshal(envelope.Payload, &hello); err != nil {
		return protocol.NodeIdentity{}, "", fmt.Errorf("agentlink: decode the hello: %w", err)
	}
	if strings.TrimSpace(hello.Identity.EngineID) == "" {
		return protocol.NodeIdentity{}, "", fmt.Errorf("%w: the hello names no engine", ErrIdentityMismatch)
	}
	return hello.Identity, strings.TrimSpace(hello.AgentVersion), nil
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

// unregister removes the connection from the hub when it ends. The second
// return value reports whether the connection was still the live one of its
// node.
func (h *Hub) unregister(node *connection) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	if current, ok := h.connections[node.nodeID]; ok && current == node {
		delete(h.connections, node.nodeID)
		return true
	}
	return false
}

// notify hands a frame of an agent to the subscribers of its type.
func (h *Hub) notify(ctx context.Context, nodeID string, envelope protocol.Envelope) {
	// The result of a container operation belongs to the request that
	// waits for it. It reaches no subscriber, so the resolution runs
	// before the fan-out and the frame ends here.
	if envelope.Type == protocol.TypeContainerResult {
		h.resolvePending(nodeID, envelope)
		return
	}

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
	// inboundControl carries the control frames of the agent to the
	// subscribers.
	inboundControl chan protocol.Envelope
	// inboundMonitor carries the monitoring frames of the agent to the
	// subscribers.
	inboundMonitor chan protocol.Envelope

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
	// record is the enrolled record of the node. It holds the role and the
	// cluster of the node, so the control端 never trusts the self reported
	// role of an agent.
	record Node
	// identity is the identity the agent named in its hello. Its role and
	// its cluster are the ones of the enrolled record.
	identity protocol.NodeIdentity
	// agentVersion is the version of the agent the hello reported.
	agentVersion string
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
//
// The record is the enrolled record of the node. It decides about the role
// and the cluster of the node, so the identity of the connection carries
// the values of the record and not the ones of the hello.
func newConnection(hub *Hub, nodeID string, conn *websocket.Conn, identity protocol.NodeIdentity, record Node, agentVersion string) *connection {
	return &connection{
		hub:            hub,
		nodeID:         nodeID,
		conn:           conn,
		control:        make(chan []byte, controlBuffer),
		monitor:        make(chan []byte, monitorBuffer),
		inboundControl: make(chan protocol.Envelope, InboundControlBuffer),
		inboundMonitor: make(chan protocol.Envelope, InboundMonitorBuffer),
		done:           make(chan struct{}),
		record:         record,
		identity:       identity,
		agentVersion:   agentVersion,
		pending:        make(map[string]*pendingSnapshot),
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
	case protocol.TypeHeartbeat, protocol.TypeInventory, protocol.TypeDeployOutput,
		protocol.TypeMetricsSample, protocol.TypeLogsFrame:
		// The fragments of the output of a task, the samples of the
		// containers and the pieces of the logs wait behind the receipts of
		// the tasks, so a flood of logs cannot hold back the answer of the
		// control端.
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
		if err := c.dispatch(envelope); err != nil {
			_ = c.conn.Close(websocket.StatusPolicyViolation, "the control frames of the node wait too long")
			return err
		}
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
	case protocol.TypeDeployReceipt, protocol.TypeDeployQueryResult, protocol.TypeDeployOutput,
		protocol.TypeContainerResult,
		protocol.TypeMetricsSample, protocol.TypeContainerEvent, protocol.TypeLogsFrame:
		// The report of a task, the result of an operation, the sample of
		// a container, the event of a container and the piece of a log
		// reach the subscribers of the hub. The hub does not judge them
		// itself.
		return nil
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
	// The role and the cluster of an inventory are the ones of the
	// enrollment, not the ones that the agent reports.
	batch.Identity.SwarmID = c.identity.SwarmID
	batch.Identity.Role = c.identity.Role

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
	var networks []protocol.Network
	var images []protocol.Image
	var volumes []protocol.Volume
	var swarmNodes []protocol.SwarmNode
	var swarmServices []protocol.SwarmService
	var swarmTasks []protocol.SwarmTask
	var collectedAt time.Time
	for index := 0; index < pending.count; index++ {
		part := pending.batches[index]
		containers = append(containers, part.Containers...)
		if len(part.Networks) > 0 {
			networks = part.Networks
		}
		if len(part.Images) > 0 {
			images = part.Images
		}
		if len(part.Volumes) > 0 {
			volumes = part.Volumes
		}
		if len(part.SwarmNodes) > 0 {
			swarmNodes = part.SwarmNodes
		}
		if len(part.SwarmServices) > 0 {
			swarmServices = part.SwarmServices
		}
		if len(part.SwarmTasks) > 0 {
			swarmTasks = part.SwarmTasks
		}
		if collectedAt.IsZero() {
			collectedAt = part.CollectedAt
		}
	}
	c.inventory = &protocol.Inventory{
		Identity:      pending.batches[0].Identity,
		Containers:    containers,
		SwarmNodes:    swarmNodes,
		SwarmServices: swarmServices,
		SwarmTasks:    swarmTasks,
		Networks:      networks,
		Images:        images,
		Volumes:       volumes,
		CollectedAt:   collectedAt,
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
// The control frames and the monitoring frames wait in their own queue, so
// a flood of the fragments of the logs cannot push the report of a task out
// of the queue.
//
// A control queue that is full refuses the frame: a report of a task may not
// be lost, so the connection of the node ends instead. A monitoring queue
// that is full drops the frame: a fragment of a log may be fetched again.
func (c *connection) dispatch(envelope protocol.Envelope) error {
	if isMonitoringType(envelope.Type) {
		select {
		case c.inboundMonitor <- envelope:
			return nil
		case <-c.done:
			return nil
		default:
			c.dropOnce.Do(func() {
				log.Printf("control: the connection of the node %s is too slow, the monitoring frames are dropped", c.nodeID)
			})
			return nil
		}
	}

	select {
	case c.inboundControl <- envelope:
		return nil
	case <-c.done:
		return nil
	default:
		return fmt.Errorf("%w: the control frames of the node %s wait too long", ErrQueueFull, c.nodeID)
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
		// The control frames are served first, so the report of a task is
		// never delayed by the fragments of the logs.
		if envelope, ok := c.nextInbound(); ok {
			c.hub.notify(ctx, c.nodeID, envelope)
			continue
		}
		select {
		case <-c.done:
			return
		case envelope := <-c.inboundControl:
			c.hub.notify(ctx, c.nodeID, envelope)
		case envelope := <-c.inboundMonitor:
			c.hub.notify(ctx, c.nodeID, envelope)
		}
	}
}

// nextInbound returns the next control frame of the agent, if one waits.
func (c *connection) nextInbound() (protocol.Envelope, bool) {
	select {
	case envelope := <-c.inboundControl:
		return envelope, true
	default:
		return protocol.Envelope{}, false
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
		AgentVersion: c.agentVersion,
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
