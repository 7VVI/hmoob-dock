package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"hmoob-dock/internal/model"
	"hmoob-dock/internal/protocol"
)

// countingExecutor counts the executions of the commands.
//
// The executor replaces the Docker engine in the tests: the ledger of the
// agent is a real SQLite file, so the protection against a repeated
// execution is the real one.
type countingExecutor struct {
	mu sync.Mutex
	// calls counts the executions of the executor.
	calls int
	// output is written into the writer of the execution.
	output string
}

// Execute runs the command of the test.
func (c *countingExecutor) Execute(ctx context.Context, cmd model.Command, output io.Writer) (model.Result, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()

	if c.output != "" {
		if _, err := io.WriteString(output, c.output); err != nil {
			return model.Result{}, err
		}
	}
	return model.Result{State: model.Succeeded, Code: "compose_ready"}, nil
}

// count returns the number of the executions.
func (c *countingExecutor) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// recordedFrame is a frame the agent sent.
type recordedFrame struct {
	// messageType is the type of the frame.
	messageType string
	// payload is the message of the frame.
	payload any
}

// recordingSender records the frames the agent sends.
//
// It replaces the connection of the control端 in the tests: the ledger of
// the agent and the worker are the real ones.
type recordingSender struct {
	mu sync.Mutex
	// frames holds the frames the agent sent.
	frames []recordedFrame
}

// sendControl records a control frame.
func (s *recordingSender) sendControl(messageType string, payload any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, recordedFrame{messageType: messageType, payload: payload})
	return nil
}

// sendLog records a log frame.
func (s *recordingSender) sendLog(messageType string, payload any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, recordedFrame{messageType: messageType, payload: payload})
	return nil
}

// of returns the frames of the given type.
func (s *recordingSender) of(messageType string) []recordedFrame {
	s.mu.Lock()
	defer s.mu.Unlock()
	var frames []recordedFrame
	for _, frame := range s.frames {
		if frame.messageType == messageType {
			frames = append(frames, frame)
		}
	}
	return frames
}

// waitFor waits until the condition holds.
func waitForAgent(t *testing.T, what string, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout while waiting for %s", what)
}

// newTestAgent builds an agent with a ledger and a handler of the
// deployments.
func newTestAgent(t *testing.T, executor *countingExecutor) (*Ledger, *deployments) {
	t.Helper()

	return newTestAgentAt(t, executor, filepath.Join(t.TempDir(), "ledger.db"))
}

// newTestAgentAt builds an agent with a ledger at the given path.
func newTestAgentAt(t *testing.T, executor *countingExecutor, path string) (*Ledger, *deployments) {
	t.Helper()

	ledger, err := OpenLedger(path)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}
	t.Cleanup(func() { ledger.Close() })

	handler := newDeployments(NewWorker(ledger, executor), ledger)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	handler.start(ctx)
	return ledger, handler
}

// uncertainTask puts a task into a ledger that waits for its check.
//
// The ledger of the agent knows a task whose outcome is unknown after the
// agent stopped in the middle of it. The task holds its application until an
// administrator releases it.
func uncertainTask(t *testing.T, path, taskID, appID string) {
	t.Helper()

	ledger, err := OpenLedger(path)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}
	command := model.Command{
		Task: model.Task{ID: taskID, AppID: appID, Digest: "digest-1"},
		App:  model.App{ID: appID, Mode: "compose"},
	}
	if _, _, err := ledger.Accept(context.Background(), command); err != nil {
		t.Fatalf("accept the command: %v", err)
	}
	if _, err := ledger.Begin(context.Background(), taskID); err != nil {
		t.Fatalf("take the intent of the task: %v", err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatalf("close the ledger: %v", err)
	}
}

// commandFrame builds the frame of a command of the control端.
func commandFrame(t *testing.T, command model.Command) protocol.Envelope {
	t.Helper()

	payload, err := json.Marshal(protocol.DeployCommand{Command: command})
	if err != nil {
		t.Fatalf("encode the command: %v", err)
	}
	return protocol.Envelope{Version: protocol.WireVersion, Type: protocol.TypeDeployCommand, Payload: payload}
}

