package deploy_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"hmoob-dock/internal/deploy"
	"hmoob-dock/internal/model"
	"hmoob-dock/internal/protocol"
)

// answeringTransport answers the questions of the dispatcher with the
// receipt the test prepared.
type answeringTransport struct {
	*recordingTransport
	// dispatcher receives the answer of the node.
	dispatcher *deploy.Dispatcher
	// answer is the answer of the node.
	answer protocol.DeployQueryResult
	// asked counts the questions the node received.
	asked int
}

// Send records a frame and answers a question of the dispatcher.
func (a *answeringTransport) Send(ctx context.Context, nodeID string, envelope protocol.Envelope) error {
	if envelope.Type == protocol.TypeDeployQuery {
		a.mu.Lock()
		a.asked++
		var query protocol.DeployQuery
		if err := json.Unmarshal(envelope.Payload, &query); err != nil {
			a.mu.Unlock()
			return err
		}
		answer := a.answer
		a.mu.Unlock()

		answer.TaskID = query.TaskID
		// The answer reaches the dispatcher through the frame of the node,
		// so the test proves the whole path.
		go func() {
			_ = a.dispatcher.ApplyQueryResult(context.Background(), nodeID, answer)
		}()
		return nil
	}
	return a.recordingTransport.Send(ctx, nodeID, envelope)
}

// questions returns the number of questions the node received.
func (a *answeringTransport) questions() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.asked
}

// TestTheReconcileReadsTheLedgerOfTheNode proves that the control端 asks the
// node about a task and applies its answer.
func TestTheReconcileReadsTheLedgerOfTheNode(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")
	task := env.submit(app, composeConfig("nginx:alpine"))

	transport := &recordingTransport{}
	transport.connect(env.nodeID)
	answering := &answeringTransport{recordingTransport: transport}
	answering.answer = protocol.DeployQueryResult{
		Found: true,
		Receipt: model.Receipt{
			TaskID: task.ID,
			Digest: task.Digest,
			Seq:    3,
			Result: model.Result{State: model.Succeeded, Code: "compose_ready"},
		},
	}
	dispatcher := deploy.NewDispatcher(env.service, answering)
	answering.dispatcher = dispatcher
	if err := dispatcher.Dispatch(context.Background(), task.ID); err != nil {
		t.Fatalf("dispatch the task: %v", err)
	}

	updated, err := dispatcher.Reconcile(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("reconcile the task: %v", err)
	}
	if updated.State != model.Succeeded {
		t.Fatalf("the task is in the phase %q, want succeeded", updated.State)
	}
	if updated.LastSeq != 3 {
		t.Fatalf("the sequence number = %d, want 3", updated.LastSeq)
	}
}

// TestTheRestartOfTheControlEndDoesNotRunTheDeploymentAgain proves that a
// control端 that restarts asks the node about its tasks instead of sending
// the deployments a second time.
func TestTheRestartOfTheControlEndDoesNotRunTheDeploymentAgain(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")
	task := env.submit(app, composeConfig("nginx:alpine"))

	transport := &recordingTransport{}
	transport.connect(env.nodeID)
	answering := &answeringTransport{recordingTransport: transport}
	answering.answer = protocol.DeployQueryResult{
		Found: true,
		Receipt: model.Receipt{
			TaskID: task.ID,
			Digest: task.Digest,
			Seq:    3,
			Result: model.Result{State: model.Succeeded, Code: "compose_ready"},
		},
	}
	restarted := deploy.NewDispatcher(env.service, answering)
	answering.dispatcher = restarted
	if err := restarted.Dispatch(context.Background(), task.ID); err != nil {
		t.Fatalf("dispatch the task: %v", err)
	}
	commands := len(answering.commands())

	// The control端 restarts and recovers its tasks.
	if err := restarted.Recover(context.Background()); err != nil {
		t.Fatalf("recover the deployments: %v", err)
	}
	if got := len(answering.commands()); got != commands {
		t.Fatalf("commands = %d, want %d: the deployment was sent a second time", got, commands)
	}
	if answering.questions() != 1 {
		t.Fatalf("questions = %d, want 1", answering.questions())
	}
	current, err := env.service.Task(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("read the task: %v", err)
	}
	if current.State != model.Succeeded {
		t.Fatalf("the task is in the phase %q, want succeeded", current.State)
	}
}

