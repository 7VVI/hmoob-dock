package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modernc.org/sqlite"

	"hmoob-dock/internal/model"
	"hmoob-dock/internal/store"
)

// The limits of the ledger of the agent.
const (
	// ledgerBusyTimeoutMs bounds how long a statement waits for a lock
	// held by another connection before it fails.
	ledgerBusyTimeoutMs = 5000
	// MaxTaskOutputBytes is the largest output of a task that the ledger
	// keeps.
	//
	// The output beyond the limit is dropped and the task is marked as
	// truncated, so the control端 learns that the output is incomplete
	// instead of believing that the command was quiet.
	MaxTaskOutputBytes = 2 * 1024 * 1024
)

// The errors of the ledger of the agent.
var (
	// ErrDigestConflict reports a task ID that arrives again with another
	// configuration. The task is known, so the command is refused instead
	// of being executed.
	ErrDigestConflict = errors.New("agent: the task was received with another configuration")
	// ErrAppBusy reports an application that holds a task that did not
	// finish. A task whose result is unknown holds its application as
	// well: nobody may prove that the target is untouched.
	ErrAppBusy = errors.New("agent: the application holds a task that did not finish")
	// ErrTaskNotFound reports a task that the ledger does not know.
	ErrTaskNotFound = errors.New("agent: the task is not in the ledger")
)

// The codes of the outcomes that the ledger itself produces.
const (
	// CodeAccepted marks a task that was received and waits for its
	// execution.
	CodeAccepted = "accepted"
	// CodeRunning marks a task whose execution began.
	CodeRunning = "running"
	// CodeAgentRestarted marks a task whose execution was interrupted by
	// the stop of the agent.
	CodeAgentRestarted = "agent_restarted"
)

// ledgerSchema is the schema of the ledger of the agent.
//
// The table holds the identity of a task, the phase it is in, the sequence
// number of its last receipt, its outcome and the cursor of its output. The
// document and the variables of a deployment never reach the ledger: they
// belong to the application, and the agent has no reason to keep them.
const ledgerSchema = `
CREATE TABLE IF NOT EXISTS agent_tasks (
    id                 TEXT PRIMARY KEY,
    app_id             TEXT NOT NULL,
    version_id         TEXT NOT NULL,
    digest             TEXT NOT NULL,
    state              TEXT NOT NULL,
    seq                INTEGER NOT NULL DEFAULT 0,
    execution_started  INTEGER NOT NULL DEFAULT 0,
    result_code        TEXT NOT NULL DEFAULT '',
    result_message     TEXT NOT NULL DEFAULT '',
    result_rolled_back INTEGER NOT NULL DEFAULT 0,
    result_partial     INTEGER NOT NULL DEFAULT 0,
    output_bytes       INTEGER NOT NULL DEFAULT 0,
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS agent_tasks_of_app
ON agent_tasks(app_id);

-- An application holds at most one task that did not finish. The index is
-- partial, so the finished tasks do not block a new deployment. A task
-- whose result is uncertain blocks the application as well: nobody may
-- prove that the target is untouched.
CREATE UNIQUE INDEX IF NOT EXISTS one_open_task_per_app
ON agent_tasks(app_id)
WHERE app_id <> ''
  AND state IN ('queued','preflight','running','verifying','uncertain');
`

// openStates are the phases in which a task holds its application.
//
// A task whose result is uncertain holds it as well: the target may still
// be changing, so a new deployment of the same application waits until the
// task is checked.
var openStates = []string{
	string(model.Queued),
	string(model.Preflight),
	string(model.Running),
	string(model.Verifying),
	string(model.Uncertain),
}

// finalStates are the phases in which the outcome of a task is known.
var finalStates = []string{
	string(model.Succeeded),
	string(model.Failed),
	string(model.Uncertain),
}

