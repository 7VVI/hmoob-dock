package agentlink_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"hmoob-dock/internal/agentlink"
	"hmoob-dock/internal/model"
	"hmoob-dock/internal/protocol"
	"hmoob-dock/internal/store"
)

// testHub holds the hub, the enrollment and the server that accepts the
// connections of the agents.
type testHub struct {
	hub        *agentlink.Hub
	enrollment *agentlink.Enrollment
	server     *httptest.Server
}

// newTestHub builds a hub on top of a real database file and a server that
// accepts the connections of the agents.
func newTestHub(t *testing.T) testHub {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "nodes.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	enrollment := agentlink.NewEnrollment(db, time.Now)
	hub := agentlink.NewHub(db)
	// A revoked credential closes the connection of its node, the same way
	// the control端 does it.
	enrollment.OnRevoke(hub.Disconnect)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent/connect", func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		credential, err := enrollment.Authenticate(r.Context(), token)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_ = hub.Serve(r.Context(), credential.NodeID, conn)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return testHub{hub: hub, enrollment: enrollment, server: server}
}

// enroll registers a node and returns its credential.
func (h testHub) enroll(t *testing.T, identity protocol.NodeIdentity) agentlink.Credential {
	t.Helper()

	ctx := context.Background()
	token, err := h.enrollment.Issue(ctx)
	if err != nil {
		t.Fatalf("issue the enrollment token: %v", err)
	}
	credential, err := h.enrollment.Redeem(ctx, token, identity)
	if err != nil {
		t.Fatalf("enroll the node: %v", err)
	}
	return credential
}

// connect opens the connection of an agent.
func (h testHub) connect(t *testing.T, credential agentlink.Credential) *websocket.Conn {
	t.Helper()

	url := "ws" + strings.TrimPrefix(h.server.URL, "http") + "/api/v1/agent/connect"
	header := http.Header{}
	header.Set("Authorization", "Bearer "+credential.Token)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("dial the control端: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}

// send writes a frame to the control端.
func send(t *testing.T, conn *websocket.Conn, envelope protocol.Envelope) {
	t.Helper()

	data, err := protocol.Encode(envelope)
	if err != nil {
		t.Fatalf("encode the envelope: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write the envelope: %v", err)
	}
}

// hello returns the frame that opens the conversation of a connection.
func hello(t *testing.T, identity protocol.NodeIdentity) protocol.Envelope {
	t.Helper()

	payload, err := json.Marshal(protocol.Hello{Identity: identity})
	if err != nil {
		t.Fatalf("encode the hello: %v", err)
	}
	return protocol.Envelope{
		Version: protocol.WireVersion,
		Type:    protocol.TypeHello,
		Seq:     1,
		Payload: payload,
	}
}

// heartbeat returns the frame that reports the agent as alive.
func heartbeat(t *testing.T) protocol.Envelope {
	t.Helper()

	payload, err := json.Marshal(protocol.Heartbeat{SentAt: time.Now().UTC()})
	if err != nil {
		t.Fatalf("encode the heartbeat: %v", err)
	}
	return protocol.Envelope{
		Version: protocol.WireVersion,
		Type:    protocol.TypeHeartbeat,
		Seq:     2,
		Payload: payload,
	}
}

// receiptFrame returns the frame that carries the report of a task.
func receiptFrame(t *testing.T, seq uint64) protocol.Envelope {
	t.Helper()

	payload, err := json.Marshal(protocol.DeployReceipt{Receipt: model.Receipt{
		TaskID: "task-1",
		Digest: "digest-1",
		Seq:    seq,
		Result: model.Result{State: model.Succeeded, Code: "deployed"},
	}})
	if err != nil {
		t.Fatalf("encode the receipt: %v", err)
	}
	return protocol.Envelope{
		Version: protocol.WireVersion,
		Type:    protocol.TypeDeployReceipt,
		Seq:     seq,
		Payload: payload,
	}
}

// outputFrame returns the frame that carries a fragment of the output of a
// task.
func outputFrame(t *testing.T, seq uint64) protocol.Envelope {
	t.Helper()

	payload, err := json.Marshal(protocol.DeployOutput{
		TaskID: "task-1",
		Offset: 0,
		Data:   []byte("a fragment of the log"),
	})
	if err != nil {
		t.Fatalf("encode the output: %v", err)
	}
	return protocol.Envelope{
		Version: protocol.WireVersion,
		Type:    protocol.TypeDeployOutput,
		Seq:     seq,
		Payload: payload,
	}
}

// sendIgnoringErrors writes a frame to the control端 and ignores a write
// that fails.
//
// The control端 closes the connection when it refuses a frame, so the frames
// that follow the refused one cannot be written any more.
func sendIgnoringErrors(conn *websocket.Conn, envelope protocol.Envelope) {
	data, err := protocol.Encode(envelope)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = conn.Write(ctx, websocket.MessageText, data)
}

// inventoryBatch returns a frame that carries one batch of an inventory.
func inventoryBatch(t *testing.T, snapshotID string, index, count int, identity protocol.NodeIdentity, containers []protocol.Container) protocol.Envelope {
	t.Helper()

	payload, err := json.Marshal(protocol.InventoryBatch{
		SnapshotID:  snapshotID,
		BatchIndex:  index,
		BatchCount:  count,
		Complete:    index == count-1,
		Identity:    identity,
		Containers:  containers,
		CollectedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("encode the inventory batch: %v", err)
	}
	return protocol.Envelope{
		Version: protocol.WireVersion,
		Type:    protocol.TypeInventory,
		Seq:     uint64(index + 3),
		Payload: payload,
	}
}

// waitFor waits until the condition holds.
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout while waiting for %s", what)
}

// expectClosed waits until the control端 closed the connection.
func expectClosed(t *testing.T, conn *websocket.Conn) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, _, err := conn.Read(ctx)
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			t.Fatal("the connection of the agent stayed open")
		}
		return
	}
}