// TestTheTaskOfALostNodeBecomesUncertain proves that a task whose node does
// not answer keeps its application and becomes uncertain.
func TestTheTaskOfALostNodeBecomesUncertain(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")
	task := env.submit(app, composeConfig("nginx:alpine"))

	transport := &recordingTransport{}
	transport.connect(env.nodeID)
	dispatcher := deploy.NewDispatcher(env.service, transport)
	if err := dispatcher.Dispatch(context.Background(), task.ID); err != nil {
		t.Fatalf("dispatch the task: %v", err)
	}

	// The node is gone when the control端 recovers its tasks.
	transport.mu.Lock()
	transport.connected = map[string]bool{}
	transport.mu.Unlock()

	restarted := deploy.NewDispatcher(env.service, transport)
	if err := restarted.Recover(context.Background()); err != nil {
		t.Fatalf("recover the deployments: %v", err)
	}
	current, err := env.service.Task(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("read the task: %v", err)
	}
	if current.State != model.Uncertain {
		t.Fatalf("the task is in the phase %q, want uncertain", current.State)
	}

	// The application is still busy: nobody may prove that the target is
	// untouched.
	if _, err := env.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:          app.ID,
		BaseVersionID:  task.VersionID,
		IdempotencyKey: "another-request",
		Config:         composeConfig("nginx:1.27"),
		PreviewDigest:  env.preview(app, task.VersionID, composeConfig("nginx:1.27")).Digest,
	}); !errors.Is(err, deploy.ErrBusy) {
		t.Fatalf("error = %v, want a busy application", err)
	}
}

// TestTheManualResolutionNeedsAConfirmationAndAReason proves that a release
// without the confirmation of the administrator or without a reason is
// refused.
func TestTheManualResolutionNeedsAConfirmationAndAReason(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")
	task := env.submit(app, composeConfig("nginx:alpine"))

	transport := &recordingTransport{}
	transport.connect(env.nodeID)
	dispatcher := deploy.NewDispatcher(env.service, transport)
	if err := dispatcher.Dispatch(context.Background(), task.ID); err != nil {
		t.Fatalf("dispatch the task: %v", err)
	}

	if err := dispatcher.Resolve(context.Background(), task.ID, 0, false, "the executor stopped"); err == nil {
		t.Fatal("the release without a confirmation was accepted")
	}
	if err := dispatcher.Resolve(context.Background(), task.ID, 0, true, "   "); err == nil {
		t.Fatal("the release without a reason was accepted")
	}
	if err := dispatcher.Resolve(context.Background(), task.ID, 7, true, "the executor stopped"); err == nil {
		t.Fatal("the release with another sequence number was accepted")
	}
}

// TestTheManualResolutionWaitsForTheNode proves that a release of a task
// whose node cannot be reached waits for its confirmation and keeps the
// application busy.
func TestTheManualResolutionWaitsForTheNode(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")
	task := env.submit(app, composeConfig("nginx:alpine"))

	transport := &recordingTransport{}
	transport.connect(env.nodeID)
	dispatcher := deploy.NewDispatcher(env.service, transport)
	if err := dispatcher.Dispatch(context.Background(), task.ID); err != nil {
		t.Fatalf("dispatch the task: %v", err)
	}

	// The node is gone when the administrator releases the task.
	transport.mu.Lock()
	transport.err = errors.New("the node is not connected")
	transport.mu.Unlock()

	err := dispatcher.Resolve(context.Background(), task.ID, 0, true, "the executor of the task was stopped by hand")
	if !errors.Is(err, deploy.ErrResolvePending) {
		t.Fatalf("error = %v, want a pending release", err)
	}

	deployment, err := env.service.Deployment(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("read the deployment: %v", err)
	}
	if deployment.ManualResolution == nil || !deployment.ManualResolution.Pending {
		t.Fatalf("the deployment = %+v, want a pending release", deployment)
	}
	if deployment.State == model.Failed {
		t.Fatal("the task ended before the node confirmed the release")
	}
}

