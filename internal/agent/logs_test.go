package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"hmoob-dock/internal/dockerops"
	"hmoob-dock/internal/protocol"
)

// scriptedLogSource plays the log stream of Docker.
type scriptedLogSource struct {
	mu      sync.Mutex
	calls   []protocol.LogRequest
	started chan protocol.LogRequest
	run     func(context.Context, protocol.LogRequest, func(protocol.LogFrame) error) error
}

// Logs runs the script of the test.
func (s *scriptedLogSource) Logs(ctx context.Context, req protocol.LogRequest, emit func(protocol.LogFrame) error) error {
	s.mu.Lock()
	s.calls = append(s.calls, req)
	run := s.run
	started := s.started
	s.mu.Unlock()
	if started != nil {
		started <- req
	}
	return run(ctx, req, emit)
}

// requests returns the requests the source received.
func (s *scriptedLogSource) requests() []protocol.LogRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]protocol.LogRequest(nil), s.calls...)
}

// waitForFrame waits until the sender holds a frame of the given kind and
// returns its payload of the requested type.
func waitForLogFrame(t *testing.T, sender *gatedSender, what string) protocol.LogFrame {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, frame := range sender.frames(false) {
			if frame.messageType != protocol.TypeLogsFrame {
				continue
			}
			entry, ok := frame.payload.(protocol.LogFrame)
			if !ok {
				t.Fatalf("the payload = %T, want a protocol.LogFrame", frame.payload)
			}
			return entry
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout while waiting for %s", what)
	return protocol.LogFrame{}
}

// TestTheSubscriptionStreamsAndTheUnsubscribeStopsTheSource proves the
// lifecycle of one subscription.
func TestTheSubscriptionStreamsAndTheUnsubscribeStopsTheSource(t *testing.T) {
	started := make(chan protocol.LogRequest, 1)
	canceled := make(chan struct{})
	source := &scriptedLogSource{started: started}
	source.run = func(ctx context.Context, _ protocol.LogRequest, emit func(protocol.LogFrame) error) error {
		if err := emit(protocol.LogFrame{Text: "first line\n"}); err != nil {
			return err
		}
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	}
	sender := &gatedSender{}
	manager := newLogManager(context.Background(), source, sender)
	defer manager.Close()

	request := protocol.LogRequest{SubscriptionID: "sub-1", ContainerID: "c1", Tail: 50, Follow: true}
	if err := manager.subscribe(request); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	received := <-started
	if received.SubscriptionID != "" {
		t.Fatalf("the source received the subscription ID %q, want it filled per subscriber", received.SubscriptionID)
	}
	if received.ContainerID != "c1" || received.Tail != 50 || !received.Follow {
		t.Fatalf("the source received %+v, want the request of the test", received)
	}

	frame := waitForLogFrame(t, sender, "the first line")
	if frame.SubscriptionID != "sub-1" || frame.Text != "first line\n" {
		t.Fatalf("the frame = %+v, want the first line of the subscription", frame)
	}

	manager.unsubscribe("sub-1")
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("the source was not canceled by the unsubscribe")
	}
}

