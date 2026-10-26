//go:build integration

package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"hmoob-dock/internal/agent"
	"hmoob-dock/internal/agentlink"
	deployment "hmoob-dock/internal/deploy"
	"hmoob-dock/internal/model"
	"hmoob-dock/internal/protocol"
	"hmoob-dock/internal/secret"
	"hmoob-dock/internal/store"
)

// countingExecutor counts the executions of the deployments.
//
// The executor replaces the Docker engine of the node: the ledger of the
// node and the database of the control端 are the real ones, so the
// protection against a repeated execution is the real one.
type countingExecutor struct {
	mu sync.Mutex
	// calls counts the executions.
	calls int
	// hold blocks the execution until it is released.
	hold chan struct{}
}

// Execute runs the command of the deployment.
func (c *countingExecutor) Execute(ctx context.Context, cmd model.Command, output io.Writer) (model.Result, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()

	if c.hold != nil {
		select {
		case <-c.hold:
		case <-ctx.Done():
			return model.Result{State: model.Uncertain, Code: "execution_uncertain"}, ctx.Err()
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

// nodeTransport hands the frames of the control端 to the ledger of a node.
//
// The transport replaces the connection of the agents in the test: the
// ledger of the node and the database of the control端 are the real ones.
type nodeTransport struct {
	mu sync.Mutex
	// ledger holds the tasks of the node.
	ledger *agent.Ledger
	// commands holds the commands the control端 sent.
	commands []model.Command
	// queries counts the questions the control端 asked.
	queries int
	// connected reports whether the node answers.
	connected bool
	// nodeID is the node the transport serves.
	nodeID string
	// dispatcher receives the answers of the node.
	dispatcher *deployment.Dispatcher
}

// fixedInventory is the state of the target of the tests.
//
// It replaces the Docker boundary of the tests only: the database of the
// control端 is a real SQLite file.
type fixedInventory struct {
	value protocol.Inventory
}

// ReadInventory returns the state of the target.
func (f *fixedInventory) ReadInventory(ctx context.Context, mode, targetID string) (protocol.Inventory, error) {
	return f.value, nil
}

// Send records a frame and answers a question about the ledger.
func (t *nodeTransport) Send(ctx context.Context, nodeID string, envelope protocol.Envelope) error {
	t.mu.Lock()
	switch envelope.Type {
	case protocol.TypeDeployCommand:
		var message protocol.DeployCommand
		if err := json.Unmarshal(envelope.Payload, &message); err != nil {
			t.mu.Unlock()
			return err
		}
		t.commands = append(t.commands, message.Command)
	case protocol.TypeDeployQuery:
		var query protocol.DeployQuery
		if err := json.Unmarshal(envelope.Payload, &query); err != nil {
			t.mu.Unlock()
			return err
		}
		t.queries++
		ledger := t.ledger
		t.mu.Unlock()

		answer := protocol.DeployQueryResult{TaskID: query.TaskID}
		receipt, err := ledger.Lookup(ctx, query.TaskID)
		if err == nil {
			answer.Found = true
			answer.Receipt = receipt
		} else if !errors.Is(err, agent.ErrTaskNotFound) {
			return err
		}
		return t.answer(answer)
	default:
		t.mu.Unlock()
		return nil
	}
	t.mu.Unlock()
	return nil
}

// answer hands the answer of the node to the dispatcher.
func (t *nodeTransport) answer(answer protocol.DeployQueryResult) error {
	t.mu.Lock()
	dispatcher := t.dispatcher
	t.mu.Unlock()
	if dispatcher == nil {
		return errors.New("the node has no dispatcher")
	}
	return dispatcher.ApplyQueryResult(context.Background(), t.nodeID, answer)
}

// Connected reports whether the node answers.
func (t *nodeTransport) Connected(nodeID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.connected
}

// sentCommands returns the commands the control端 sent.
func (t *nodeTransport) sentCommands() []model.Command {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]model.Command(nil), t.commands...)
}

// askedQueries returns the number of the questions the control端 asked.
func (t *nodeTransport) askedQueries() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.queries
}

// recoveryControl is a control端 with a node and a deployment service.
type recoveryControl struct {
	// db is the database of the control端.
	db *sql.DB
	// service owns the applications of the control端.
	service *deployment.Service
	// nodeID is the node that executes the deployments.
	nodeID string
}

// newRecoveryControl builds a control端 with a node.
func newRecoveryControl(t *testing.T, path string) *recoveryControl {
	t.Helper()

	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	box, err := secret.New(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatalf("build the secret box: %v", err)
	}

	enrollment := agentlink.NewEnrollment(db, time.Now)
	token, err := enrollment.Issue(context.Background())
	if err != nil {
		t.Fatalf("issue the enrollment token: %v", err)
	}
	credential, err := enrollment.Redeem(context.Background(), token, protocol.NodeIdentity{
		EngineID: "engine-of-the-recovery",
	})
	if err != nil {
		t.Fatalf("redeem the enrollment token: %v", err)
	}

	source := &fixedInventory{value: protocol.Inventory{
		Identity:    protocol.NodeIdentity{EngineID: "engine-of-the-recovery"},
		CollectedAt: time.Now(),
	}}
	return &recoveryControl{
		db:      db,
		service: deployment.New(db, box, source),
		nodeID:  credential.NodeID,
	}
}

