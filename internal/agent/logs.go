package agent

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"

	"hmoob-dock/internal/dockerops"
	"hmoob-dock/internal/protocol"
)

// The limits of the queue of one subscription of the logs.
//
// The queue belongs to a subscriber and not to the connection, so a page
// that reads slowly loses its own tail and never holds the other pages or
// the control frames of the agent.
const (
	// maxLogQueueBytes is the largest text a subscription queues for a
	// slow reader.
	maxLogQueueBytes = 256 * 1024
	// maxLogQueueFrames bounds the number of the frames of a queue that
	// carries very small pieces.
	maxLogQueueFrames = 1024
)

// The reasons a stream of the logs ended without its text.
const (
	// logCodeContainerNotFound reports a container that disappeared.
	logCodeContainerNotFound = "container_not_found"
	// logCodeUnavailable reports a stream that the engine cannot read.
	logCodeUnavailable = "logs_unavailable"
)

// logSource streams the logs of a container.
type logSource interface {
	// Logs hands the pieces of the logs to emit until the stream ends.
	Logs(ctx context.Context, req protocol.LogRequest, emit func(protocol.LogFrame) error) error
}

// logKey identifies a shared read of the logs. Subscriptions of the same
// container with the same tail and follow share one stream of Docker.
type logKey struct {
	containerID string
	tail        int
	follow      bool
}

// logReader is one shared read of the logs of a container.
type logReader struct {
	key logKey
	// ctx is canceled when the last subscriber left.
	ctx    context.Context
	cancel context.CancelFunc
	// subscribers are the subscriptions that read this stream.
	subscribers map[string]*logSubscriber
}

// logManager owns the subscriptions of the logs of one connection.
//
// The manager is the reference count of the reads: two pages that watch the
// same container share one stream of Docker, and the stream ends when the
// last subscription ends. Every subscriber has its own bounded queue and
// its own writer, so a slow page does not hold the engine or the other
// pages.
type logManager struct {
	ctx    context.Context
	source logSource
	out    sender

	mu      sync.Mutex
	readers map[logKey]*logReader
	subs    map[string]*logSubscriber
	closed  bool

	closeOnce sync.Once
	wg        sync.WaitGroup
}

// newLogManager builds the manager of the log subscriptions of a
// connection.
func newLogManager(ctx context.Context, source logSource, out sender) *logManager {
	if ctx == nil {
		ctx = context.Background()
	}
	return &logManager{
		ctx:     ctx,
		source:  source,
		out:     out,
		readers: make(map[logKey]*logReader),
		subs:    make(map[string]*logSubscriber),
	}
}

// subscribe starts a subscription and the shared read it belongs to.
//
// A subscription of a container that another subscription already reads
// joins the existing read; only the last subscription cancels it.
func (m *logManager) subscribe(req protocol.LogRequest) error {
	if m == nil || m.source == nil || m.out == nil {
		return errors.New("agent: the logs of the node are not available")
	}
	id := strings.TrimSpace(req.SubscriptionID)
	if id == "" {
		return errors.New("agent: a log subscription without an ID")
	}
	containerID := strings.TrimSpace(req.ContainerID)
	if containerID == "" {
		return errors.New("agent: a log subscription without a container")
	}

	key := logKey{
		containerID: containerID,
		tail:        protocol.NormalizeLogTail(req.Tail),
		follow:      req.Follow,
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("agent: the connection of the agent ended")
	}
	if _, exists := m.subs[id]; exists {
		// A repeated subscription of the same ID reads the same stream
		// once.
		return nil
	}

	reader, exists := m.readers[key]
	if !exists {
		readerCtx, cancel := context.WithCancel(m.ctx)
		reader = &logReader{
			key:         key,
			ctx:         readerCtx,
			cancel:      cancel,
			subscribers: make(map[string]*logSubscriber),
		}
		m.readers[key] = reader
		m.wg.Add(1)
		go m.read(reader)
	}

	subscriber := newLogSubscriber(m, id, key)
	reader.subscribers[id] = subscriber
	m.subs[id] = subscriber
	m.wg.Add(1)
	go subscriber.run()
	return nil
}