// TestTwoSubscriptionsShareOneReadWithAReferenceCount proves that two pages
// of the same container share one read of Docker and that the read ends only
// with the last subscription.
func TestTwoSubscriptionsShareOneReadWithAReferenceCount(t *testing.T) {
	started := make(chan protocol.LogRequest, 4)
	canceled := make(chan struct{})
	var once sync.Once
	frames := make(chan protocol.LogFrame)
	source := &scriptedLogSource{started: started}
	source.run = func(ctx context.Context, _ protocol.LogRequest, emit func(protocol.LogFrame) error) error {
		for {
			select {
			case frame := <-frames:
				if err := emit(frame); err != nil {
					return err
				}
			case <-ctx.Done():
				once.Do(func() { close(canceled) })
				return ctx.Err()
			}
		}
	}
	sender := &gatedSender{}
	manager := newLogManager(context.Background(), source, sender)
	defer manager.Close()

	for _, id := range []string{"sub-1", "sub-2"} {
		if err := manager.subscribe(protocol.LogRequest{
			SubscriptionID: id, ContainerID: "c1", Tail: 200, Follow: true,
		}); err != nil {
			t.Fatalf("subscribe %s: %v", id, err)
		}
	}
	<-started
	if calls := source.requests(); len(calls) != 1 {
		t.Fatalf("the source was opened %d times, want one shared read", len(calls))
	}

	frames <- protocol.LogFrame{Text: "shared line\n"}
	waitFor(t, "both subscriptions to receive the line", func() bool {
		seen := map[string]bool{}
		for _, frame := range sender.frames(false) {
			entry := frame.payload.(protocol.LogFrame)
			if entry.Text == "shared line\n" {
				seen[entry.SubscriptionID] = true
			}
		}
		return seen["sub-1"] && seen["sub-2"]
	})

	manager.unsubscribe("sub-1")
	select {
	case <-canceled:
		t.Fatal("the shared read stopped with the first subscription")
	case <-time.After(100 * time.Millisecond):
	}

	frames <- protocol.LogFrame{Text: "still shared\n"}
	waitFor(t, "the remaining subscription to receive the line", func() bool {
		for _, frame := range sender.frames(false) {
			entry := frame.payload.(protocol.LogFrame)
			if entry.SubscriptionID == "sub-2" && entry.Text == "still shared\n" {
				return true
			}
		}
		return false
	})

	manager.unsubscribe("sub-2")
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("the shared read was not canceled by the last subscription")
	}
}

// logFramesFor returns the log frames the sender handed to one
// subscription.
func logFramesFor(sender *gatedSender, subscriptionID string) []protocol.LogFrame {
	var frames []protocol.LogFrame
	for _, frame := range sender.frames(false) {
		entry, ok := frame.payload.(protocol.LogFrame)
		if !ok || entry.SubscriptionID != subscriptionID {
			continue
		}
		frames = append(frames, entry)
	}
	return frames
}

// TestALateSubscriptionReceivesTheBufferedTail proves that a page that joins
// a shared read after the tail of Docker arrived still receives that tail.
// Docker only reads the tail when the stream is opened, so a shared read
// replays its recent frames to the pages that join later.
func TestALateSubscriptionReceivesTheBufferedTail(t *testing.T) {
	source := &scriptedLogSource{}
	source.run = func(ctx context.Context, _ protocol.LogRequest, emit func(protocol.LogFrame) error) error {
		if err := emit(protocol.LogFrame{Text: "first line\n"}); err != nil {
			return err
		}
		if err := emit(protocol.LogFrame{Text: "second line\n"}); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}
	sender := &gatedSender{}
	manager := newLogManager(context.Background(), source, sender)
	defer manager.Close()

	if err := manager.subscribe(protocol.LogRequest{SubscriptionID: "sub-1", ContainerID: "c1", Tail: 200, Follow: true}); err != nil {
		t.Fatalf("subscribe sub-1: %v", err)
	}
	waitFor(t, "the first subscription to receive the tail", func() bool {
		return len(logFramesFor(sender, "sub-1")) >= 2
	})

	if err := manager.subscribe(protocol.LogRequest{SubscriptionID: "sub-2", ContainerID: "c1", Tail: 200, Follow: true}); err != nil {
		t.Fatalf("subscribe sub-2: %v", err)
	}
	waitFor(t, "the late subscription to receive the tail", func() bool {
		frames := logFramesFor(sender, "sub-2")
		return len(frames) >= 2 && frames[0].Text == "first line\n" && frames[1].Text == "second line\n"
	})
	for _, frame := range logFramesFor(sender, "sub-2") {
		if frame.Truncated {
			t.Fatalf("the complete tail of the late subscription is marked truncated: %+v", frame)
		}
	}
}

