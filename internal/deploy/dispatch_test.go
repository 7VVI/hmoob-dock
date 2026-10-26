package deploy_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"hmoob-dock/internal/agentlink"
	"hmoob-dock/internal/deploy"
	"hmoob-dock/internal/model"
	"hmoob-dock/internal/protocol"
)

// sentFrame is a frame the dispatcher sent.
type sentFrame struct {
	// nodeID is the node the frame was sent to.
	nodeID string
	// envelope is the frame itself.
	envelope protocol.Envelope
}

// recordingTransport records the frames the dispatcher sends.
//
// It replaces the hub of the agents in the tests only: the database of the
// tests is a real SQLite file and the identity of the node comes from a real
// enrollment.
type recordingTransport struct {
	mu sync.Mutex
	// frames holds the frames the dispatcher sent.
	frames []sentFrame
	// connected holds the nodes that answer.
	connected map[string]bool
	// err is the error a send fails with.
	err error
	// onSend runs for every frame. It may answer a question of the
	// dispatcher.
	onSend func(nodeID string, envelope protocol.Envelope) error
}

// Send records a frame.
func (r *recordingTransport) Send(ctx context.Context, nodeID string, envelope protocol.Envelope) error {
	r.mu.Lock()
	r.frames = append(r.frames, sentFrame{nodeID: nodeID, envelope: envelope})
	hook := r.onSend
	err := r.err
	r.mu.Unlock()

	if err != nil {
		return err
	}
	if hook != nil {
		return hook(nodeID, envelope)
	}
	return nil
}

// Connected reports whether the node answers.
func (r *recordingTransport) Connected(nodeID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.connected[nodeID]
}

// connect marks a node as connected.
func (r *recordingTransport) connect(nodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.connected == nil {
		r.connected = make(map[string]bool)
	}
	r.connected[nodeID] = true
}

// sent returns the frames the transport received.
func (r *recordingTransport) sent() []sentFrame {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]sentFrame(nil), r.frames...)
}

// commands returns the commands the transport received.
func (r *recordingTransport) commands() []model.Command {
	var commands []model.Command
	for _, frame := range r.sent() {
		if frame.envelope.Type != protocol.TypeDeployCommand {
			continue
		}
		var message protocol.DeployCommand
		if err := json.Unmarshal(frame.envelope.Payload, &message); err != nil {
			continue
		}
		commands = append(commands, message.Command)
	}
	return commands
}

// enrollNode registers another node at the control端.
func (h *harness) enrollNode(t *testing.T, engineID string) string {
	t.Helper()

	enrollment := agentlink.NewEnrollment(h.db, time.Now)
	token, err := enrollment.Issue(context.Background())
	if err != nil {
		t.Fatalf("issue the enrollment token: %v", err)
	}
	credential, err := enrollment.Redeem(context.Background(), token, protocol.NodeIdentity{EngineID: engineID})
	if err != nil {
		t.Fatalf("redeem the enrollment token: %v", err)
	}
	return credential.NodeID
}

// submit deploys a configuration of an application.
//
// The base version of the request is the version the application points at,
// so a deployment that follows another one does not fight with it.
func (h *harness) submit(app model.App, cfg model.Config, baseVersionID ...string) model.Task {
	h.t.Helper()

	base := ""
	if len(baseVersionID) > 0 {
		base = baseVersionID[0]
	}
	preview, err := h.service.Preview(context.Background(), app.ID, base, cfg)
	if err != nil {
		h.t.Fatalf("preview the deployment: %v", err)
	}
	task, err := h.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:          app.ID,
		BaseVersionID:  base,
		IdempotencyKey: "request-" + app.ID + "-" + preview.Digest,
		Config:         cfg,
		PreviewDigest:  preview.Digest,
	})
	if err != nil {
		h.t.Fatalf("submit the deployment: %v", err)
	}
	return task
}

// successfulVersion reads the version the application points at.
func (h *harness) successfulVersion(appID string) string {
	h.t.Helper()

	var version string
	if err := h.db.QueryRow(`SELECT successful_version_id FROM apps WHERE id = ?`, appID).Scan(&version); err != nil {
		h.t.Fatalf("read the application: %v", err)
	}
	return version
}

// TestTaskTransitionDoesNotRegress proves that a task that ended keeps its
// outcome and that a running task may become uncertain.
func TestTaskTransitionDoesNotRegress(t *testing.T) {
	for _, to := range []model.TaskState{model.Running, model.Queued, model.Uncertain} {
		if deploy.CanTransition(model.Succeeded, to) {
			t.Fatalf("success regressed to %s", to)
		}
	}
	if !deploy.CanTransition(model.Running, model.Uncertain) {
		t.Fatal("disconnect cannot become uncertain")
	}
}

