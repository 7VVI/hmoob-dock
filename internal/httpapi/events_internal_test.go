package httpapi

import (
	"context"
	"strings"
	"testing"
	"time"
)

// drainEvents reads every event the subscriber holds right now.
func drainEvents(t *testing.T, subscriber *eventSubscriber) []Event {
	t.Helper()
	var events []Event
	for {
		event, got, _ := subscriber.next(context.Background(), 20*time.Millisecond)
		if !got {
			return events
		}
		events = append(events, event)
	}
}

// TestALogFloodKeepsTheDeploymentEvents proves that the state of a
// deployment is never crowded out by the logs: the queue drops its oldest
// piece of the logs, keeps the text of the logs within the limit of the
// protocol and marks the gap.
func TestALogFloodKeepsTheDeploymentEvents(t *testing.T) {
	broker := newEventBroker()
	subscriber, cancel := broker.subscribe("session-1")
	defer cancel()

	for index := 0; index < 40; index++ {
		broker.publish(Event{
			Type:      EventLog,
			ObjectID:  "sub-1",
			Text:      strings.Repeat("x", 32*1024),
			sessionID: "session-1",
		})
	}
	broker.publish(Event{Type: EventDeployment, ObjectID: "task-1"})

	logBytes, logEvents := 0, 0
	deployment := false
	truncated := false
	for _, event := range drainEvents(t, subscriber) {
		switch event.Type {
		case EventLog:
			logBytes += len(event.Text)
			logEvents++
			truncated = truncated || event.Truncated
		case EventDeployment:
			if event.ObjectID == "task-1" {
				deployment = true
			}
		}
	}
	if !deployment {
		t.Fatal("the event of the deployment was crowded out by the logs")
	}
	if logBytes > maxEventLogBytes {
		t.Fatalf("the queue holds %d bytes of logs, want at most %d", logBytes, maxEventLogBytes)
	}
	if !truncated {
		t.Fatal("the dropped log pieces were not marked as truncated")
	}
	if logEvents > eventBufferSize {
		t.Fatalf("the queue holds %d events of the logs, want at most %d", logEvents, eventBufferSize)
	}
}

// TestADeploymentEventDisplacesALogEvent proves the frame limit as well: a
// full queue loses its oldest piece of the logs instead of the state of the
// deployment, and the next piece of the logs carries the gap.
func TestADeploymentEventDisplacesALogEvent(t *testing.T) {
	broker := newEventBroker()
	subscriber, cancel := broker.subscribe("session-1")
	defer cancel()

	for index := 0; index < eventBufferSize; index++ {
		broker.publish(Event{Type: EventLog, ObjectID: "sub-1", Text: "line\n", sessionID: "session-1"})
	}
	broker.publish(Event{Type: EventDeployment, ObjectID: "task-1"})

	events := drainEvents(t, subscriber)
	deployment := false
	logEvents := 0
	for _, event := range events {
		switch event.Type {
		case EventDeployment:
			if event.ObjectID == "task-1" {
				deployment = true
			}
		case EventLog:
			logEvents++
		}
	}
	if !deployment {
		t.Fatal("the event of the deployment was crowded out by the logs")
	}
	if len(events) > eventBufferSize {
		t.Fatalf("the queue holds %d events, want at most %d", len(events), eventBufferSize)
	}
	if logEvents != eventBufferSize-1 {
		t.Fatalf("the queue holds %d log events, want %d", logEvents, eventBufferSize-1)
	}

	// The next piece of the logs tells the page that a piece is missing.
	broker.publish(Event{Type: EventLog, ObjectID: "sub-1", Text: "after the gap\n", sessionID: "session-1"})
	drained := drainEvents(t, subscriber)
	if len(drained) != 1 || !drained[0].Truncated {
		t.Fatalf("the piece after the dropped log = %+v, want the gap marked", drained)
	}
}
