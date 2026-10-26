package agent_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"hmoob-dock/internal/agent"
	"hmoob-dock/internal/model"
)

// fakeExecutor is the executor of the tests.
//
// The executor counts its calls, so a test can prove that a command runs
// once. It can also wait for a release, so a test can interrupt the
// execution of a task.
type fakeExecutor struct {
	mu sync.Mutex
	// calls counts the executions of the executor.
	calls int
	// result is the outcome the executor reports.
	result model.Result
	// err is the error the executor reports.
	err error
	// output is written into the writer of the execution.
	output []byte

	// started closes when the executor begins.
	started chan struct{}
	// release unblocks the executor.
	release chan struct{}
	// once guards the closing of started.
	once sync.Once
}

// Execute runs the command of the test.
func (f *fakeExecutor) Execute(ctx context.Context, cmd model.Command, output io.Writer) (model.Result, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()

	if f.started != nil {
		f.once.Do(func() { close(f.started) })
	}
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return model.Result{State: model.Failed, Code: "cancelled"}, ctx.Err()
		}
	}
	if len(f.output) > 0 {
		if _, err := output.Write(f.output); err != nil {
			return model.Result{State: model.Failed, Code: "write_failed"}, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.result, f.err
}

// count returns the number of executions of the executor.
func (f *fakeExecutor) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// TestTheWorkerRunsACommandOnlyOnce proves that a command that arrives
// twice runs the Docker command at most once.
func TestTheWorkerRunsACommandOnlyOnce(t *testing.T) {
	ledger := openLedger(t, ledgerPath(t))
	executor := &fakeExecutor{result: model.Result{State: model.Succeeded, Code: "deployed"}}
	worker := agent.NewWorker(ledger, executor)
	ctx := context.Background()
	cmd := commandOf("task-1", "app-1", "digest-1")

	first, err := worker.Handle(ctx, cmd, io.Discard)
	if err != nil {
		t.Fatalf("handle the command: %v", err)
	}
	if first.Result.State != model.Succeeded {
		t.Fatalf("the state of the first receipt = %q, want %q", first.Result.State, model.Succeeded)
	}

	second, err := worker.Handle(ctx, cmd, io.Discard)
	if err != nil {
		t.Fatalf("handle the command again: %v", err)
	}
	if executor.count() != 1 {
		t.Fatalf("the executor ran %d times, want 1", executor.count())
	}
	if second.Seq != first.Seq {
		t.Fatalf("the sequence number of the repeated receipt = %d, want %d", second.Seq, first.Seq)
	}
	if second.Result.State != model.Succeeded {
		t.Fatalf("the state of the repeated receipt = %q, want %q", second.Result.State, model.Succeeded)
	}
}

// TestTheWorkerKeepsTheTaskRunningWhenTheConnectionEnds proves that a
// connection that ends does not cancel the Docker command of a task that
// the agent accepted.
func TestTheWorkerKeepsTheTaskRunningWhenTheConnectionEnds(t *testing.T) {
	ledger := openLedger(t, ledgerPath(t))
	executor := &fakeExecutor{
		result:  model.Result{State: model.Succeeded, Code: "deployed"},
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	worker := agent.NewWorker(ledger, executor)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type outcome struct {
		receipt model.Receipt
		err     error
	}
	results := make(chan outcome, 1)
	go func() {
		receipt, err := worker.Handle(ctx, commandOf("task-1", "app-1", "digest-1"), io.Discard)
		results <- outcome{receipt: receipt, err: err}
	}()

	<-executor.started
	// The connection of the control端 ends while the command runs.
	cancel()

	select {
	case result := <-results:
		t.Fatalf("the task ended with the connection: %+v", result)
	case <-time.After(200 * time.Millisecond):
	}

	close(executor.release)
	result := <-results
	if result.err != nil {
		t.Fatalf("handle the command: %v", result.err)
	}
	if result.receipt.Result.State != model.Succeeded {
		t.Fatalf("the state of the receipt = %q, want %q", result.receipt.Result.State, model.Succeeded)
	}
}

// TestTheWorkerRecordsTheOutcomeOfTheExecutor proves that the outcome of
// the executor reaches the ledger.
func TestTheWorkerRecordsTheOutcomeOfTheExecutor(t *testing.T) {
	ledger := openLedger(t, ledgerPath(t))
	executor := &fakeExecutor{result: model.Result{
		State:         model.Failed,
		Code:          "not_ready",
		Message:       "the service did not become ready",
		PartialUpdate: true,
	}}
	worker := agent.NewWorker(ledger, executor)
	ctx := context.Background()

	receipt, err := worker.Handle(ctx, commandOf("task-1", "app-1", "digest-1"), io.Discard)
	if err != nil {
		t.Fatalf("handle the command: %v", err)
	}
	if receipt.Result.State != model.Failed || receipt.Result.Code != "not_ready" {
		t.Fatalf("the result of the receipt = %+v, want the outcome of the executor", receipt.Result)
	}

	looked, err := ledger.Lookup(ctx, "task-1")
	if err != nil {
		t.Fatalf("look the task up: %v", err)
	}
	if looked.Seq != receipt.Seq {
		t.Fatalf("the sequence number of the ledger = %d, want %d", looked.Seq, receipt.Seq)
	}
	if looked.Result.State != model.Failed {
		t.Fatalf("the state in the ledger = %q, want %q", looked.Result.State, model.Failed)
	}
}

// TestTheWorkerMarksATaskUncertainWhenTheCommandRunsOutOfTime proves that
// a command that ran out of time leaves the result of the task unknown.
func TestTheWorkerMarksATaskUncertainWhenTheCommandRunsOutOfTime(t *testing.T) {
	ledger := openLedger(t, ledgerPath(t))
	executor := &fakeExecutor{err: context.DeadlineExceeded}
	worker := agent.NewWorker(ledger, executor)
	ctx := context.Background()

	receipt, err := worker.Handle(ctx, commandOf("task-1", "app-1", "digest-1"), io.Discard)
	if err != nil {
		t.Fatalf("handle the command: %v", err)
	}
	if receipt.Result.State != model.Uncertain {
		t.Fatalf("the state of the receipt = %q, want %q", receipt.Result.State, model.Uncertain)
	}
}

// TestTheWorkerMarksATaskFailedWhenTheExecutorFails proves that an executor
// that fails in a way it cannot describe reports a failed task.
func TestTheWorkerMarksATaskFailedWhenTheExecutorFails(t *testing.T) {
	ledger := openLedger(t, ledgerPath(t))
	executor := &fakeExecutor{err: errors.New("the docker binary is missing")}
	worker := agent.NewWorker(ledger, executor)
	ctx := context.Background()

	receipt, err := worker.Handle(ctx, commandOf("task-1", "app-1", "digest-1"), io.Discard)
	if err != nil {
		t.Fatalf("handle the command: %v", err)
	}
	if receipt.Result.State != model.Failed {
		t.Fatalf("the state of the receipt = %q, want %q", receipt.Result.State, model.Failed)
	}
	if !strings.Contains(receipt.Result.Message, "docker binary") {
		t.Fatalf("the message of the receipt = %q, want the error of the executor", receipt.Result.Message)
	}
}

// TestTheWorkerLimitsTheOutputOfATask proves that the output of a task is
// limited and that the truncation is reported.
func TestTheWorkerLimitsTheOutputOfATask(t *testing.T) {
	ledger := openLedger(t, ledgerPath(t))
	executor := &fakeExecutor{
		result: model.Result{State: model.Succeeded, Code: "deployed"},
		output: bytes.Repeat([]byte("x"), 3*1024*1024),
	}
	worker := agent.NewWorker(ledger, executor)
	ctx := context.Background()

	var sink bytes.Buffer
	receipt, err := worker.Handle(ctx, commandOf("task-1", "app-1", "digest-1"), &sink)
	if err != nil {
		t.Fatalf("handle the command: %v", err)
	}
	if sink.Len() > agent.MaxTaskOutputBytes {
		t.Fatalf("the output holds %d bytes, want at most %d", sink.Len(), agent.MaxTaskOutputBytes)
	}
	if !strings.Contains(receipt.Result.Message, "截断") {
		t.Fatalf("the message of the receipt = %q, want a notice about the truncated output", receipt.Result.Message)
	}
}

// TestTheWorkerRefusesACommandOfAnotherApplication proves that the worker
// reports the refusal of the ledger instead of running the command.
func TestTheWorkerRefusesACommandOfAnotherApplication(t *testing.T) {
	ledger := openLedger(t, ledgerPath(t))
	executor := &fakeExecutor{
		result:  model.Result{State: model.Succeeded, Code: "deployed"},
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	worker := agent.NewWorker(ledger, executor)
	ctx := context.Background()

	// The first task runs, so the application is held.
	results := make(chan error, 1)
	go func() {
		_, err := worker.Handle(ctx, commandOf("task-1", "app-1", "digest-1"), io.Discard)
		results <- err
	}()
	<-executor.started

	_, err := worker.Handle(ctx, commandOf("task-2", "app-1", "digest-2"), io.Discard)
	if !errors.Is(err, agent.ErrAppBusy) {
		t.Fatalf("the worker reported %v, want a busy application", err)
	}
	if executor.count() != 1 {
		t.Fatalf("the executor ran %d times, want 1", executor.count())
	}

	close(executor.release)
	if err := <-results; err != nil {
		t.Fatalf("handle the command: %v", err)
	}
}

// TestTheWorkerContinuesATaskThatWasInterruptedBeforeItsIntent proves that
// a task whose reception reached the disk but whose execution never began
// runs after the restart of the agent.
func TestTheWorkerContinuesATaskThatWasInterruptedBeforeItsIntent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	ledger, err := agent.OpenLedger(path)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}
	ctx := context.Background()

	// The command was received, but the agent stopped before it took the
	// intent to execute it.
	if _, fresh, err := ledger.Accept(ctx, commandOf("task-1", "app-1", "digest-1")); err != nil || !fresh {
		t.Fatalf("accept: %v", err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openLedger(t, path)
	executor := &fakeExecutor{result: model.Result{State: model.Succeeded, Code: "deployed"}}
	worker := agent.NewWorker(reopened, executor)

	receipt, err := worker.Handle(ctx, commandOf("task-1", "app-1", "digest-1"), io.Discard)
	if err != nil {
		t.Fatalf("handle the command: %v", err)
	}
	if executor.count() != 1 {
		t.Fatalf("the executor ran %d times, want 1", executor.count())
	}
	if receipt.Result.State != model.Succeeded {
		t.Fatalf("the state of the receipt = %q, want %q", receipt.Result.State, model.Succeeded)
	}
}

// TestTheWorkerKeepsTheResultOfATaskAfterARestart proves that the outcome
// of a task survives the restart of the agent and that the task is not run
// a second time.
func TestTheWorkerKeepsTheResultOfATaskAfterARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	ledger, err := agent.OpenLedger(path)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}
	executor := &fakeExecutor{result: model.Result{State: model.Succeeded, Code: "deployed"}}
	worker := agent.NewWorker(ledger, executor)
	ctx := context.Background()

	receipt, err := worker.Handle(ctx, commandOf("task-1", "app-1", "digest-1"), io.Discard)
	if err != nil {
		t.Fatalf("handle the command: %v", err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openLedger(t, path)
	restarted := agent.NewWorker(reopened, executor)
	again, err := restarted.Handle(ctx, commandOf("task-1", "app-1", "digest-1"), io.Discard)
	if err != nil {
		t.Fatalf("handle the command again: %v", err)
	}
	if executor.count() != 1 {
		t.Fatalf("the executor ran %d times, want 1", executor.count())
	}
	if again.Seq != receipt.Seq || again.Result.State != model.Succeeded {
		t.Fatalf("the receipt after the restart = %+v, want %+v", again, receipt)
	}
}
