package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// The limits of the stream of the events.
const (
	// eventBufferSize is the number of events that wait for a subscriber
	// that reads slowly.
	eventBufferSize = 64
	// eventKeepAlive is the time after which the control端 writes a comment
	// into the stream, so a proxy in front of it does not close it.
	eventKeepAlive = 30 * time.Second
)

// The types of the events of the control端.
const (
	// EventDeployment reports a change of a deployment.
	EventDeployment = "deployment"
	// EventOutput reports new output of a deployment.
	EventOutput = "output"
	// EventLog reports a piece of the logs of a container.
	EventLog = "log"
)

// Event is a change of the control端 that the browser learns about.
//
// The event names the kind of the change, the object that changed and the
// sequence number of the change. The events of a deployment hold no
// configuration and no variable: the browser reads them when it opens the
// object. The event of a piece of the logs carries the text and its
// position; it is only sent to the streams of the session that subscribed to
// it.
type Event struct {
	// Type names the kind of the change.
	Type string `json:"type"`
	// ObjectID identifies the object that changed.
	ObjectID string `json:"object_id"`
	// Seq is the sequence number of the change.
	Seq uint64 `json:"seq"`
	// Offset is the position of the piece in the logs of a subscription.
	Offset uint64 `json:"offset,omitempty"`
	// Text is the piece of the logs of a subscription.
	Text string `json:"text,omitempty"`
	// Truncated reports that a piece before this one was lost.
	Truncated bool `json:"truncated,omitempty"`
	// ErrorCode names the reason a stream of the logs ended.
	ErrorCode string `json:"error_code,omitempty"`

	// sessionID is the session that owns the subscription of a piece of the
	// logs. A piece of the logs is only handed to the streams of that
	// session, so a subscription never leaks into another session. The
	// field does not travel in the stream.
	sessionID string
}

// eventSubscriber is the queue of one event stream of the browser.
type eventSubscriber struct {
	// events carries the events of the stream.
	events chan Event
	// sessionID is the session of the stream. A piece of the logs is only
	// handed to the streams of the session that owns its subscription.
	sessionID string
}

// eventBroker hands the events of the control端 to the subscribers.
//
// The broker never blocks: a subscriber that does not read fast enough
// loses its oldest event, so the newest state of an object always arrives.
// A slow browser may not hold the deployments of the control端. When the
// broker drops an event of the logs, the next piece carries the truncation,
// so a gap of a stream is never shown as contiguous text.
type eventBroker struct {
	mu sync.Mutex
	// seq counts the events of the control端.
	seq uint64
	// subscribers holds the queues of the subscribers.
	subscribers map[*eventSubscriber]bool
	// dropped marks the subscribers whose queue lost an event since their
	// last event of the logs.
	dropped map[*eventSubscriber]bool
}

// newEventBroker builds the broker of the events.
func newEventBroker() *eventBroker {
	return &eventBroker{
		subscribers: make(map[*eventSubscriber]bool),
		dropped:     make(map[*eventSubscriber]bool),
	}
}

// publish hands an event to the subscribers.
func (b *eventBroker) publish(event Event) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.seq++
	event.Seq = b.seq
	for subscriber := range b.subscribers {
		if event.Type == EventLog && subscriber.sessionID != event.sessionID {
			// The subscription of the piece belongs to another session.
			continue
		}
		delivered := event
		if b.dropped[subscriber] && event.Type == EventLog {
			delivered.Truncated = true
			b.dropped[subscriber] = false
		}
		select {
		case subscriber.events <- delivered:
			continue
		default:
		}

		// The subscriber is too slow. Its oldest event is dropped, so the
		// newest state of the object arrives anyway.
		droppedLog := false
		select {
		case oldest := <-subscriber.events:
			droppedLog = oldest.Type == EventLog
		default:
		}
		if droppedLog {
			if event.Type == EventLog {
				// The gap belongs to the event of the logs that follows
				// it.
				delivered.Truncated = true
				b.dropped[subscriber] = false
			} else {
				// The next event of the logs carries the gap.
				b.dropped[subscriber] = true
			}
		}
		select {
		case subscriber.events <- delivered:
		default:
		}
	}
}

// subscribe registers a subscriber of a session.
//
// The second return value unregisters it again: a browser that closes the
// stream only removes its subscription.
func (b *eventBroker) subscribe(sessionID string) (<-chan Event, func()) {
	subscriber := &eventSubscriber{
		events:    make(chan Event, eventBufferSize),
		sessionID: sessionID,
	}

	b.mu.Lock()
	b.subscribers[subscriber] = true
	b.mu.Unlock()

	return subscriber.events, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.subscribers[subscriber] {
			delete(b.subscribers, subscriber)
			delete(b.dropped, subscriber)
			close(subscriber.events)
		}
	}
}

// registerEventRoutes mounts the stream of the events.
func (s *Server) registerEventRoutes() {
	s.mux.HandleFunc("/api/v1/events", s.handleEvents)
}

// handleEvents streams the changes of the control端 to the browser.
//
// The stream belongs to the administrator and uses the session of the
// administrator. The browser carries the session in its cookie and never in
// the address of the request.
//
// A browser that closes the stream only unregisters its subscription: the
// deployments of the control端 go on.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "该端点只接受 GET 请求")
		return
	}
	session, ok := s.requireSession(w, r)
	if !ok {
		return
	}

	// The subscriptions of the logs of the session live as long as the
	// session has an event stream. The last stream ends them, so a browser
	// that disappeared leaves no source of the logs open.
	if s.logs != nil {
		s.logs.attachStream(session)
		defer s.logs.detachStream(session)
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal_error", "无法推送事件")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	// A proxy that buffers the stream would delay every event until the
	// buffer is full.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	events, cancel := s.events.subscribe(session.ID)
	defer cancel()

	keepAlive := time.NewTicker(eventKeepAlive)
	defer keepAlive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case event, open := <-events:
			if !open {
				return
			}
			if err := writeEvent(w, event); err != nil {
				return
			}
			flusher.Flush()
		case <-keepAlive.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeEvent writes an event into the stream.
func writeEvent(w io.Writer, event Event) error {
	encoded, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("httpapi: encode the event: %w", err)
	}
	if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.Seq, event.Type, encoded); err != nil {
		return fmt.Errorf("httpapi: write the event: %w", err)
	}
	return nil
}