func TestHubAcceptsAnAgentThatNamesItsNode(t *testing.T) {
	h := newTestHub(t)
	identity := protocol.NodeIdentity{EngineID: "engine-a", Hostname: "worker-1"}
	credential := h.enroll(t, identity)

	conn := h.connect(t, credential)
	send(t, conn, hello(t, identity))

	waitFor(t, "the node to be connected", func() bool {
		return h.hub.Connected(credential.NodeID)
	})

	status, ok := h.hub.Status(credential.NodeID)
	if !ok {
		t.Fatal("the hub does not know the node")
	}
	if status.Identity.EngineID != "engine-a" {
		t.Fatalf("engine ID = %q, want engine-a", status.Identity.EngineID)
	}
	if status.LastSeen.IsZero() {
		t.Fatal("the node was never seen")
	}
}

func TestHubRefusesAnAgentThatClaimsAnotherEngine(t *testing.T) {
	h := newTestHub(t)
	credential := h.enroll(t, protocol.NodeIdentity{EngineID: "engine-a"})

	conn := h.connect(t, credential)
	send(t, conn, hello(t, protocol.NodeIdentity{EngineID: "engine-b"}))

	expectClosed(t, conn)
	if h.hub.Connected(credential.NodeID) {
		t.Fatal("the node is connected although its identity does not match")
	}
}

func TestHubRefusesAnUnknownMessageType(t *testing.T) {
	h := newTestHub(t)
	identity := protocol.NodeIdentity{EngineID: "engine-a"}
	credential := h.enroll(t, identity)

	conn := h.connect(t, credential)
	send(t, conn, hello(t, identity))
	waitFor(t, "the node to be connected", func() bool {
		return h.hub.Connected(credential.NodeID)
	})

	send(t, conn, protocol.Envelope{Version: protocol.WireVersion, Type: "deploy.command", Seq: 2})

	expectClosed(t, conn)
}

func TestHubRefusesAnUnknownProtocolVersion(t *testing.T) {
	h := newTestHub(t)
	identity := protocol.NodeIdentity{EngineID: "engine-a"}
	credential := h.enroll(t, identity)

	conn := h.connect(t, credential)
	envelope := hello(t, identity)
	envelope.Version = protocol.WireVersion + 1
	send(t, conn, envelope)

	expectClosed(t, conn)
}

func TestHubRefusesAnOversizedMessage(t *testing.T) {
	h := newTestHub(t)
	identity := protocol.NodeIdentity{EngineID: "engine-a"}
	credential := h.enroll(t, identity)

	conn := h.connect(t, credential)
	send(t, conn, hello(t, identity))
	waitFor(t, "the node to be connected", func() bool {
		return h.hub.Connected(credential.NodeID)
	})

	send(t, conn, protocol.Envelope{
		Version: protocol.WireVersion,
		Type:    protocol.TypeHeartbeat,
		Seq:     2,
		Payload: json.RawMessage(`"` + strings.Repeat("a", protocol.MaxPayloadSize+1) + `"`),
	})

	expectClosed(t, conn)
}

