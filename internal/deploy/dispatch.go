package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"hmoob-dock/internal/model"
	"hmoob-dock/internal/protocol"
	"hmoob-dock/internal/store"
	"hmoob-dock/internal/yamlspec"
)

// Transport hands a frame to the agent of a node.
//
// The hub of the agents implements it: the control端 sends the commands, the
// questions and the resolutions of the deployments through the connections of
// the agents. The interface keeps the package of the deployments free of the
// package of the connections, so the dispatcher can be tested without a
// network.
type Transport interface {
	// Send hands a frame to the agent of the node.
	//
	// An error does not mean that the agent did not receive the frame: the
	// frame may have reached the node before the connection broke.
	Send(ctx context.Context, nodeID string, envelope protocol.Envelope) error
	// Connected reports whether the agent of the node is connected.
	Connected(nodeID string) bool
}

// Dispatcher owns the dispatch of the deployment tasks.
//
// The dispatcher binds a task to a node before it sends the command, so a
// task that was sent is never sent to another node: the ledger of a node
// holds the tasks it received, and a second node would run the same
// deployment twice.
type Dispatcher struct {
	// service owns the applications and their tasks.
	service *Service
	// transport hands the frames to the agents.
	transport Transport
	// now provides the timestamps of the records.
	now func() time.Time

	// mu guards the questions that wait for their answer.
	mu sync.Mutex
	// questions holds the questions about the tasks that wait for the
	// answer of a node.
	questions map[string]*pendingQuestion
}

// pendingQuestion is a question about a task that waits for its answer.
//
// The question is closed when the answer arrives, so every waiter of the
// same question reads the same answer.
type pendingQuestion struct {
	// done closes when the answer arrived.
	done chan struct{}
	// result is the answer of the node.
	result protocol.DeployQueryResult
}

// NewDispatcher builds the dispatcher of the deployment tasks.
//
// The service owns the tasks and the transport carries the frames to the
// agents. The dispatcher holds no state of its own that outlives a task, so
// a control端 that restarts starts with an empty dispatcher and recovers its
// tasks from the database.
func NewDispatcher(service *Service, transport Transport) *Dispatcher {
	return &Dispatcher{
		service:   service,
		transport: transport,
		now:       time.Now,
		questions: make(map[string]*pendingQuestion),
	}
}

// Dispatch sends the command of a task to the node that executes it.
//
// The node is chosen and written into the task before the command is sent.
// The write is a compare and swap, so two dispatchers that race send the
// command to one node only.
//
// A task that is bound already is sent to the node that holds it. The node
// is never exchanged, not even when the send failed: the command may have
// reached the node even though the connection broke, and a second node would
// run the same deployment a second time.
func (d *Dispatcher) Dispatch(ctx context.Context, taskID string) error {
	if d.service == nil {
		return errors.New("deploy: the dispatcher has no service")
	}
	if d.transport == nil {
		return errors.New("deploy: the dispatcher has no transport")
	}

	record, err := d.service.task(ctx, taskID)
	if err != nil {
		return err
	}
	if finalState(record.task.State) {
		// The task ended already. A command of a finished task would only
		// ask the node about a task it knows already.
		return fmt.Errorf("%w: the task %s already ended in %q", ErrConflict, taskID, record.task.State)
	}
	if record.task.AgentID != "" {
		if record.task.State != model.Queued {
			return fmt.Errorf("%w: the task %s is in the phase %q", ErrConflict, taskID, record.task.State)
		}
		// The task is bound already: only the node that holds it may run
		// it. The node is never exchanged, not even when the send failed:
		// the command may have reached the node even though the connection
		// broke, and a second node would run the same deployment twice.
		return d.send(ctx, taskID)
	}
	if record.task.State != model.Queued {
		return fmt.Errorf("%w: the task %s is in the phase %q", ErrConflict, taskID, record.task.State)
	}
	app, err := d.service.app(ctx, record.task.AppID)
	if err != nil {
		return err
	}
	nodeID, err := d.selectExecutor(ctx, app)
	if err != nil {
		return err
	}
	if _, err := d.bind(ctx, taskID, nodeID); err != nil {
		return err
	}
	return d.send(ctx, taskID)
}

