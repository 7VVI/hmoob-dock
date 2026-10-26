package deploy

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"hmoob-dock/internal/model"
	"hmoob-dock/internal/store"
)

// The limits of the reconciliation.
const (
	// queryTimeout bounds the wait for the answer of a node.
	//
	// The answer of a node is a read of its ledger, so it arrives at once.
	// The limit is generous, because a node that is busy may answer a
	// little later.
	queryTimeout = 5 * time.Second
)

// The actions of the audit events of the deployments.
const (
	// AuditManualResolution marks the request of an administrator to end a
	// task by hand.
	AuditManualResolution = "manual_resolution"
)

// The results of the audit events of the deployments.
const (
	// AuditResultRequested marks a request that waits for its confirmation.
	AuditResultRequested = "requested"
	// AuditResultConfirmed marks a request the node confirmed.
	AuditResultConfirmed = "confirmed"
)

// auditMaxEvents is the number of audit events the control端 keeps.
//
// The table of the audit events has no expiry, so it has to be bounded: a
// request that arrives again may not fill the database of the control端.
const auditMaxEvents = 10000

// Reconcile compares the state of a task with the ledger of the node that
// executes it.
//
// The ledger of the node is the truth about what happened on the target: the
// control端 asks the node about the task and applies its answer. A task that
// was never sent is dispatched, because nothing ran on the target yet.
//
// The two sides may disagree: the node knows an outcome the control端 does
// not know, or the control端 released a task the node still holds. Such a
// disagreement is reported instead of being hidden, and the answer of the
// node wins, because the node is the only side that observed the target.
func (d *Dispatcher) Reconcile(ctx context.Context, taskID string) (model.Task, error) {
	record, err := d.service.task(ctx, taskID)
	if err != nil {
		return model.Task{}, err
	}
	task := record.task
	if task.AgentID == "" {
		// The command was never sent, so nothing ran on the target and the
		// task may be sent.
		if err := d.Dispatch(ctx, taskID); err != nil {
			return model.Task{}, err
		}
		return d.service.Task(ctx, taskID)
	}

	local, err := d.service.Deployment(ctx, taskID)
	if err != nil {
		return model.Task{}, err
	}
	answer, err := d.askNode(ctx, task)
	if err != nil {
		return task, err
	}
	if !answer.Found {
		// The ledger of the node does not know the task.
		confirmed, updated, err := d.confirmManualResolutionWithoutTask(ctx, task)
		if err != nil {
			return task, err
		}
		if confirmed {
			// The node has no executor for the task, so the release of the
			// administrator is complete and the application is free again.
			return updated, nil
		}
		if task.LastSeq == 0 && task.State == model.Queued {
			// The command never reached the node. It is sent again to the
			// node that holds the task: the ledger of the node knows a
			// command at most once, so the deployment runs at most once.
			if err := d.send(ctx, taskID); err != nil {
				return task, err
			}
			log.Printf("control: the command of the task %s was sent again to the node %s",
				taskID, task.AgentID)
			return d.service.Task(ctx, taskID)
		}
		// Nobody may prove that the target is untouched, so the task holds
		// its application and the administrator has to release it by hand.
		return task, fmt.Errorf("%w: the node %s does not know the task %s",
			ErrDivergence, task.AgentID, task.ID)
	}

	disagreement := divergence(local, answer.Receipt)
	if err := d.ApplyReceipt(ctx, task.AgentID, answer.Receipt); err != nil {
		return task, fmt.Errorf("%w: the node %s reports %q: %v",
			ErrDivergence, task.AgentID, answer.Receipt.Result.State, err)
	}
	updated, err := d.service.Task(ctx, taskID)
	if err != nil {
		return updated, err
	}
	if disagreement {
		return updated, fmt.Errorf("%w: the node %s reports %q, the control端 holds %q",
			ErrDivergence, task.AgentID, answer.Receipt.Result.State, local.State)
	}
	return updated, nil
}

// divergence reports whether the control端 and a node disagree about the
// outcome of a task.
//
// The two sides disagree when both of them know an outcome and the outcomes
// differ, when the control端 knows an outcome the node does not know any
// more, or when the administrator released a task the node still holds. A
// node that knows an outcome the control端 does not know yet is no
// disagreement: the report is simply late, and it is applied.
func divergence(local Deployment, receipt model.Receipt) bool {
	switch {
	case local.ManualResolution != nil && local.ManualResolution.Pending:
		// The administrator released the task, the node did not.
		return receipt.Result.Code != CodeManualResolution
	case finalState(local.State) && finalState(receipt.Result.State):
		if local.State != receipt.Result.State {
			return true
		}
		return local.ResultCode != "" && receipt.Result.Code != "" && local.ResultCode != receipt.Result.Code
	case finalState(local.State):
		// The control端 knows an outcome the node does not know any more.
		return true
	default:
		return false
	}
}

