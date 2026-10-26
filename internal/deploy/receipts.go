package deploy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"hmoob-dock/internal/model"
	"hmoob-dock/internal/store"
)

// The codes of the outcomes that the control端 writes into a task.
const (
	// CodeManualResolution marks a task that an administrator ended by
	// hand. The node writes the same code into its ledger, so the two
	// sides of a resolution can be compared.
	CodeManualResolution = "manual_resolution"
)

// The errors of the dispatcher.
var (
	// ErrDivergence reports that the control端 and a node disagree about the
	// outcome of a task.
	ErrDivergence = errors.New("deploy: the control端 and the node disagree about the outcome of the task")
	// ErrResolvePending reports that a manual resolution waits for the
	// confirmation of the node that holds the task.
	ErrResolvePending = errors.New("deploy: the resolution waits for the confirmation of the node")
)

// CanTransition reports whether a task may change from one phase into
// another.
//
// The phases of a task only move forward. A task that succeeded or failed
// keeps its outcome for ever, so a report that arrives late never rewrites
// it. A task whose outcome is unknown waits for its check: it goes back to a
// running phase or to a final one, but it never goes back to the queue.
//
// A report that names the phase the task already holds is accepted while
// that phase is not a final one: it carries a newer sequence number and no
// change of the phase. A final outcome is never rewritten, not even by a
// report of the same outcome.
func CanTransition(from, to model.TaskState) bool {
	if from == to {
		return !finalState(from)
	}
	switch from {
	case model.Queued:
		return forward(to, model.Preflight, model.Running, model.Verifying, model.Succeeded, model.Failed, model.Uncertain)
	case model.Preflight:
		return forward(to, model.Running, model.Verifying, model.Succeeded, model.Failed, model.Uncertain)
	case model.Running:
		return forward(to, model.Verifying, model.Succeeded, model.Failed, model.Uncertain)
	case model.Verifying:
		return forward(to, model.Succeeded, model.Failed, model.Uncertain)
	case model.Uncertain:
		// A task whose outcome is unknown is only released by evidence: it
		// goes back to a running phase or it ends.
		return forward(to, model.Running, model.Verifying, model.Succeeded, model.Failed)
	default:
		// A task that succeeded or failed keeps its outcome.
		return false
	}
}

// forward reports whether the target phase is one of the given phases.
func forward(to model.TaskState, phases ...model.TaskState) bool {
	for _, phase := range phases {
		if to == phase {
			return true
		}
	}
	return false
}

// ApplyReceipt applies the report of an agent about a task.
//
// The report is applied only when it belongs to the task: the node that
// reports it has to be the node the task was bound to, and the digest of the
// report has to be the digest of the task. A report of another node or of
// another configuration is refused.
//
// The sequence number of the task only grows: a report that arrives again is
// ignored and an old report never overwrites a newer one.
func (d *Dispatcher) ApplyReceipt(ctx context.Context, nodeID string, receipt model.Receipt) error {
	record, err := d.service.task(ctx, receipt.TaskID)
	if err != nil {
		return err
	}
	task := record.task
	if task.AgentID == "" {
		return fmt.Errorf("%w: the task %s was not sent to a node", ErrConflict, task.ID)
	}
	if task.AgentID != nodeID {
		return fmt.Errorf("%w: the task %s belongs to the node %s, the report comes from the node %s",
			ErrConflict, task.ID, task.AgentID, nodeID)
	}
	if task.Digest != receipt.Digest {
		return fmt.Errorf("%w: the task %s holds the digest %s", ErrConflict, task.ID, task.Digest)
	}
	if receipt.Seq <= task.LastSeq {
		// The report arrived already or it is older than the one the task
		// holds. The task keeps its state.
		return nil
	}
	if !CanTransition(task.State, receipt.Result.State) {
		return fmt.Errorf("%w: the task %s cannot change from %q to %q",
			ErrConflict, task.ID, task.State, receipt.Result.State)
	}
	return d.writeReceipt(ctx, task, receipt)
}