// bind writes the node that executes the task into the task.
//
// The write is a compare and swap: only the caller that finds the task
// unbound and queued binds it. The time of the dispatch is written with it,
// so the control端 knows that the command was handed to the node even when
// the send failed afterwards.
func (d *Dispatcher) bind(ctx context.Context, taskID, nodeID string) (model.Task, error) {
	now := store.TimeToMillis(d.now())
	result, err := d.service.db.ExecContext(ctx,
		`UPDATE tasks SET agent_id = ?, dispatch_started_at = ?, updated_at = ?
		 WHERE id = ? AND agent_id = '' AND state = 'queued'`,
		nodeID, now, now, taskID)
	if err != nil {
		return model.Task{}, fmt.Errorf("deploy: bind the task %s to the node %s: %w", taskID, nodeID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return model.Task{}, fmt.Errorf("deploy: bind the task %s to the node %s: %w", taskID, nodeID, err)
	}
	if affected == 0 {
		return model.Task{}, fmt.Errorf("%w: the task %s was bound already", ErrConflict, taskID)
	}
	return d.service.Task(ctx, taskID)
}

// selectExecutor chooses the node that executes the deployment of an
// application.
//
// A Compose application runs on its target node, a Swarm application on a
// manager of its cluster. The role of a node is the one of its enrollment
// and never the one an agent claims for itself, so only an enrolled manager
// may execute a Swarm deployment.
//
// The node has to be connected: a command that nobody reads would leave the
// task waiting for ever.
func (d *Dispatcher) selectExecutor(ctx context.Context, app model.App) (string, error) {
	switch app.Mode {
	case yamlspec.ModeCompose:
		if !d.transport.Connected(app.TargetID) {
			return "", fmt.Errorf("%w: the node %s is not connected", ErrTargetUnavailable, app.TargetID)
		}
		return app.TargetID, nil
	case yamlspec.ModeSwarm:
		managers, err := d.managersOfCluster(ctx, app.TargetID)
		if err != nil {
			return "", err
		}
		for _, manager := range managers {
			if d.transport.Connected(manager) {
				return manager, nil
			}
		}
		return "", fmt.Errorf("%w: no manager of the cluster %s is connected", ErrTargetUnavailable, app.TargetID)
	default:
		return "", fmt.Errorf("%w: the mode %q is not supported", ErrInvalidApp, app.Mode)
	}
}

// managersOfCluster returns the enrolled managers of a Swarm cluster.
//
// The rows are read completely before the caller uses them: the write pool
// of the control端 holds a single connection, so a query that walks a row set
// may not run another query before it closed its rows.
func (d *Dispatcher) managersOfCluster(ctx context.Context, clusterID string) ([]string, error) {
	rows, err := d.service.db.QueryContext(ctx,
		`SELECT id FROM nodes
		 WHERE swarm_id = ? AND role = ? AND revoked_at IS NULL
		 ORDER BY id`, clusterID, protocol.RoleManager)
	if err != nil {
		return nil, fmt.Errorf("deploy: read the managers of the cluster %s: %w", clusterID, err)
	}
	defer rows.Close()

	var managers []string
	for rows.Next() {
		var identifier string
		if err := rows.Scan(&identifier); err != nil {
			return nil, fmt.Errorf("deploy: read the managers of the cluster %s: %w", clusterID, err)
		}
		managers = append(managers, identifier)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("deploy: read the managers of the cluster %s: %w", clusterID, err)
	}
	return managers, nil
}

// send hands the command of a task to the node that executes it.
//
// The command carries the application and its mode, so the node chooses the
// executor by the mode of the application and not by a path a client may
// send.
func (d *Dispatcher) send(ctx context.Context, taskID string) error {
	command, err := d.service.LoadCommand(ctx, taskID)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(protocol.DeployCommand{Command: command})
	if err != nil {
		return fmt.Errorf("deploy: encode the command of the task %s: %w", taskID, err)
	}
	envelope := protocol.Envelope{
		Version: protocol.WireVersion,
		Type:    protocol.TypeDeployCommand,
		Payload: payload,
	}
	if err := d.transport.Send(ctx, command.Task.AgentID, envelope); err != nil {
		// The command may have reached the node even though the send
		// failed, so the binding of the task stays: the task is never
		// handed to another node.
		return fmt.Errorf("deploy: send the command of the task %s to the node %s: %w",
			taskID, command.Task.AgentID, err)
	}
	return nil
}

// sendResolution hands the resolution of a task to the node that holds it.
//
// The resolution names the sequence number the task holds, so the node can
// refuse a resolution that covers a newer state of the task.
func (d *Dispatcher) sendResolution(ctx context.Context, task model.Task, expectedSeq uint64, reason string) error {
	payload, err := json.Marshal(protocol.DeployResolve{
		TaskID:      task.ID,
		Digest:      task.Digest,
		ExpectedSeq: expectedSeq,
		Reason:      reason,
	})
	if err != nil {
		return fmt.Errorf("deploy: encode the resolution of the task %s: %w", task.ID, err)
	}
	envelope := protocol.Envelope{
		Version: protocol.WireVersion,
		Type:    protocol.TypeDeployResolve,
		Payload: payload,
	}
	if err := d.transport.Send(ctx, task.AgentID, envelope); err != nil {
		return fmt.Errorf("deploy: send the resolution of the task %s to the node %s: %w",
			task.ID, task.AgentID, err)
	}
	return nil
}

// askNode asks a node about a task and waits for the answer.
//
// The question is registered before it is sent, so an answer that arrives
// very fast is not lost. Every waiter of the same question reads the same
// answer.
func (d *Dispatcher) askNode(ctx context.Context, task model.Task) (protocol.DeployQueryResult, error) {
	d.mu.Lock()
	question, waiting := d.questions[task.ID]
	if !waiting {
		question = &pendingQuestion{done: make(chan struct{})}
		d.questions[task.ID] = question
	}
	d.mu.Unlock()

	if !waiting {
		payload, err := json.Marshal(protocol.DeployQuery{TaskID: task.ID})
		if err != nil {
			d.forget(task.ID, question)
			return protocol.DeployQueryResult{}, fmt.Errorf("deploy: encode the question about the task %s: %w", task.ID, err)
		}
		envelope := protocol.Envelope{
			Version: protocol.WireVersion,
			Type:    protocol.TypeDeployQuery,
			Payload: payload,
		}
		if err := d.transport.Send(ctx, task.AgentID, envelope); err != nil {
			d.forget(task.ID, question)
			return protocol.DeployQueryResult{}, fmt.Errorf("deploy: ask the node %s about the task %s: %w",
				task.AgentID, task.ID, err)
		}
	}

	select {
	case <-question.done:
		return question.result, nil
	case <-ctx.Done():
		return protocol.DeployQueryResult{}, ctx.Err()
	case <-time.After(queryTimeout):
		return protocol.DeployQueryResult{}, fmt.Errorf("%w: the node %s did not answer the question about the task %s",
			ErrTargetUnavailable, task.AgentID, task.ID)
	}
}

// forget removes a question that nobody waits for any more.
func (d *Dispatcher) forget(taskID string, question *pendingQuestion) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if current, ok := d.questions[taskID]; ok && current == question {
		delete(d.questions, taskID)
	}
}