// Resolve ends a task whose outcome could not be proven.
//
// The administrator has to confirm that the executor of the task stopped:
// nobody may prove that a Docker command of a lost node stopped, so the
// confirmation is the decision of the administrator and not a fact of the
// platform. The sequence number the task holds makes the request a compare
// and swap, and the reason is written into the task and into the audit log.
//
// The resolution is only complete when the node confirms it: the node
// answers with a report whose sequence number grew and whose code names the
// manual resolution. Until then the task holds its application.
func (d *Dispatcher) Resolve(ctx context.Context, taskID string, expectedSeq uint64, executorStopped bool, reason string) error {
	trimmed := strings.TrimSpace(reason)
	if !executorStopped {
		return fmt.Errorf("%w: the administrator has to confirm that the executor of the task stopped",
			ErrInvalidRequest)
	}
	if trimmed == "" {
		return fmt.Errorf("%w: the resolution has no reason", ErrInvalidRequest)
	}

	record, err := d.service.task(ctx, taskID)
	if err != nil {
		return err
	}
	task := record.task
	if finalState(task.State) {
		return fmt.Errorf("%w: the task %s already ended in %q", ErrConflict, taskID, task.State)
	}
	if task.AgentID == "" {
		return fmt.Errorf("%w: the task %s was never sent to a node", ErrConflict, taskID)
	}
	if expectedSeq != task.LastSeq {
		return fmt.Errorf("%w: the task %s holds the sequence number %d, the request holds %d",
			ErrConflict, taskID, task.LastSeq, expectedSeq)
	}

	// The request is written into the task before it is sent, so a node
	// that cannot be reached does not lose the request: it is sent again
	// when the node comes back.
	if err := d.service.markManualResolution(ctx, taskID, expectedSeq, trimmed); err != nil {
		return err
	}
	if err := d.service.recordAudit(ctx, AuditManualResolution, taskID, AuditResultRequested, trimmed); err != nil {
		return err
	}
	if err := d.sendResolution(ctx, task, expectedSeq, trimmed); err != nil {
		return fmt.Errorf("%w: %v", ErrResolvePending, err)
	}
	return nil
}

// confirmManualResolutionWithoutTask ends a task that an administrator
// released by hand and that the node does not know.
//
// The node answered that its ledger does not hold the task, so the node has
// no executor for it: the release of the administrator is complete and the
// application is free again. The node is the only side that watched the
// target, so its answer ends the waiting.
//
// The first return value reports whether a release was confirmed. The second
// one is the state of the task afterwards.
func (d *Dispatcher) confirmManualResolutionWithoutTask(ctx context.Context, task model.Task) (bool, model.Task, error) {
	deployment, err := d.service.Deployment(ctx, task.ID)
	if err != nil {
		return false, task, err
	}
	if deployment.ManualResolution == nil || !deployment.ManualResolution.Pending {
		// Nobody asked for a release, so the answer of the node changes
		// nothing.
		return false, task, nil
	}
	if !CanTransition(task.State, model.Failed) {
		// The task ended already, so its outcome stays.
		return false, task, nil
	}
	// The outcome is the decision of the administrator and not a report of
	// the node, so the reason of the administrator is the message of the
	// outcome.
	receipt := model.Receipt{
		TaskID: task.ID,
		Digest: task.Digest,
		Seq:    task.LastSeq + 1,
		Result: model.Result{
			State:   model.Failed,
			Code:    CodeManualResolution,
			Message: deployment.ManualResolution.Reason,
		},
	}
	if err := d.writeReceipt(ctx, task, receipt); err != nil {
		return false, task, err
	}
	updated, err := d.service.Task(ctx, task.ID)
	if err != nil {
		return false, task, err
	}
	return updated.State == model.Failed, updated, nil
}

// Recover reconciles the deployments that did not finish.
//
// The control端 asks the node of every task about its ledger, so a restart
// of the control端 does not run a deployment a second time. A task that was
// never sent is dispatched, because nothing ran on the target. A task whose
// node does not answer becomes uncertain: nobody may prove that the target
// is untouched, so the task holds its application until it is checked.
func (d *Dispatcher) Recover(ctx context.Context) error {
	tasks, err := d.service.openTasks(ctx)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		switch {
		case task.AgentID == "":
			// The command was never sent, so the task may be sent.
			if err := d.Dispatch(ctx, task.ID); err != nil {
				log.Printf("control: the task %s was not sent: %v", task.ID, err)
			}
		case !d.transport.Connected(task.AgentID):
			// The node does not answer, so the outcome of the task is
			// unknown. The task keeps its application.
			if err := d.markUncertain(ctx, task); err != nil {
				log.Printf("control: the task %s was not marked as uncertain: %v", task.ID, err)
			}
		default:
			if _, err := d.Reconcile(ctx, task.ID); err != nil {
				log.Printf("control: the task %s was not reconciled: %v", task.ID, err)
			}
		}
	}
	return nil
}