// TestDeploymentRecovery proves that a control端 that restarts asks the node
// about its task instead of running the deployment a second time.
//
// The test stands for the fault injection of the plan: the report of the
// node is lost, the control端 restarts and the task must not run again.
func TestDeploymentRecovery(t *testing.T) {
	control := newRecoveryControl(t, filepath.Join(t.TempDir(), "control.db"))

	ledger, err := agent.OpenLedger(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}
	t.Cleanup(func() { ledger.Close() })

	executor := &countingExecutor{}
	worker := agent.NewWorker(ledger, executor).UseRootContext(context.Background())

	transport := &nodeTransport{ledger: ledger, connected: true, nodeID: control.nodeID}
	dispatcher := deployment.NewDispatcher(control.service, transport)
	transport.dispatcher = dispatcher

	app, err := control.service.CreateApp(context.Background(), model.App{
		Name:      "demo",
		Mode:      "compose",
		TargetID:  control.nodeID,
		Namespace: "demo",
	}, false)
	if err != nil {
		t.Fatalf("create the application: %v", err)
	}
	config := model.Config{YAML: "services:\n  api:\n    image: nginx:alpine\n"}
	preview, err := control.service.Preview(context.Background(), app.ID, "", config)
	if err != nil {
		t.Fatalf("preview the deployment: %v", err)
	}
	task, err := control.service.Submit(context.Background(), deployment.SubmitRequest{
		AppID:          app.ID,
		IdempotencyKey: "request-1",
		Config:         config,
		PreviewDigest:  preview.Digest,
	})
	if err != nil {
		t.Fatalf("submit the deployment: %v", err)
	}

	// The control端 sends the command to the node.
	if err := dispatcher.Dispatch(context.Background(), task.ID); err != nil {
		t.Fatalf("dispatch the task: %v", err)
	}
	commands := transport.sentCommands()
	if len(commands) != 1 {
		t.Fatalf("commands = %d, want 1", len(commands))
	}

	// The node runs the deployment and the report of the node is lost: the
	// connection breaks before the report reaches the control端.
	if _, err := worker.Handle(context.Background(), commands[0], io.Discard); err != nil {
		t.Fatalf("run the deployment: %v", err)
	}
	if executor.count() != 1 {
		t.Fatalf("executions = %d, want 1", executor.count())
	}

	// The control端 restarts and recovers its tasks.
	restarted := deployment.NewDispatcher(control.service, transport)
	transport.dispatcher = restarted
	if err := restarted.Recover(context.Background()); err != nil {
		t.Fatalf("recover the deployments: %v", err)
	}

	// The node was asked about its task, and the command was not sent a
	// second time.
	if transport.askedQueries() != 1 {
		t.Fatalf("questions = %d, want 1", transport.askedQueries())
	}
	if got := len(transport.sentCommands()); got != 1 {
		t.Fatalf("commands = %d, want 1: the deployment was sent a second time", got)
	}
	if executor.count() != 1 {
		t.Fatalf("executions = %d, want 1: the deployment ran a second time", executor.count())
	}

	// The control端 knows the outcome of the deployment.
	current, err := control.service.Task(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("read the task: %v", err)
	}
	if current.State != model.Succeeded {
		t.Fatalf("the task is in the phase %q, want succeeded", current.State)
	}
}