// TestALateSubscriptionIsTruncatedWhenTheTailDoesNotFit proves that the
// replay of a shared read is bounded: when the tail of Docker does not fit
// into the buffer of the read, the late page learns that its tail has a gap
// instead of believing that the newest pieces are the whole tail.
func TestALateSubscriptionIsTruncatedWhenTheTailDoesNotFit(t *testing.T) {
	const frameSize = protocol.MaxLogFrameTextSize
	const total = 12
	emitted := make(chan struct{})
	source := &scriptedLogSource{}
	source.run = func(ctx context.Context, _ protocol.LogRequest, emit func(protocol.LogFrame) error) error {
		for index := 0; index < total; index++ {
			if err := emit(protocol.LogFrame{Text: strings.Repeat("x", frameSize)}); err != nil {
				return err
			}
		}
		close(emitted)
		<-ctx.Done()
		return ctx.Err()
	}
	sender := &gatedSender{}
	manager := newLogManager(context.Background(), source, sender)
	defer manager.Close()

	if err := manager.subscribe(protocol.LogRequest{SubscriptionID: "sub-1", ContainerID: "c1", Tail: 200, Follow: true}); err != nil {
		t.Fatalf("subscribe sub-1: %v", err)
	}
	select {
	case <-emitted:
	case <-time.After(5 * time.Second):
		t.Fatal("the source did not emit its frames")
	}
	waitFor(t, "the first subscription to receive every frame", func() bool {
		return len(logFramesFor(sender, "sub-1")) >= total
	})

	if err := manager.subscribe(protocol.LogRequest{SubscriptionID: "sub-2", ContainerID: "c1", Tail: 200, Follow: true}); err != nil {
		t.Fatalf("subscribe sub-2: %v", err)
	}
	waitFor(t, "the late subscription to receive its truncated tail", func() bool {
		return len(logFramesFor(sender, "sub-2")) > 0
	})
	frames := logFramesFor(sender, "sub-2")
	if !frames[0].Truncated {
		t.Fatalf("the first frame of the late tail = %+v, want the gap marked", frames[0])
	}
	bytes := 0
	for _, frame := range frames {
		bytes += len(frame.Text)
	}
	if limit := 256 * 1024; bytes > limit {
		t.Fatalf("the late subscription received %d bytes of tail, want at most %d", bytes, limit)
	}
}

// TestTheSharedHistoryIsBoundedByFrames proves that a stream of tiny lines
// cannot pile up the structures of the history: the buffer keeps the newest
// frames and drops the oldest ones even when their text stays small.
func TestTheSharedHistoryIsBoundedByFrames(t *testing.T) {
	reader := &logReader{}
	total := maxLogQueueFrames + 200
	for index := 0; index < total; index++ {
		reader.remember(protocol.LogFrame{Offset: uint64(index), Text: fmt.Sprintf("%d\n", index)})
	}
	if len(reader.history) != maxLogQueueFrames {
		t.Fatalf("the history holds %d frames, want %d", len(reader.history), maxLogQueueFrames)
	}
	if !reader.historyDropped {
		t.Fatal("the history does not report the frames it dropped")
	}
	if first := reader.history[0].Offset; first != uint64(total-maxLogQueueFrames) {
		t.Fatalf("the history starts at the offset %d, want %d", first, total-maxLogQueueFrames)
	}
	if last := reader.history[len(reader.history)-1].Offset; last != uint64(total-1) {
		t.Fatalf("the history ends at the offset %d, want %d", last, total-1)
	}
}