// RecoverNode reconciles the tasks of a node that connected again.
//
// A connection that ended may have swallowed the reports of the node, so the
// control端 asks the node about the tasks it holds. The tasks that were
// never sent are dispatched as well, because a node that is back may be the
// one that executes them.
func (d *Dispatcher) RecoverNode(ctx context.Context, nodeID string) error {
	tasks, err := d.service.openTasks(ctx)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		switch {
		case task.AgentID == "":
			if err := d.Dispatch(ctx, task.ID); err != nil {
				log.Printf("control: the task %s was not sent: %v", task.ID, err)
			}
		case task.AgentID != nodeID:
			// The task belongs to another node.
			continue
		default:
			if _, err := d.Reconcile(ctx, task.ID); err != nil {
				log.Printf("control: the task %s was not reconciled: %v", task.ID, err)
			}
		}
	}
	return nil
}

// openTasks returns the tasks that did not finish.
//
// A task whose result is uncertain belongs to them: nobody may prove that
// the target is untouched, so the task holds its application until it is
// checked.
func (s *Service) openTasks(ctx context.Context) ([]model.Task, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, app_id, version_id, agent_id, state, last_seq, digest
		 FROM tasks
		 WHERE state IN ('queued','preflight','running','verifying','uncertain')
		 ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("deploy: read the open tasks: %w", err)
	}
	defer rows.Close()

	var tasks []model.Task
	for rows.Next() {
		var task model.Task
		var lastSeq int64
		if err := rows.Scan(&task.ID, &task.AppID, &task.VersionID, &task.AgentID,
			&task.State, &lastSeq, &task.Digest); err != nil {
			return nil, fmt.Errorf("deploy: read the open tasks: %w", err)
		}
		task.LastSeq = uint64(lastSeq)
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("deploy: read the open tasks: %w", err)
	}
	return tasks, nil
}

// markUncertain writes the phase of a task whose outcome is unknown.
//
// The write is a compare and swap: only a task that is not finished yet is
// marked, so a report that arrived in the meantime keeps its outcome.
func (d *Dispatcher) markUncertain(ctx context.Context, task model.Task) error {
	result, err := d.service.db.ExecContext(ctx,
		`UPDATE tasks SET state = ?, updated_at = ?
		 WHERE id = ? AND state IN ('queued','preflight','running','verifying')`,
		string(model.Uncertain), store.TimeToMillis(d.now()), task.ID)
	if err != nil {
		return fmt.Errorf("deploy: mark the task %s as uncertain: %w", task.ID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("deploy: mark the task %s as uncertain: %w", task.ID, err)
	}
	if affected == 0 {
		return nil
	}
	return nil
}

// markManualResolution writes the request of the administrator into the
// task.
//
// The request is written before it is sent, so a node that cannot be
// reached does not lose it. The task keeps its application until the node
// confirms the request.
func (s *Service) markManualResolution(ctx context.Context, taskID string, expectedSeq uint64, reason string) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE tasks
		    SET manual_resolve_seq = ?, manual_resolve_reason = ?,
		        manual_resolve_pending = 1, updated_at = ?
		  WHERE id = ? AND last_seq = ?`,
		expectedSeq, reason, store.TimeToMillis(s.now()), taskID, expectedSeq)
	if err != nil {
		return fmt.Errorf("deploy: record the resolution of the task %s: %w", taskID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("deploy: record the resolution of the task %s: %w", taskID, err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: the task %s holds another sequence number", ErrConflict, taskID)
	}
	return nil
}

// markManualResolutionConfirmed writes the confirmation of the node into the
// task.
func (s *Service) markManualResolutionConfirmed(ctx context.Context, taskID string) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET manual_resolve_pending = 0, updated_at = ? WHERE id = ?`,
		store.TimeToMillis(s.now()), taskID); err != nil {
		return fmt.Errorf("deploy: record the confirmation of the task %s: %w", taskID, err)
	}
	return nil
}

// recordAudit writes an audit event of the deployments.
//
// The event holds the action, the object it belongs to and the reason of the
// administrator. It holds no configuration, no credential and no digest.
func (s *Service) recordAudit(ctx context.Context, action, targetID, result, detail string) error {
	identifier, err := randomID()
	if err != nil {
		return err
	}
	stored, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_events (id, occurred_at, action, target_id, result, detail)
		 SELECT ?, ?, ?, ?, ?, ? WHERE (SELECT count(*) FROM audit_events) < ?`,
		identifier, store.TimeToMillis(s.now()), action, targetID, result, detail, auditMaxEvents)
	if err != nil {
		return fmt.Errorf("deploy: store the audit event: %w", err)
	}
	affected, err := stored.RowsAffected()
	if err != nil {
		return fmt.Errorf("deploy: store the audit event: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("deploy: store the audit event: %w", ErrAuditFull)
	}
	return nil
}

// ErrAuditFull reports that the audit table reached its capacity.
var ErrAuditFull = errors.New("deploy: the audit table is full")
