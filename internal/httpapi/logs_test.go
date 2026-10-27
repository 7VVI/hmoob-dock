package httpapi_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	api "hmoob-dock/internal/httpapi"
	"hmoob-dock/internal/protocol"
)

// sseEvent is the data of one event of the stream.
type sseEvent struct {
	Type      string `json:"type"`
	ObjectID  string `json:"object_id"`
	Seq       uint64 `json:"seq"`
	Offset    uint64 `json:"offset"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
	ErrorCode string `json:"error_code"`
}

// eventStream is an open stream of the events of a test.
type eventStream struct {
	cancel   context.CancelFunc
	response *http.Response
	reader   *bufio.Reader
}

// close ends the stream.
func (s *eventStream) close() {
	s.cancel()
	_ = s.response.Body.Close()
}

// next reads the next event of the stream.
func (s *eventStream) next(t *testing.T) sseEvent {
	t.Helper()

	type result struct {
		event sseEvent
		err   error
	}
	results := make(chan result, 1)
	go func() {
		event, err := readSSEEvent(s.reader)
		results <- result{event: event, err: err}
	}()
	select {
	case got := <-results:
		if got.err != nil {
			t.Fatalf("read the event stream: %v", got.err)
		}
		return got.event
	case <-time.After(5 * time.Second):
		t.Fatal("timeout while waiting for an event")
		return sseEvent{}
	}
}

// nextWithin reads the next event of the stream and reports whether one
// arrived within the given time. A missing event is not a failure: it is
// the expected answer of a stream that owns no subscription.
func (s *eventStream) nextWithin(t *testing.T, timeout time.Duration) (sseEvent, bool) {
	t.Helper()

	type result struct {
		event sseEvent
		err   error
	}
	results := make(chan result, 1)
	go func() {
		event, err := readSSEEvent(s.reader)
		results <- result{event: event, err: err}
	}()
	select {
	case got := <-results:
		if got.err != nil {
			t.Fatalf("read the event stream: %v", got.err)
		}
		return got.event, true
	case <-time.After(timeout):
		return sseEvent{}, false
	}
}

// readSSEEvent reads one data line of the stream.
func readSSEEvent(reader *bufio.Reader) (sseEvent, error) {
	var data string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return sseEvent{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if data == "" {
				continue
			}
			var event sseEvent
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				return sseEvent{}, err
			}
			return event, nil
		case strings.HasPrefix(line, "data:"):
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
}

// logTestControl is a control端 with an agent and one running container.
type logTestControl struct {
	*authServer
	csrf       string
	nodeID     string
	credential string
	agent      *websocket.Conn
}

// newLogTestControl starts a control端, enrolls a node and connects its
// agent with an inventory that holds the container c1.
func newLogTestControl(t *testing.T) *logTestControl {
	t.Helper()

	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()
	token := server.createAgentToken(t, csrf)
	payload := server.enroll(t, token, nodeIdentity)
	nodeID, _ := payload["node_id"].(string)
	credential, _ := payload["credential"].(string)
	if nodeID == "" || credential == "" {
		t.Fatalf("the enrollment answered %v, want a node and a credential", payload)
	}

	control := &logTestControl{authServer: server, csrf: csrf, nodeID: nodeID, credential: credential}
	control.agent = dialTestAgent(t, server, credential)
	sendTestAgentFrame(t, control.agent, protocol.TypeHello, protocol.Hello{Identity: nodeIdentity})
	sendTestAgentFrame(t, control.agent, protocol.TypeInventory, protocol.InventoryBatch{
		SnapshotID:  "snapshot-1",
		BatchIndex:  0,
		BatchCount:  1,
		Complete:    true,
		Identity:    nodeIdentity,
		Containers:  []protocol.Container{{ID: "c1", Name: "web", Image: "nginx", State: "running"}},
		CollectedAt: time.Now().UTC(),
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		nodes := server.nodeList(t)
		if len(nodes) == 1 && nodes[0]["container_count"] == float64(1) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the inventory of the node did not arrive: %v", nodes)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return control
}

// dialTestAgent opens the connection of a test agent.
func dialTestAgent(t *testing.T, server *authServer, credential string) *websocket.Conn {
	t.Helper()

	header := http.Header{}
	header.Set("Authorization", "Bearer "+credential)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "wss://"+server.host()+"/api/v1/agent/connect",
		&websocket.DialOptions{HTTPClient: server.client, HTTPHeader: header})
	if err != nil {
		t.Fatalf("dial the control端: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}

// sendTestAgentFrame writes a frame of the test agent.
func sendTestAgentFrame(t *testing.T, conn *websocket.Conn, messageType string, payload any) {
	t.Helper()

	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode the payload: %v", err)
	}
	frame, err := protocol.Encode(protocol.Envelope{
		Version: protocol.WireVersion,
		Type:    messageType,
		Seq:     1,
		Payload: encoded,
	})
	if err != nil {
		t.Fatalf("encode the envelope: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatalf("write the frame: %v", err)
	}
}

// readTestAgentFrame reads a frame of the control端.
func readTestAgentFrame(t *testing.T, conn *websocket.Conn) protocol.Envelope {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read the frame of the control端: %v", err)
	}
	envelope, err := protocol.Decode(data)
	if err != nil {
		t.Fatalf("decode the frame of the control端: %v", err)
	}
	return envelope
}

// agentFrameReader reads the frames of the test agent in the background, so
// a test can wait for a frame and can prove that no frame arrived without
// closing the connection.
type agentFrameReader struct {
	frames chan protocol.Envelope
	errors chan error
}

// startAgentFrameReader starts reading the frames of the test agent.
func startAgentFrameReader(t *testing.T, conn *websocket.Conn) *agentFrameReader {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reader := &agentFrameReader{
		frames: make(chan protocol.Envelope, 8),
		errors: make(chan error, 1),
	}
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				reader.errors <- err
				return
			}
			envelope, err := protocol.Decode(data)
			if err != nil {
				reader.errors <- err
				return
			}
			reader.frames <- envelope
		}
	}()
	return reader
}

// next waits for the next frame of the agent.
func (r *agentFrameReader) next(t *testing.T) protocol.Envelope {
	t.Helper()
	select {
	case envelope := <-r.frames:
		return envelope
	case err := <-r.errors:
		t.Fatalf("read the frame of the control端: %v", err)
		return protocol.Envelope{}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout while waiting for a frame of the control端")
		return protocol.Envelope{}
	}
}

// absent reports whether no frame arrived within the given time.
func (r *agentFrameReader) absent(t *testing.T, timeout time.Duration) bool {
	t.Helper()
	select {
	case envelope := <-r.frames:
		t.Fatalf("the control端 sent the frame %s, want no frame", envelope.Type)
		return false
	case err := <-r.errors:
		t.Fatalf("read the frame of the control端: %v", err)
		return false
	case <-time.After(timeout):
		return true
	}
}

// openEventStream opens the stream of the events of the administrator.
func openEventStream(t *testing.T, server *authServer) *eventStream {
	t.Helper()
	return openEventStreamWithClient(t, server, server.client)
}

// openEventStreamWithClient opens the stream of the events with the client
// of a session of the test.
func openEventStreamWithClient(t *testing.T, server *authServer, client *http.Client) *eventStream {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.server.URL+"/api/v1/events", nil)
	if err != nil {
		cancel()
		t.Fatalf("build the event request: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		cancel()
		t.Fatalf("open the event stream: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		cancel()
		_ = response.Body.Close()
		t.Fatalf("the event stream returned %d, want %d", response.StatusCode, http.StatusOK)
	}
	stream := &eventStream{cancel: cancel, response: response, reader: bufio.NewReader(response.Body)}
	t.Cleanup(stream.close)
	return stream
}

// createLogSubscription creates a subscription as the administrator.
func (c *logTestControl) createLogSubscription(t *testing.T, body map[string]any) (map[string]any, int) {
	t.Helper()

	response := c.post("/api/v1/log-subscriptions", body, withHeader("X-CSRF-Token", c.csrf))
	if response.StatusCode == http.StatusCreated {
		return c.decode(response), response.StatusCode
	}
	return c.decode(response), response.StatusCode
}

// deleteLogSubscription deletes a subscription as the administrator.
func (c *logTestControl) deleteLogSubscription(t *testing.T, id string) *http.Response {
	t.Helper()
	return c.do(http.MethodDelete, "/api/v1/log-subscriptions/"+id, nil,
		withOrigin(c.server.URL), withHeader("X-CSRF-Token", c.csrf))
}

// subscriptionRequest is the body of a creation of a subscription.
func subscriptionRequest(nodeID, containerID string, tail int) map[string]any {
	return map[string]any{
		"node_id":      nodeID,
		"container_id": containerID,
		"tail":         tail,
		"follow":       true,
	}
}

// TestALogSubscriptionStreamsFramesAndStopsItsSource proves the happy path:
// the browser creates a subscription, the agent receives the request, the
// frames arrive over the existing event stream and the deletion stops the
// source.
func TestALogSubscriptionStreamsFramesAndStopsItsSource(t *testing.T) {
	control := newLogTestControl(t)

	// The metrics and the container events are accepted on the same
	// connection before the log subscription.
	sendTestAgentFrame(t, control.agent, protocol.TypeMetricsSample, protocol.RawSample{
		ContainerID: "c1", CollectedAt: time.Now().UTC(),
	})
	sendTestAgentFrame(t, control.agent, protocol.TypeContainerEvent, protocol.ContainerEvent{
		ContainerID: "c1", Action: "start", OccurredAt: time.Now().UTC(),
	})

	stream := openEventStream(t, control.authServer)
	payload, status := control.createLogSubscription(t, subscriptionRequest(control.nodeID, "c1", 50))
	if status != http.StatusCreated {
		t.Fatalf("the creation returned %d: %v", status, payload)
	}
	id, _ := payload["id"].(string)
	if id == "" {
		t.Fatalf("the creation answered %v, want a subscription ID", payload)
	}
	if payload["node_id"] != control.nodeID || payload["container_id"] != "c1" {
		t.Fatalf("the creation answered %v, want the target of the request", payload)
	}

	envelope := readTestAgentFrame(t, control.agent)
	if envelope.Type != protocol.TypeLogsSubscribe {
		t.Fatalf("the frame = %q, want %q", envelope.Type, protocol.TypeLogsSubscribe)
	}
	var request protocol.LogRequest
	if err := json.Unmarshal(envelope.Payload, &request); err != nil {
		t.Fatalf("decode the log request: %v", err)
	}
	if request.SubscriptionID != id || request.ContainerID != "c1" || request.Tail != 50 || !request.Follow {
		t.Fatalf("the request = %+v, want the subscription of the test", request)
	}

	sendTestAgentFrame(t, control.agent, protocol.TypeLogsFrame, protocol.LogFrame{
		SubscriptionID: id,
		Offset:         0,
		Text:           "line one\n",
	})
	event := stream.next(t)
	if event.Type != "log" || event.ObjectID != id || event.Text != "line one\n" || event.Offset != 0 {
		t.Fatalf("the event = %+v, want the first line of the subscription", event)
	}

	sendTestAgentFrame(t, control.agent, protocol.TypeLogsFrame, protocol.LogFrame{
		SubscriptionID: id,
		Offset:         9,
		Text:           "line two\n",
		Truncated:      true,
	})
	event = stream.next(t)
	if event.Text != "line two\n" || !event.Truncated || event.Offset != 9 {
		t.Fatalf("the event = %+v, want the truncated second line", event)
	}

	response := control.deleteLogSubscription(t, id)
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("the deletion returned %d, want %d", response.StatusCode, http.StatusNoContent)
	}

	envelope = readTestAgentFrame(t, control.agent)
	if envelope.Type != protocol.TypeLogsUnsubscribe {
		t.Fatalf("the frame = %q, want %q", envelope.Type, protocol.TypeLogsUnsubscribe)
	}
	if err := json.Unmarshal(envelope.Payload, &request); err != nil {
		t.Fatalf("decode the log request: %v", err)
	}
	if request.SubscriptionID != id {
		t.Fatalf("the unsubscribe names %q, want %q", request.SubscriptionID, id)
	}
}

// TestLogFramesReachOnlyTheOwningSession proves that a subscription belongs
// to the session that created it: the event stream of another session never
// receives its pieces.
func TestLogFramesReachOnlyTheOwningSession(t *testing.T) {
	control := newLogTestControl(t)
	owner := openEventStream(t, control.authServer)

	otherClient, _ := loginAnotherSession(t, control.authServer)
	other := openEventStreamWithClient(t, control.authServer, otherClient)

	payload, status := control.createLogSubscription(t, subscriptionRequest(control.nodeID, "c1", 20))
	if status != http.StatusCreated {
		t.Fatalf("the creation returned %d: %v", status, payload)
	}
	id, _ := payload["id"].(string)
	readTestAgentFrame(t, control.agent)

	sendTestAgentFrame(t, control.agent, protocol.TypeLogsFrame, protocol.LogFrame{
		SubscriptionID: id,
		Text:           "private line\n",
	})
	if event := owner.next(t); event.Text != "private line\n" {
		t.Fatalf("the owner received %+v, want the line of its subscription", event)
	}
	if event, arrived := other.nextWithin(t, 300*time.Millisecond); arrived {
		t.Fatalf("another session received %+v, want no piece of the subscription", event)
	}
}

// TestTwoEventStreamsOfASessionShareTheSubscription proves the reference
// count of the pages of a session: the first page that closes does not end
// the source, the last one does.
func TestTwoEventStreamsOfASessionShareTheSubscription(t *testing.T) {
	control := newLogTestControl(t)
	frames := startAgentFrameReader(t, control.agent)

	first := openEventStream(t, control.authServer)
	second := openEventStream(t, control.authServer)

	payload, status := control.createLogSubscription(t, subscriptionRequest(control.nodeID, "c1", 20))
	if status != http.StatusCreated {
		t.Fatalf("the creation returned %d: %v", status, payload)
	}
	id, _ := payload["id"].(string)
	if frame := frames.next(t); frame.Type != protocol.TypeLogsSubscribe {
		t.Fatalf("the frame = %q, want %q", frame.Type, protocol.TypeLogsSubscribe)
	}

	first.close()
	if !frames.absent(t, 300*time.Millisecond) {
		t.Fatal("the first page ended the subscription of the session")
	}

	second.close()
	if frame := frames.next(t); frame.Type != protocol.TypeLogsUnsubscribe {
		t.Fatalf("the frame = %q, want %q", frame.Type, protocol.TypeLogsUnsubscribe)
	} else {
		var request protocol.LogRequest
		if err := json.Unmarshal(frame.Payload, &request); err != nil {
			t.Fatalf("decode the log request: %v", err)
		}
		if request.SubscriptionID != id {
			t.Fatalf("the unsubscribe names %q, want %q", request.SubscriptionID, id)
		}
	}
}

// TestAGapBetweenTheOffsetsIsMarkedTruncated proves that a piece lost in a
// queue of the transport is shown as a gap and not as contiguous text.
func TestAGapBetweenTheOffsetsIsMarkedTruncated(t *testing.T) {
	control := newLogTestControl(t)
	stream := openEventStream(t, control.authServer)

	payload, status := control.createLogSubscription(t, subscriptionRequest(control.nodeID, "c1", 20))
	if status != http.StatusCreated {
		t.Fatalf("the creation returned %d: %v", status, payload)
	}
	id, _ := payload["id"].(string)
	readTestAgentFrame(t, control.agent)

	sendTestAgentFrame(t, control.agent, protocol.TypeLogsFrame, protocol.LogFrame{
		SubscriptionID: id,
		Offset:         0,
		Text:           "hello\n",
	})
	if event := stream.next(t); event.Truncated {
		t.Fatalf("the first piece = %+v, want no gap", event)
	}

	sendTestAgentFrame(t, control.agent, protocol.TypeLogsFrame, protocol.LogFrame{
		SubscriptionID: id,
		Offset:         20,
		Text:           "world\n",
	})
	event := stream.next(t)
	if !event.Truncated {
		t.Fatalf("the piece after the gap = %+v, want the gap marked", event)
	}
}

// TestASubscriptionThatStartsAtANonZeroOffsetIsTruncated proves that the
// first frame of a subscription that does not start at the beginning of its
// stream is marked as a gap. A subscription that joins a shared read of the
// agent may start in the middle of the tail of the container.
func TestASubscriptionThatStartsAtANonZeroOffsetIsTruncated(t *testing.T) {
	control := newLogTestControl(t)
	stream := openEventStream(t, control.authServer)

	payload, status := control.createLogSubscription(t, subscriptionRequest(control.nodeID, "c1", 20))
	if status != http.StatusCreated {
		t.Fatalf("the creation returned %d: %v", status, payload)
	}
	id, _ := payload["id"].(string)
	readTestAgentFrame(t, control.agent)

	sendTestAgentFrame(t, control.agent, protocol.TypeLogsFrame, protocol.LogFrame{
		SubscriptionID: id,
		Offset:         120,
		Text:           "the tail that survived\n",
	})
	event := stream.next(t)
	if !event.Truncated {
		t.Fatalf("the first piece = %+v, want the missing head marked", event)
	}
	if event.Offset != 120 || event.Text != "the tail that survived\n" {
		t.Fatalf("the event = %+v, want the first piece of the subscription", event)
	}
}

// TestARevokedNodeCannotBeALogTarget proves that a revoked node is not a
// target of a new subscription.
func TestARevokedNodeCannotBeALogTarget(t *testing.T) {
	control := newLogTestControl(t)

	response := control.post("/api/v1/nodes/"+control.nodeID+"/revoke", nil,
		withHeader("X-CSRF-Token", control.csrf))
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("the revocation returned %d, want %d", response.StatusCode, http.StatusNoContent)
	}

	payload, status := control.createLogSubscription(t, subscriptionRequest(control.nodeID, "c1", 20))
	if status != http.StatusNotFound {
		t.Fatalf("the creation returned %d, want %d: %v", status, http.StatusNotFound, payload)
	}
	if payload["code"] != "node_not_found" {
		t.Fatalf("the code = %v, want node_not_found", payload["code"])
	}
}

// TestLogSubscriptionsValidateTheTargetAndTheTail proves that the browser
// cannot name an arbitrary target and that the limits of the tail hold.
func TestLogSubscriptionsValidateTheTargetAndTheTail(t *testing.T) {
	control := newLogTestControl(t)
	// A subscription needs an event stream that carries its pieces.
	openEventStream(t, control.authServer)

	cases := []struct {
		name   string
		body   map[string]any
		status int
		code   string
	}{
		{
			name:   "unknown node",
			body:   subscriptionRequest("node-that-does-not-exist", "c1", 200),
			status: http.StatusNotFound,
			code:   "node_not_found",
		},
		{
			name:   "container of another node",
			body:   subscriptionRequest(control.nodeID, "container-that-does-not-exist", 200),
			status: http.StatusNotFound,
			code:   "container_not_found",
		},
		{
			name:   "the tail is above the limit",
			body:   subscriptionRequest(control.nodeID, "c1", protocol.MaxLogTail+1),
			status: http.StatusBadRequest,
			code:   "validation_failed",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			payload, status := control.createLogSubscription(t, c.body)
			if status != c.status {
				t.Fatalf("the status = %d, want %d: %v", status, c.status, payload)
			}
			if payload["code"] != c.code {
				t.Fatalf("the code = %v, want %q", payload["code"], c.code)
			}
		})
	}

	// The CSRF token guards the creation.
	response := control.post("/api/v1/log-subscriptions", subscriptionRequest(control.nodeID, "c1", 200))
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("the creation without the CSRF token returned %d, want %d", response.StatusCode, http.StatusForbidden)
	}

	// The creation belongs to the origin of the control端.
	response = control.do(http.MethodPost, "/api/v1/log-subscriptions",
		subscriptionRequest(control.nodeID, "c1", 200),
		withOrigin("https://another.example"), withHeader("X-CSRF-Token", control.csrf))
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("the cross origin creation returned %d, want %d", response.StatusCode, http.StatusForbidden)
	}

	// The default of the tail is 200 and a subscription follows by default.
	payload, status := control.createLogSubscription(t, map[string]any{
		"node_id":      control.nodeID,
		"container_id": "c1",
	})
	if status != http.StatusCreated {
		t.Fatalf("the creation returned %d: %v", status, payload)
	}
	id, _ := payload["id"].(string)
	envelope := readTestAgentFrame(t, control.agent)
	var request protocol.LogRequest
	if err := json.Unmarshal(envelope.Payload, &request); err != nil {
		t.Fatalf("decode the log request: %v", err)
	}
	if request.Tail != protocol.DefaultLogTail || !request.Follow {
		t.Fatalf("the request = %+v, want the default tail and follow", request)
	}
	deleted := control.deleteLogSubscription(t, id)
	deleted.Body.Close()

	// A request without the session of the administrator is refused.
	anonymous := &http.Client{Transport: control.server.Client().Transport}
	requestBody, err := json.Marshal(subscriptionRequest(control.nodeID, "c1", 200))
	if err != nil {
		t.Fatalf("encode the request: %v", err)
	}
	httpRequest, err := http.NewRequest(http.MethodPost, control.server.URL+"/api/v1/log-subscriptions",
		bytes.NewReader(requestBody))
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	httpRequest.Header.Set("Origin", control.server.URL)
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err = anonymous.Do(httpRequest)
	if err != nil {
		t.Fatalf("send the request: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the anonymous creation returned %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
}

// TestALogSubscriptionNeedsAConnectedNode proves that a node without a live
// inventory cannot be a target.
func TestALogSubscriptionNeedsAConnectedNode(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()
	token := server.createAgentToken(t, csrf)
	payload := server.enroll(t, token, nodeIdentity)
	nodeID, _ := payload["node_id"].(string)

	response := server.post("/api/v1/log-subscriptions",
		subscriptionRequest(nodeID, "c1", 200), withHeader("X-CSRF-Token", csrf))
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("the creation returned %d, want %d", response.StatusCode, http.StatusServiceUnavailable)
	}
	answer := server.decode(response)
	if answer["code"] != "node_unavailable" {
		t.Fatalf("the code = %v, want node_unavailable", answer["code"])
	}
}

// TestALogSubscriptionBelongsToItsSession proves that another session cannot
// delete the subscription of the administrator.
func TestALogSubscriptionBelongsToItsSession(t *testing.T) {
	control := newLogTestControl(t)
	openEventStream(t, control.authServer)
	payload, status := control.createLogSubscription(t, subscriptionRequest(control.nodeID, "c1", 200))
	if status != http.StatusCreated {
		t.Fatalf("the creation returned %d: %v", status, payload)
	}
	id, _ := payload["id"].(string)
	readTestAgentFrame(t, control.agent)

	client, csrf := loginAnotherSession(t, control.authServer)
	request, err := http.NewRequest(http.MethodDelete,
		control.server.URL+"/api/v1/log-subscriptions/"+id, nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	request.Header.Set("Origin", control.server.URL)
	request.Header.Set("X-CSRF-Token", csrf)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("send the request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("the foreign deletion returned %d, want %d", response.StatusCode, http.StatusNotFound)
	}

	deleted := control.deleteLogSubscription(t, id)
	defer deleted.Body.Close()
	if deleted.StatusCode != http.StatusNoContent {
		t.Fatalf("the deletion returned %d, want %d", deleted.StatusCode, http.StatusNoContent)
	}
	if envelope := readTestAgentFrame(t, control.agent); envelope.Type != protocol.TypeLogsUnsubscribe {
		t.Fatalf("the frame = %q, want %q", envelope.Type, protocol.TypeLogsUnsubscribe)
	}
}

// TestClosingTheEventStreamStopsTheSessionSubscriptions proves that a
// browser that loses its event stream does not leave the source of the logs
// open.
func TestClosingTheEventStreamStopsTheSessionSubscriptions(t *testing.T) {
	control := newLogTestControl(t)
	stream := openEventStream(t, control.authServer)

	payload, status := control.createLogSubscription(t, subscriptionRequest(control.nodeID, "c1", 200))
	if status != http.StatusCreated {
		t.Fatalf("the creation returned %d: %v", status, payload)
	}
	id, _ := payload["id"].(string)
	readTestAgentFrame(t, control.agent)

	sendTestAgentFrame(t, control.agent, protocol.TypeLogsFrame, protocol.LogFrame{
		SubscriptionID: id,
		Text:           "before the close\n",
	})
	if event := stream.next(t); event.Text != "before the close\n" {
		t.Fatalf("the event = %+v, want the line before the close", event)
	}

	stream.close()

	envelope := readTestAgentFrame(t, control.agent)
	if envelope.Type != protocol.TypeLogsUnsubscribe {
		t.Fatalf("the frame = %q, want %q", envelope.Type, protocol.TypeLogsUnsubscribe)
	}
	var request protocol.LogRequest
	if err := json.Unmarshal(envelope.Payload, &request); err != nil {
		t.Fatalf("decode the log request: %v", err)
	}
	if request.SubscriptionID != id {
		t.Fatalf("the unsubscribe names %q, want %q", request.SubscriptionID, id)
	}
}

// waitForFrameType waits for the next frame of the given type and ignores
// the other frames of the control端.
func (r *agentFrameReader) waitForFrameType(t *testing.T, messageType string) protocol.Envelope {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("timeout while waiting for a %s frame", messageType)
		}
		select {
		case envelope := <-r.frames:
			if envelope.Type == messageType {
				return envelope
			}
		case err := <-r.errors:
			t.Fatalf("read the frame of the control端: %v", err)
		case <-time.After(remaining):
			t.Fatalf("timeout while waiting for a %s frame", messageType)
		}
	}
}

// TestACreationNeedsAnOpenEventStream proves that a subscription cannot be
// created without an event stream: a browser that creates one before it
// opens the stream would lose the first pieces of the tail.
func TestACreationNeedsAnOpenEventStream(t *testing.T) {
	control := newLogTestControl(t)
	frames := startAgentFrameReader(t, control.agent)

	payload, status := control.createLogSubscription(t, subscriptionRequest(control.nodeID, "c1", 50))
	if status != http.StatusConflict {
		t.Fatalf("the creation without a stream returned %d, want %d: %v", status, http.StatusConflict, payload)
	}
	if payload["code"] != "event_stream_required" {
		t.Fatalf("the code = %v, want event_stream_required", payload["code"])
	}
	if !frames.absent(t, 300*time.Millisecond) {
		t.Fatal("the control端 asked for the logs without an event stream")
	}

	openEventStream(t, control.authServer)
	payload, status = control.createLogSubscription(t, subscriptionRequest(control.nodeID, "c1", 50))
	if status != http.StatusCreated {
		t.Fatalf("the creation with a stream returned %d: %v", status, payload)
	}
	if frame := frames.next(t); frame.Type != protocol.TypeLogsSubscribe {
		t.Fatalf("the frame = %q, want %q", frame.Type, protocol.TypeLogsSubscribe)
	}
}

// TestTheSubscriptionsAreResentWhenTheAgentReconnects proves that a page
// keeps its stream when the agent reconnects: the control端 starts the
// subscription of the new connection again with the same ID, and the pieces
// of the new read reach the page.
func TestTheSubscriptionsAreResentWhenTheAgentReconnects(t *testing.T) {
	t.Cleanup(api.SetLogReconnectGraceForTest(10 * time.Second))

	control := newLogTestControl(t)
	stream := openEventStream(t, control.authServer)

	payload, status := control.createLogSubscription(t, subscriptionRequest(control.nodeID, "c1", 50))
	if status != http.StatusCreated {
		t.Fatalf("the creation returned %d: %v", status, payload)
	}
	id, _ := payload["id"].(string)
	readTestAgentFrame(t, control.agent)

	sendTestAgentFrame(t, control.agent, protocol.TypeLogsFrame, protocol.LogFrame{
		SubscriptionID: id,
		Text:           "before the reconnect\n",
	})
	if event := stream.next(t); event.Text != "before the reconnect\n" {
		t.Fatalf("the event = %+v, want the line before the reconnect", event)
	}

	// The agent loses its connection and opens a new one.
	control.agent.CloseNow()
	replacement := dialTestAgent(t, control.authServer, control.credential)
	sendTestAgentFrame(t, replacement, protocol.TypeHello, protocol.Hello{Identity: nodeIdentity})
	sendTestAgentFrame(t, replacement, protocol.TypeInventory, protocol.InventoryBatch{
		SnapshotID:  "snapshot-2",
		BatchIndex:  0,
		BatchCount:  1,
		Complete:    true,
		Identity:    nodeIdentity,
		Containers:  []protocol.Container{{ID: "c1", Name: "web", Image: "nginx", State: "running"}},
		CollectedAt: time.Now().UTC(),
	})

	frames := startAgentFrameReader(t, replacement)
	resend := frames.waitForFrameType(t, protocol.TypeLogsSubscribe)
	var request protocol.LogRequest
	if err := json.Unmarshal(resend.Payload, &request); err != nil {
		t.Fatalf("decode the resent log request: %v", err)
	}
	if request.SubscriptionID != id || request.ContainerID != "c1" || request.Tail != 50 || !request.Follow {
		t.Fatalf("the resent request = %+v, want the subscription of the page", request)
	}

	sendTestAgentFrame(t, replacement, protocol.TypeLogsFrame, protocol.LogFrame{
		SubscriptionID: id,
		Text:           "after the reconnect\n",
	})
	marker := false
	line := false
	for !marker || !line {
		event := stream.next(t)
		if event.ObjectID != id {
			continue
		}
		switch {
		case event.Truncated && event.Text == "":
			marker = true
		case event.Text == "after the reconnect\n":
			line = true
		}
	}
}

// TestALostAgentEndsItsSubscriptionsWithAnError proves that a subscription
// does not wait for pieces that cannot arrive: a node that stays away ends
// its subscriptions with an error event after the grace period.
func TestALostAgentEndsItsSubscriptionsWithAnError(t *testing.T) {
	t.Cleanup(api.SetLogReconnectGraceForTest(100 * time.Millisecond))

	control := newLogTestControl(t)
	stream := openEventStream(t, control.authServer)

	payload, status := control.createLogSubscription(t, subscriptionRequest(control.nodeID, "c1", 50))
	if status != http.StatusCreated {
		t.Fatalf("the creation returned %d: %v", status, payload)
	}
	id, _ := payload["id"].(string)
	readTestAgentFrame(t, control.agent)

	control.agent.CloseNow()

	event := stream.next(t)
	if event.Type != "log" || event.ObjectID != id || event.ErrorCode != "node_disconnected" {
		t.Fatalf("the event = %+v, want the end of the subscription of the lost node", event)
	}

	// The state of the ended subscription is gone.
	response := control.deleteLogSubscription(t, id)
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("the deletion after the end returned %d, want %d", response.StatusCode, http.StatusNotFound)
	}
}

// TestARevokedNodeEndsItsLogSubscriptions proves that revoking a node ends
// the subscriptions of its pages with an explicit error event.
func TestARevokedNodeEndsItsLogSubscriptions(t *testing.T) {
	control := newLogTestControl(t)
	stream := openEventStream(t, control.authServer)

	payload, status := control.createLogSubscription(t, subscriptionRequest(control.nodeID, "c1", 50))
	if status != http.StatusCreated {
		t.Fatalf("the creation returned %d: %v", status, payload)
	}
	id, _ := payload["id"].(string)
	readTestAgentFrame(t, control.agent)

	response := control.post("/api/v1/nodes/"+control.nodeID+"/revoke", nil,
		withHeader("X-CSRF-Token", control.csrf))
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("the revocation returned %d, want %d", response.StatusCode, http.StatusNoContent)
	}

	event := stream.next(t)
	if event.Type != "log" || event.ObjectID != id || event.ErrorCode != "node_revoked" {
		t.Fatalf("the event = %+v, want the end of the subscription of the revoked node", event)
	}
}

// loginAnotherSession opens a second session of the administrator in a
// client of its own.
func loginAnotherSession(t *testing.T, server *authServer) (*http.Client, string) {
	t.Helper()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	client := &http.Client{Transport: server.server.Client().Transport, Jar: jar}

	body, err := json.Marshal(map[string]string{"password": adminPassword})
	if err != nil {
		t.Fatalf("encode the login: %v", err)
	}
	request, err := http.NewRequest(http.MethodPost, server.server.URL+"/api/v1/auth/login",
		bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build the login: %v", err)
	}
	request.Header.Set("Origin", server.server.URL)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("send the login: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		contents, _ := io.ReadAll(response.Body)
		t.Fatalf("the second login returned %d: %s", response.StatusCode, contents)
	}
	var payload struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decode the login: %v", err)
	}
	if payload.CSRF == "" {
		t.Fatal("the login answered no CSRF token")
	}
	return client, payload.CSRF
}
