package agent_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"hmoob-dock/internal/agent"
	"hmoob-dock/internal/model"
)

// ledgerPath builds the path of a ledger in a directory of the test.
func ledgerPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "ledger.db")
}

// openLedger opens a ledger of the test.
func openLedger(t *testing.T, path string) *agent.Ledger {
	t.Helper()
	ledger, err := agent.OpenLedger(path)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	return ledger
}

// commandOf builds the command of a task.
func commandOf(taskID, appID, digest string) model.Command {
	return model.Command{
		Task: model.Task{ID: taskID, AppID: appID, VersionID: "v1", Digest: digest},
	}
}

// uncertainTask accepts a task, takes the intent to execute it and reopens
// the ledger, so the task waits for a check.
//
// The agent was stopped in the middle of the task, so the ledger does not
// know the outcome of it.
func uncertainTask(t *testing.T, path string) *agent.Ledger {
	t.Helper()

	ledger, err := agent.OpenLedger(path)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}
	ctx := context.Background()
	if _, fresh, err := ledger.Accept(ctx, commandOf("task-1", "app-1", "digest-1")); err != nil || !fresh {
		t.Fatalf("accept: %v", err)
	}
	if begin, err := ledger.Begin(ctx, "task-1"); err != nil || !begin {
		t.Fatalf("begin: %v", err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := agent.OpenLedger(path)
	if err != nil {
		t.Fatalf("open the ledger again: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	return reopened
}

// TestExecutionIntentSurvivesReopen proves that a task whose execution
// began is not executed a second time after a restart of the agent.
func TestExecutionIntentSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	ledger, err := agent.OpenLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cmd := model.Command{Task: model.Task{ID: "task-1", AppID: "app-1", VersionID: "v1", Digest: "digest-1"}}
	if _, fresh, err := ledger.Accept(ctx, cmd); err != nil || !fresh {
		t.Fatalf("accept: %v", err)
	}
	if begin, err := ledger.Begin(ctx, "task-1"); err != nil || !begin {
		t.Fatalf("begin: %v", err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := agent.OpenLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if begin, err := reopened.Begin(ctx, "task-1"); err != nil || begin {
		t.Fatalf("execution replay after restart: %v", err)
	}
}

// TestTheLedgerRefusesTheSameTaskWithAnotherConfiguration proves that a task
// ID that arrives again with another configuration is refused.
func TestTheLedgerRefusesTheSameTaskWithAnotherConfiguration(t *testing.T) {
	ledger := openLedger(t, ledgerPath(t))
	ctx := context.Background()

	if _, fresh, err := ledger.Accept(ctx, commandOf("task-1", "app-1", "digest-1")); err != nil || !fresh {
		t.Fatalf("accept: %v", err)
	}

	_, fresh, err := ledger.Accept(ctx, commandOf("task-1", "app-1", "digest-2"))
	if err == nil {
		t.Fatal("the ledger accepted the same task with another configuration")
	}
	if !errors.Is(err, agent.ErrDigestConflict) {
		t.Fatalf("the ledger reported %v, want a conflict of the configuration", err)
	}
	if fresh {
		t.Fatal("the ledger reported a new reception of a task it knows")
	}
}

// TestTheLedgerRefusesASecondTaskOfTheSameApplication proves that an
// application holds at most one task that did not finish.
func TestTheLedgerRefusesASecondTaskOfTheSameApplication(t *testing.T) {
	ledger := openLedger(t, ledgerPath(t))
	ctx := context.Background()

	if _, fresh, err := ledger.Accept(ctx, commandOf("task-1", "app-1", "digest-1")); err != nil || !fresh {
		t.Fatalf("accept: %v", err)
	}

	_, fresh, err := ledger.Accept(ctx, commandOf("task-2", "app-1", "digest-2"))
	if err == nil {
		t.Fatal("the ledger accepted a second task of the same application")
	}
	if !errors.Is(err, agent.ErrAppBusy) {
		t.Fatalf("the ledger reported %v, want a busy application", err)
	}
	if fresh {
		t.Fatal("the ledger reported a new reception of a task it refused")
	}
}

// TestTheLedgerAcceptsATaskOnlyOnce proves that a repeated command is
// answered with the receipt of the first reception.
func TestTheLedgerAcceptsATaskOnlyOnce(t *testing.T) {
	ledger := openLedger(t, ledgerPath(t))
	ctx := context.Background()
	cmd := commandOf("task-1", "app-1", "digest-1")

	first, fresh, err := ledger.Accept(ctx, cmd)
	if err != nil || !fresh {
		t.Fatalf("accept: %v", err)
	}
	if first.TaskID != "task-1" || first.Digest != "digest-1" {
		t.Fatalf("the receipt = %+v, want the task and its configuration", first)
	}
	if first.Result.State != model.Queued {
		t.Fatalf("the state of the receipt = %q, want %q", first.Result.State, model.Queued)
	}

	second, fresh, err := ledger.Accept(ctx, cmd)
	if err != nil {
		t.Fatalf("accept the command again: %v", err)
	}
	if fresh {
		t.Fatal("the ledger reported a new reception of a task it knows")
	}
	if second.Seq != first.Seq {
		t.Fatalf("the sequence number of the repeated receipt = %d, want %d", second.Seq, first.Seq)
	}
	if second.Result.State != first.Result.State {
		t.Fatalf("the state of the repeated receipt = %q, want %q", second.Result.State, first.Result.State)
	}
}

// TestTheLedgerRecordsTheOutcomeOfATask proves that the outcome of a task
// survives the closing of the ledger.
func TestTheLedgerRecordsTheOutcomeOfATask(t *testing.T) {
	path := ledgerPath(t)
	ledger := openLedger(t, path)
	ctx := context.Background()
	cmd := commandOf("task-1", "app-1", "digest-1")

	if _, fresh, err := ledger.Accept(ctx, cmd); err != nil || !fresh {
		t.Fatalf("accept: %v", err)
	}
	if begin, err := ledger.Begin(ctx, "task-1"); err != nil || !begin {
		t.Fatalf("begin: %v", err)
	}

	outcome := model.Result{
		State:         model.Succeeded,
		Code:          "deployed",
		Message:       "the application is ready",
		PartialUpdate: true,
	}
	receipt, err := ledger.Record(ctx, "task-1", outcome)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if receipt.Seq <= 1 {
		t.Fatalf("the sequence number of the receipt = %d, want more than the reception", receipt.Seq)
	}
	if receipt.Result.State != model.Succeeded || receipt.Result.Code != "deployed" {
		t.Fatalf("the result of the receipt = %+v, want the recorded one", receipt.Result)
	}
	if !receipt.Result.PartialUpdate {
		t.Fatal("the receipt lost the partial update of the result")
	}

	reopened := openLedger(t, path)
	looked, err := reopened.Lookup(ctx, "task-1")
	if err != nil {
		t.Fatalf("look the task up: %v", err)
	}
	if looked.Seq != receipt.Seq || looked.Result.State != model.Succeeded {
		t.Fatalf("the receipt of the reopened ledger = %+v, want %+v", looked, receipt)
	}
}

// TestTheLedgerKeepsTheSequenceOfARespeatedOutcome proves that a repeated
// record does not change the receipt of a task that already finished.
func TestTheLedgerKeepsTheSequenceOfARespeatedOutcome(t *testing.T) {
	ledger := openLedger(t, ledgerPath(t))
	ctx := context.Background()

	if _, fresh, err := ledger.Accept(ctx, commandOf("task-1", "app-1", "digest-1")); err != nil || !fresh {
		t.Fatalf("accept: %v", err)
	}
	if begin, err := ledger.Begin(ctx, "task-1"); err != nil || !begin {
		t.Fatalf("begin: %v", err)
	}

	first, err := ledger.Record(ctx, "task-1", model.Result{State: model.Succeeded, Code: "deployed"})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	second, err := ledger.Record(ctx, "task-1", model.Result{State: model.Succeeded, Code: "deployed"})
	if err != nil {
		t.Fatalf("record the outcome again: %v", err)
	}
	if second.Seq != first.Seq {
		t.Fatalf("the sequence number of the repeated receipt = %d, want %d", second.Seq, first.Seq)
	}
}

// TestTheLedgerMarksAnInterruptedTaskUncertain proves that a task whose
// execution began before the agent stopped waits for a check.
func TestTheLedgerMarksAnInterruptedTaskUncertain(t *testing.T) {
	path := ledgerPath(t)
	ledger := openLedger(t, path)
	ctx := context.Background()

	if _, fresh, err := ledger.Accept(ctx, commandOf("task-1", "app-1", "digest-1")); err != nil || !fresh {
		t.Fatalf("accept: %v", err)
	}
	if begin, err := ledger.Begin(ctx, "task-1"); err != nil || !begin {
		t.Fatalf("begin: %v", err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openLedger(t, path)
	receipt, err := reopened.Lookup(ctx, "task-1")
	if err != nil {
		t.Fatalf("look the task up: %v", err)
	}
	if receipt.Result.State != model.Uncertain {
		t.Fatalf("the state of the interrupted task = %q, want %q", receipt.Result.State, model.Uncertain)
	}
	if receipt.Seq <= 1 {
		t.Fatalf("the sequence number of the receipt = %d, want more than the reception", receipt.Seq)
	}
}

// TestTheLedgerKeepsAnUncertainTaskExclusive proves that a task whose result
// is unknown still holds its application.
func TestTheLedgerKeepsAnUncertainTaskExclusive(t *testing.T) {
	path := ledgerPath(t)
	ledger := openLedger(t, path)
	ctx := context.Background()

	if _, fresh, err := ledger.Accept(ctx, commandOf("task-1", "app-1", "digest-1")); err != nil || !fresh {
		t.Fatalf("accept: %v", err)
	}
	if begin, err := ledger.Begin(ctx, "task-1"); err != nil || !begin {
		t.Fatalf("begin: %v", err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openLedger(t, path)
	if _, _, err := reopened.Accept(ctx, commandOf("task-2", "app-1", "digest-2")); err == nil {
		t.Fatal("the ledger accepted a second task of an application whose result is unknown")
	} else if !errors.Is(err, agent.ErrAppBusy) {
		t.Fatalf("the ledger reported %v, want a busy application", err)
	}
}

// TestTheLedgerKeepsTheConfigurationOfATaskOutOfItsFile proves that the
// ledger does not hold the document or the variables of a deployment.
func TestTheLedgerKeepsTheConfigurationOfATaskOutOfItsFile(t *testing.T) {
	path := ledgerPath(t)
	ledger := openLedger(t, path)
	ctx := context.Background()

	cmd := commandOf("task-1", "app-1", "digest-1")
	cmd.Config = model.Config{
		YAML:      "services:\n  api:\n    image: nginx\n",
		Variables: map[string]string{"PASSWORD": "the-secret-of-the-application"},
	}
	if _, fresh, err := ledger.Accept(ctx, cmd); err != nil || !fresh {
		t.Fatalf("accept: %v", err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}

	// The data of the ledger may still be in the write ahead log, so
	// both files are read.
	content := ""
	for _, name := range []string{path, path + "-wal"} {
		data, err := os.ReadFile(name)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			t.Fatalf("read %s: %v", name, err)
		}
		content += string(data)
	}
	if strings.Contains(content, "the-secret-of-the-application") {
		t.Fatal("the ledger holds the variables of the application in plain text")
	}
	if strings.Contains(content, "image: nginx") {
		t.Fatal("the ledger holds the document of the application in plain text")
	}
}

// TestTheLedgerFileIsNotReadableByOthers proves that the ledger of the node
// is not readable by the other users of the host.
func TestTheLedgerFileIsNotReadableByOthers(t *testing.T) {
	path := ledgerPath(t)
	openLedger(t, path)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("inspect the ledger: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("the permissions of the ledger = %o, want 600", info.Mode().Perm())
	}
}

// TestTheLedgerHoldsAnApplicationInTheDatabaseItself proves that the
// database itself refuses a second open task of an application, so the
// exclusion does not depend on the order of the statements of the agent.
func TestTheLedgerHoldsAnApplicationInTheDatabaseItself(t *testing.T) {
	path := ledgerPath(t)
	ledger := openLedger(t, path)
	ctx := context.Background()

	if _, fresh, err := ledger.Accept(ctx, commandOf("task-1", "app-1", "digest-1")); err != nil || !fresh {
		t.Fatalf("accept: %v", err)
	}

	db, err := sql.Open("sqlite", filepath.ToSlash(path))
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}
	defer db.Close()

	_, err = db.Exec(`INSERT INTO agent_tasks (
			id, app_id, version_id, digest, state, seq, execution_started,
			result_code, result_message, result_rolled_back, result_partial,
			output_bytes, created_at, updated_at)
		VALUES ('task-2', 'app-1', 'v1', 'digest-2', 'queued', 1, 0, '', '', 0, 0, 0, 0, 0)`)
	if err == nil {
		t.Fatal("the ledger accepted a second open task of the same application")
	}
}

// TestTheLedgerDoesNotKnowAnUnknownTask proves that a query about a task
// that the ledger never received is reported.
func TestTheLedgerDoesNotKnowAnUnknownTask(t *testing.T) {
	ledger := openLedger(t, ledgerPath(t))
	ctx := context.Background()

	if _, err := ledger.Lookup(ctx, "task-1"); !errors.Is(err, agent.ErrTaskNotFound) {
		t.Fatalf("the ledger reported %v, want an unknown task", err)
	}
	if _, err := ledger.Record(ctx, "task-1", model.Result{State: model.Succeeded}); !errors.Is(err, agent.ErrTaskNotFound) {
		t.Fatalf("the ledger reported %v, want an unknown task", err)
	}
	if _, err := ledger.Begin(ctx, "task-1"); !errors.Is(err, agent.ErrTaskNotFound) {
		t.Fatalf("the ledger reported %v, want an unknown task", err)
	}
}

// TestTheLedgerRefusesACommandWithoutATask proves that a command without a
// task is refused, because the ledger cannot keep a task it cannot name.
func TestTheLedgerRefusesACommandWithoutATask(t *testing.T) {
	ledger := openLedger(t, ledgerPath(t))
	ctx := context.Background()

	_, fresh, err := ledger.Accept(ctx, commandOf("", "app-1", "digest-1"))
	if err == nil {
		t.Fatal("the ledger accepted a command that names no task")
	}
	if !errors.Is(err, agent.ErrIncompleteCommand) {
		t.Fatalf("the ledger reported %v, want an incomplete command", err)
	}
	if fresh {
		t.Fatal("the ledger reported a new reception of a command it refused")
	}
	if _, err := ledger.Lookup(ctx, ""); err == nil {
		t.Fatal("the ledger answered a query about a task that has no name")
	}
}

// TestTheLedgerRefusesACommandWithoutAnApplication proves that a command
// without an application is refused, because an application that holds no
// name cannot be held exclusively.
func TestTheLedgerRefusesACommandWithoutAnApplication(t *testing.T) {
	ledger := openLedger(t, ledgerPath(t))
	ctx := context.Background()

	_, fresh, err := ledger.Accept(ctx, commandOf("task-1", "", "digest-1"))
	if err == nil {
		t.Fatal("the ledger accepted a command that names no application")
	}
	if !errors.Is(err, agent.ErrIncompleteCommand) {
		t.Fatalf("the ledger reported %v, want an incomplete command", err)
	}
	if fresh {
		t.Fatal("the ledger reported a new reception of a command it refused")
	}
}

// TestTheLedgerRefusesACommandWithoutADigest proves that a command without a
// digest is refused, because the digest is what proves the configuration of
// a repeated command.
func TestTheLedgerRefusesACommandWithoutADigest(t *testing.T) {
	ledger := openLedger(t, ledgerPath(t))
	ctx := context.Background()

	_, fresh, err := ledger.Accept(ctx, commandOf("task-1", "app-1", ""))
	if err == nil {
		t.Fatal("the ledger accepted a command that names no configuration")
	}
	if !errors.Is(err, agent.ErrIncompleteCommand) {
		t.Fatalf("the ledger reported %v, want an incomplete command", err)
	}
	if fresh {
		t.Fatal("the ledger reported a new reception of a command it refused")
	}
}

// TestTheLedgerRefusesTheSameTaskOfAnotherApplication proves that a task ID
// that arrives again for another application is refused, even when the
// configuration of it is the same.
func TestTheLedgerRefusesTheSameTaskOfAnotherApplication(t *testing.T) {
	ledger := openLedger(t, ledgerPath(t))
	ctx := context.Background()

	if _, fresh, err := ledger.Accept(ctx, commandOf("task-1", "app-1", "digest-1")); err != nil || !fresh {
		t.Fatalf("accept: %v", err)
	}

	_, fresh, err := ledger.Accept(ctx, commandOf("task-1", "app-2", "digest-1"))
	if err == nil {
		t.Fatal("the ledger accepted the same task for another application")
	}
	if !errors.Is(err, agent.ErrAppConflict) {
		t.Fatalf("the ledger reported %v, want a conflict of the application", err)
	}
	if fresh {
		t.Fatal("the ledger reported a new reception of a task it refused")
	}

	// The ledger keeps the application of the first reception.
	looked, err := ledger.Lookup(ctx, "task-1")
	if err != nil {
		t.Fatalf("look the task up: %v", err)
	}
	if looked.Digest != "digest-1" {
		t.Fatalf("the digest of the task = %q, want the one of the first reception", looked.Digest)
	}
}

// TestTheLedgerRefusesTheSameTaskOfAnotherVersion proves that a task ID
// that arrives again for another version is refused.
func TestTheLedgerRefusesTheSameTaskOfAnotherVersion(t *testing.T) {
	ledger := openLedger(t, ledgerPath(t))
	ctx := context.Background()

	if _, fresh, err := ledger.Accept(ctx, commandOf("task-1", "app-1", "digest-1")); err != nil || !fresh {
		t.Fatalf("accept: %v", err)
	}

	other := commandOf("task-1", "app-1", "digest-1")
	other.Task.VersionID = "v2"
	_, fresh, err := ledger.Accept(ctx, other)
	if err == nil {
		t.Fatal("the ledger accepted the same task for another version")
	}
	if !errors.Is(err, agent.ErrVersionConflict) {
		t.Fatalf("the ledger reported %v, want a conflict of the version", err)
	}
	if fresh {
		t.Fatal("the ledger reported a new reception of a task it refused")
	}
}

// TestTheLedgerAcceptsTheSameTaskAgain proves that a command that arrives
// again with the same task, application, version and digest is answered with
// the receipt of the first reception.
func TestTheLedgerAcceptsTheSameTaskAgain(t *testing.T) {
	ledger := openLedger(t, ledgerPath(t))
	ctx := context.Background()
	cmd := commandOf("task-1", "app-1", "digest-1")

	first, fresh, err := ledger.Accept(ctx, cmd)
	if err != nil || !fresh {
		t.Fatalf("accept: %v", err)
	}

	second, fresh, err := ledger.Accept(ctx, cmd)
	if err != nil {
		t.Fatalf("accept the command again: %v", err)
	}
	if fresh {
		t.Fatal("the ledger reported a new reception of a task it knows")
	}
	if second.Seq != first.Seq || second.TaskID != first.TaskID || second.Digest != first.Digest {
		t.Fatalf("the repeated receipt = %+v, want %+v", second, first)
	}
}

// TestTheLedgerRefusesAnOutcomeThatIsNotFinal proves that a phase is not
// written into the ledger as the outcome of a task.
//
// A task that stays in a phase would hold its application for ever.
func TestTheLedgerRefusesAnOutcomeThatIsNotFinal(t *testing.T) {
	ledger := openLedger(t, ledgerPath(t))
	ctx := context.Background()

	if _, fresh, err := ledger.Accept(ctx, commandOf("task-1", "app-1", "digest-1")); err != nil || !fresh {
		t.Fatalf("accept: %v", err)
	}
	if _, err := ledger.Record(ctx, "task-1", model.Result{State: model.Running}); err == nil {
		t.Fatal("the ledger recorded a phase as the outcome of a task")
	}
}

// TestTheLedgerRecordsAnOutcomeThatArrivesAfterAnUncertainTask proves that a
// report that arrives after the agent lost the outcome of a task is written
// into the ledger.
//
// The truth of a deployment may not be thrown away because it arrived late.
func TestTheLedgerRecordsAnOutcomeThatArrivesAfterAnUncertainTask(t *testing.T) {
	ledger := uncertainTask(t, ledgerPath(t))
	ctx := context.Background()

	current, err := ledger.Lookup(ctx, "task-1")
	if err != nil {
		t.Fatalf("look the task up: %v", err)
	}

	receipt, err := ledger.Record(ctx, "task-1", model.Result{
		State:   model.Succeeded,
		Code:    "deployed",
		Message: "the application is ready",
	})
	if err != nil {
		t.Fatalf("record the outcome: %v", err)
	}
	if receipt.Result.State != model.Succeeded {
		t.Fatalf("the state of the receipt = %q, want %q", receipt.Result.State, model.Succeeded)
	}
	if receipt.Seq <= current.Seq {
		t.Fatalf("the sequence number of the receipt = %d, want more than %d", receipt.Seq, current.Seq)
	}

	looked, err := ledger.Lookup(ctx, "task-1")
	if err != nil {
		t.Fatalf("look the task up: %v", err)
	}
	if looked.Result.State != model.Succeeded {
		t.Fatalf("the state in the ledger = %q, want %q", looked.Result.State, model.Succeeded)
	}
}

// TestTheLedgerResolvesAnUncertainTask proves that a task whose result is
// unknown may be resolved into a final outcome.
func TestTheLedgerResolvesAnUncertainTask(t *testing.T) {
	ledger := uncertainTask(t, ledgerPath(t))
	ctx := context.Background()

	current, err := ledger.Lookup(ctx, "task-1")
	if err != nil {
		t.Fatalf("look the task up: %v", err)
	}
	if current.Result.State != model.Uncertain {
		t.Fatalf("the state of the task = %q, want %q", current.Result.State, model.Uncertain)
	}

	receipt, err := ledger.Resolve(ctx, "task-1", current.Seq, model.Result{
		State:   model.Failed,
		Message: "管理员确认执行端已经停止",
	})
	if err != nil {
		t.Fatalf("resolve the task: %v", err)
	}
	if receipt.Result.State != model.Failed {
		t.Fatalf("the state of the receipt = %q, want %q", receipt.Result.State, model.Failed)
	}
	if receipt.Seq <= current.Seq {
		t.Fatalf("the sequence number of the receipt = %d, want more than %d", receipt.Seq, current.Seq)
	}
}

// TestTheLedgerRefusesAResolutionWithAnotherSequence proves that a request
// that carries another sequence number than the task holds is refused, so a
// stale request cannot cover a newer state of the task.
func TestTheLedgerRefusesAResolutionWithAnotherSequence(t *testing.T) {
	ledger := uncertainTask(t, ledgerPath(t))
	ctx := context.Background()

	current, err := ledger.Lookup(ctx, "task-1")
	if err != nil {
		t.Fatalf("look the task up: %v", err)
	}

	_, err = ledger.Resolve(ctx, "task-1", current.Seq+1, model.Result{State: model.Failed})
	if err == nil {
		t.Fatal("the ledger resolved a task with another sequence number")
	}
	if !errors.Is(err, agent.ErrSeqConflict) {
		t.Fatalf("the ledger reported %v, want a conflict of the sequence number", err)
	}

	looked, err := ledger.Lookup(ctx, "task-1")
	if err != nil {
		t.Fatalf("look the task up: %v", err)
	}
	if looked.Result.State != model.Uncertain {
		t.Fatalf("the state of the task = %q, want %q", looked.Result.State, model.Uncertain)
	}
	if looked.Seq != current.Seq {
		t.Fatalf("the sequence number of the task = %d, want %d", looked.Seq, current.Seq)
	}
}

// TestTheLedgerFreesTheApplicationAfterAResolution proves that a resolved
// task holds its application no more, so the application may be deployed
// again.
func TestTheLedgerFreesTheApplicationAfterAResolution(t *testing.T) {
	ledger := uncertainTask(t, ledgerPath(t))
	ctx := context.Background()

	current, err := ledger.Lookup(ctx, "task-1")
	if err != nil {
		t.Fatalf("look the task up: %v", err)
	}
	if _, err := ledger.Resolve(ctx, "task-1", current.Seq, model.Result{
		State:   model.Failed,
		Message: "管理员确认执行端已经停止",
	}); err != nil {
		t.Fatalf("resolve the task: %v", err)
	}

	if _, fresh, err := ledger.Accept(ctx, commandOf("task-2", "app-1", "digest-2")); err != nil || !fresh {
		t.Fatalf("accept the task of the freed application: %v", err)
	}
}

// TestTheLedgerKeepsTheTraceOfAResolution proves that the ledger shows the
// final outcome of a resolved task and that it names the source of it.
func TestTheLedgerKeepsTheTraceOfAResolution(t *testing.T) {
	ledger := uncertainTask(t, ledgerPath(t))
	ctx := context.Background()

	current, err := ledger.Lookup(ctx, "task-1")
	if err != nil {
		t.Fatalf("look the task up: %v", err)
	}
	receipt, err := ledger.Resolve(ctx, "task-1", current.Seq, model.Result{
		State:   model.Failed,
		Message: "管理员确认执行端已经停止",
	})
	if err != nil {
		t.Fatalf("resolve the task: %v", err)
	}
	if receipt.Result.Code != agent.CodeManualResolution {
		t.Fatalf("the code of the receipt = %q, want %q", receipt.Result.Code, agent.CodeManualResolution)
	}
	if !strings.Contains(receipt.Result.Message, "管理员确认") {
		t.Fatalf("the message of the receipt = %q, want the reason of the resolution", receipt.Result.Message)
	}

	looked, err := ledger.Lookup(ctx, "task-1")
	if err != nil {
		t.Fatalf("look the task up: %v", err)
	}
	if looked.Result.State != model.Failed {
		t.Fatalf("the state in the ledger = %q, want %q", looked.Result.State, model.Failed)
	}
	if looked.Result.Code != agent.CodeManualResolution {
		t.Fatalf("the code in the ledger = %q, want %q", looked.Result.Code, agent.CodeManualResolution)
	}
	if !strings.Contains(looked.Result.Message, "管理员确认") {
		t.Fatalf("the message in the ledger = %q, want the reason of the resolution", looked.Result.Message)
	}
}
