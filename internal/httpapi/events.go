package httpapi

import (
	"context"
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
	// maxEventLogBytes is the largest text of the log events that wait for
	// one subscriber. A log piece can be fetched again; the state of a
	// deployment cannot, so the logs are dropped before the other events.
	maxEventLogBytes = 256 * 1024
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
//
// The queue belongs to one stream, so a slow page loses its own events and
// never holds the events of the other pages. The queue drops an event of the
// logs before it drops an event of a deployment: the logs can be fetched
// again, the state of a deployment cannot.
type eventSubscriber struct {
	// sessionID is the session of the stream. A piece of the logs is only
	// handed to the streams of the session that owns its subscription.
	sessionID string

	// mu guards the queue and the state of the stream.
	mu sync.Mutex
	// queue holds the events that wait for the stream.
	queue []Event
	// logBytes is the text of the log events that wait in the queue.
	logBytes int
	// closed reports that the stream was unregistered.
	closed bool
	// dropped reports that an event of the logs was lost and the next event
	// of the logs tells the page about the gap.
	dropped bool
	// wake signals a waiting writer that the queue changed.
	wake chan struct{}
}

// newEventSubscriber builds the queue of one stream.
func newEventSubscriber(sessionID string) *eventSubscriber {
	return &eventSubscriber{sessionID: sessionID, wake: make(chan struct{}, 1)}
}

// signal wakes the writer of the stream.
func (s *eventSubscriber) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// enqueue adds an event to the queue.
//
// A full queue loses its oldest event of the logs, so the events of the
// deployments arrive even while a flood of the logs runs. A queue that holds
// no event of the logs drops the new piece of the logs instead: the state of
// a deployment cannot be fetched again. The text of the logs is bounded, so
// the queue of one stream stays within the limit of the protocol.
func (s *eventSubscriber) enqueue(event Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return
	}
	if event.Type == EventLog && s.dropped {
		// The pieces before this one were lost.
		event.Truncated = true
		s.dropped = false
	}

	if event.Type != EventLog {
		if len(s.queue) >= eventBufferSize {
			if s.dropOldestLogLocked() {
				// The next event of the logs tells the page about the gap.
				s.dropped = true
			} else if len(s.queue) > 0 {
				s.queue = s.queue[1:]
			}
		}
		s.queue = append(s.queue, event)
		s.signal()
		return
	}

	if len(event.Text) > maxEventLogBytes {
		s.dropped = true
		return
	}
	trimmed := false
	for s.logBytes+len(event.Text) > maxEventLogBytes {
		if !s.dropOldestLogLocked() {
			break
		}
		trimmed = true
	}
	if s.logBytes+len(event.Text) > maxEventLogBytes {
		s.dropped = true
		return
	}
	if len(s.queue) >= eventBufferSize {
		if s.dropOldestLogLocked() {
			trimmed = true
		} else {
			// The queue holds no log event to give up. The state of a
			// deployment cannot be fetched again, so it stays and the new
			// piece of the logs is dropped instead. The next piece carries
			// the gap.
			s.dropped = true
			return
		}
	}
	if len(s.queue) >= eventBufferSize {
		s.dropped = true
		return
	}
	if trimmed {
		// The text before this piece left the queue.
		event.Truncated = true
	}
	s.queue = append(s.queue, event)
	s.logBytes += len(event.Text)
	s.signal()
}

// dropOldestLogLocked drops the oldest event of the logs. The caller holds
// the lock. A queue without an event of the logs keeps every event.
func (s *eventSubscriber) dropOldestLogLocked() bool {
	for index, event := range s.queue {
		if event.Type != EventLog {
			continue
		}
		s.logBytes -= len(event.Text)
		s.queue = append(s.queue[:index], s.queue[index+1:]...)
		return true
	}
	return false
}

// next hands the next event to the writer of the stream.
//
// The second return value reports whether an event arrived, the third
// whether the stream is still open. A stream without an event within the
// timeout is open and quiet; the writer sends a keep-alive comment then.
func (s *eventSubscriber) next(ctx context.Context, timeout time.Duration) (Event, bool, bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		s.mu.Lock()
		if len(s.queue) > 0 {
			event := s.queue[0]
			s.queue = s.queue[1:]
			if event.Type == EventLog {
				s.logBytes -= len(event.Text)
			}
			s.mu.Unlock()
			return event, true, true
		}
		if s.closed {
			s.mu.Unlock()
			return Event{}, false, false
		}
		s.mu.Unlock()

		select {
		case <-s.wake:
		case <-timer.C:
			return Event{}, false, true
		case <-ctx.Done():
			return Event{}, false, false
		}
	}
}

// close ends the stream. The queued events are dropped: nobody reads them.
func (s *eventSubscriber) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.queue = nil
	s.logBytes = 0
	s.signal()
}

// eventBroker hands the events of the control端 to the subscribers.
//
// The broker never blocks: a subscriber that does not read fast enough
// loses its oldest event of the logs, so the newest state of an object
// always arrives. A slow browser may not hold the deployments of the
// control端. When the broker drops an event of the logs, the next piece
// carries the truncation, so a gap of a stream is never shown as contiguous
// text.
type eventBroker struct {
	mu sync.Mutex
	// seq counts the events of the control端.
	seq uint64
	// subscribers holds the queues of the subscribers.
	subscribers map[*eventSubscriber]bool
}

// newEventBroker builds the broker of the events.
func newEventBroker() *eventBroker {
	return &eventBroker{subscribers: make(map[*eventSubscriber]bool)}
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
		subscriber.enqueue(event)
	}
}

// subscribe registers a subscriber of a session.
//
// The second return value unregisters it again: a browser that closes the
// stream only removes its subscription.
func (b *eventBroker) subscribe(sessionID string) (*eventSubscriber, func()) {
	subscriber := newEventSubscriber(sessionID)

	b.mu.Lock()
	b.subscribers[subscriber] = true
	b.mu.Unlock()

	return subscriber, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.subscribers[subscriber] {
			delete(b.subscribers, subscriber)
			subscriber.close()
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

	subscriber, cancel := s.events.subscribe(session.ID)
	defer cancel()

	for {
		event, got, open := subscriber.next(r.Context(), eventKeepAlive)
		if !open {
			return
		}
		if !got {
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
			continue
		}
		if err := writeEvent(w, event); err != nil {
			return
		}
		flusher.Flush()
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