// TestTheFailedTaskKeepsItsOutcome proves that a task that failed is not
// rewritten by a report that arrives late.
func TestTheFailedTaskKeepsItsOutcome(t *testing.T) {
	for _, to := range []model.TaskState{model.Running, model.Queued, model.Succeeded, model.Uncertain} {
		if deploy.CanTransition(model.Failed, to) {
			t.Fatalf("failure regressed to %s", to)
		}
	}
}

// TestTheUncertainTaskIsOnlyReleasedByEvidence proves that a task whose
// outcome is unknown goes back to a running phase or ends, but never back to
// the queue.
func TestTheUncertainTaskIsOnlyReleasedByEvidence(t *testing.T) {
	for _, to := range []model.TaskState{model.Running, model.Verifying, model.Succeeded, model.Failed} {
		if !deploy.CanTransition(model.Uncertain, to) {
			t.Fatalf("the uncertain task cannot become %s", to)
		}
	}
	if deploy.CanTransition(model.Uncertain, model.Queued) {
		t.Fatal("the uncertain task went back to the queue")
	}
}

// TestTheDispatchBindsTheTaskBeforeItSends proves that the node that
// executes a task is written into the task before the command leaves the
// control端.
func TestTheDispatchBindsTheTaskBeforeItSends(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")
	task := env.submit(app, composeConfig("nginx:alpine"))

	transport := &recordingTransport{}
	transport.connect(env.nodeID)
	// The transport reads the task while the command is sent: the node has
	// to be written into the task already, otherwise a command that leaves
	// the control端 without a binding could be sent to another node.
	transport.onSend = func(nodeID string, envelope protocol.Envelope) error {
		if envelope.Type != protocol.TypeDeployCommand {
			return nil
		}
		var bound string
		if err := env.db.QueryRow(`SELECT agent_id FROM tasks WHERE id = ?`, task.ID).Scan(&bound); err != nil {
			t.Errorf("read the bound node: %v", err)
			return nil
		}
		if bound == "" {
			t.Error("the command was sent before the task was bound to a node")
		}
		return nil
	}

	dispatcher := deploy.NewDispatcher(env.service, transport)
	if err := dispatcher.Dispatch(context.Background(), task.ID); err != nil {
		t.Fatalf("dispatch the task: %v", err)
	}

	frames := transport.sent()
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	if frames[0].nodeID != env.nodeID {
		t.Fatalf("the frame went to the node %s, want %s", frames[0].nodeID, env.nodeID)
	}
	commands := transport.commands()
	if len(commands) != 1 {
		t.Fatalf("commands = %d, want 1", len(commands))
	}
	if commands[0].Task.ID != task.ID {
		t.Fatalf("the command carries the task %s, want %s", commands[0].Task.ID, task.ID)
	}
	if commands[0].Config.YAML == "" {
		t.Fatal("the command carries no configuration")
	}
}

// TestTheBoundTaskIsNeverSentToAnotherNode proves that a task that was
// bound keeps its node, even when the control端 restarts.
func TestTheBoundTaskIsNeverSentToAnotherNode(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")
	task := env.submit(app, composeConfig("nginx:alpine"))
	other := env.enrollNode(t, "engine-b")

	transport := &recordingTransport{}
	transport.connect(env.nodeID)
	transport.connect(other)

	dispatcher := deploy.NewDispatcher(env.service, transport)
	if err := dispatcher.Dispatch(context.Background(), task.ID); err != nil {
		t.Fatalf("dispatch the task: %v", err)
	}

	// The connection of the node breaks and the control端 starts again.
	transport.mu.Lock()
	transport.err = errors.New("the connection broke")
	transport.mu.Unlock()

	restarted := deploy.NewDispatcher(env.service, transport)
	if err := restarted.Dispatch(context.Background(), task.ID); err == nil {
		t.Fatal("the dispatch of a task of a lost node reported success")
	}

	frames := transport.sent()
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(frames))
	}
	for _, frame := range frames {
		if frame.nodeID != env.nodeID {
			t.Fatalf("the frame went to the node %s, want the bound node %s", frame.nodeID, env.nodeID)
		}
	}
}

// TestTheReportOfAnotherNodeIsRefused proves that a node may not report
// about the task of another node.
func TestTheReportOfAnotherNodeIsRefused(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")
	task := env.submit(app, composeConfig("nginx:alpine"))

	transport := &recordingTransport{}
	transport.connect(env.nodeID)
	dispatcher := deploy.NewDispatcher(env.service, transport)
	if err := dispatcher.Dispatch(context.Background(), task.ID); err != nil {
		t.Fatalf("dispatch the task: %v", err)
	}

	report := model.Receipt{
		TaskID: task.ID,
		Digest: task.Digest,
		Seq:    3,
		Result: model.Result{State: model.Succeeded, Code: "compose_ready"},
	}
	if err := dispatcher.ApplyReceipt(context.Background(), "another-node", report); err == nil {
		t.Fatal("the report of another node was accepted")
	}

	current, err := env.service.Task(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("read the task: %v", err)
	}
	if current.State != model.Queued {
		t.Fatalf("the task is in the phase %q, want the phase it had", current.State)
	}
}