// TestTheCommandOfTheControlEndRunsThroughTheWorker proves that a command
// reaches the executor through the worker, so the intent of the execution is
// written into the ledger before the Docker command runs.
func TestTheCommandOfTheControlEndRunsThroughTheWorker(t *testing.T) {
	executor := &countingExecutor{}
	ledger, handler := newTestAgent(t, executor)
	sender := &recordingSender{}

	command := model.Command{
		Task: model.Task{ID: "task-1", AppID: "app-1", Digest: "digest-1"},
		App:  model.App{ID: "app-1", Mode: "compose"},
	}
	handler.handle(context.Background(), commandFrame(t, command), sender)

	waitForAgent(t, "the report of the task", func() bool {
		return len(sender.of(protocol.TypeDeployReceipt)) == 1
	})
	if executor.count() != 1 {
		t.Fatalf("executions = %d, want 1", executor.count())
	}

	// The task is in the ledger of the node.
	receipt, err := ledger.Lookup(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("read the task: %v", err)
	}
	if receipt.Result.State != model.Succeeded {
		t.Fatalf("the task is in the phase %q, want succeeded", receipt.Result.State)
	}

	// The report that reached the control端 holds the same outcome.
	frames := sender.of(protocol.TypeDeployReceipt)
	var message protocol.DeployReceipt
	if err := json.Unmarshal(mustJSON(t, frames[0].payload), &message); err != nil {
		t.Fatalf("decode the report: %v", err)
	}
	if message.Receipt.TaskID != "task-1" {
		t.Fatalf("the report holds the task %s, want task-1", message.Receipt.TaskID)
	}
	if message.Receipt.Seq != receipt.Seq {
		t.Fatalf("the report holds the sequence number %d, want %d", message.Receipt.Seq, receipt.Seq)
	}
}

// TestTheCommandThatArrivesTwiceRunsOnce proves that the ledger of the node
// protects the execution against a repeated command.
func TestTheCommandThatArrivesTwiceRunsOnce(t *testing.T) {
	executor := &countingExecutor{}
	_, handler := newTestAgent(t, executor)
	sender := &recordingSender{}

	command := model.Command{
		Task: model.Task{ID: "task-1", AppID: "app-1", Digest: "digest-1"},
		App:  model.App{ID: "app-1", Mode: "compose"},
	}
	frame := commandFrame(t, command)
	handler.handle(context.Background(), frame, sender)

	waitForAgent(t, "the report of the task", func() bool {
		return len(sender.of(protocol.TypeDeployReceipt)) == 1
	})

	handler.handle(context.Background(), frame, sender)
	waitForAgent(t, "the second report of the task", func() bool {
		return len(sender.of(protocol.TypeDeployReceipt)) == 2
	})
	if executor.count() != 1 {
		t.Fatalf("executions = %d, want 1", executor.count())
	}
}

// TestTheOutputOfTheTaskIsSentInFragments proves that the output of a task
// reaches the control端 with its offset.
func TestTheOutputOfTheTaskIsSentInFragments(t *testing.T) {
	executor := &countingExecutor{output: "the output of the task"}
	_, handler := newTestAgent(t, executor)
	sender := &recordingSender{}

	command := model.Command{
		Task: model.Task{ID: "task-1", AppID: "app-1", Digest: "digest-1"},
		App:  model.App{ID: "app-1", Mode: "compose"},
	}
	handler.handle(context.Background(), commandFrame(t, command), sender)

	waitForAgent(t, "the output of the task", func() bool {
		return len(sender.of(protocol.TypeDeployOutput)) == 1
	})
	var fragment protocol.DeployOutput
	if err := json.Unmarshal(mustJSON(t, sender.of(protocol.TypeDeployOutput)[0].payload), &fragment); err != nil {
		t.Fatalf("decode the fragment: %v", err)
	}
	if fragment.TaskID != "task-1" {
		t.Fatalf("the fragment holds the task %s, want task-1", fragment.TaskID)
	}
	if fragment.Offset != 0 {
		t.Fatalf("the offset = %d, want 0", fragment.Offset)
	}
	if string(fragment.Data) != "the output of the task" {
		t.Fatalf("the fragment holds %q, want the output of the task", fragment.Data)
	}
}

