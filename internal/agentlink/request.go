package agentlink

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"hmoob-dock/internal/protocol"
)

// The errors of a request to a node.
var (
	// ErrRequestTimeout reports a node that did not answer in time. An
	// operation that answered too late may still run on the node, so a
	// timeout is not a proof of failure.
	ErrRequestTimeout = errors.New("agentlink: the node did not answer the request in time")
	// ErrRequestRefused reports a node that answered the command with a
	// failure.
	ErrRequestRefused = errors.New("agentlink: the node refused the request")
)

// pendingRequest is one command of the control端 that waits for the result
// of its node.
type pendingRequest struct {
	nodeID string
	result chan protocol.ContainerResult
}

// requestID returns a random identifier of a request. The agent carries it
// back in its result, and the results of two requests never share an ID.
func newRequestID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// A random source that fails once fails for the process. The
		// fallback keeps the requests apart by the time instead.
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(raw[:])
}

// Request sends a container command to a node and waits for its result.
//
// The answer of one command is exactly one frame, so the result either
// arrives or the request times out. A timeout does not prove that the
// operation failed: the node may still be running it, and the caller of the
// request has to say so instead of claiming a result. The timeout bounds
// the wait; a stop with its grace period fits into the timeout of the
// platform with room to spare.
func (h *Hub) Request(ctx context.Context, nodeID, action, containerID string, timeout time.Duration) (protocol.ContainerResult, error) {
	if _, ok := h.connection(nodeID); !ok {
		return protocol.ContainerResult{}, ErrNodeNotConnected
	}

	requestID := newRequestID()
	command := protocol.ContainerCommand{
		RequestID:   requestID,
		Action:      action,
		ContainerID: containerID,
	}
	payload, err := json.Marshal(command)
	if err != nil {
		return protocol.ContainerResult{}, fmt.Errorf("agentlink: encode the container command: %w", err)
	}

	pending := &pendingRequest{nodeID: nodeID, result: make(chan protocol.ContainerResult, 1)}
	h.pendingMu.Lock()
	h.pending[requestID] = pending
	h.pendingMu.Unlock()

	// The pending request is dropped in every path that leaves this
	// function, so a lost frame never holds the memory of the hub.
	defer func() {
		h.pendingMu.Lock()
		delete(h.pending, requestID)
		h.pendingMu.Unlock()
	}()

	envelope := protocol.Envelope{
		Version:   protocol.WireVersion,
		Type:      protocol.TypeContainerCommand,
		RequestID: requestID,
		Payload:   payload,
	}
	if err := h.Send(ctx, nodeID, envelope); err != nil {
		return protocol.ContainerResult{}, err
	}

	deadline := time.After(timeout)
	select {
	case result := <-pending.result:
		if !result.OK {
			return result, fmt.Errorf("%w: %s", ErrRequestRefused, result.Message)
		}
		return result, nil
	case <-deadline:
		return protocol.ContainerResult{}, ErrRequestTimeout
	case <-ctx.Done():
		return protocol.ContainerResult{}, ctx.Err()
	}
}

// resolvePending hands the result of a node to the request that waits for
// it. A result without a pending request is dropped: the request either
// timed out or belonged to a connection that ended.
func (h *Hub) resolvePending(nodeID string, envelope protocol.Envelope) {
	result, err := protocol.DecodeContainerResult(envelope.Payload)
	if err != nil {
		// The frame of a node that does not follow the protocol is not
		// accepted in the first place; this guard keeps a malformed
		// payload from reaching a caller.
		return
	}
	h.pendingMu.Lock()
	pending, ok := h.pending[result.RequestID]
	if ok && pending.nodeID != nodeID {
		// The result names the request of another node: the connection of
		// a node never answers for the requests of another one.
		ok = false
	}
	if ok {
		delete(h.pending, result.RequestID)
	}
	h.pendingMu.Unlock()
	if ok {
		pending.result <- result
	}
}
