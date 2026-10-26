package agent

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"unicode/utf8"

	"hmoob-dock/internal/dockerops"
	"hmoob-dock/internal/model"
)

// The codes of the outcomes that the worker produces.
const (
	// CodeExecutionUncertain marks a task whose command ran out of time.
	// The Docker daemon may still work on the change.
	CodeExecutionUncertain = "execution_uncertain"
	// CodeExecutionFailed marks a task whose command failed in a way the
	// executor could not describe.
	CodeExecutionFailed = "execution_failed"
)

// maxResultMessage bounds the message of an outcome. The message reaches
// the page of the administrator, so it stays short.
const maxResultMessage = 512

// Worker executes the deployment commands of the control端.
//
// The worker owns the execution of a task: it writes the command into the
// ledger, takes the intent to execute it and runs the Docker command once.
// A command that arrives twice runs the Docker command at most once, not
// even after a restart of the agent.
//
// The worker lowers the risk of a repeated execution; it does not promise
// that a command runs exactly once in every case. A command that was
// received but whose intent was not written may run after a restart of the
// agent, and a command whose intent was written never runs a second time,
// but its result stays unknown when the agent stops in the middle of it.
type Worker struct {
	// ledger holds the tasks of the node.
	ledger *Ledger
	// executor runs the Docker command of a task.
	executor dockerops.Executor
	// root is the context of the process of the agent. The execution of a
	// task runs under it, so a connection that ends does not cancel a
	// command that already runs.
	root context.Context
}

// NewWorker builds the worker of the agent.
func NewWorker(ledger *Ledger, executor dockerops.Executor) *Worker {
	return &Worker{
		ledger:   ledger,
		executor: executor,
		root:     context.Background(),
	}
}

// Handle executes a command of the control端.
//
// The command is written into the ledger before anything runs, so a command
// that is delivered twice runs the Docker command at most once. The
// execution runs under the context of the process of the agent: a
// connection that ends does not cancel a Docker command that already runs.
//
// The returned error reports a failure of the agent itself, for example a
// command the ledger refused. The outcome of the task is in the receipt,
// also when the task failed.
func (w *Worker) Handle(ctx context.Context, cmd model.Command, output io.Writer) (model.Receipt, error) {
	if w.ledger == nil {
		return model.Receipt{}, errors.New("agent: the worker has no ledger")
	}
	if w.executor == nil {
		return model.Receipt{}, errors.New("agent: the worker has no executor")
	}

	// The reception is written with the context of the caller: the command
	// belongs to the delivery that brought it.
	if _, _, err := w.ledger.Accept(ctx, cmd); err != nil {
		return model.Receipt{}, err
	}

	// The intent is taken with the context of the process, so a connection
	// that ends between the reception and the intent does not leave the
	// task without an owner.
	started, err := w.ledger.Begin(w.root, cmd.Task.ID)
	if err != nil {
		return model.Receipt{}, err
	}
	if !started {
		// The execution of the task began already, either in this process
		// or before a restart of the agent. The Docker command must not run
		// a second time, so the worker reports the state of the task.
		return w.ledger.Lookup(w.root, cmd.Task.ID)
	}

	return w.run(cmd, output)
}

