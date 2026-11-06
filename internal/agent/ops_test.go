package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"hmoob-dock/internal/dockerops"
	"hmoob-dock/internal/protocol"
)

// opRecorder is a controller of the operations that records every action
// it was asked to run and answers with the state it was told to report.
type opRecorder struct {
	// states maps the container to the state an inspect reports.
	states map[string]*protocol.Container
	// errOf maps an action to the error the operation returns.
	errOf map[string]error
	// calls records the actions in the order they ran.
	calls []string
	// delay holds the time every operation sleeps, so a test can prove
	// the queueing of the slots.
	delay time.Duration
}

func (r *opRecorder) record(action string) {
	r.calls = append(r.calls, action)
	if r.delay > 0 {
		time.Sleep(r.delay)
	}
}

func (r *opRecorder) StartContainer(_ context.Context, _ string) error {
	r.record(protocol.ContainerActionStart)
	return r.errOf[protocol.ContainerActionStart]
}

func (r *opRecorder) StopContainer(_ context.Context, _ string) error {
	r.record(protocol.ContainerActionStop)
	return r.errOf[protocol.ContainerActionStop]
}

func (r *opRecorder) RestartContainer(_ context.Context, _ string) error {
	r.record(protocol.ContainerActionRestart)
	return r.errOf[protocol.ContainerActionRestart]
}

func (r *opRecorder) RemoveContainer(_ context.Context, _ string) error {
	r.record(protocol.ContainerActionRemove)
	return r.errOf[protocol.ContainerActionRemove]
}

func (r *opRecorder) InspectContainer(_ context.Context, containerID string) (*protocol.Container, error) {
	state, ok := r.states[containerID]
	if !ok {
		return nil, dockerops.ErrContainerNotFound
	}
	return state, nil
}

// opOut collects the frames the manager answers with.
type opOut struct {
	mu     sync.Mutex
	frames []protocol.ContainerResult
}

func (o *opOut) sendControl(messageType string, payload any) error {
	if messageType != protocol.TypeContainerResult {
		return errors.New("the manager answered with a frame that is not a result")
	}
	frame, _ := payload.(protocol.ContainerResult)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.frames = append(o.frames, frame)
	return nil
}

// results returns a copy of the frames the manager answered with.
func (o *opOut) results() []protocol.ContainerResult {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]protocol.ContainerResult(nil), o.frames...)
}

func (o *opOut) sendLog(string, any) error { return nil }

// commandEnvelope builds the envelope of a container command.
func commandEnvelope(requestID, action, containerID string) protocol.Envelope {
	payload, _ := json.Marshal(protocol.ContainerCommand{
		RequestID: requestID, Action: action, ContainerID: containerID,
	})
	return protocol.Envelope{Version: protocol.WireVersion, Type: protocol.TypeContainerCommand, Payload: payload}
}