// TestAReplayKeepsTheNewestFrames proves that a page that joins a read with
// more tail than its queue holds receives the newest lines, not the oldest
// ones: the replay keeps the recent segment and marks the gap on the oldest
// frame it sends.
func TestAReplayKeepsTheNewestFrames(t *testing.T) {
	const total = maxLogQueueFrames + 400
	emitted := make(chan struct{})
	source := &scriptedLogSource{}
	source.run = func(ctx context.Context, _ protocol.LogRequest, emit func(protocol.LogFrame) error) error {
		for index := 0; index < total; index++ {
			if err := emit(protocol.LogFrame{Text: fmt.Sprintf("line-%04d\n", index)}); err != nil {
				return err
			}
		}
		close(emitted)
		<-ctx.Done()
		return ctx.Err()
	}
	sender := &gatedSender{}
	manager := newLogManager(context.Background(), source, sender)
	defer manager.Close()

	if err := manager.subscribe(protocol.LogRequest{SubscriptionID: "sub-1", ContainerID: "c1", Tail: 2000, Follow: true}); err != nil {
		t.Fatalf("subscribe sub-1: %v", err)
	}
	select {
	case <-emitted:
	case <-time.After(5 * time.Second):
		t.Fatal("the source did not emit its frames")
	}

	if err := manager.subscribe(protocol.LogRequest{SubscriptionID: "sub-2", ContainerID: "c1", Tail: 2000, Follow: true}); err != nil {
		t.Fatalf("subscribe sub-2: %v", err)
	}
	waitFor(t, "the late subscription to receive the newest tail", func() bool {
		return len(logFramesFor(sender, "sub-2")) >= maxLogQueueFrames
	})
	frames := logFramesFor(sender, "sub-2")
	if len(frames) != maxLogQueueFrames {
		t.Fatalf("the late subscription received %d frames, want %d", len(frames), maxLogQueueFrames)
	}
	for index, frame := range frames {
		want := fmt.Sprintf("line-%04d\n", total-len(frames)+index)
		if frame.Text != want {
			t.Fatalf("the frame %d = %q, want %q", index, frame.Text, want)
		}
		if index == 0 && !frame.Truncated {
			t.Fatalf("the first frame of the late tail = %+v, want the gap marked", frame)
		}
		if index > 0 && frame.Truncated {
			t.Fatalf("the frame %d = %+v, want only the first frame marked", index, frame)
		}
	}
}

// TestTheFramesThatArriveDuringAReplayFollowIt proves that a frame of the
// live read that arrives while the snapshot is handed over neither overtakes
// the snapshot nor repeats it.
func TestTheFramesThatArriveDuringAReplayFollowIt(t *testing.T) {
	subscriber := newLogSubscriber(nil, "sub-1", logKey{})
	subscriber.replaying = true
	subscriber.enqueue(protocol.LogFrame{Text: "live\n"})

	subscriber.beginReplay([]protocol.LogFrame{{Text: "old\n"}}, false)

	subscriber.mu.Lock()
	frames := append([]protocol.LogFrame(nil), subscriber.frames...)
	subscriber.mu.Unlock()
	if len(frames) != 2 || frames[0].Text != "old\n" || frames[1].Text != "live\n" {
		t.Fatalf("the queue = %+v, want the replay followed by the live frame", frames)
	}
	for _, frame := range frames {
		if frame.Truncated {
			t.Fatalf("the complete replay is marked truncated: %+v", frames)
		}
	}
}