// Ledger is the durable ledger of the tasks of a node.
//
// The ledger is a database of its own: it is not the database of the
// control端, so the state of a node survives the state of the control端.
// Every write of the ledger is committed before it reports success, so a
// task that was received survives a crash of the node.
//
// The ledger is not a completely stateless component of the agent: a task
// whose execution began is never executed a second time, not even after a
// restart of the agent.
type Ledger struct {
	// db is the pool of the ledger. It holds a single connection, so the
	// writes are serialized.
	db *sql.DB
	// now reads the time of the ledger. It is a field so that a test can
	// control it.
	now func() time.Time
}

// OpenLedger opens the ledger of the agent at path.
//
// The file is created with restricted permissions, because the ledger names
// the tasks of the node. The ledger is opened with FULL synchronous: a
// write that reports success has reached the disk.
func OpenLedger(path string) (*Ledger, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("agent: the ledger has no path")
	}

	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("agent: create the directory %s of the ledger: %w", directory, err)
	}
	// The file is created before the driver opens it, so it never exists
	// with the permissions of the directory.
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("agent: create the ledger %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("agent: close the ledger %s: %w", path, err)
	}
	// A ledger that was left with wider permissions is restricted again.
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("agent: restrict the ledger %s: %w", path, err)
	}

	db, err := sql.Open("sqlite", ledgerDSN(path))
	if err != nil {
		return nil, fmt.Errorf("agent: open the ledger %s: %w", path, err)
	}
	// A single connection serializes the writes of the ledger.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("agent: connect to the ledger %s: %w", path, err)
	}
	if _, err := db.Exec(ledgerSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("agent: prepare the schema of the ledger %s: %w", path, err)
	}

	ledger := &Ledger{db: db, now: time.Now}
	if err := ledger.recover(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return ledger, nil
}

// Close releases the ledger of the agent.
func (l *Ledger) Close() error {
	if err := l.db.Close(); err != nil {
		return fmt.Errorf("agent: close the ledger: %w", err)
	}
	return nil
}