func TestHubClosesTheConnectionOfARevokedNode(t *testing.T) {
	h := newTestHub(t)
	identity := protocol.NodeIdentity{EngineID: "engine-a"}
	credential := h.enroll(t, identity)

	conn := h.connect(t, credential)
	send(t, conn, hello(t, identity))
	waitFor(t, "the node to be connected", func() bool {
		return h.hub.Connected(credential.NodeID)
	})

	if err := h.enrollment.Revoke(context.Background(), credential.NodeID); err != nil {
		t.Fatalf("revoke the node: %v", err)
	}

	expectClosed(t, conn)
	waitFor(t, "the node to be disconnected", func() bool {
		return !h.hub.Connected(credential.NodeID)
	})
}

func TestHubReplacesADuplicateConnection(t *testing.T) {
	h := newTestHub(t)
	identity := protocol.NodeIdentity{EngineID: "engine-a"}
	credential := h.enroll(t, identity)

	first := h.connect(t, credential)
	send(t, first, hello(t, identity))
	waitFor(t, "the node to be connected", func() bool {
		return h.hub.Connected(credential.NodeID)
	})

	second := h.connect(t, credential)
	send(t, second, hello(t, identity))

	// The first connection is replaced by the second one.
	expectClosed(t, first)
	waitFor(t, "the node to stay connected", func() bool {
		return h.hub.Connected(credential.NodeID)
	})
}

func TestHubAssemblesTheBatchesOfAnInventory(t *testing.T) {
	h := newTestHub(t)
	identity := protocol.NodeIdentity{EngineID: "engine-a", Hostname: "worker-1"}
	credential := h.enroll(t, identity)

	conn := h.connect(t, credential)
	send(t, conn, hello(t, identity))
	waitFor(t, "the node to be connected", func() bool {
		return h.hub.Connected(credential.NodeID)
	})

	send(t, conn, inventoryBatch(t, "snapshot-1", 0, 2, identity, []protocol.Container{
		{ID: "c1", Name: "web", Image: "nginx", State: "running"},
	}))
	send(t, conn, inventoryBatch(t, "snapshot-1", 1, 2, identity, []protocol.Container{
		{ID: "c2", Name: "db", Image: "postgres", State: "exited"},
	}))

	var inventory protocol.Inventory
	waitFor(t, "the inventory to be complete", func() bool {
		var err error
		inventory, err = h.hub.ReadInventory(context.Background(), "compose", credential.NodeID)
		return err == nil
	})

	if len(inventory.Containers) != 2 {
		t.Fatalf("containers = %d, want 2", len(inventory.Containers))
	}
	if inventory.Containers[0].Name != "web" || inventory.Containers[1].Name != "db" {
		t.Fatalf("containers = %+v, want the containers of both batches in order", inventory.Containers)
	}
	if inventory.Identity.EngineID != "engine-a" {
		t.Fatalf("engine ID = %q, want engine-a", inventory.Identity.EngineID)
	}
}

func TestHubKeepsAnInventoryIncompleteUntilEveryBatchArrived(t *testing.T) {
	h := newTestHub(t)
	identity := protocol.NodeIdentity{EngineID: "engine-a"}
	credential := h.enroll(t, identity)

	conn := h.connect(t, credential)
	send(t, conn, hello(t, identity))
	waitFor(t, "the node to be connected", func() bool {
		return h.hub.Connected(credential.NodeID)
	})

	send(t, conn, inventoryBatch(t, "snapshot-1", 0, 2, identity, []protocol.Container{
		{ID: "c1", Name: "web", State: "running"},
	}))

	// The snapshot is not complete, so the control端 has no inventory of
	// the node.
	if _, err := h.hub.ReadInventory(context.Background(), "compose", credential.NodeID); err == nil {
		t.Fatal("the control端 answered with an incomplete inventory")
	}
}

