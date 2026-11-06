package agentlink_test

import (
	"encoding/json"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coder/websocket"

	"hmoob-dock/internal/agentlink"
	"hmoob-dock/internal/protocol"
)

// TestARequestIsAnsweredByTheResultOfTheNode proves that the control端
// receives the result a node answers with and that the answer carries the
// state the node read after the operation.
func TestARequestIsAnsweredByTheResultOfTheNode(t *testing.T) {
	h := newTestHub(t)
	identity := protocol.NodeIdentity{EngineID: "engine-a"}
	credential := h.enroll(t, identity)
	conn := h.connect(t, credential)
	send(t, conn, hello(t, identity))
	waitFor(t, "the node to be connected", func() bool {
		return h.hub.Connected(credential.NodeID)
	})

	type answer struct {
		result protocol.ContainerResult
		err    error
	}
	answers := make(chan answer, 1)
	go func() {
		result, err := h.hub.Request(context.Background(), credential.NodeID,
			protocol.ContainerActionStop, "abcdef1234567890", 5*time.Second)
		answers <- answer{result, err}
	}()

	// The agent reads the command and answers with the result that names
	// the same request.
	command := readEnvelope(t, conn, protocol.TypeContainerCommand)
	var request protocol.ContainerCommand
	decodePayload(t, command.Payload, &request)
	result := protocol.ContainerResult{
		RequestID: request.RequestID,
		OK:        true,
		Container: &protocol.Container{ID: request.ContainerID, Name: "demo", State: "exited"},
	}
	payload := mustMarshal(t, result)
	send(t, conn, protocol.Envelope{
		Version:   protocol.WireVersion,
		Type:      protocol.TypeContainerResult,
		RequestID: request.RequestID,
		Seq:       3,
		Payload:   payload,
	})

	received := <-answers
	if received.err != nil {
		t.Fatalf("the request failed: %v", received.err)
	}
	if received.result.Container == nil || received.result.Container.State != "exited" {
		t.Fatalf("the result carries no state: %+v", received.result)
	}
}

// TestARefusedRequestCarriesTheReasonOfTheNode proves that a failure of the
// node reaches the control端 as a refusal and not as a transport error.
func TestARefusedRequestCarriesTheReasonOfTheNode(t *testing.T) {
	h := newTestHub(t)
	identity := protocol.NodeIdentity{EngineID: "engine-a"}
	credential := h.enroll(t, identity)
	conn := h.connect(t, credential)
	send(t, conn, hello(t, identity))
	waitFor(t, "the node to be connected", func() bool {
		return h.hub.Connected(credential.NodeID)
	})

	answers := make(chan error, 1)
	go func() {
		_, err := h.hub.Request(context.Background(), credential.NodeID,
			protocol.ContainerActionRemove, "abcdef1234567890", 5*time.Second)
		answers <- err
	}()

	command := readEnvelope(t, conn, protocol.TypeContainerCommand)
	var request protocol.ContainerCommand
	decodePayload(t, command.Payload, &request)
	result := protocol.ContainerResult{
		RequestID: request.RequestID,
		OK:        false,
		ErrorCode: protocol.ResultCodeFailed,
		Message:   "the engine refused the removal",
	}
	send(t, conn, protocol.Envelope{
		Version:   protocol.WireVersion,
		Type:      protocol.TypeContainerResult,
		RequestID: request.RequestID,
		Seq:       3,
		Payload:   mustMarshal(t, result),
	})

	err := <-answers
	if !errors.Is(err, agentlink.ErrRequestRefused) {
		t.Fatalf("the refusal was not reported: %v", err)
	}
}

// TestARequestTimesOutWhenTheNodeDoesNotAnswer proves that a waiting
// request ends with a timeout and never claims a result.
func TestARequestTimesOutWhenTheNodeDoesNotAnswer(t *testing.T) {
	h := newTestHub(t)
	identity := protocol.NodeIdentity{EngineID: "engine-a"}
	credential := h.enroll(t, identity)
	conn := h.connect(t, credential)
	send(t, conn, hello(t, identity))
	waitFor(t, "the node to be connected", func() bool {
		return h.hub.Connected(credential.NodeID)
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := h.hub.Request(context.Background(), credential.NodeID,
			protocol.ContainerActionStop, "abcdef1234567890", 150*time.Millisecond)
		if !errors.Is(err, agentlink.ErrRequestTimeout) {
			t.Errorf("the request did not time out: %v", err)
		}
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the request did not return after its timeout")
	}
}

// TestARequestToAnUnknownNodeIsRefused proves that a request for a node
// without a connection fails at once.
func TestARequestToAnUnknownNodeIsRefused(t *testing.T) {
	h := newTestHub(t)
	_, err := h.hub.Request(context.Background(), "node-that-is-away",
		protocol.ContainerActionStop, "abcdef1234567890", time.Second)
	if !errors.Is(err, agentlink.ErrNodeNotConnected) {
		t.Fatalf("the request to an away node was not refused: %v", err)
	}
}

// TestTheResultOfAnotherNodeNeverResolvesARequest proves that the result
// frame of one node cannot answer the request of another one.
func TestTheResultOfAnotherNodeNeverResolvesARequest(t *testing.T) {
	h := newTestHub(t)
	first := h.enroll(t, protocol.NodeIdentity{EngineID: "engine-a"})
	second := h.enroll(t, protocol.NodeIdentity{EngineID: "engine-b"})
	firstConn := h.connect(t, first)
	send(t, firstConn, hello(t, protocol.NodeIdentity{EngineID: "engine-a"}))
	secondConn := h.connect(t, second)
	send(t, secondConn, hello(t, protocol.NodeIdentity{EngineID: "engine-b"}))
	waitFor(t, "both nodes to be connected", func() bool {
		return h.hub.Connected(first.NodeID) && h.hub.Connected(second.NodeID)
	})

	answers := make(chan error, 1)
	go func() {
		_, err := h.hub.Request(context.Background(), first.NodeID,
			protocol.ContainerActionStop, "abcdef1234567890", 700*time.Millisecond)
		answers <- err
	}()

	// The first node receives the command; the second node answers with a
	// result that carries the request ID it never saw.
	command := readEnvelope(t, firstConn, protocol.TypeContainerCommand)
	var request protocol.ContainerCommand
	decodePayload(t, command.Payload, &request)
	result := protocol.ContainerResult{RequestID: request.RequestID, OK: true}
	send(t, secondConn, protocol.Envelope{
		Version:   protocol.WireVersion,
		Type:      protocol.TypeContainerResult,
		RequestID: request.RequestID,
		Seq:       3,
		Payload:   mustMarshal(t, result),
	})

	if err := <-answers; !errors.Is(err, agentlink.ErrRequestTimeout) {
		t.Fatalf("the result of another node resolved the request: %v", err)
	}
}

// readEnvelope reads one frame of the connection and proves its type.
func readEnvelope(t *testing.T, conn *websocket.Conn, messageType string) protocol.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read the frame: %v", err)
	}
	envelope, err := protocol.Decode(data)
	if err != nil {
		t.Fatalf("decode the frame: %v", err)
	}
	if envelope.Type != messageType {
		t.Fatalf("the frame is a %s, want a %s", envelope.Type, messageType)
	}
	return envelope
}

// decodePayload reads the payload of a frame into the given message.
func decodePayload(t *testing.T, payload []byte, into any) {
	t.Helper()
	if err := json.Unmarshal(payload, into); err != nil {
		t.Fatalf("decode the payload: %v", err)
	}
}

// mustMarshal encodes a payload or stops the test.
func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode the payload: %v", err)
	}
	return data
}
