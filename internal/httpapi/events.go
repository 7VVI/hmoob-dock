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
)

// Event is a change of the control端 that the browser learns about.
//
// The event names the kind of the change, the object that changed and the
// sequence number of the change. It holds no configuration and no variable:
// the browser reads them when it opens the object.
type Event struct {
	// Type names the kind of the change.
	Type string `json:"type"`
	// ObjectID identifies the object that changed.
	ObjectID string `json:"object_id"`
	// Seq is the sequence number of the change.
	Seq uint64 `json:"seq"`
}

// eventBroker hands the events of the control端 to the subscribers.
//
// The broker never blocks: a subscriber that does not read fast enough
// loses its oldest event, so the newest state of an object always arrives.
// A slow browser may not hold the deployments of the control端.
type eventBroker struct {
	mu sync.Mutex
	// seq counts the events of the control端.
	seq uint64
	// subscribers holds the channels of the subscribers.
	subscribers map[chan Event]bool
}

// newEventBroker builds the broker of the events.
func newEventBroker() *eventBroker {
	return &eventBroker{subscribers: make(map[chan Event]bool)}
}

// publish hands an event to the subscribers.
func (b *eventBroker) publish(event Event) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.seq++
	event.Seq = b.seq
	for subscriber := range b.subscribers {
		select {
		case subscriber <- event:
		default:
			// The subscriber is too slow. Its oldest event is dropped, so
			// the newest state of the object arrives anyway.
			select {
			case <-subscriber:
			default:
			}
			select {
			case subscriber <- event:
			default:
			}
		}
	}
}

// subscribe registers a subscriber.
//
// The second return value unregisters it again: a browser that closes the
// stream only removes its subscription.
func (b *eventBroker) subscribe() (<-chan Event, func()) {
	events := make(chan Event, eventBufferSize)

	b.mu.Lock()
	b.subscribers[events] = true
	b.mu.Unlock()

	return events, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.subscribers[events] {
			delete(b.subscribers, events)
			close(events)
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
	if _, ok := s.requireSession(w, r); !ok {
		return
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

	events, cancel := s.events.subscribe()
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