func TestHubKeepsTheInventoryOfANodeWithoutContainers(t *testing.T) {
	h := newTestHub(t)
	identity := protocol.NodeIdentity{EngineID: "engine-a"}
	credential := h.enroll(t, identity)

	conn := h.connect(t, credential)
	send(t, conn, hello(t, identity))
	waitFor(t, "the node to be connected", func() bool {
		return h.hub.Connected(credential.NodeID)
	})

	// A node without containers sends an empty inventory.
	send(t, conn, inventoryBatch(t, "snapshot-1", 0, 1, identity, nil))

	var inventory protocol.Inventory
	waitFor(t, "the inventory of the node", func() bool {
		var err error
		inventory, err = h.hub.ReadInventory(context.Background(), "compose", credential.NodeID)
		return err == nil
	})
	if len(inventory.Containers) != 0 {
		t.Fatalf("containers = %d, want none", len(inventory.Containers))
	}
}

func TestHubRefusesToReadTheInventoryOfADisconnectedNode(t *testing.T) {
	h := newTestHub(t)
	identity := protocol.NodeIdentity{EngineID: "engine-a"}
	credential := h.enroll(t, identity)

	conn := h.connect(t, credential)
	send(t, conn, hello(t, identity))
	waitFor(t, "the node to be connected", func() bool {
		return h.hub.Connected(credential.NodeID)
	})
	send(t, conn, inventoryBatch(t, "snapshot-1", 0, 1, identity, []protocol.Container{
		{ID: "c1", Name: "web", State: "running"},
	}))

	conn.CloseNow()
	waitFor(t, "the node to be disconnected", func() bool {
		return !h.hub.Connected(credential.NodeID)
	})

	if _, err := h.hub.ReadInventory(context.Background(), "compose", credential.NodeID); err == nil {
		t.Fatal("the control端 answered with the inventory of a disconnected node")
	}
}

func TestHubRefusesAnUnknownMode(t *testing.T) {
	h := newTestHub(t)

	if _, err := h.hub.ReadInventory(context.Background(), "kubernetes", "node-1"); err == nil {
		t.Fatal("the control端 answered for an unknown mode")
	}
}

func TestHubReadsTheInventoryOfAManagerOfACluster(t *testing.T) {
	h := newTestHub(t)
	managerIdentity := protocol.NodeIdentity{
		EngineID:    "engine-manager",
		SwarmID:     "cluster-1",
		SwarmNodeID: "node-manager",
		Role:        protocol.RoleManager,
	}
	workerIdentity := protocol.NodeIdentity{
		EngineID:    "engine-worker",
		SwarmID:     "cluster-1",
		SwarmNodeID: "node-worker",
		Role:        protocol.RoleWorker,
	}
	manager := h.enroll(t, managerIdentity)
	worker := h.enroll(t, workerIdentity)

	managerConn := h.connect(t, manager)
	send(t, managerConn, hello(t, managerIdentity))
	waitFor(t, "the manager to be connected", func() bool {
		return h.hub.Connected(manager.NodeID)
	})
	send(t, managerConn, inventoryBatch(t, "snapshot-1", 0, 1, managerIdentity, []protocol.Container{
		{ID: "c1", Name: "manager-web", State: "running"},
	}))

	workerConn := h.connect(t, worker)
	send(t, workerConn, hello(t, workerIdentity))
	waitFor(t, "the worker to be connected", func() bool {
		return h.hub.Connected(worker.NodeID)
	})

	var inventory protocol.Inventory
	waitFor(t, "the inventory of the cluster", func() bool {
		var err error
		inventory, err = h.hub.ReadInventory(context.Background(), "swarm", "cluster-1")
		return err == nil
	})
	if len(inventory.Containers) != 1 || inventory.Containers[0].Name != "manager-web" {
		t.Fatalf("containers = %+v, want the containers of the manager", inventory.Containers)
	}
}

func TestHubRefusesAnAgentThatClaimsTheManagerRole(t *testing.T) {
	h := newTestHub(t)
	workerIdentity := protocol.NodeIdentity{
		EngineID:    "engine-worker",
		SwarmID:     "cluster-1",
		SwarmNodeID: "node-worker",
		Role:        protocol.RoleWorker,
	}
	worker := h.enroll(t, workerIdentity)

	// The agent claims the role of a manager in its hello, although its
	// enrollment says that it is a worker.
	claimed := workerIdentity
	claimed.Role = protocol.RoleManager

	conn := h.connect(t, worker)
	send(t, conn, hello(t, claimed))

	// The control端 refuses the connection, because the role of the hello
	// and the role of the enrollment disagree.
	expectClosed(t, conn)
	if h.hub.Connected(worker.NodeID) {
		t.Fatal("the node is connected although its hello claims another role")
	}

	// The node never reports an inventory, so the cluster has no manager
	// and the control端 cannot answer with the containers of the node.
	if _, err := h.hub.ReadInventory(context.Background(), "swarm", "cluster-1"); err == nil {
		t.Fatal("the control端 answered with the cluster inventory of a node that claims the manager role")
	}
}