// TestTheManualResolutionIsConfirmedByTheNode proves that the release ends
// the task only when the node confirms it.
func TestTheManualResolutionIsConfirmedByTheNode(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")
	task := env.submit(app, composeConfig("nginx:alpine"))

	transport := &recordingTransport{}
	transport.connect(env.nodeID)
	dispatcher := deploy.NewDispatcher(env.service, transport)
	if err := dispatcher.Dispatch(context.Background(), task.ID); err != nil {
		t.Fatalf("dispatch the task: %v", err)
	}
	if err := dispatcher.Resolve(context.Background(), task.ID, 0, true, "the executor of the task was stopped by hand"); err != nil {
		t.Fatalf("release the task: %v", err)
	}

	// The node confirms the release.
	report := model.Receipt{
		TaskID: task.ID,
		Digest: task.Digest,
		Seq:    1,
		Result: model.Result{
			State:   model.Failed,
			Code:    deploy.CodeManualResolution,
			Message: "the executor of the task was stopped by hand",
		},
	}
	if err := dispatcher.ApplyReceipt(context.Background(), env.nodeID, report); err != nil {
		t.Fatalf("apply the report: %v", err)
	}

	deployment, err := env.service.Deployment(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("read the deployment: %v", err)
	}
	if deployment.State != model.Failed {
		t.Fatalf("the task is in the phase %q, want failed", deployment.State)
	}
	if deployment.ResultCode != deploy.CodeManualResolution {
		t.Fatalf("the result code = %q, want %q", deployment.ResultCode, deploy.CodeManualResolution)
	}
	if deployment.ManualResolution == nil || deployment.ManualResolution.Pending {
		t.Fatalf("the deployment = %+v, want a confirmed release", deployment)
	}

	// The application is free again.
	if _, err := env.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:          app.ID,
		BaseVersionID:  task.VersionID,
		IdempotencyKey: "another-request",
		Config:         composeConfig("nginx:1.27"),
		PreviewDigest:  env.preview(app, task.VersionID, composeConfig("nginx:1.27")).Digest,
	}); err != nil {
		t.Fatalf("deploy the application again: %v", err)
	}
}

// TestTheDisagreementOfTheTwoSidesIsReported proves that a node that knows
// another outcome than the control端 is reported instead of being hidden.
func TestTheDisagreementOfTheTwoSidesIsReported(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")
	task := env.submit(app, composeConfig("nginx:alpine"))

	transport := &recordingTransport{}
	transport.connect(env.nodeID)
	answering := &answeringTransport{recordingTransport: transport}
	answering.answer = protocol.DeployQueryResult{
		Found: true,
		Receipt: model.Receipt{
			TaskID: task.ID,
			Digest: task.Digest,
			Seq:    3,
			Result: model.Result{State: model.Succeeded, Code: "compose_ready"},
		},
	}
	dispatcher := deploy.NewDispatcher(env.service, answering)
	answering.dispatcher = dispatcher
	if err := dispatcher.Dispatch(context.Background(), task.ID); err != nil {
		t.Fatalf("dispatch the task: %v", err)
	}

	// The control端 heard that the task failed, the node knows that it
	// succeeded.
	failure := model.Receipt{
		TaskID: task.ID,
		Digest: task.Digest,
		Seq:    2,
		Result: model.Result{State: model.Failed, Code: "compose_up_failed"},
	}
	if err := dispatcher.ApplyReceipt(context.Background(), env.nodeID, failure); err != nil {
		t.Fatalf("apply the report: %v", err)
	}

	if _, err := dispatcher.Reconcile(context.Background(), task.ID); !errors.Is(err, deploy.ErrDivergence) {
		t.Fatalf("error = %v, want a disagreement", err)
	}
}