// TestTheQuestionAboutTheLedgerRunsNoCommand proves that a question is
// answered from the ledger and runs nothing.
func TestTheQuestionAboutTheLedgerRunsNoCommand(t *testing.T) {
	executor := &countingExecutor{}
	ledger, handler := newTestAgent(t, executor)
	sender := &recordingSender{}

	command := model.Command{
		Task: model.Task{ID: "task-1", AppID: "app-1", Digest: "digest-1"},
		App:  model.App{ID: "app-1", Mode: "compose"},
	}
	handler.handle(context.Background(), commandFrame(t, command), sender)
	waitForAgent(t, "the report of the task", func() bool {
		return len(sender.of(protocol.TypeDeployReceipt)) == 1
	})

	payload, err := json.Marshal(protocol.DeployQuery{TaskID: "task-1"})
	if err != nil {
		t.Fatalf("encode the question: %v", err)
	}
	handler.handle(context.Background(), protocol.Envelope{
		Version: protocol.WireVersion, Type: protocol.TypeDeployQuery, Payload: payload,
	}, sender)

	waitForAgent(t, "the answer of the node", func() bool {
		return len(sender.of(protocol.TypeDeployQueryResult)) == 1
	})
	var answer protocol.DeployQueryResult
	if err := json.Unmarshal(mustJSON(t, sender.of(protocol.TypeDeployQueryResult)[0].payload), &answer); err != nil {
		t.Fatalf("decode the answer: %v", err)
	}
	if !answer.Found {
		t.Fatal("the node does not know the task it executed")
	}
	if answer.Receipt.TaskID != "task-1" {
		t.Fatalf("the answer holds the task %s, want task-1", answer.Receipt.TaskID)
	}
	if executor.count() != 1 {
		t.Fatalf("executions = %d, want 1", executor.count())
	}
	_ = ledger
}

// TestTheQuestionAboutAnUnknownTaskIsAnswered proves that the node answers
// a question about a task it does not know.
func TestTheQuestionAboutAnUnknownTaskIsAnswered(t *testing.T) {
	executor := &countingExecutor{}
	_, handler := newTestAgent(t, executor)
	sender := &recordingSender{}

	payload, err := json.Marshal(protocol.DeployQuery{TaskID: "task-that-never-arrived"})
	if err != nil {
		t.Fatalf("encode the question: %v", err)
	}
	handler.handle(context.Background(), protocol.Envelope{
		Version: protocol.WireVersion, Type: protocol.TypeDeployQuery, Payload: payload,
	}, sender)

	waitForAgent(t, "the answer of the node", func() bool {
		return len(sender.of(protocol.TypeDeployQueryResult)) == 1
	})
	var answer protocol.DeployQueryResult
	if err := json.Unmarshal(mustJSON(t, sender.of(protocol.TypeDeployQueryResult)[0].payload), &answer); err != nil {
		t.Fatalf("decode the answer: %v", err)
	}
	if answer.Found {
		t.Fatal("the node knows a task it never received")
	}
	if executor.count() != 0 {
		t.Fatalf("executions = %d, want 0", executor.count())
	}
}

// TestTheResolutionEndsTheTaskInTheLedger proves that a resolution of the
// control端 ends the task in the ledger of the node.
func TestTheResolutionEndsTheTaskInTheLedger(t *testing.T) {
	executor := &countingExecutor{}
	path := filepath.Join(t.TempDir(), "ledger.db")
	uncertainTask(t, path, "task-1", "app-1")
	ledger, handler := newTestAgentAt(t, executor, path)
	sender := &recordingSender{}

	// The node does not know the outcome of the task.
	known, err := ledger.Lookup(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("read the task: %v", err)
	}
	if known.Result.State != model.Uncertain {
		t.Fatalf("the task is in the phase %q, want uncertain", known.Result.State)
	}

	payload, err := json.Marshal(protocol.DeployResolve{
		TaskID:      "task-1",
		Digest:      "digest-1",
		ExpectedSeq: known.Seq,
		Reason:      "the executor of the task was stopped by hand",
	})
	if err != nil {
		t.Fatalf("encode the resolution: %v", err)
	}
	handler.handle(context.Background(), protocol.Envelope{
		Version: protocol.WireVersion, Type: protocol.TypeDeployResolve, Payload: payload,
	}, sender)

	waitForAgent(t, "the report of the resolution", func() bool {
		return len(sender.of(protocol.TypeDeployReceipt)) == 1
	})
	var message protocol.DeployReceipt
	if err := json.Unmarshal(mustJSON(t, sender.of(protocol.TypeDeployReceipt)[0].payload), &message); err != nil {
		t.Fatalf("decode the report: %v", err)
	}
	if message.Receipt.Result.Code != CodeManualResolution {
		t.Fatalf("the report holds the code %q, want %q", message.Receipt.Result.Code, CodeManualResolution)
	}
	if message.Receipt.Result.State != model.Failed {
		t.Fatalf("the task is in the phase %q, want failed", message.Receipt.Result.State)
	}
}