func TestHubRefusesTheInventoryOfAClusterWithoutAManager(t *testing.T) {
	h := newTestHub(t)
	workerIdentity := protocol.NodeIdentity{
		EngineID:    "engine-worker",
		SwarmID:     "cluster-1",
		SwarmNodeID: "node-worker",
		Role:        protocol.RoleWorker,
	}
	worker := h.enroll(t, workerIdentity)

	conn := h.connect(t, worker)
	send(t, conn, hello(t, workerIdentity))
	waitFor(t, "the worker to be connected", func() bool {
		return h.hub.Connected(worker.NodeID)
	})

	if _, err := h.hub.ReadInventory(context.Background(), "swarm", "cluster-1"); err == nil {
		t.Fatal("the control端 answered with a cluster that has no manager")
	}
}

func TestHubDeliversAFrameToTheAgent(t *testing.T) {
	h := newTestHub(t)
	identity := protocol.NodeIdentity{EngineID: "engine-a"}
	credential := h.enroll(t, identity)

	conn := h.connect(t, credential)
	send(t, conn, hello(t, identity))
	waitFor(t, "the node to be connected", func() bool {
		return h.hub.Connected(credential.NodeID)
	})

	envelope := protocol.Envelope{Version: protocol.WireVersion, Type: protocol.TypeHello, Seq: 7}
	if err := h.hub.Send(context.Background(), credential.NodeID, envelope); err != nil {
		t.Fatalf("send the frame: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read the frame: %v", err)
	}
	received, err := protocol.Decode(data)
	if err != nil {
		t.Fatalf("decode the frame: %v", err)
	}
	if received.Seq != 7 {
		t.Fatalf("sequence = %d, want 7", received.Seq)
	}
}

func TestHubRefusesToSendAFrameToAnUnknownNode(t *testing.T) {
	h := newTestHub(t)

	err := h.hub.Send(context.Background(), "node-that-does-not-exist", protocol.Envelope{
		Version: protocol.WireVersion,
		Type:    protocol.TypeHello,
	})
	if err == nil {
		t.Fatal("the control端 sent a frame to a node that is not connected")
	}
}

func TestHubDoesNotHoldBackOtherNodesOnASlowSubscriber(t *testing.T) {
	h := newTestHub(t)

	release := make(chan struct{})
	var once sync.Once
	h.hub.Subscribe(protocol.TypeHeartbeat, func(ctx context.Context, nodeID string, envelope protocol.Envelope) error {
		// The first heartbeat blocks until the test releases it.
		once.Do(func() { <-release })
		return nil
	})

	firstIdentity := protocol.NodeIdentity{EngineID: "engine-a"}
	secondIdentity := protocol.NodeIdentity{EngineID: "engine-b"}
	first := h.enroll(t, firstIdentity)
	second := h.enroll(t, secondIdentity)

	firstConn := h.connect(t, first)
	send(t, firstConn, hello(t, firstIdentity))
	waitFor(t, "the first node to be connected", func() bool {
		return h.hub.Connected(first.NodeID)
	})

	secondConn := h.connect(t, second)
	send(t, secondConn, hello(t, secondIdentity))
	waitFor(t, "the second node to be connected", func() bool {
		return h.hub.Connected(second.NodeID)
	})

	// The heartbeat of the first node blocks its own subscriber.
	send(t, firstConn, heartbeat(t))
	send(t, secondConn, heartbeat(t))

	// The heartbeat of the second node is still processed.
	waitFor(t, "the heartbeat of the second node", func() bool {
		status, ok := h.hub.Status(second.NodeID)
		return ok && !status.LastSeen.IsZero()
	})

	close(release)
}