// TestTheManualReleaseNeedsTheNode proves that the release of a task is only
// complete when the node confirms it.
//
// The test stands for the fault injection of the plan: the node stops in the
// middle of the deployment, the control端 cannot prove the outcome and the
// administrator releases the task by hand.
func TestTheManualReleaseNeedsTheNode(t *testing.T) {
	control := newRecoveryControl(t, filepath.Join(t.TempDir(), "control.db"))

	path := filepath.Join(t.TempDir(), "ledger.db")
	ledger, err := agent.OpenLedger(path)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}
	executor := &countingExecutor{hold: make(chan struct{})}
	worker := agent.NewWorker(ledger, executor).UseRootContext(context.Background())

	transport := &nodeTransport{ledger: ledger, connected: true, nodeID: control.nodeID}
	dispatcher := deployment.NewDispatcher(control.service, transport)
	transport.dispatcher = dispatcher

	app, err := control.service.CreateApp(context.Background(), model.App{
		Name:      "demo",
		Mode:      "compose",
		TargetID:  control.nodeID,
		Namespace: "demo",
	}, false)
	if err != nil {
		t.Fatalf("create the application: %v", err)
	}
	config := model.Config{YAML: "services:\n  api:\n    image: nginx:alpine\n"}
	preview, err := control.service.Preview(context.Background(), app.ID, "", config)
	if err != nil {
		t.Fatalf("preview the deployment: %v", err)
	}
	task, err := control.service.Submit(context.Background(), deployment.SubmitRequest{
		AppID:          app.ID,
		IdempotencyKey: "request-1",
		Config:         config,
		PreviewDigest:  preview.Digest,
	})
	if err != nil {
		t.Fatalf("submit the deployment: %v", err)
	}
	if err := dispatcher.Dispatch(context.Background(), task.ID); err != nil {
		t.Fatalf("dispatch the task: %v", err)
	}
	commands := transport.sentCommands()
	if len(commands) != 1 {
		t.Fatalf("commands = %d, want 1", len(commands))
	}

	// The node begins the deployment and the deployment never ends.
	finished := make(chan model.Receipt, 1)
	go func() {
		receipt, err := worker.Handle(context.Background(), commands[0], io.Discard)
		if err != nil {
			t.Errorf("run the deployment: %v", err)
		}
		finished <- receipt
	}()

	// The node stops in the middle of the deployment: its ledger marks the
	// task as uncertain.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		receipt, err := ledger.Lookup(context.Background(), task.ID)
		if err == nil && receipt.Result.State == model.Running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := ledger.Close(); err != nil {
		t.Fatalf("close the ledger: %v", err)
	}
	reopened, err := agent.OpenLedger(path)
	if err != nil {
		t.Fatalf("open the ledger again: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })
	transport.mu.Lock()
	transport.ledger = reopened
	transport.mu.Unlock()

	known, err := reopened.Lookup(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("read the task: %v", err)
	}
	if known.Result.State != model.Uncertain {
		t.Fatalf("the task is in the phase %q, want uncertain", known.Result.State)
	}

	// The control端 asks the node about the task and marks it as uncertain.
	if _, err := dispatcher.Reconcile(context.Background(), task.ID); err != nil {
		t.Fatalf("reconcile the task: %v", err)
	}
	current, err := control.service.Task(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("read the task: %v", err)
	}
	if current.State != model.Uncertain {
		t.Fatalf("the task is in the phase %q, want uncertain", current.State)
	}

	// The release needs the confirmation of the administrator and a reason.
	if err := dispatcher.Resolve(context.Background(), task.ID, current.LastSeq, false, "the executor stopped"); err == nil {
		t.Fatal("the release without a confirmation was accepted")
	}
	if err := dispatcher.Resolve(context.Background(), task.ID, current.LastSeq, true, "  "); err == nil {
		t.Fatal("the release without a reason was accepted")
	}

	// The release waits for the confirmation of the node.
	if err := dispatcher.Resolve(context.Background(), task.ID, current.LastSeq, true,
		"the executor of the task was stopped by hand"); err != nil {
		t.Fatalf("release the task: %v", err)
	}
	state, err := control.service.Deployment(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("read the deployment: %v", err)
	}
	if state.State == model.Failed {
		t.Fatalf("the task ended before the node confirmed the release: %+v", state)
	}
	if state.ManualResolution == nil || !state.ManualResolution.Pending {
		t.Fatalf("the deployment = %+v, want a pending release", state)
	}

	// The node confirms the release and the application is free again.
	receipt, err := reopened.Resolve(context.Background(), task.ID, known.Seq, model.Result{
		State:   model.Failed,
		Message: "the executor of the task was stopped by hand",
	})
	if err != nil {
		t.Fatalf("release the task in the ledger: %v", err)
	}
	if receipt.Result.Code != agent.CodeManualResolution {
		t.Fatalf("the outcome holds the code %q, want %q", receipt.Result.Code, agent.CodeManualResolution)
	}
	if err := dispatcher.ApplyReceipt(context.Background(), control.nodeID, receipt); err != nil {
		t.Fatalf("apply the report: %v", err)
	}
	state, err = control.service.Deployment(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("read the deployment: %v", err)
	}
	if state.State != model.Failed {
		t.Fatalf("the task is in the phase %q, want failed", state.State)
	}
	if state.ManualResolution == nil || state.ManualResolution.Pending {
		t.Fatalf("the deployment = %+v, want a confirmed release", state)
	}

	// The application may be deployed again.
	next := model.Config{YAML: "services:\n  api:\n    image: nginx:1.27\n"}
	preview, err = control.service.Preview(context.Background(), app.ID, task.VersionID, next)
	if err != nil {
		t.Fatalf("preview the next deployment: %v", err)
	}
	if _, err := control.service.Submit(context.Background(), deployment.SubmitRequest{
		AppID:          app.ID,
		BaseVersionID:  task.VersionID,
		IdempotencyKey: "request-2",
		Config:         next,
		PreviewDigest:  preview.Digest,
	}); err != nil {
		t.Fatalf("deploy the application again: %v", err)
	}
}
