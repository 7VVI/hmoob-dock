package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"hmoob-dock/internal/agentlink"
	"hmoob-dock/internal/protocol"
)

// gatedFrame is a frame that the agent handed to its sender.
type gatedFrame struct {
	messageType string
	payload     any
}

// gatedSender collects the frames of the agent. A gate may hold the log
// frames, so a test can play the slow consumer of the control端.
type gatedSender struct {
	mu      sync.Mutex
	control []gatedFrame
	logs    []gatedFrame
	logErr  error
	logGate <-chan struct{}
}

// sendControl records a control frame.
func (s *gatedSender) sendControl(messageType string, payload any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.control = append(s.control, gatedFrame{messageType: messageType, payload: payload})
	return nil
}

// sendLog records a log frame. The gate delays the frame like a slow
// control端 that does not read its queue.
func (s *gatedSender) sendLog(messageType string, payload any) error {
	if s.logGate != nil {
		select {
		case <-s.logGate:
		case <-time.After(5 * time.Second):
			return errors.New("the test sender did not release the log frame")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logErr != nil {
		return s.logErr
	}
	s.logs = append(s.logs, gatedFrame{messageType: messageType, payload: payload})
	return nil
}

// frames returns a copy of the recorded frames of a kind.
func (s *gatedSender) frames(control bool) []gatedFrame {
	s.mu.Lock()
	defer s.mu.Unlock()
	if control {
		return append([]gatedFrame(nil), s.control...)
	}
	return append([]gatedFrame(nil), s.logs...)
}

// telemetryEngine is a Docker engine of the tests. It counts the samples
// that run at the same time and honours the context of a sample.
type telemetryEngine struct {
	identity  protocol.NodeIdentity
	inventory protocol.Inventory

	mu        sync.Mutex
	delays    map[string]time.Duration
	failures  map[string]error
	samples   []protocol.RawSample
	active    atomic.Int32
	maxActive atomic.Int32
}

// Identity returns the identity of the engine.
func (e *telemetryEngine) Identity(context.Context) (protocol.NodeIdentity, error) {
	return e.identity, nil
}

// Inventory returns the containers of the engine.
func (e *telemetryEngine) Inventory(context.Context) (protocol.Inventory, error) {
	return e.inventory, nil
}

// Sample returns a sample of a container after the delay of the test.
func (e *telemetryEngine) Sample(ctx context.Context, containerID string) (protocol.RawSample, error) {
	active := e.active.Add(1)
	defer e.active.Add(-1)
	for {
		maximum := e.maxActive.Load()
		if active <= maximum || e.maxActive.CompareAndSwap(maximum, active) {
			break
		}
	}

	e.mu.Lock()
	delay := e.delays[containerID]
	failure := e.failures[containerID]
	e.mu.Unlock()

	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return protocol.RawSample{}, ctx.Err()
		case <-timer.C:
		}
	}
	if failure != nil {
		return protocol.RawSample{}, failure
	}

	sample := protocol.RawSample{ContainerID: containerID, CollectedAt: time.Now().UTC()}
	e.mu.Lock()
	e.samples = append(e.samples, sample)
	e.mu.Unlock()
	return sample, nil
}

// sampled returns a copy of the recorded samples.
func (e *telemetryEngine) sampled() []protocol.RawSample {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]protocol.RawSample(nil), e.samples...)
}

// containersOf builds running containers with the given identifiers.
func containersOf(ids ...string) []protocol.Container {
	containers := make([]protocol.Container, 0, len(ids))
	for _, id := range ids {
		containers = append(containers, protocol.Container{ID: id, State: "running"})
	}
	return containers
}

// TestSamplesAreBoundedAndAllContainersAreSampled proves that a round of
// sampling uses a bounded number of requests and still visits every
// container.
func TestSamplesAreBoundedAndAllContainersAreSampled(t *testing.T) {
	ids := make([]string, 0, 14)
	delays := make(map[string]time.Duration, 14)
	for index := 0; index < 14; index++ {
		id := fmt.Sprintf("c%d", index)
		ids = append(ids, id)
		delays[id] = 20 * time.Millisecond
	}
	engine := &telemetryEngine{delays: delays}
	sender := &gatedSender{}

	sampleRound(context.Background(), containersOf(ids...), engine, sender, 8, time.Second)

	samples := engine.sampled()
	if len(samples) != len(ids) {
		t.Fatalf("samples = %d, want %d", len(samples), len(ids))
	}
	if got := int(engine.maxActive.Load()); got > 8 {
		t.Fatalf("%d samples ran at the same time, want at most 8", got)
	}
	if got := int(engine.maxActive.Load()); got < 2 {
		t.Fatalf("%d samples ran at the same time, want a bounded parallel round", got)
	}
	logs := sender.frames(false)
	if len(logs) != len(ids) {
		t.Fatalf("frames = %d, want %d", len(logs), len(ids))
	}
	for _, frame := range logs {
		if frame.messageType != protocol.TypeMetricsSample {
			t.Fatalf("the frame = %q, want %q", frame.messageType, protocol.TypeMetricsSample)
		}
	}
}