// run executes the command of a task and records its outcome.
//
// The intent of the task is durable before this function is called, so the
// Docker command runs at most once even when the agent stops in the middle
// of it. A task whose outcome was not recorded waits for a check then.
func (w *Worker) run(cmd model.Command, output io.Writer) (model.Receipt, error) {
	writer := newOutputWriter(output, w.ledger, cmd.Task.ID, w.root)

	outcome, err := w.executor.Execute(w.root, cmd, writer)
	switch {
	case err != nil:
		outcome = outcomeOf(err)
	case !finished(outcome.State):
		// An executor that reports a phase instead of an outcome would
		// leave the task unfinished forever, and the application blocked
		// with it.
		outcome = model.Result{
			State:   model.Failed,
			Code:    CodeExecutionFailed,
			Message: "执行器没有报告任务的最终状态",
		}
	}
	if writer.truncated {
		outcome.Message = strings.TrimSpace(outcome.Message + " " + truncationNotice)
	}
	// The cursor of the output is a note about the task, not its outcome.
	// A cursor that could not be written is reported, but it does not turn
	// a finished deployment into a failure.
	if cursorErr := writer.close(); cursorErr != nil {
		log.Printf("agent: record the output of the task %s: %v", cmd.Task.ID, cursorErr)
	}

	receipt, err := w.ledger.Record(w.root, cmd.Task.ID, outcome)
	if err != nil {
		return model.Receipt{}, err
	}
	return receipt, nil
}

// truncationNotice tells the administrator that the output of the task was
// cut short.
const truncationNotice = "输出已截断：任务的输出超过了 2 MiB 的上限"

// outcomeOf turns the error of the executor into the outcome of a task.
//
// A command that ran out of time may still be working in the Docker daemon,
// so the outcome of the task is uncertain. Every other error is a failure
// that the executor could not describe.
func outcomeOf(err error) model.Result {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return model.Result{
			State:   model.Uncertain,
			Code:    CodeExecutionUncertain,
			Message: "命令没有在期限内结束，目标可能仍在变更",
		}
	}
	return model.Result{
		State:   model.Failed,
		Code:    CodeExecutionFailed,
		Message: truncateMessage(err.Error()),
	}
}

// truncateMessage shortens the message of an outcome.
func truncateMessage(message string) string {
	message = strings.TrimSpace(message)
	if len(message) <= maxResultMessage {
		return message
	}
	cut := message[:maxResultMessage]
	// The cut may fall into a character of a text of several bytes.
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}

// outputWriter streams the output of a task to the control端.
//
// The output of a task is limited: the bytes beyond the limit are dropped
// and the task is marked as truncated. The limit belongs here and not in
// the executor, so every executor is limited the same way.
type outputWriter struct {
	// sink receives the output of the task.
	sink io.Writer
	// ledger holds the cursor of the output.
	ledger *Ledger
	// taskID is the task the output belongs to.
	taskID string
	// ctx is the context of the process of the agent.
	ctx context.Context

	// limit is the largest number of bytes the task may produce.
	limit int64
	// written is the number of bytes the task produced.
	written int64
	// truncated reports that the output of the task was cut short.
	truncated bool
	// err is the error of the sink.
	err error
}

// newOutputWriter builds the writer of the output of a task.
func newOutputWriter(sink io.Writer, ledger *Ledger, taskID string, ctx context.Context) *outputWriter {
	if sink == nil {
		sink = io.Discard
	}
	return &outputWriter{
		sink:   sink,
		ledger: ledger,
		taskID: taskID,
		ctx:    ctx,
		limit:  MaxTaskOutputBytes,
	}
}

// Write forwards the output of the task to the control端.
//
// The bytes beyond the limit of the task are dropped on purpose. The writer
// reports them as written, so the command does not fail because of the
// limit of the output.
func (w *outputWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}

	total := len(p)
	remaining := w.limit - w.written
	if remaining <= 0 {
		w.truncated = true
		return total, nil
	}

	chunk := p
	if int64(len(chunk)) > remaining {
		chunk = chunk[:remaining]
		w.truncated = true
	}

	written, err := w.sink.Write(chunk)
	w.written += int64(written)
	if err != nil {
		w.err = err
		return written, err
	}
	if len(chunk) < len(p) {
		return total, nil
	}
	return written, nil
}

// close records the cursor of the output in the ledger.
func (w *outputWriter) close() error {
	if w.written == 0 {
		return nil
	}
	if _, err := w.ledger.AdvanceOutput(w.ctx, w.taskID, w.written); err != nil {
		return err
	}
	return nil
}