// waitForResult waits until the manager answered with the frame of the
// given request.
func waitForResult(t *testing.T, out *opOut, requestID string) protocol.ContainerResult {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, frame := range out.results() {
			if frame.RequestID == requestID {
				return frame
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the manager never answered the request %s", requestID)
	return protocol.ContainerResult{}
}

// TestTheOperationOfAContainerIsAnsweredWithItsState proves that a well
// formed command runs on the engine and answers with exactly one result
// that carries the state of the container.
func TestTheOperationOfAContainerIsAnsweredWithItsState(t *testing.T) {
	state := &protocol.Container{ID: "abcdef1234567890", Name: "demo", State: "exited"}
	recorder := &opRecorder{states: map[string]*protocol.Container{"abcdef1234567890": state}}
	manager := newOpsManager(context.Background(), recorder)
	defer manager.Close()
	out := &opOut{}

	manager.handle(commandEnvelope("r1", protocol.ContainerActionStop, "abcdef1234567890"), out)
	result := waitForResult(t, out, "r1")

	if !result.OK || result.Container == nil || result.Container.State != "exited" {
		t.Fatalf("the result does not carry the state of the container: %+v", result)
	}
	if !slices.Equal(recorder.calls, []string{protocol.ContainerActionStop}) {
		t.Fatalf("the engine saw %v, want exactly one stop", recorder.calls)
	}
	if len(out.results()) != 1 {
		t.Fatalf("the manager answered %d times, want once", len(out.results()))
	}
}

// TestTheRemoveOfAContainerReportsNoState proves that a removed container
// succeeds without a state, because there is nothing to inspect any more.
func TestTheRemoveOfAContainerReportsNoState(t *testing.T) {
	recorder := &opRecorder{states: map[string]*protocol.Container{}}
	manager := newOpsManager(context.Background(), recorder)
	defer manager.Close()
	out := &opOut{}

	manager.handle(commandEnvelope("r1", protocol.ContainerActionRemove, "abcdef1234567890"), out)
	result := waitForResult(t, out, "r1")

	if !result.OK || result.Container != nil {
		t.Fatalf("the removal of a container succeeded with a state: %+v", result)
	}
}

// TestAFailedOperationCarriesItsReason proves that the errors of the engine
// reach the control端 with the reason the page shows.
func TestAFailedOperationCarriesItsReason(t *testing.T) {
	recorder := &opRecorder{errOf: map[string]error{
		protocol.ContainerActionStop:    dockerops.ErrContainerNotFound,
		protocol.ContainerActionRestart: errors.New("the engine refused the operation"),
	}}
	manager := newOpsManager(context.Background(), recorder)
	defer manager.Close()
	out := &opOut{}

	manager.handle(commandEnvelope("r1", protocol.ContainerActionStop, "abcdef1234567890"), out)
	result := waitForResult(t, out, "r1")
	if result.OK || result.ErrorCode != protocol.ResultCodeNotFound {
		t.Fatalf("a missing container was not reported as missing: %+v", result)
	}

	manager.handle(commandEnvelope("r2", protocol.ContainerActionRestart, "abcdef1234567890"), out)
	result = waitForResult(t, out, "r2")
	if result.OK || result.ErrorCode != protocol.ResultCodeFailed || result.Message == "" {
		t.Fatalf("a refused operation carries no reason: %+v", result)
	}
}

// TestABrokenCommandIsNotForwarded proves that a command the protocol
// refuses never reaches the engine.
func TestABrokenCommandIsNotForwarded(t *testing.T) {
	recorder := &opRecorder{}
	manager := newOpsManager(context.Background(), recorder)
	defer manager.Close()
	out := &opOut{}

	manager.handle(protocol.Envelope{
		Version: protocol.WireVersion,
		Type:    protocol.TypeContainerCommand,
		Payload: []byte(`{"request_id":"r1","action":"exec","container_id":"abcdef1234567890"}`),
	}, out)

	time.Sleep(50 * time.Millisecond)
	if len(recorder.calls) != 0 {
		t.Fatalf("the engine saw %v, want nothing", recorder.calls)
	}
	if len(out.results()) != 0 {
		t.Fatalf("the manager answered a broken command: %v", out.results())
	}
}

// TestTheOperationsOfANodeAreBounded proves that a node runs a bounded
// number of the operations at the same time, so a flood of the commands
// does not exhaust the engine.
func TestTheOperationsOfANodeAreBounded(t *testing.T) {
	recorder := &opRecorder{
		delay: 100 * time.Millisecond,
		states: map[string]*protocol.Container{
			"abcdef1234567890": {ID: "abcdef1234567890", State: "running"},
		},
	}
	manager := newOpsManager(context.Background(), recorder)
	defer manager.Close()
	out := &opOut{}

	for index := 0; index < maxConcurrentOps+2; index++ {
		manager.handle(commandEnvelope("r"+string(rune('a'+index)), protocol.ContainerActionStart, "abcdef1234567890"), out)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(recorder.calls) < maxConcurrentOps+2 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(recorder.calls) != maxConcurrentOps+2 {
		t.Fatalf("the engine saw %d operations, want %d", len(recorder.calls), maxConcurrentOps+2)
	}
	for index := 0; index < maxConcurrentOps+2; index++ {
		waitForResult(t, out, "r"+string(rune('a'+index)))
	}
}

// TestTheCloseOfTheManagerEndsTheRunningOperations proves that a manager
// that closes no longer starts operations and waits for the ones that run.
func TestTheCloseOfTheManagerEndsTheRunningOperations(t *testing.T) {
	recorder := &opRecorder{}
	manager := newOpsManager(context.Background(), recorder)
	out := &opOut{}

	manager.handle(commandEnvelope("r1", protocol.ContainerActionStart, "abcdef1234567890"), out)
	manager.Close()
	// A second command after the close is answered with a refusal, and
	// the engine never sees it.
	manager.handle(commandEnvelope("r2", protocol.ContainerActionStart, "abcdef1234567890"), out)
	result := waitForResult(t, out, "r2")

	if result.OK {
		t.Fatalf("a command after the close succeeded: %+v", result)
	}
	time.Sleep(50 * time.Millisecond)
	if len(recorder.calls) != 1 {
		t.Fatalf("the engine saw %d operations, want exactly the one before the close", len(recorder.calls))
	}
}