// TestASlowContainerDoesNotHoldTheRound proves that a container that does
// not answer within its own timeout does not stop the other containers of
// the round.
func TestASlowContainerDoesNotHoldTheRound(t *testing.T) {
	captureLog(t)
	engine := &telemetryEngine{
		delays: map[string]time.Duration{
			"slow": 5 * time.Second,
			"fast": 0,
		},
	}
	sender := &gatedSender{}

	started := time.Now()
	sampleRound(context.Background(), containersOf("slow", "fast"), engine, sender, 2, 50*time.Millisecond)
	elapsed := time.Since(started)

	if elapsed > time.Second {
		t.Fatalf("the round took %s, want it bounded by the timeout of the slow container", elapsed)
	}
	samples := engine.sampled()
	if len(samples) != 1 || samples[0].ContainerID != "fast" {
		t.Fatalf("samples = %+v, want the sample of the fast container", samples)
	}
}

// TestOnlyRunningContainersAreSampled proves that a container that ended is
// not asked for stats.
func TestOnlyRunningContainersAreSampled(t *testing.T) {
	engine := &telemetryEngine{}
	sender := &gatedSender{}
	containers := []protocol.Container{
		{ID: "running", State: "running"},
		{ID: "exited", State: "exited"},
		{ID: "paused", State: "paused"},
	}

	sampleRound(context.Background(), containers, engine, sender, 8, time.Second)

	samples := engine.sampled()
	if len(samples) != 1 || samples[0].ContainerID != "running" {
		t.Fatalf("samples = %+v, want only the running container", samples)
	}
}

// TestAFailingContainerDoesNotStopTheRound proves that an error of one
// container is reported and the other containers are sampled anyway.
func TestAFailingContainerDoesNotStopTheRound(t *testing.T) {
	captureLog(t)
	engine := &telemetryEngine{
		failures: map[string]error{"broken": errors.New("the container disappeared")},
	}
	sender := &gatedSender{}

	sampleRound(context.Background(), containersOf("broken", "healthy"), engine, sender, 8, time.Second)

	samples := engine.sampled()
	if len(samples) != 1 || samples[0].ContainerID != "healthy" {
		t.Fatalf("samples = %+v, want the sample of the healthy container", samples)
	}
}

// scriptedEvents plays the event stream of Docker.
type scriptedEvents struct {
	mu    sync.Mutex
	calls int
	run   func(context.Context, func(protocol.ContainerEvent) error) error
}

// Events runs the stream of the test.
func (e *scriptedEvents) Events(ctx context.Context, emit func(protocol.ContainerEvent) error) error {
	e.mu.Lock()
	e.calls++
	run := e.run
	e.mu.Unlock()
	return run(ctx, emit)
}

// callCount returns the number of the subscriptions of the test.
func (e *scriptedEvents) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

// TestEventsAreForwardedAndTheStreamIsOpenedAgain proves that the events of
// Docker are sent over the control queue and that a stream that ended is
// opened again without inventing the events of the gap.
func TestEventsAreForwardedAndTheStreamIsOpenedAgain(t *testing.T) {
	previous := eventRetryDelay
	eventRetryDelay = 5 * time.Millisecond
	defer func() { eventRetryDelay = previous }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := &scriptedEvents{}
	engine.run = func(ctx context.Context, emit func(protocol.ContainerEvent) error) error {
		if err := emit(protocol.ContainerEvent{
			ContainerID: "c1",
			Action:      "die",
			OccurredAt:  time.Unix(100, 0).UTC(),
		}); err != nil {
			return err
		}
		return errors.New("the stream ended")
	}
	sender := &gatedSender{}

	done := make(chan struct{})
	go func() {
		defer close(done)
		reportEvents(ctx, engine, sender)
	}()

	waitFor(t, "the event stream to open again", func() bool {
		return engine.callCount() >= 2
	})
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the reporter of the events did not stop")
	}

	frames := sender.frames(true)
	if len(frames) < 2 {
		t.Fatalf("frames = %d, want the event of every stream", len(frames))
	}
	for _, frame := range frames {
		if frame.messageType != protocol.TypeContainerEvent {
			t.Fatalf("the frame = %q, want %q", frame.messageType, protocol.TypeContainerEvent)
		}
		event, ok := frame.payload.(protocol.ContainerEvent)
		if !ok {
			t.Fatalf("the payload = %T, want a protocol.ContainerEvent", frame.payload)
		}
		if event.ContainerID != "c1" || event.Action != "die" {
			t.Fatalf("the event = %+v, want the event of the stream", event)
		}
	}
}

// TestTheTelemetrySpeaksTheRegisteredTypes proves that the control端 accepts
// the frames of the telemetry.
func TestTheTelemetrySpeaksTheRegisteredTypes(t *testing.T) {
	for _, messageType := range []string{
		protocol.TypeMetricsSample,
		protocol.TypeContainerEvent,
		protocol.TypeLogsFrame,
		protocol.TypeLogsSubscribe,
		protocol.TypeLogsUnsubscribe,
	} {
		if !protocol.IsRegisteredType(messageType) {
			t.Fatalf("the protocol does not accept %q", messageType)
		}
	}
}