// unsubscribe ends one subscription. The shared read ends with the last
// subscription of it.
func (m *logManager) unsubscribe(subscriptionID string) {
	if m == nil {
		return
	}
	id := strings.TrimSpace(subscriptionID)
	if id == "" {
		return
	}

	m.mu.Lock()
	subscriber, known := m.subs[id]
	if !known {
		m.mu.Unlock()
		return
	}
	delete(m.subs, id)
	reader := m.readers[subscriber.key]
	if reader != nil {
		delete(reader.subscribers, id)
		if len(reader.subscribers) == 0 {
			delete(m.readers, subscriber.key)
			reader.cancel()
		}
	}
	m.mu.Unlock()

	// The page deleted the subscription, so its queued text is not sent
	// any more.
	subscriber.stop()
}

// read runs the shared stream of the logs of a container.
func (m *logManager) read(reader *logReader) {
	defer m.wg.Done()

	err := m.source.Logs(reader.ctx, protocol.LogRequest{
		ContainerID: reader.key.containerID,
		Tail:        reader.key.tail,
		Follow:      reader.key.follow,
	}, func(frame protocol.LogFrame) error {
		m.broadcast(reader, frame)
		return nil
	})
	m.finish(reader, err)
}

// broadcast hands one frame of the shared read to every subscription.
func (m *logManager) broadcast(reader *logReader, frame protocol.LogFrame) {
	m.mu.Lock()
	if m.closed || m.readers[reader.key] != reader {
		m.mu.Unlock()
		return
	}
	subscribers := make([]*logSubscriber, 0, len(reader.subscribers))
	for _, subscriber := range reader.subscribers {
		subscribers = append(subscribers, subscriber)
	}
	m.mu.Unlock()

	for _, subscriber := range subscribers {
		frame.SubscriptionID = subscriber.id
		subscriber.enqueue(frame)
	}
}

// finish ends the shared read and tells every subscription whether its
// stream ended with a reason.
func (m *logManager) finish(reader *logReader, err error) {
	code := ""
	if err != nil && !errors.Is(err, context.Canceled) && reader.ctx.Err() == nil {
		code = logCodeUnavailable
		if errors.Is(err, dockerops.ErrContainerNotFound) {
			code = logCodeContainerNotFound
		}
	}

	m.mu.Lock()
	if m.readers[reader.key] != reader {
		// The subscription was canceled before the engine ended the
		// stream, or the manager was closed.
		m.mu.Unlock()
		return
	}
	delete(m.readers, reader.key)
	subscribers := make([]*logSubscriber, 0, len(reader.subscribers))
	for id, subscriber := range reader.subscribers {
		delete(m.subs, id)
		subscribers = append(subscribers, subscriber)
	}
	m.mu.Unlock()

	reader.cancel()
	if code != "" {
		for _, subscriber := range subscribers {
			// Every page reads its own subscription ID, so the error frame
			// is named like the frame of the stream it ends.
			subscriber.enqueue(protocol.LogFrame{
				SubscriptionID: subscriber.id,
				ErrorCode:      code,
			})
		}
	}
	for _, subscriber := range subscribers {
		subscriber.close()
	}
}

// handle serves the frames of the log subscriptions of the control端.
func (m *logManager) handle(envelope protocol.Envelope) {
	switch envelope.Type {
	case protocol.TypeLogsSubscribe:
		request, err := protocol.DecodeLogRequest(envelope.Payload)
		if err != nil {
			log.Printf("agent: the log subscription of the control端 is not valid: %v", err)
			return
		}
		if err := m.subscribe(request); err != nil {
			log.Printf("agent: subscribe the logs of the container %s: %v", request.ContainerID, err)
		}
	case protocol.TypeLogsUnsubscribe:
		request, err := protocol.DecodeLogRequest(envelope.Payload)
		if err != nil {
			log.Printf("agent: the log unsubscribe of the control端 is not valid: %v", err)
			return
		}
		m.unsubscribe(request.SubscriptionID)
	default:
		// The log manager serves the subscriptions only.
	}
}

// Close ends every subscription and every shared read of the connection.
func (m *logManager) Close() {
	if m == nil {
		return
	}
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		readers := make([]*logReader, 0, len(m.readers))
		for key, reader := range m.readers {
			readers = append(readers, reader)
			delete(m.readers, key)
		}
		subscribers := make([]*logSubscriber, 0, len(m.subs))
		for id, subscriber := range m.subs {
			subscribers = append(subscribers, subscriber)
			delete(m.subs, id)
		}
		m.mu.Unlock()

		for _, reader := range readers {
			reader.cancel()
		}
		for _, subscriber := range subscribers {
			subscriber.stop()
		}
		m.wg.Wait()
	})
}