// Accept records the reception of a command.
//
// The command is written into the ledger before anything runs, so a command
// that is delivered twice is known the second time. The second return value
// reports whether the command was received for the first time: it says
// nothing about the right to execute the command, which only the intent of
// Begin grants.
//
// A task ID that arrives again with another configuration is refused, and so
// is a command of an application that holds a task that did not finish.
func (l *Ledger) Accept(ctx context.Context, cmd model.Command) (model.Receipt, bool, error) {
	taskID := strings.TrimSpace(cmd.Task.ID)
	if taskID == "" {
		return model.Receipt{}, false, errors.New("agent: the command names no task")
	}
	digest := strings.TrimSpace(cmd.Task.Digest)
	appID := strings.TrimSpace(cmd.Task.AppID)

	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Receipt{}, false, fmt.Errorf("agent: start the transaction of the task %s: %w", taskID, err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, found, err := readTask(ctx, tx, taskID)
	if err != nil {
		return model.Receipt{}, false, err
	}
	if found {
		if existing.digest != digest {
			return model.Receipt{}, false, fmt.Errorf("%w: the task %s holds the digest %s",
				ErrDigestConflict, taskID, existing.digest)
		}
		if err := tx.Commit(); err != nil {
			return model.Receipt{}, false, fmt.Errorf("agent: commit the transaction of the task %s: %w", taskID, err)
		}
		return existing.receipt(), false, nil
	}

	// The application holds at most one task that did not finish.
	blocking, err := openTaskOfApp(ctx, tx, appID)
	if err != nil {
		return model.Receipt{}, false, err
	}
	if blocking != "" {
		return model.Receipt{}, false, fmt.Errorf("%w: the application %s holds the task %s",
			ErrAppBusy, appID, blocking)
	}

	now := store.TimeToMillis(l.now())
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO agent_tasks (
			id, app_id, version_id, digest, state, seq, execution_started,
			result_code, result_message, result_rolled_back, result_partial,
			output_bytes, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 1, 0, ?, '', 0, 0, 0, ?, ?)`,
		taskID, appID, strings.TrimSpace(cmd.Task.VersionID), digest,
		string(model.Queued), CodeAccepted, now, now); err != nil {
		// The index of the open tasks refuses the command, when another
		// task of the application reached the ledger in the meantime.
		if isUniqueViolation(err) {
			return model.Receipt{}, false, fmt.Errorf("%w: the application %s holds a task that did not finish",
				ErrAppBusy, appID)
		}
		return model.Receipt{}, false, fmt.Errorf("agent: record the task %s: %w", taskID, err)
	}
	if err := tx.Commit(); err != nil {
		return model.Receipt{}, false, fmt.Errorf("agent: commit the transaction of the task %s: %w", taskID, err)
	}

	return model.Receipt{
		TaskID: taskID,
		Digest: digest,
		Seq:    1,
		Result: model.Result{State: model.Queued, Code: CodeAccepted},
	}, true, nil
}

// Begin marks the intent to execute a task.
//
// The intent is written with a compare and swap, so only the first caller
// that finds the task without an intent gets the right to execute it. Every
// other caller — a repeated command, a second connection, an agent that
// restarted — sees the intent of the first one and leaves the task alone.
//
// The transaction is committed before the function returns, so the Docker
// command of the task runs only after the intent reached the disk.
func (l *Ledger) Begin(ctx context.Context, taskID string) (bool, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return false, errors.New("agent: the task has no name")
	}

	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("agent: start the transaction of the task %s: %w", taskID, err)
	}
	defer func() { _ = tx.Rollback() }()

	_, found, err := readTask(ctx, tx, taskID)
	if err != nil {
		return false, err
	}
	if !found {
		return false, fmt.Errorf("%w: %s", ErrTaskNotFound, taskID)
	}

	result, err := tx.ExecContext(ctx, `
		UPDATE agent_tasks
		   SET execution_started = 1, state = ?, seq = seq + 1,
		       result_code = ?, result_message = '', updated_at = ?
		 WHERE id = ? AND execution_started = 0`,
		string(model.Running), CodeRunning, store.TimeToMillis(l.now()), taskID)
	if err != nil {
		return false, fmt.Errorf("agent: mark the intent of the task %s: %w", taskID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("agent: mark the intent of the task %s: %w", taskID, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("agent: commit the transaction of the task %s: %w", taskID, err)
	}
	return affected == 1, nil
}

// Record writes the outcome of a task.
//
// The outcome of a task that already finished is not written a second time:
// the first outcome and its sequence number stay, so a repeated report of
// the control端 does not look like a new one.
func (l *Ledger) Record(ctx context.Context, taskID string, outcome model.Result) (model.Receipt, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return model.Receipt{}, errors.New("agent: the task has no name")
	}

	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Receipt{}, fmt.Errorf("agent: start the transaction of the task %s: %w", taskID, err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, found, err := readTask(ctx, tx, taskID)
	if err != nil {
		return model.Receipt{}, err
	}
	if !found {
		return model.Receipt{}, fmt.Errorf("%w: %s", ErrTaskNotFound, taskID)
	}
	if finished(existing.state) {
		if err := tx.Commit(); err != nil {
			return model.Receipt{}, fmt.Errorf("agent: commit the transaction of the task %s: %w", taskID, err)
		}
		return existing.receipt(), nil
	}

	seq := existing.seq + 1
	if _, err := tx.ExecContext(ctx, `
		UPDATE agent_tasks
		   SET state = ?, seq = ?, result_code = ?, result_message = ?,
		       result_rolled_back = ?, result_partial = ?, updated_at = ?
		 WHERE id = ?`,
		string(outcome.State), seq, outcome.Code, outcome.Message,
		boolToInt(outcome.RolledBack), boolToInt(outcome.PartialUpdate),
		store.TimeToMillis(l.now()), taskID); err != nil {
		return model.Receipt{}, fmt.Errorf("agent: record the outcome of the task %s: %w", taskID, err)
	}
	if err := tx.Commit(); err != nil {
		return model.Receipt{}, fmt.Errorf("agent: commit the transaction of the task %s: %w", taskID, err)
	}

	return model.Receipt{
		TaskID: taskID,
		Digest: existing.digest,
		Seq:    seq,
		Result: outcome,
	}, nil
}

// Lookup reads the last receipt of a task.
func (l *Ledger) Lookup(ctx context.Context, taskID string) (model.Receipt, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return model.Receipt{}, errors.New("agent: the task has no name")
	}

	existing, found, err := readTask(ctx, l.db, taskID)
	if err != nil {
		return model.Receipt{}, err
	}
	if !found {
		return model.Receipt{}, fmt.Errorf("%w: %s", ErrTaskNotFound, taskID)
	}
	return existing.receipt(), nil
}

// AdvanceOutput moves the cursor of the output of a task forward and
// returns the new cursor.
//
// The cursor is the number of bytes the task produced. The output of a task
// is limited, and the cursor is durable, so the output of a task never
// grows beyond its limit.
func (l *Ledger) AdvanceOutput(ctx context.Context, taskID string, size int64) (int64, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return 0, errors.New("agent: the task has no name")
	}
	if size < 0 {
		return 0, fmt.Errorf("agent: the output of the task %s grew by %d bytes", taskID, size)
	}

	if _, err := l.db.ExecContext(ctx, `
		UPDATE agent_tasks
		   SET output_bytes = MIN(output_bytes + ?, ?), updated_at = ?
		 WHERE id = ?`,
		size, int64(MaxTaskOutputBytes), store.TimeToMillis(l.now()), taskID); err != nil {
		return 0, fmt.Errorf("agent: record the output of the task %s: %w", taskID, err)
	}

	var cursor int64
	if err := l.db.QueryRowContext(ctx,
		`SELECT output_bytes FROM agent_tasks WHERE id = ?`, taskID).Scan(&cursor); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("%w: %s", ErrTaskNotFound, taskID)
		}
		return 0, fmt.Errorf("agent: read the output of the task %s: %w", taskID, err)
	}
	return cursor, nil
}

// recover marks the tasks whose execution was interrupted.
//
// A task whose intent was written but whose outcome was not recorded was
// running when the agent stopped. The Docker daemon may still work on the
// change, so the task waits for a check instead of running a second time.
func (l *Ledger) recover(ctx context.Context) error {
	now := store.TimeToMillis(l.now())
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(finalStates)), ",")
	arguments := make([]any, 0, len(finalStates)+1)
	arguments = append(arguments, string(model.Uncertain), CodeAgentRestarted,
		"代理在任务运行期间停止，任务结果待核对", now)
	for _, state := range finalStates {
		arguments = append(arguments, state)
	}

	if _, err := l.db.ExecContext(ctx, `
		UPDATE agent_tasks
		   SET state = ?, seq = seq + 1, result_code = ?, result_message = ?, updated_at = ?
		 WHERE execution_started = 1 AND state NOT IN (`+placeholders+`)`, arguments...); err != nil {
		return fmt.Errorf("agent: mark the interrupted tasks: %w", err)
	}
	return nil
}

// taskRow is a task of the ledger.
type taskRow struct {
	// id is the identifier of the task.
	id string
	// appID is the application the task deploys.
	appID string
	// versionID is the version the task deploys.
	versionID string
	// digest identifies the configuration the task deploys.
	digest string
	// state is the phase the task is in.
	state model.TaskState
	// seq is the sequence number of the last receipt of the task.
	seq uint64
	// executionStarted reports whether the execution of the task began.
	executionStarted bool
	// code names the outcome of the task.
	code string
	// message describes the outcome of the task.
	message string
	// rolledBack reports whether the target rolled the change back.
	rolledBack bool
	// partialUpdate reports whether only part of the application changed.
	partialUpdate bool
	// outputBytes is the cursor of the output of the task.
	outputBytes int64
}

// receipt turns the row into the receipt of the task.
func (r taskRow) receipt() model.Receipt {
	return model.Receipt{
		TaskID: r.id,
		Digest: r.digest,
		Seq:    r.seq,
		Result: model.Result{
			State:         r.state,
			Code:          r.code,
			Message:       r.message,
			RolledBack:    r.rolledBack,
			PartialUpdate: r.partialUpdate,
		},
	}
}

// queryer reads rows of the ledger. Both the pool and a transaction of the
// ledger satisfy it.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// readTask reads the row of a task.
func readTask(ctx context.Context, source queryer, taskID string) (taskRow, bool, error) {
	var row taskRow
	var seq int64
	var executionStarted int64
	var rolledBack int64
	var partial int64

	err := source.QueryRowContext(ctx, `
		SELECT id, app_id, version_id, digest, state, seq, execution_started,
		       result_code, result_message, result_rolled_back, result_partial, output_bytes
		  FROM agent_tasks WHERE id = ?`, taskID).
		Scan(&row.id, &row.appID, &row.versionID, &row.digest, &row.state, &seq,
			&executionStarted, &row.code, &row.message, &rolledBack, &partial, &row.outputBytes)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return taskRow{}, false, nil
	case err != nil:
		return taskRow{}, false, fmt.Errorf("agent: read the task %s: %w", taskID, err)
	}

	row.seq = uint64(seq)
	row.executionStarted = executionStarted != 0
	row.rolledBack = rolledBack != 0
	row.partialUpdate = partial != 0
	return row, true, nil
}

// openTaskOfApp returns the task that holds the application.
func openTaskOfApp(ctx context.Context, tx *sql.Tx, appID string) (string, error) {
	if appID == "" {
		return "", nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(openStates)), ",")
	arguments := make([]any, 0, len(openStates)+1)
	arguments = append(arguments, appID)
	for _, state := range openStates {
		arguments = append(arguments, state)
	}

	var taskID string
	err := tx.QueryRowContext(ctx, `
		SELECT id FROM agent_tasks
		 WHERE app_id = ? AND state IN (`+placeholders+`)
		 ORDER BY id LIMIT 1`, arguments...).Scan(&taskID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("agent: read the tasks of the application %s: %w", appID, err)
	}
	return taskID, nil
}

// finished reports whether the state of a task is a final one.
func finished(state model.TaskState) bool {
	for _, final := range finalStates {
		if string(state) == final {
			return true
		}
	}
	return false
}

// boolToInt turns a flag into the integer of the schema.
func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// sqliteConstraintUnique is the result code of SQLite for a violation of a
// unique constraint.
const sqliteConstraintUnique = 2067

// isUniqueViolation reports whether the error is a violation of a unique
// constraint.
//
// The driver reports the result code of SQLite, so the busy application is
// named instead of a bare constraint error.
func isUniqueViolation(err error) bool {
	var failure *sqlite.Error
	if !errors.As(err, &failure) {
		return false
	}
	return failure.Code() == sqliteConstraintUnique
}

// ledgerDSN builds the connection string of the ledger.
//
// The ledger is written by the agent alone, so a single connection
// serializes the writes. FULL synchronous makes a write durable before it
// reports success: a task that was received has reached the disk.
func ledgerDSN(path string) string {
	params := []string{
		fmt.Sprintf("_pragma=busy_timeout(%d)", ledgerBusyTimeoutMs),
		"_pragma=foreign_keys(1)",
		"_pragma=journal_mode(WAL)",
		"_pragma=synchronous(FULL)",
	}
	return filepath.ToSlash(path) + "?" + strings.Join(params, "&")
}