// TestTheHubDeliversAControlFrameWhenTheMonitorQueueIsFull proves that a
// receipt of a task reaches the subscribers even when the queue of the
// fragments of the logs is full.
//
// The fragments of the logs are dropped when their queue is full, but a
// receipt may not be lost: it is the report of the outcome of a task.
func TestTheHubDeliversAControlFrameWhenTheMonitorQueueIsFull(t *testing.T) {
	h := newTestHub(t)
	identity := protocol.NodeIdentity{EngineID: "engine-a"}
	credential := h.enroll(t, identity)

	conn := h.connect(t, credential)
	send(t, conn, hello(t, identity))
	waitFor(t, "the node to be connected", func() bool {
		return h.hub.Connected(credential.NodeID)
	})

	// The first fragment of the log blocks the dispatcher of the
	// connection, so the queue of the fragments fills up.
	releaseFirst := make(chan struct{})
	releaseRest := make(chan struct{})
	defer close(releaseRest)
	var fragments atomic.Int64
	h.hub.Subscribe(protocol.TypeDeployOutput, func(ctx context.Context, nodeID string, envelope protocol.Envelope) error {
		if fragments.Add(1) == 1 {
			<-releaseFirst
			return nil
		}
		// Every other fragment waits, so a fragment that is served before
		// the receipt shows that the control frames are not served first.
		<-releaseRest
		return nil
	})

	delivered := make(chan int64, 1)
	h.hub.Subscribe(protocol.TypeDeployReceipt, func(ctx context.Context, nodeID string, envelope protocol.Envelope) error {
		// The number of the fragments that were served before the receipt
		// tells whether the control frames are served first.
		delivered <- fragments.Load()
		return nil
	})

	send(t, conn, outputFrame(t, 2))
	for index := 0; index < agentlink.InboundMonitorBuffer+5; index++ {
		send(t, conn, outputFrame(t, uint64(index+3)))
	}

	// The fragments of the logs are dropped, but the connection of the node
	// stays alive.
	if !h.hub.Connected(credential.NodeID) {
		t.Fatal("the control端 closed the connection although only fragments of the log were dropped")
	}

	// The receipt waits in the queue of the control frames.
	send(t, conn, receiptFrame(t, uint64(agentlink.InboundMonitorBuffer+10)))

	// The heartbeat proves that the control端 read the receipt, because the
	// frames of a connection are read in the order they were sent.
	before, ok := h.hub.Status(credential.NodeID)
	if !ok {
		t.Fatal("the hub does not know the node")
	}
	send(t, conn, heartbeat(t))
	waitFor(t, "the control端 to read the receipt", func() bool {
		status, ok := h.hub.Status(credential.NodeID)
		return ok && status.LastSeen.After(before.LastSeen)
	})

	close(releaseFirst)

	select {
	case served := <-delivered:
		// The receipt was served before the fragments that waited in the
		// queue of the logs.
		if served != 1 {
			t.Fatalf("%d fragments of the log were served before the receipt, want 1", served)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the receipt never reached the subscribers")
	}
}

// TestTheHubRefusesAControlFrameWhenTheControlQueueIsFull proves that a
// receipt of a task is not dropped when the subscribers do not keep up.
//
// A control frame may not be lost, so the connection of the node is refused
// instead of throwing the report of the task away.
func TestTheHubRefusesAControlFrameWhenTheControlQueueIsFull(t *testing.T) {
	h := newTestHub(t)
	identity := protocol.NodeIdentity{EngineID: "engine-a"}
	credential := h.enroll(t, identity)

	conn := h.connect(t, credential)
	send(t, conn, hello(t, identity))
	waitFor(t, "the node to be connected", func() bool {
		return h.hub.Connected(credential.NodeID)
	})

	// The subscribers of the receipts do not keep up.
	release := make(chan struct{})
	defer close(release)
	h.hub.Subscribe(protocol.TypeDeployReceipt, func(ctx context.Context, nodeID string, envelope protocol.Envelope) error {
		<-release
		return nil
	})

	// The first receipt blocks the dispatcher of the connection, the others
	// fill the queue of the control frames up.
	send(t, conn, receiptFrame(t, 1))
	for index := 0; index < agentlink.InboundControlBuffer+5; index++ {
		sendIgnoringErrors(conn, receiptFrame(t, uint64(index+2)))
	}

	expectClosed(t, conn)
}

// TestHubCarriesTheNetworksAndTheImagesOfAnInventory proves that the
// networks and the images of a snapshot reach the inventory of the node,
// although they travel only in the first batch of it.
func TestHubCarriesTheNetworksAndTheImagesOfAnInventory(t *testing.T) {
	h := newTestHub(t)
	identity := protocol.NodeIdentity{EngineID: "engine-a", Hostname: "worker-1"}
	credential := h.enroll(t, identity)

	conn := h.connect(t, credential)
	send(t, conn, hello(t, identity))
	waitFor(t, "the node to be connected", func() bool {
		return h.hub.Connected(credential.NodeID)
	})

	batch := protocol.InventoryBatch{
		SnapshotID:  "snapshot-1",
		BatchIndex:  0,
		BatchCount:  1,
		Complete:    true,
		Identity:    identity,
		Containers:  []protocol.Container{{ID: "c1", Name: "web", Image: "nginx:alpine", State: "running"}},
		Networks:    []protocol.Network{{ID: "net-1", Name: "bridge", IPAMSubnet: "172.17.0.0/16", IPAMGateway: "172.17.0.1"}},
		Images:      []protocol.Image{{ID: "sha256:aaa111", Tags: []string{"nginx:alpine"}, Size: 73400320}},
		CollectedAt: time.Now().UTC(),
	}
	payload, err := json.Marshal(batch)
	if err != nil {
		t.Fatalf("encode the inventory batch: %v", err)
	}
	send(t, conn, protocol.Envelope{
		Version: protocol.WireVersion,
		Type:    protocol.TypeInventory,
		Seq:     3,
		Payload: payload,
	})

	var inventory protocol.Inventory
	waitFor(t, "the inventory to be complete", func() bool {
		var err error
		inventory, err = h.hub.ReadInventory(context.Background(), "compose", credential.NodeID)
		return err == nil
	})

	if len(inventory.Networks) != 1 || inventory.Networks[0].Name != "bridge" {
		t.Fatalf("networks = %+v, want the network of the snapshot", inventory.Networks)
	}
	if inventory.Networks[0].IPAMSubnet != "172.17.0.0/16" {
		t.Fatalf("subnet = %q, want the subnet of the network", inventory.Networks[0].IPAMSubnet)
	}
	if len(inventory.Images) != 1 || inventory.Images[0].ID != "sha256:aaa111" {
		t.Fatalf("images = %+v, want the image of the snapshot", inventory.Images)
	}
	if inventory.Images[0].Tags[0] != "nginx:alpine" {
		t.Fatalf("tags = %v, want the reference of the image", inventory.Images[0].Tags)
	}
}

// TestHubCarriesTheVolumesOfAnInventory proves that the named volumes of a
// snapshot reach the inventory of the node.
func TestHubCarriesTheVolumesOfAnInventory(t *testing.T) {
	h := newTestHub(t)
	identity := protocol.NodeIdentity{EngineID: "engine-a", Hostname: "worker-1"}
	credential := h.enroll(t, identity)

	conn := h.connect(t, credential)
	send(t, conn, hello(t, identity))
	waitFor(t, "the node to be connected", func() bool {
		return h.hub.Connected(credential.NodeID)
	})

	batch := protocol.InventoryBatch{
		SnapshotID:  "snapshot-1",
		BatchIndex:  0,
		BatchCount:  1,
		Complete:    true,
		Identity:    identity,
		Containers:  []protocol.Container{{ID: "c1", Name: "web", Image: "nginx:alpine", State: "running", Mounts: []protocol.Mount{{Type: "volume", Source: "web-data", Destination: "/data"}}}},
		Volumes:     []protocol.Volume{{Name: "web-data", Driver: "local", Mountpoint: "/var/lib/docker/volumes/web-data/_data"}},
		CollectedAt: time.Now().UTC(),
	}
	payload, err := json.Marshal(batch)
	if err != nil {
		t.Fatalf("encode the inventory batch: %v", err)
	}
	send(t, conn, protocol.Envelope{
		Version: protocol.WireVersion,
		Type:    protocol.TypeInventory,
		Seq:     3,
		Payload: payload,
	})

	var inventory protocol.Inventory
	waitFor(t, "the inventory to be complete", func() bool {
		var err error
		inventory, err = h.hub.ReadInventory(context.Background(), "compose", credential.NodeID)
		return err == nil
	})

	if len(inventory.Volumes) != 1 || inventory.Volumes[0].Name != "web-data" {
		t.Fatalf("volumes = %+v, want the volume of the snapshot", inventory.Volumes)
	}
	if inventory.Volumes[0].Mountpoint != "/var/lib/docker/volumes/web-data/_data" {
		t.Fatalf("mountpoint = %q, want the path of the volume", inventory.Volumes[0].Mountpoint)
	}
}
