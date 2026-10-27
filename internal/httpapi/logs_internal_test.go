package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"hmoob-dock/internal/agentlink"
	"hmoob-dock/internal/auth"
	"hmoob-dock/internal/protocol"
)

// sentLogFrame is one frame a fake sender handed to an agent.
type sentLogFrame struct {
	nodeID   string
	envelope protocol.Envelope
}

// fakeLogSender plays the hub of the agents.
type fakeLogSender struct {
	mu        sync.Mutex
	sends     []sentLogFrame
	err       error
	connected bool
}

// Send records a frame or returns the error of the test.
func (f *fakeLogSender) Send(_ context.Context, nodeID string, message protocol.Envelope) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sends = append(f.sends, sentLogFrame{nodeID: nodeID, envelope: message})
	return nil
}

// Connected reports the connection of the test.
func (f *fakeLogSender) Connected(string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

// setError makes the next sends fail with the given error.
func (f *fakeLogSender) setError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// frames returns the frames the sender accepted.
func (f *fakeLogSender) frames() []sentLogFrame {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentLogFrame(nil), f.sends...)
}

// count returns the number of the subscriptions the manager holds.
func (m *logSubscriptions) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.subs)
}

// TestAnEndedSubscriptionWaitsForTheQueueOfTheNode proves that an end is
// not lost when the queue of the node is full: the subscription keeps its
// state until the agent accepted the unsubscribe, so the read of Docker
// does not outlive the page.
func TestAnEndedSubscriptionWaitsForTheQueueOfTheNode(t *testing.T) {
	previous := logRetryInterval
	logRetryInterval = 10 * time.Millisecond
	t.Cleanup(func() { logRetryInterval = previous })

	sender := &fakeLogSender{connected: true}
	manager := newLogSubscriptions(sender, newEventBroker())
	session := auth.Session{ID: "session-1"}
	manager.attachStream(session)

	subscription, err := manager.create(context.Background(), session, "node-1", "c1", 50, true)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := len(sender.frames()); got != 1 {
		t.Fatalf("the creation sent %d frames, want one subscription", got)
	}

	sender.setError(agentlink.ErrQueueFull)
	if _, known := manager.delete(session.ID, subscription.id, true); !known {
		t.Fatal("the deletion did not find the subscription")
	}
	// The state stays while the queue refuses the end; the read of the
	// agent still exists.
	if got := manager.count(); got != 1 {
		t.Fatalf("the manager holds %d subscriptions, want the subscription that waits", got)
	}
	time.Sleep(50 * time.Millisecond)
	if got := len(sender.frames()); got != 1 {
		t.Fatalf("the sender accepted %d frames while its queue was full, want one", got)
	}

	sender.setError(nil)
	deadline := time.Now().Add(2 * time.Second)
	for {
		frames := sender.frames()
		if manager.count() == 0 && len(frames) == 2 {
			if frames[1].envelope.Type != protocol.TypeLogsUnsubscribe {
				t.Fatalf("the retried frame = %q, want %q", frames[1].envelope.Type, protocol.TypeLogsUnsubscribe)
			}
			var request protocol.LogRequest
			if err := json.Unmarshal(frames[1].envelope.Payload, &request); err != nil {
				t.Fatalf("decode the retried request: %v", err)
			}
			if request.SubscriptionID != subscription.id {
				t.Fatalf("the retried request names %q, want %q", request.SubscriptionID, subscription.id)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the end was not retried: %d frames, %d subscriptions", len(frames), manager.count())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestAFailedCreationLeavesNoState proves that a subscription that never
// reached the agent is not held.
func TestAFailedCreationLeavesNoState(t *testing.T) {
	sender := &fakeLogSender{err: agentlink.ErrQueueFull, connected: true}
	manager := newLogSubscriptions(sender, newEventBroker())
	session := auth.Session{ID: "session-1"}
	manager.attachStream(session)

	if _, err := manager.create(context.Background(), session, "node-1", "c1", 50, true); !errors.Is(err, agentlink.ErrQueueFull) {
		t.Fatalf("create = %v, want %v", err, agentlink.ErrQueueFull)
	}
	if got := manager.count(); got != 0 {
		t.Fatalf("the manager holds %d subscriptions, want none", got)
	}
}

// TestAFrameWithoutAnEventStreamKeepsTheOffset proves that a piece that
// arrives while nobody listens does not count as delivered: the next page
// still learns that the head of its tail is missing.
func TestAFrameWithoutAnEventStreamKeepsTheOffset(t *testing.T) {
	manager := newLogSubscriptions(&fakeLogSender{connected: true}, newEventBroker())
	session := auth.Session{ID: "session-1"}
	subscription := &logSubscription{id: "sub-1", sessionID: session.ID, nodeID: "node-1"}
	manager.mu.Lock()
	manager.subs[subscription.id] = subscription
	manager.mu.Unlock()

	payload, err := json.Marshal(protocol.LogFrame{SubscriptionID: "sub-1", Offset: 40, Text: "tail\n"})
	if err != nil {
		t.Fatalf("encode the frame: %v", err)
	}
	envelope := protocol.Envelope{Type: protocol.TypeLogsFrame, Payload: payload}

	if err := manager.handleFrame(context.Background(), "node-1", envelope); err != nil {
		t.Fatalf("handle the frame: %v", err)
	}
	manager.mu.Lock()
	known, offset := subscription.offsetKnown, subscription.lastOffset
	manager.mu.Unlock()
	if known || offset != 0 {
		t.Fatalf("the offset without a stream = %d (known %v), want it untouched", offset, known)
	}

	manager.attachStream(session)
	if err := manager.handleFrame(context.Background(), "node-1", envelope); err != nil {
		t.Fatalf("handle the frame with a stream: %v", err)
	}
	manager.mu.Lock()
	known, offset = subscription.offsetKnown, subscription.lastOffset
	manager.mu.Unlock()
	if !known || offset != 45 {
		t.Fatalf("the offset with a stream = %d (known %v), want 45", offset, known)
	}
}

// TestACreationNeedsAStreamOfTheSession proves the check of the manager
// itself, independent of the HTTP handler.
func TestACreationNeedsAStreamOfTheSession(t *testing.T) {
	manager := newLogSubscriptions(&fakeLogSender{connected: true}, newEventBroker())
	if _, err := manager.create(context.Background(), auth.Session{ID: "session-1"}, "node-1", "c1", 50, true); !errors.Is(err, errNoEventStream) {
		t.Fatalf("create = %v, want %v", err, errNoEventStream)
	}
}

// TestAnExpiredSubscriptionEndsWhateverTheNodeDoes proves that the expiry of
// the session terminates the subscription even when the agent cannot accept
// the unsubscribe: the local state and every timer end, so an expired
// subscription never waits for a node that does not come back.
func TestAnExpiredSubscriptionEndsWhateverTheNodeDoes(t *testing.T) {
	previous := logRetryInterval
	logRetryInterval = 10 * time.Millisecond
	t.Cleanup(func() { logRetryInterval = previous })

	cases := []struct {
		name      string
		err       error
		connected bool
	}{
		{name: "the node is gone", err: agentlink.ErrNodeNotConnected, connected: false},
		{name: "the queue of the node is full", err: agentlink.ErrQueueFull, connected: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			sender := &fakeLogSender{connected: true}
			manager := newLogSubscriptions(sender, newEventBroker())
			session := auth.Session{ID: "session-1", ExpiresAt: time.Now().Add(20 * time.Millisecond)}
			manager.attachStream(session)

			subscription, err := manager.create(context.Background(), session, "node-1", "c1", 50, true)
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			sender.mu.Lock()
			sender.err = testCase.err
			sender.connected = testCase.connected
			sender.mu.Unlock()

			deadline := time.Now().Add(2 * time.Second)
			for manager.count() != 0 {
				if time.Now().After(deadline) {
					t.Fatalf("the expired subscription is still held: %d subscriptions", manager.count())
				}
				time.Sleep(5 * time.Millisecond)
			}

			// A sender that accepts frames again sees no retry: every timer
			// of the expired subscription ended with it.
			sender.mu.Lock()
			sends := len(sender.sends)
			sender.err = nil
			sender.connected = true
			sender.mu.Unlock()
			time.Sleep(5 * logRetryInterval)
			if got := len(sender.frames()); got != sends {
				t.Fatalf("the ended subscription sent %d frames after its end, want none", got-sends)
			}

			manager.mu.Lock()
			held := manager.subs[subscription.id] != nil
			timers := subscription.timer != nil || subscription.graceTimer != nil || subscription.retryTimer != nil
			manager.mu.Unlock()
			if held {
				t.Fatal("the manager still holds the expired subscription")
			}
			if timers {
				t.Fatal("a timer of the expired subscription is still armed")
			}
		})
	}
}