// TestTheReportOfAnotherConfigurationIsRefused proves that a report of
// another configuration does not change the task.
func TestTheReportOfAnotherConfigurationIsRefused(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")
	task := env.submit(app, composeConfig("nginx:alpine"))

	transport := &recordingTransport{}
	transport.connect(env.nodeID)
	dispatcher := deploy.NewDispatcher(env.service, transport)
	if err := dispatcher.Dispatch(context.Background(), task.ID); err != nil {
		t.Fatalf("dispatch the task: %v", err)
	}

	report := model.Receipt{
		TaskID: task.ID,
		Digest: "the-digest-of-another-configuration",
		Seq:    3,
		Result: model.Result{State: model.Succeeded, Code: "compose_ready"},
	}
	if err := dispatcher.ApplyReceipt(context.Background(), env.nodeID, report); err == nil {
		t.Fatal("the report of another configuration was accepted")
	}
}

// TestTheReportIsMonotonic proves that an old report never overwrites a
// newer one and that a report that arrives again is ignored.
func TestTheReportIsMonotonic(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")
	task := env.submit(app, composeConfig("nginx:alpine"))

	transport := &recordingTransport{}
	transport.connect(env.nodeID)
	dispatcher := deploy.NewDispatcher(env.service, transport)
	if err := dispatcher.Dispatch(context.Background(), task.ID); err != nil {
		t.Fatalf("dispatch the task: %v", err)
	}

	running := model.Receipt{
		TaskID: task.ID,
		Digest: task.Digest,
		Seq:    2,
		Result: model.Result{State: model.Running, Code: "running"},
	}
	if err := dispatcher.ApplyReceipt(context.Background(), env.nodeID, running); err != nil {
		t.Fatalf("apply the report: %v", err)
	}

	// The report arrives again: it changes nothing.
	if err := dispatcher.ApplyReceipt(context.Background(), env.nodeID, running); err != nil {
		t.Fatalf("apply the repeated report: %v", err)
	}
	current, err := env.service.Task(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("read the task: %v", err)
	}
	if current.LastSeq != 2 {
		t.Fatalf("the sequence number = %d, want 2", current.LastSeq)
	}
	if current.State != model.Running {
		t.Fatalf("the task is in the phase %q, want running", current.State)
	}

	// An older report does not roll the task back.
	older := model.Receipt{
		TaskID: task.ID,
		Digest: task.Digest,
		Seq:    1,
		Result: model.Result{State: model.Queued, Code: "accepted"},
	}
	if err := dispatcher.ApplyReceipt(context.Background(), env.nodeID, older); err != nil {
		t.Fatalf("apply the old report: %v", err)
	}
	current, err = env.service.Task(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("read the task: %v", err)
	}
	if current.LastSeq != 2 || current.State != model.Running {
		t.Fatalf("the task is in the phase %q with the sequence number %d, want running with 2",
			current.State, current.LastSeq)
	}
}

// TestOnlyAVerifiedSuccessPointsTheApplicationAtTheVersion proves that a
// failed or rolled back deployment keeps the version the application points
// at.
func TestOnlyAVerifiedSuccessPointsTheApplicationAtTheVersion(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")
	task := env.submit(app, composeConfig("nginx:alpine"))

	transport := &recordingTransport{}
	transport.connect(env.nodeID)
	dispatcher := deploy.NewDispatcher(env.service, transport)
	if err := dispatcher.Dispatch(context.Background(), task.ID); err != nil {
		t.Fatalf("dispatch the task: %v", err)
	}

	failure := model.Receipt{
		TaskID: task.ID,
		Digest: task.Digest,
		Seq:    3,
		Result: model.Result{
			State:      model.Failed,
			Code:       "compose_up_failed",
			RolledBack: true,
		},
	}
	if err := dispatcher.ApplyReceipt(context.Background(), env.nodeID, failure); err != nil {
		t.Fatalf("apply the report: %v", err)
	}
	if version := env.successfulVersion(app.ID); version != "" {
		t.Fatalf("the successful version = %q, want none", version)
	}

	// The next deployment succeeds and points the application at its
	// version.
	env.finishTask(task.ID)
	second := env.submit(app, composeConfig("nginx:1.27"), task.VersionID)
	if err := dispatcher.Dispatch(context.Background(), second.ID); err != nil {
		t.Fatalf("dispatch the second task: %v", err)
	}
	success := model.Receipt{
		TaskID: second.ID,
		Digest: second.Digest,
		Seq:    3,
		Result: model.Result{State: model.Succeeded, Code: "compose_ready"},
	}
	if err := dispatcher.ApplyReceipt(context.Background(), env.nodeID, success); err != nil {
		t.Fatalf("apply the report: %v", err)
	}
	if version := env.successfulVersion(app.ID); version != second.VersionID {
		t.Fatalf("the successful version = %q, want %q", version, second.VersionID)
	}
}