// TestTheResolutionOfAnotherSequenceNumberIsRefused proves that the node
// refuses a resolution that covers a newer state of the task and that it
// answers with the real state of the task.
func TestTheResolutionOfAnotherSequenceNumberIsRefused(t *testing.T) {
	executor := &countingExecutor{}
	path := filepath.Join(t.TempDir(), "ledger.db")
	uncertainTask(t, path, "task-1", "app-1")
	_, handler := newTestAgentAt(t, executor, path)
	sender := &recordingSender{}

	payload, err := json.Marshal(protocol.DeployResolve{
		TaskID:      "task-1",
		Digest:      "digest-1",
		ExpectedSeq: 7,
		Reason:      "the executor of the task was stopped by hand",
	})
	if err != nil {
		t.Fatalf("encode the resolution: %v", err)
	}
	handler.handle(context.Background(), protocol.Envelope{
		Version: protocol.WireVersion, Type: protocol.TypeDeployResolve, Payload: payload,
	}, sender)

	waitForAgent(t, "the answer of the node", func() bool {
		return len(sender.of(protocol.TypeDeployQueryResult)) == 1
	})
	var answer protocol.DeployQueryResult
	if err := json.Unmarshal(mustJSON(t, sender.of(protocol.TypeDeployQueryResult)[0].payload), &answer); err != nil {
		t.Fatalf("decode the answer: %v", err)
	}
	if !answer.Found {
		t.Fatal("the node does not know the task")
	}
	if len(sender.of(protocol.TypeDeployReceipt)) != 0 {
		t.Fatal("the node reported a resolution it refused")
	}
}

// TestTheCommandOfABusyApplicationWaits proves that a command of an
// application that holds another task waits instead of failing.
func TestTheCommandOfABusyApplicationWaits(t *testing.T) {
	previous := busyRetryInterval
	busyRetryInterval = 10 * time.Millisecond
	t.Cleanup(func() { busyRetryInterval = previous })

	executor := &countingExecutor{}
	path := filepath.Join(t.TempDir(), "ledger.db")
	uncertainTask(t, path, "task-1", "app-1")
	ledger, handler := newTestAgentAt(t, executor, path)
	sender := &recordingSender{}

	// The command of the control端 waits for the application.
	waiting := model.Command{
		Task: model.Task{ID: "task-2", AppID: "app-1", Digest: "digest-2"},
		App:  model.App{ID: "app-1", Mode: "compose"},
	}
	handler.handle(context.Background(), commandFrame(t, waiting), sender)

	time.Sleep(100 * time.Millisecond)
	if executor.count() != 0 {
		t.Fatalf("executions = %d, want 0: the command of a busy application ran", executor.count())
	}
	if len(sender.of(protocol.TypeDeployReceipt)) != 0 {
		t.Fatal("the agent reported a failure for a busy application")
	}

	// The administrator releases the task of the application, so the command
	// runs.
	known, err := ledger.Lookup(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("read the task: %v", err)
	}
	if _, err := ledger.Resolve(context.Background(), "task-1", known.Seq, model.Result{
		State:   model.Failed,
		Message: "the executor of the task was stopped by hand",
	}); err != nil {
		t.Fatalf("release the task: %v", err)
	}

	waitForAgent(t, "the report of the task", func() bool {
		return len(sender.of(protocol.TypeDeployReceipt)) == 1
	})
	if executor.count() != 1 {
		t.Fatalf("executions = %d, want 1", executor.count())
	}
}

// TestTheWorkerUsesTheContextOfTheProcess proves that the worker runs the
// command under the context of the process of the agent.
func TestTheWorkerUsesTheContextOfTheProcess(t *testing.T) {
	ledger, err := OpenLedger(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}
	t.Cleanup(func() { ledger.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	worker := NewWorker(ledger, &countingExecutor{}).UseRootContext(ctx)
	cancel()

	if _, err := worker.Handle(context.Background(), model.Command{
		Task: model.Task{ID: "task-1", AppID: "app-1", Digest: "digest-1"},
		App:  model.App{ID: "app-1", Mode: "compose"},
	}, io.Discard); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("handle the command: %v", err)
	}
}

// mustJSON encodes a payload of a frame.
func mustJSON(t *testing.T, payload any) []byte {
	t.Helper()

	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode the payload: %v", err)
	}
	return encoded
}