// TestTheSampleIntervalIsTenSeconds proves the interval of the collection
// of the platform.
func TestTheSampleIntervalIsTenSeconds(t *testing.T) {
	if sampleInterval != 10*time.Second {
		t.Fatalf("the sample interval = %s, want 10s", sampleInterval)
	}
	if maxConcurrentSamples != 8 {
		t.Fatalf("the number of parallel samples = %d, want 8", maxConcurrentSamples)
	}
}

// fullTelemetryEngine is a Docker engine that provides every telemetry of
// the agent.
type fullTelemetryEngine struct {
	*telemetryEngine
	event    protocol.ContainerEvent
	logFrame protocol.LogFrame
}

// Events runs the event stream of the test.
func (e *fullTelemetryEngine) Events(ctx context.Context, emit func(protocol.ContainerEvent) error) error {
	if err := emit(e.event); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

// Logs runs the log stream of the test.
func (e *fullTelemetryEngine) Logs(ctx context.Context, _ protocol.LogRequest, emit func(protocol.LogFrame) error) error {
	if err := emit(e.logFrame); err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}

// TestTheAgentForwardsTheTelemetryOfItsEngine proves the wiring of the
// runtime: the samples and the events of the engine reach the control端 and
// a subscription of the control端 starts a stream of the logs.
func TestTheAgentForwardsTheTelemetryOfItsEngine(t *testing.T) {
	previous := sampleInterval
	sampleInterval = 50 * time.Millisecond
	defer func() { sampleInterval = previous }()
	captureLog(t)

	db, handler := newControl(t)
	enrollment := agentlink.NewEnrollment(db, time.Now)
	hub := agentlink.NewHub(db)

	control := &testControl{db: db}
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
	mux.Handle("/", handler)
	control.server = httptest.NewTLSServer(mux)
	t.Cleanup(control.server.Close)

	dir := t.TempDir()
	cfg := agentConfig(t, control, dir)
	issueEnrollmentToken(t, control, cfg.EnrollmentFile)

	samples := make(chan protocol.RawSample, 8)
	events := make(chan protocol.ContainerEvent, 8)
	logs := make(chan protocol.LogFrame, 8)
	hub.Subscribe(protocol.TypeMetricsSample, func(_ context.Context, _ string, envelope protocol.Envelope) error {
		var sample protocol.RawSample
		if err := json.Unmarshal(envelope.Payload, &sample); err != nil {
			return err
		}
		samples <- sample
		return nil
	})
	hub.Subscribe(protocol.TypeContainerEvent, func(_ context.Context, _ string, envelope protocol.Envelope) error {
		var event protocol.ContainerEvent
		if err := json.Unmarshal(envelope.Payload, &event); err != nil {
			return err
		}
		events <- event
		return nil
	})
	hub.Subscribe(protocol.TypeLogsFrame, func(_ context.Context, _ string, envelope protocol.Envelope) error {
		var frame protocol.LogFrame
		if err := json.Unmarshal(envelope.Payload, &frame); err != nil {
			return err
		}
		logs <- frame
		return nil
	})
	connected := make(chan string, 1)
	hub.OnConnect(func(nodeID string) {
		select {
		case connected <- nodeID:
		default:
		}
	})

	identity := protocol.NodeIdentity{EngineID: "engine-telemetry", Hostname: "worker", Role: protocol.RoleStandalone}
	engine := &fullTelemetryEngine{
		telemetryEngine: &telemetryEngine{
			identity:  identity,
			inventory: protocol.Inventory{Identity: identity, Containers: containersOf("c1")},
		},
		event:    protocol.ContainerEvent{ContainerID: "c1", Action: "start", OccurredAt: time.Now().UTC()},
		logFrame: protocol.LogFrame{Text: "the line of the engine\n"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, engine, nil) }()

	var nodeID string
	select {
	case nodeID = <-connected:
	case <-time.After(15 * time.Second):
		t.Fatal("the agent did not connect")
	}
	select {
	case sample := <-samples:
		if sample.ContainerID != "c1" {
			t.Fatalf("the sample names %q, want c1", sample.ContainerID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the sample of the engine did not arrive")
	}
	select {
	case event := <-events:
		if event.ContainerID != "c1" || event.Action != "start" {
			t.Fatalf("the event = %+v, want the event of the engine", event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the event of the engine did not arrive")
	}

	payload, err := json.Marshal(protocol.LogRequest{
		SubscriptionID: "sub-1", ContainerID: "c1", Tail: 20, Follow: true,
	})
	if err != nil {
		t.Fatalf("encode the log subscription: %v", err)
	}
	if err := hub.Send(ctx, nodeID, protocol.Envelope{
		Version: protocol.WireVersion,
		Type:    protocol.TypeLogsSubscribe,
		Payload: payload,
	}); err != nil {
		t.Fatalf("send the log subscription: %v", err)
	}
	select {
	case frame := <-logs:
		if frame.SubscriptionID != "sub-1" || frame.Text != "the line of the engine\n" {
			t.Fatalf("the log frame = %+v, want the line of the subscription", frame)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the log frame of the engine did not arrive")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the agent stopped: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the agent did not stop")
	}
}