// TestTheOutputIsStoredOnceAndTruncated proves that a fragment that arrives
// twice is stored once and that the output of a task is limited.
func TestTheOutputIsStoredOnceAndTruncated(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")
	task := env.submit(app, composeConfig("nginx:alpine"))

	transport := &recordingTransport{}
	transport.connect(env.nodeID)
	dispatcher := deploy.NewDispatcher(env.service, transport)
	if err := dispatcher.Dispatch(context.Background(), task.ID); err != nil {
		t.Fatalf("dispatch the task: %v", err)
	}

	fragment := protocol.DeployOutput{TaskID: task.ID, Offset: 0, Data: []byte("the output of the task")}
	if err := dispatcher.ApplyOutput(context.Background(), env.nodeID, fragment); err != nil {
		t.Fatalf("store the fragment: %v", err)
	}
	if err := dispatcher.ApplyOutput(context.Background(), env.nodeID, fragment); err != nil {
		t.Fatalf("store the repeated fragment: %v", err)
	}

	output, err := env.service.Output(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("read the output: %v", err)
	}
	if len(output.Fragments) != 1 {
		t.Fatalf("fragments = %d, want 1", len(output.Fragments))
	}
	if string(output.Fragments[0].Data) != "the output of the task" {
		t.Fatalf("the fragment holds %q, want the output of the task", output.Fragments[0].Data)
	}
	if output.Truncated {
		t.Fatal("the output of a short task is truncated")
	}

	// The output beyond the limit of the task is dropped and the task is
	// marked as truncated.
	huge := make([]byte, deploy.MaxTaskOutputBytes+1024)
	for index := range huge {
		huge[index] = 'x'
	}
	if err := dispatcher.ApplyOutput(context.Background(), env.nodeID, protocol.DeployOutput{
		TaskID: task.ID, Offset: 1, Data: huge,
	}); err != nil {
		t.Fatalf("store the huge fragment: %v", err)
	}
	output, err = env.service.Output(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("read the output: %v", err)
	}
	if output.TotalBytes > deploy.MaxTaskOutputBytes {
		t.Fatalf("the output holds %d bytes, want at most %d", output.TotalBytes, deploy.MaxTaskOutputBytes)
	}
	if !output.Truncated {
		t.Fatal("the output of the task is not marked as truncated")
	}
}

// TestTheOutputOfAnotherNodeIsRefused proves that a node may not write the
// output of the task of another node.
func TestTheOutputOfAnotherNodeIsRefused(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")
	task := env.submit(app, composeConfig("nginx:alpine"))

	transport := &recordingTransport{}
	transport.connect(env.nodeID)
	dispatcher := deploy.NewDispatcher(env.service, transport)
	if err := dispatcher.Dispatch(context.Background(), task.ID); err != nil {
		t.Fatalf("dispatch the task: %v", err)
	}

	err := dispatcher.ApplyOutput(context.Background(), "another-node", protocol.DeployOutput{
		TaskID: task.ID, Offset: 0, Data: []byte("the output of another node"),
	})
	if err == nil {
		t.Fatal("the output of another node was accepted")
	}
}

// TestTheRecoveryWaitsForTheNodeThatAnswers proves that a node that comes
// back is asked about the tasks it holds.
func TestTheRecoveryWaitsForTheNodeThatAnswers(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")
	task := env.submit(app, composeConfig("nginx:alpine"))

	transport := &recordingTransport{}
	transport.connect(env.nodeID)
	answering := &answeringTransport{recordingTransport: transport}
	answering.answer = protocol.DeployQueryResult{
		Found: true,
		Receipt: model.Receipt{
			TaskID: task.ID,
			Digest: task.Digest,
			Seq:    2,
			Result: model.Result{State: model.Running, Code: "running"},
		},
	}
	dispatcher := deploy.NewDispatcher(env.service, answering)
	answering.dispatcher = dispatcher
	if err := dispatcher.Dispatch(context.Background(), task.ID); err != nil {
		t.Fatalf("dispatch the task: %v", err)
	}

	if err := dispatcher.RecoverNode(context.Background(), env.nodeID); err != nil {
		t.Fatalf("recover the node: %v", err)
	}
	if answering.questions() != 1 {
		t.Fatalf("questions = %d, want 1", answering.questions())
	}
	current, err := env.service.Task(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("read the task: %v", err)
	}
	if current.State != model.Running {
		t.Fatalf("the task is in the phase %q, want running", current.State)
	}
}

// TestTheQueryThatNobodyAnswersIsReported proves that a node that does not
// answer does not hold the control端 for ever.
func TestTheQueryThatNobodyAnswersIsReported(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")
	task := env.submit(app, composeConfig("nginx:alpine"))

	transport := &recordingTransport{}
	transport.connect(env.nodeID)
	dispatcher := deploy.NewDispatcher(env.service, transport)
	if err := dispatcher.Dispatch(context.Background(), task.ID); err != nil {
		t.Fatalf("dispatch the task: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := dispatcher.Reconcile(ctx, task.ID); err == nil {
		t.Fatal("the reconcile of a silent node reported success")
	}
}