// TestDifferentLogRequestsDoNotShareTheRead proves that a different tail or
// follow belongs to a read of its own.
func TestDifferentLogRequestsDoNotShareTheRead(t *testing.T) {
	started := make(chan protocol.LogRequest, 4)
	source := &scriptedLogSource{started: started}
	source.run = func(ctx context.Context, _ protocol.LogRequest, _ func(protocol.LogFrame) error) error {
		<-ctx.Done()
		return ctx.Err()
	}
	manager := newLogManager(context.Background(), source, &gatedSender{})
	defer manager.Close()

	if err := manager.subscribe(protocol.LogRequest{SubscriptionID: "sub-1", ContainerID: "c1", Tail: 10, Follow: true}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := manager.subscribe(protocol.LogRequest{SubscriptionID: "sub-2", ContainerID: "c1", Tail: 20, Follow: true}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	<-started
	<-started
	if calls := source.requests(); len(calls) != 2 {
		t.Fatalf("the source was opened %d times, want two reads", len(calls))
	}
}

// TestTheTailIsNormalized proves the default and the upper bound of the
// number of the lines of a subscription.
func TestTheTailIsNormalized(t *testing.T) {
	started := make(chan protocol.LogRequest, 4)
	source := &scriptedLogSource{started: started}
	source.run = func(ctx context.Context, _ protocol.LogRequest, _ func(protocol.LogFrame) error) error {
		<-ctx.Done()
		return ctx.Err()
	}
	manager := newLogManager(context.Background(), source, &gatedSender{})
	defer manager.Close()

	if err := manager.subscribe(protocol.LogRequest{SubscriptionID: "sub-1", ContainerID: "c1"}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if got := (<-started).Tail; got != protocol.DefaultLogTail {
		t.Fatalf("the default tail = %d, want %d", got, protocol.DefaultLogTail)
	}
	if err := manager.subscribe(protocol.LogRequest{SubscriptionID: "sub-2", ContainerID: "c2", Tail: 5000}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if got := (<-started).Tail; got != protocol.MaxLogTail {
		t.Fatalf("the capped tail = %d, want %d", got, protocol.MaxLogTail)
	}
}

// TestASlowSubscriberIsTruncatedWithoutHoldingTheSource proves that the
// queue of a subscription is bounded and that a gap is marked as truncated.
func TestASlowSubscriberIsTruncatedWithoutHoldingTheSource(t *testing.T) {
	const frameSize = protocol.MaxLogFrameTextSize
	gate := make(chan struct{})
	var release sync.Once
	releaseGate := func() { release.Do(func() { close(gate) }) }
	defer releaseGate()

	emitted := make(chan struct{})
	source := &scriptedLogSource{}
	source.run = func(ctx context.Context, _ protocol.LogRequest, emit func(protocol.LogFrame) error) error {
		for index := 0; index < 24; index++ {
			if err := emit(protocol.LogFrame{Text: strings.Repeat("x", frameSize)}); err != nil {
				return err
			}
		}
		close(emitted)
		return nil
	}
	sender := &gatedSender{logGate: gate}
	manager := newLogManager(context.Background(), source, sender)
	defer manager.Close()

	if err := manager.subscribe(protocol.LogRequest{SubscriptionID: "sub-slow", ContainerID: "c1", Tail: 200, Follow: true}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// The source is not held by the slow consumer: it finishes its frames
	// while the sender still waits.
	select {
	case <-emitted:
	case <-time.After(5 * time.Second):
		t.Fatal("the slow subscriber held the source of the logs")
	}
	releaseGate()
	manager.Close()

	frames := sender.frames(false)
	if len(frames) == 0 {
		t.Fatal("the slow subscriber received no frame")
	}
	bytes := 0
	truncated := false
	for _, frame := range frames {
		entry, ok := frame.payload.(protocol.LogFrame)
		if !ok {
			t.Fatalf("the payload = %T, want a protocol.LogFrame", frame.payload)
		}
		bytes += len(entry.Text)
		if len(entry.Text) > protocol.MaxLogFrameTextSize {
			t.Fatalf("a frame holds %d bytes, want at most %d", len(entry.Text), protocol.MaxLogFrameTextSize)
		}
		if entry.Truncated {
			truncated = true
		}
	}
	if !truncated {
		t.Fatal("the dropped frames were not marked as truncated")
	}
	if limit := maxLogQueueBytes + protocol.MaxLogFrameTextSize; bytes > limit {
		t.Fatalf("the subscriber received %d bytes, want at most %d", bytes, limit)
	}
}

// TestASourceErrorBecomesAnErrorFrame proves that the reason of a missing
// stream reaches the page instead of being reported as an empty log.
func TestASourceErrorBecomesAnErrorFrame(t *testing.T) {
	source := &scriptedLogSource{}
	source.run = func(context.Context, protocol.LogRequest, func(protocol.LogFrame) error) error {
		return fmt.Errorf("read the logs: %w", errors.New("the driver does not support reading"))
	}
	sender := &gatedSender{}
	manager := newLogManager(context.Background(), source, sender)
	defer manager.Close()

	if err := manager.subscribe(protocol.LogRequest{SubscriptionID: "sub-1", ContainerID: "c1"}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	frame := waitForLogFrame(t, sender, "the error frame")
	if frame.SubscriptionID != "sub-1" {
		t.Fatalf("the error frame names %q, want sub-1", frame.SubscriptionID)
	}
	if frame.ErrorCode != "logs_unavailable" {
		t.Fatalf("the error code = %q, want logs_unavailable", frame.ErrorCode)
	}
}

// TestAMissingContainerBecomesItsOwnErrorFrame proves that a container that
// disappeared is told apart from a logging driver that cannot be read.
func TestAMissingContainerBecomesItsOwnErrorFrame(t *testing.T) {
	source := &scriptedLogSource{}
	source.run = func(context.Context, protocol.LogRequest, func(protocol.LogFrame) error) error {
		return fmt.Errorf("inspect the container: %w", dockerops.ErrContainerNotFound)
	}
	sender := &gatedSender{}
	manager := newLogManager(context.Background(), source, sender)
	defer manager.Close()

	if err := manager.subscribe(protocol.LogRequest{SubscriptionID: "sub-1", ContainerID: "c1"}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	frame := waitForLogFrame(t, sender, "the error frame")
	if frame.ErrorCode != "container_not_found" {
		t.Fatalf("the error code = %q, want container_not_found", frame.ErrorCode)
	}
}

// TestClosingTheManagerCancelsEverySource proves that the session of a log
// subscription leaves no goroutine behind and that close is idempotent.
func TestClosingTheManagerCancelsEverySource(t *testing.T) {
	canceled := make(chan struct{}, 2)
	var once sync.Once
	source := &scriptedLogSource{}
	source.run = func(ctx context.Context, _ protocol.LogRequest, _ func(protocol.LogFrame) error) error {
		<-ctx.Done()
		once.Do(func() { close(canceled) })
		return ctx.Err()
	}
	manager := newLogManager(context.Background(), source, &gatedSender{})
	if err := manager.subscribe(protocol.LogRequest{SubscriptionID: "sub-1", ContainerID: "c1", Follow: true}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		manager.Close()
		manager.Close()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("closing the manager did not return")
	}
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("closing the manager did not cancel the source")
	}
}

// TestAnUnsubscribeOfAnUnknownSubscriptionIsIgnored proves that a stale
// unsubscribe does not disturb the other subscriptions.
func TestAnUnsubscribeOfAnUnknownSubscriptionIsIgnored(t *testing.T) {
	source := &scriptedLogSource{}
	source.run = func(ctx context.Context, _ protocol.LogRequest, _ func(protocol.LogFrame) error) error {
		<-ctx.Done()
		return ctx.Err()
	}
	manager := newLogManager(context.Background(), source, &gatedSender{})
	defer manager.Close()

	manager.unsubscribe("sub-that-does-not-exist")
	if err := manager.subscribe(protocol.LogRequest{SubscriptionID: "sub-1", ContainerID: "c1"}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
}

// TestTheManagerServesTheFramesOfTheControl proves that the frames of the
// control端 start and end a subscription.
func TestTheManagerServesTheFramesOfTheControl(t *testing.T) {
	started := make(chan protocol.LogRequest, 1)
	canceled := make(chan struct{})
	source := &scriptedLogSource{started: started}
	source.run = func(ctx context.Context, _ protocol.LogRequest, _ func(protocol.LogFrame) error) error {
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	}
	manager := newLogManager(context.Background(), source, &gatedSender{})
	defer manager.Close()

	subscribe, err := json.Marshal(protocol.LogRequest{
		SubscriptionID: "sub-1", ContainerID: "c1", Tail: 10, Follow: true,
	})
	if err != nil {
		t.Fatalf("encode the request: %v", err)
	}
	manager.handle(protocol.Envelope{Type: protocol.TypeLogsSubscribe, Payload: subscribe})
	<-started

	unsubscribe, err := json.Marshal(protocol.LogRequest{SubscriptionID: "sub-1"})
	if err != nil {
		t.Fatalf("encode the request: %v", err)
	}
	manager.handle(protocol.Envelope{Type: protocol.TypeLogsUnsubscribe, Payload: unsubscribe})
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("the frame of the control端 did not end the subscription")
	}
}