// logSubscriber is one page that reads the logs of a container.
type logSubscriber struct {
	manager *logManager
	id      string
	key     logKey

	mu   sync.Mutex
	cond *sync.Cond
	// frames wait for the writer of the subscription.
	frames []protocol.LogFrame
	// bytes is the text that waits in the queue.
	bytes int
	// lastOffset is the position after the last frame that was handed
	// over.
	lastOffset uint64
	// closed reports that the stream ended and the queue is drained
	// before the writer stops.
	closed bool
	// stopped reports that the subscription was deleted; the queue is
	// dropped.
	stopped bool
	// dropped reports that a frame was lost before the next frame.
	dropped bool
}

// newLogSubscriber builds the subscription of one page.
func newLogSubscriber(manager *logManager, id string, key logKey) *logSubscriber {
	subscriber := &logSubscriber{manager: manager, id: id, key: key}
	subscriber.cond = sync.NewCond(&subscriber.mu)
	return subscriber
}

// enqueue adds a frame to the queue of the subscription. A frame that does
// not fit is dropped and the next frame tells the page that the text has a
// gap.
func (s *logSubscriber) enqueue(frame protocol.LogFrame) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || s.stopped {
		return
	}
	text := frame.Text
	if len(text) <= protocol.MaxLogFrameTextSize {
		s.pushLocked(frame)
		return
	}

	// The protocol bounds one frame; a source that hands over more is
	// split here as well.
	offset := frame.Offset
	for len(text) > 0 {
		size := len(text)
		if size > protocol.MaxLogFrameTextSize {
			size = protocol.MaxLogFrameTextSize
		}
		part := frame
		part.Text = text[:size]
		part.Offset = offset
		s.pushLocked(part)
		offset += uint64(size)
		text = text[size:]
	}
}

// pushLocked adds one frame that fits into a message.
func (s *logSubscriber) pushLocked(frame protocol.LogFrame) {
	if len(s.frames) >= maxLogQueueFrames || s.bytes+len(frame.Text) > maxLogQueueBytes {
		s.dropped = true
		return
	}
	s.frames = append(s.frames, frame)
	s.bytes += len(frame.Text)
	s.cond.Signal()
}

// next hands the oldest frame to the writer.
func (s *logSubscriber) next() (protocol.LogFrame, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for len(s.frames) == 0 && !s.closed {
		s.cond.Wait()
	}
	if s.stopped || len(s.frames) == 0 {
		return protocol.LogFrame{}, false
	}
	frame := s.frames[0]
	s.frames = s.frames[1:]
	s.bytes -= len(frame.Text)
	if s.dropped {
		// The pages before this frame were lost.
		frame.Truncated = true
		s.dropped = false
	}
	s.lastOffset = frame.Offset + uint64(len(frame.Text))
	return frame, true
}

// noteDrop remembers that the connection dropped a frame.
func (s *logSubscriber) noteDrop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropped = true
}

// truncatedTail reports that the queue ended with a gap that no frame
// carried.
func (s *logSubscriber) truncatedTail() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || !s.dropped {
		return false
	}
	s.dropped = false
	return true
}

// close ends the stream: the queued frames are still delivered.
func (s *logSubscriber) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.cond.Broadcast()
}

// stop ends the subscription and drops the queued frames.
func (s *logSubscriber) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.stopped = true
	s.frames = nil
	s.bytes = 0
	s.cond.Broadcast()
}

// run hands the queued frames of the subscription to the control端.
func (s *logSubscriber) run() {
	defer s.manager.wg.Done()

	for {
		frame, ok := s.next()
		if !ok {
			s.mu.Lock()
			done := s.stopped
			s.mu.Unlock()
			if done {
				return
			}
			if s.truncatedTail() {
				s.mu.Lock()
				offset := s.lastOffset
				id := s.id
				s.mu.Unlock()
				// The last piece of the queue was lost and no frame is
				// left to carry the truncation. The empty frame tells
				// the page that its tail is incomplete.
				if err := s.manager.out.sendLog(protocol.TypeLogsFrame, protocol.LogFrame{
					SubscriptionID: id,
					Offset:         offset,
					Truncated:      true,
				}); err != nil {
					log.Printf("agent: mark the logs of the subscription %s as truncated: %v", id, err)
				}
			}
			return
		}
		if err := s.manager.out.sendLog(protocol.TypeLogsFrame, frame); err != nil {
			s.noteDrop()
		}
	}
}