// writeReceipt writes a report of an agent into its task.
//
// The write is a compare and swap: the report is only written when the task
// still holds the sequence number the report was checked against, so two
// reports that arrive at the same time cannot overwrite each other.
func (d *Dispatcher) writeReceipt(ctx context.Context, task model.Task, receipt model.Receipt) error {
	now := store.TimeToMillis(d.now())
	var finished any
	if finalState(receipt.Result.State) {
		finished = now
	}
	result, err := d.service.db.ExecContext(ctx,
		`UPDATE tasks
		    SET state = ?, last_seq = ?, result_code = ?, result_message = ?,
		        rolled_back = ?, partial_update = ?, updated_at = ?, finished_at = ?
		  WHERE id = ? AND agent_id = ? AND digest = ? AND last_seq < ?`,
		string(receipt.Result.State), receipt.Seq, receipt.Result.Code, receipt.Result.Message,
		boolToInt(receipt.Result.RolledBack), boolToInt(receipt.Result.PartialUpdate),
		now, finished, task.ID, task.AgentID, task.Digest, receipt.Seq)
	if err != nil {
		return fmt.Errorf("deploy: write the report of the task %s: %w", task.ID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("deploy: write the report of the task %s: %w", task.ID, err)
	}
	if affected == 0 {
		// Another report won the race. The newer one holds the task.
		return nil
	}
	if receipt.Result.Code == CodeManualResolution {
		// The node confirmed the manual resolution of the administrator.
		if err := d.service.markManualResolutionConfirmed(ctx, task.ID); err != nil {
			return err
		}
		if err := d.service.recordAudit(ctx, AuditManualResolution, task.ID, AuditResultConfirmed, receipt.Result.Message); err != nil {
			return err
		}
	}
	if !verifiedSuccess(receipt) {
		// A deployment that failed or that was rolled back does not change
		// the version the application points at.
		return nil
	}
	if _, err := d.service.db.ExecContext(ctx,
		`UPDATE apps SET successful_version_id = ? WHERE id = ?`,
		task.VersionID, task.AppID); err != nil {
		return fmt.Errorf("deploy: point the application %s at the version %s: %w", task.AppID, task.VersionID, err)
	}
	return nil
}

// verifiedSuccess reports whether a report proves that the deployment
// succeeded.
//
// A deployment is verified when the node checked that the services of the
// application became ready. The report of a manual resolution is never a
// success: the administrator did not deploy anything.
func verifiedSuccess(receipt model.Receipt) bool {
	return receipt.Result.State == model.Succeeded && receipt.Result.Code != CodeManualResolution
}

// ManualResolution is the request of an administrator to end a task by hand.
type ManualResolution struct {
	// Seq is the sequence number the task held when the administrator
	// confirmed the release.
	Seq uint64 `json:"seq"`
	// Reason is the reason of the administrator.
	Reason string `json:"reason"`
	// Pending reports that the node did not confirm the request yet. The
	// task holds its application until the node confirms it.
	Pending bool `json:"pending"`
}

// Deployment is the state of a deployment task.
//
// The deployment holds the state of the task, the outcome the node reported
// and the request of the administrator, if there is one. It holds no
// configuration: the configuration of a version is read when the
// administrator opens it.
type Deployment struct {
	// ID identifies the task.
	ID string `json:"id"`
	// AppID is the application the task deploys.
	AppID string `json:"app_id"`
	// VersionID is the version the task deploys.
	VersionID string `json:"version_id"`
	// AgentID is the node that executes the task.
	AgentID string `json:"agent_id"`
	// State is the phase of the task.
	State model.TaskState `json:"state"`
	// LastSeq is the sequence number of the last report of the task.
	LastSeq uint64 `json:"last_seq"`
	// ResultCode names the kind of the outcome.
	ResultCode string `json:"result_code"`
	// ResultMessage describes the outcome.
	ResultMessage string `json:"result_message"`
	// RolledBack reports whether the target rolled the change back.
	RolledBack bool `json:"rolled_back"`
	// PartialUpdate reports whether only part of the application changed.
	PartialUpdate bool `json:"partial_update"`
	// DispatchStartedAt is the time the command was handed to the node. It
	// is nil while the command was never sent.
	DispatchStartedAt *time.Time `json:"dispatch_started_at"`
	// ManualResolution is the request of the administrator. It is nil
	// while nobody asked for a release.
	ManualResolution *ManualResolution `json:"manual_resolution"`
	// CreatedAt is the time the task was written.
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is the time the task changed the last time.
	UpdatedAt time.Time `json:"updated_at"`
	// FinishedAt is the time the task ended. It is nil while the task did
	// not end.
	FinishedAt *time.Time `json:"finished_at"`
}

// Deployment returns the state of a deployment task.
func (s *Service) Deployment(ctx context.Context, taskID string) (Deployment, error) {
	var deployment Deployment
	var lastSeq int64
	var dispatchStartedAt sql.NullInt64
	var finishedAt sql.NullInt64
	var createdAt, updatedAt int64
	var resultCode, resultMessage, resolveReason string
	var rolledBack, partialUpdate, resolvePending int64
	var resolveSeq int64
	var digest string

	err := s.db.QueryRowContext(ctx,
		`SELECT id, app_id, version_id, agent_id, state, last_seq, digest,
		        result_code, result_message, rolled_back, partial_update,
		        dispatch_started_at, manual_resolve_seq, manual_resolve_reason,
		        manual_resolve_pending, created_at, updated_at, finished_at
		 FROM tasks WHERE id = ?`, taskID).
		Scan(&deployment.ID, &deployment.AppID, &deployment.VersionID, &deployment.AgentID,
			&deployment.State, &lastSeq, &digest, &resultCode, &resultMessage,
			&rolledBack, &partialUpdate, &dispatchStartedAt, &resolveSeq, &resolveReason,
			&resolvePending, &createdAt, &updatedAt, &finishedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Deployment{}, fmt.Errorf("%w: %s", ErrUnknownTask, taskID)
	case err != nil:
		return Deployment{}, fmt.Errorf("deploy: read the task: %w", err)
	}

	deployment.LastSeq = uint64(lastSeq)
	deployment.ResultCode = resultCode
	deployment.ResultMessage = resultMessage
	deployment.RolledBack = rolledBack != 0
	deployment.PartialUpdate = partialUpdate != 0
	deployment.CreatedAt = store.TimeFromMillis(createdAt)
	deployment.UpdatedAt = store.TimeFromMillis(updatedAt)
	if dispatchStartedAt.Valid {
		moment := store.TimeFromMillis(dispatchStartedAt.Int64)
		deployment.DispatchStartedAt = &moment
	}
	if finishedAt.Valid {
		moment := store.TimeFromMillis(finishedAt.Int64)
		deployment.FinishedAt = &moment
	}
	if resolvePending != 0 || resolveReason != "" {
		deployment.ManualResolution = &ManualResolution{
			Seq:     uint64(resolveSeq),
			Reason:  resolveReason,
			Pending: resolvePending != 0,
		}
	}
	return deployment, nil
}