// answer delivers the answer of a node to the waiting question.
//
// The answer belongs to the node that sent it, so an answer of another node
// is refused. An answer nobody waits for is applied like a report: a report
// that arrives late is not lost.
func (d *Dispatcher) answer(ctx context.Context, nodeID string, result protocol.DeployQueryResult) error {
	record, err := d.service.task(ctx, result.TaskID)
	if err != nil {
		return err
	}
	if record.task.AgentID != "" && record.task.AgentID != nodeID {
		return fmt.Errorf("%w: the task %s belongs to the node %s", ErrConflict, result.TaskID, record.task.AgentID)
	}

	d.mu.Lock()
	question, waiting := d.questions[result.TaskID]
	if waiting {
		question.result = result
		delete(d.questions, result.TaskID)
	}
	d.mu.Unlock()

	if waiting {
		close(question.done)
		return nil
	}
	if !result.Found {
		// The ledger of the node does not know the task, so there is no
		// report to apply.
		return nil
	}
	return d.ApplyReceipt(ctx, nodeID, result.Receipt)
}

// ApplyQueryResult applies the answer of a node about a task in its ledger.
//
// The answer belongs to the node that sent it, so an answer of another node
// is refused. The answer is handed to the waiting question, and the report
// it carries is applied like any other report.
func (d *Dispatcher) ApplyQueryResult(ctx context.Context, nodeID string, result protocol.DeployQueryResult) error {
	return d.answer(ctx, nodeID, result)
}

// ApplyOutput stores a fragment of the output of a task.
//
// The fragment belongs to the node that executes the task, so a fragment of
// another node is refused.
func (d *Dispatcher) ApplyOutput(ctx context.Context, nodeID string, fragment protocol.DeployOutput) error {
	record, err := d.service.task(ctx, fragment.TaskID)
	if err != nil {
		return err
	}
	if record.task.AgentID == "" || record.task.AgentID != nodeID {
		return fmt.Errorf("%w: the task %s belongs to another node", ErrConflict, fragment.TaskID)
	}
	_, err = d.service.AppendOutput(ctx, fragment.TaskID, fragment.Offset, fragment.Data, fragment.Truncated)
	return err
}

// finalState reports whether the phase of a task is a final one.
//
// A task whose outcome is uncertain is not final: nobody may prove that the
// target is untouched, so the task waits for its check.
func finalState(state model.TaskState) bool {
	return state == model.Succeeded || state == model.Failed
}

// boolToInt turns a flag into the integer of the schema.
func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
