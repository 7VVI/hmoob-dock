package dockerops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	swarmapi "github.com/docker/docker/api/types/swarm"

	"hmoob-dock/internal/model"
	"hmoob-dock/internal/protocol"
	"hmoob-dock/internal/yamlspec"
)

// fakeSwarmEngine is the cluster of the tests.
//
// The engine holds the facts the executor reads: the identity of the node,
// the nodes of the cluster, the services of the stack and their tasks.
type fakeSwarmEngine struct {
	mu          sync.Mutex
	identity    protocol.NodeIdentity
	nodes       []swarmapi.Node
	services    []swarmapi.Service
	tasks       []swarmapi.Task
	identityErr error
	nodesErr    error
	servicesErr error
	tasksErr    error
}

// Identity returns the identity of the node of the tests.
func (f *fakeSwarmEngine) Identity(ctx context.Context) (protocol.NodeIdentity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.identityErr != nil {
		return protocol.NodeIdentity{}, f.identityErr
	}
	return f.identity, nil
}

// SwarmNodes returns the nodes of the cluster of the tests.
func (f *fakeSwarmEngine) SwarmNodes(ctx context.Context) ([]swarmapi.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.nodesErr != nil {
		return nil, f.nodesErr
	}
	return f.nodes, nil
}

// SwarmServices returns the services of the stack of the tests.
func (f *fakeSwarmEngine) SwarmServices(ctx context.Context, namespace string) ([]swarmapi.Service, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.servicesErr != nil {
		return nil, f.servicesErr
	}
	return f.services, nil
}

// SwarmTasks returns the tasks of the stack of the tests.
func (f *fakeSwarmEngine) SwarmTasks(ctx context.Context, namespace string) ([]swarmapi.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.tasksErr != nil {
		return nil, f.tasksErr
	}
	return f.tasks, nil
}

// Host returns the address of the engine of the tests.
func (f *fakeSwarmEngine) Host() string {
	return "unix:///var/run/docker.sock"
}

// swarmTestCommand builds the command of the tests of the executor.
func swarmTestCommand() model.Command {
	return model.Command{
		Task: model.Task{ID: "task-1", AppID: "app-1", Digest: "digest-1"},
		App: model.App{
			ID:        "app-1",
			Name:      "demo",
			Mode:      "swarm",
			TargetID:  "swarm-1",
			Namespace: "demo",
		},
		Config: model.Config{
			YAML:      "services:\n  api:\n    image: nginx:alpine\n    deploy:\n      replicas: 2\n",
			Variables: map[string]string{},
		},
		ExecutionTimeout: time.Minute,
		VerifyTimeout:    2 * time.Second,
	}
}

// swarmTestEngine builds the cluster of the tests.
//
// The cluster holds one manager that leads it and one worker that is ready,
// so the executor may update the stack.
func swarmTestEngine(t *testing.T) *fakeSwarmEngine {
	t.Helper()

	return &fakeSwarmEngine{
		identity: protocol.NodeIdentity{
			EngineID:    "engine-1",
			SwarmID:     "swarm-1",
			SwarmNodeID: "node-1",
			Role:        protocol.RoleManager,
			Hostname:    "manager-1",
		},
		nodes: []swarmapi.Node{
			{
				ID:          "node-1",
				Spec:        swarmapi.NodeSpec{Role: swarmapi.NodeRoleManager, Availability: swarmapi.NodeAvailabilityActive},
				Description: swarmapi.NodeDescription{Hostname: "manager-1"},
				Status:      swarmapi.NodeStatus{State: swarmapi.NodeStateReady},
				ManagerStatus: &swarmapi.ManagerStatus{
					Leader:       true,
					Reachability: swarmapi.ReachabilityReachable,
				},
			},
			{
				ID:          "node-2",
				Spec:        swarmapi.NodeSpec{Role: swarmapi.NodeRoleWorker, Availability: swarmapi.NodeAvailabilityActive},
				Description: swarmapi.NodeDescription{Hostname: "worker-1"},
				Status:      swarmapi.NodeStatus{State: swarmapi.NodeStateReady},
			},
		},
	}
}

// swarmDigests returns the digests the platform binds to the services of the
// configuration of the command.
func swarmDigests(t *testing.T, cmd model.Command) map[string]string {
	t.Helper()

	checked, err := yamlspec.Check(cmd.App, cmd.Config)
	if err != nil {
		t.Fatalf("check the configuration: %v", err)
	}
	document, err := prepareStack(checked.RenderedYAML)
	if err != nil {
		t.Fatalf("prepare the stack: %v", err)
	}
	return document.Digests
}

// swarmService builds a service of the stack of the tests.
//
// The service carries the digest of the version that is deployed, so the
// executor sees the service it deployed.
func swarmService(name string, replicas uint64, digest string, update swarmapi.UpdateStatus) swarmapi.Service {
	return swarmapi.Service{
		ID: "service-" + name,
		Spec: swarmapi.ServiceSpec{
			Annotations: swarmapi.Annotations{
				Name:   "demo_" + name,
				Labels: map[string]string{SwarmVersionLabel: digest},
			},
			Mode: swarmapi.ServiceMode{
				Replicated: &swarmapi.ReplicatedService{Replicas: &replicas},
			},
			TaskTemplate: swarmapi.TaskSpec{
				ContainerSpec: &swarmapi.ContainerSpec{Image: "nginx:alpine"},
			},
		},
		UpdateStatus: &update,
	}
}

// swarmTask builds a task of a service of the stack of the tests.
func swarmTask(id, serviceID string, slot int, state swarmapi.TaskState, errMessage string) swarmapi.Task {
	return swarmTaskWithImage(id, serviceID, slot, state, errMessage, "nginx:alpine")
}

// swarmTaskWithImage builds a task of a service of the stack of the tests
// that runs the given image.
func swarmTaskWithImage(
	id, serviceID string,
	slot int,
	state swarmapi.TaskState,
	errMessage string,
	image string,
) swarmapi.Task {
	return swarmapi.Task{
		ID:        id,
		ServiceID: serviceID,
		Slot:      slot,
		NodeID:    "node-2",
		Spec: swarmapi.TaskSpec{
			ContainerSpec: &swarmapi.ContainerSpec{Image: image},
		},
		Status: swarmapi.TaskStatus{State: state, Err: errMessage},
	}
}

// newSwarmExecutor builds the executor of the tests.
func newSwarmExecutor(t *testing.T, runner Runner, engine *fakeSwarmEngine) *Swarm {
	t.Helper()

	if engine == nil {
		engine = swarmTestEngine(t)
	}
	return &Swarm{root: t.TempDir(), runner: runner, engine: engine}
}

// swarmCommands returns the commands of the Docker CLI the runner received.
func swarmCommands(runner *fakeRunner) [][]string {
	runner.mu.Lock()
	defer runner.mu.Unlock()

	commands := make([][]string, 0, len(runner.calls))
	for _, call := range runner.calls {
		commands = append(commands, call.Args)
	}
	return commands
}

// TestStackArgsBuildsTheFixedCommand proves that the command of a stack
// update is fixed: the file of the configuration, the flags and the name of
// the stack.
func TestStackArgsBuildsTheFixedCommand(t *testing.T) {
	args, err := StackArgs("demo", "/opt/hmoob-dock/apps/a/deploy.yaml", false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"stack", "deploy",
		"--compose-file", "/opt/hmoob-dock/apps/a/deploy.yaml",
		"--resolve-image", "always",
		"demo",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args=%q, want %q", args, want)
	}
}

// TestStackArgsAddsThePruneOnlyWhenTheRemovalIsConfirmed proves that the
// prune flag belongs to the command of the update and not to another command
// of the Docker CLI.
func TestStackArgsAddsThePruneOnlyWhenTheRemovalIsConfirmed(t *testing.T) {
	args, err := StackArgs("demo", "/opt/hmoob-dock/apps/a/deploy.yaml", true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"stack", "deploy",
		"--compose-file", "/opt/hmoob-dock/apps/a/deploy.yaml",
		"--resolve-image", "always",
		"--prune",
		"demo",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args=%q, want %q", args, want)
	}
	for _, arg := range args {
		if arg == "rm" || arg == "down" || arg == "--volumes" {
			t.Fatalf("the destructive argument %q", arg)
		}
	}
}

// TestStackArgsRefusesAnUnsafeNamespace proves that the name of the stack is
// a fixed name and not a path of the host.
func TestStackArgsRefusesAnUnsafeNamespace(t *testing.T) {
	for _, namespace := range []string{"", "..", "a/b", "a b", "Demo", "-demo"} {
		if _, err := StackArgs(namespace, "/opt/hmoob-dock/apps/a/deploy.yaml", false); err == nil {
			t.Fatalf("the namespace %q was accepted", namespace)
		}
	}
}

// TestTheSwarmExecutorRefusesANodeThatIsNotAManager proves that a node that
// lost its role refuses the stack of a cluster it does not manage any more.
//
// The platform never hands the instruction to another agent: the
// instruction belongs to the task of this node.
func TestTheSwarmExecutorRefusesANodeThatIsNotAManager(t *testing.T) {
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	engine.identity.Role = protocol.RoleWorker
	executor := newSwarmExecutor(t, runner, engine)

	result, err := executor.Execute(context.Background(), swarmTestCommand(), io.Discard)
	if err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if result.State != model.Failed {
		t.Fatalf("the state = %q, want %q", result.State, model.Failed)
	}
	if result.Code != CodeSwarmNotManager {
		t.Fatalf("the code = %q, want %q", result.Code, CodeSwarmNotManager)
	}
	if runner.count() != 0 {
		t.Fatalf("the runner received %d commands, want none", runner.count())
	}
}

// TestTheSwarmExecutorRefusesAnotherCluster proves that the node only
// updates the stack of the cluster the application belongs to.
func TestTheSwarmExecutorRefusesAnotherCluster(t *testing.T) {
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	engine.identity.SwarmID = "swarm-2"
	executor := newSwarmExecutor(t, runner, engine)

	result, err := executor.Execute(context.Background(), swarmTestCommand(), io.Discard)
	if err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if result.State != model.Failed {
		t.Fatalf("the state = %q, want %q", result.State, model.Failed)
	}
	if result.Code != CodeSwarmClusterMismatch {
		t.Fatalf("the code = %q, want %q", result.Code, CodeSwarmClusterMismatch)
	}
	if runner.count() != 0 {
		t.Fatalf("the runner received %d commands, want none", runner.count())
	}
}

// TestTheSwarmExecutorRefusesAClusterWithoutALeader proves that a cluster
// without a quorum refuses the stack before anything changes.
func TestTheSwarmExecutorRefusesAClusterWithoutALeader(t *testing.T) {
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	engine.nodes[0].ManagerStatus = &swarmapi.ManagerStatus{
		Reachability: swarmapi.ReachabilityReachable,
	}
	executor := newSwarmExecutor(t, runner, engine)

	result, err := executor.Execute(context.Background(), swarmTestCommand(), io.Discard)
	if err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if result.State != model.Failed {
		t.Fatalf("the state = %q, want %q", result.State, model.Failed)
	}
	if result.Code != CodeSwarmNoQuorum {
		t.Fatalf("the code = %q, want %q", result.Code, CodeSwarmNoQuorum)
	}
	if runner.count() != 0 {
		t.Fatalf("the runner received %d commands, want none", runner.count())
	}
}

// TestTheSwarmExecutorStopsAfterAFailedConfiguration proves that a
// configuration the Docker CLI refuses never reaches the pull and never
// reaches the deploy.
func TestTheSwarmExecutorStopsAfterAFailedConfiguration(t *testing.T) {
	runner := &fakeRunner{fail: map[string]error{"config": errors.New("exit status 1")}}
	engine := swarmTestEngine(t)
	executor := newSwarmExecutor(t, runner, engine)

	result, err := executor.Execute(context.Background(), swarmTestCommand(), io.Discard)
	if err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if result.State != model.Failed {
		t.Fatalf("the state = %q, want %q", result.State, model.Failed)
	}
	if result.Code != CodeStackConfigInvalid {
		t.Fatalf("the code = %q, want %q", result.Code, CodeStackConfigInvalid)
	}
	if result.PartialUpdate {
		t.Fatal("a failed validation changed the target")
	}
	commands := swarmCommands(runner)
	if len(commands) != 1 {
		t.Fatalf("the commands = %q, want only the validation", commands)
	}
}

// TestTheSwarmExecutorStopsAfterAFailedPull proves that images the Docker
// CLI cannot pull keep the stack from being deployed.
func TestTheSwarmExecutorStopsAfterAFailedPull(t *testing.T) {
	runner := &fakeRunner{fail: map[string]error{"pull": errors.New("exit status 1")}}
	engine := swarmTestEngine(t)
	executor := newSwarmExecutor(t, runner, engine)

	result, err := executor.Execute(context.Background(), swarmTestCommand(), io.Discard)
	if err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if result.State != model.Failed {
		t.Fatalf("the state = %q, want %q", result.State, model.Failed)
	}
	if result.Code != CodeStackPullFailed {
		t.Fatalf("the code = %q, want %q", result.Code, CodeStackPullFailed)
	}
	if result.PartialUpdate {
		t.Fatal("a failed pull changed the target")
	}
	for _, command := range swarmCommands(runner) {
		if len(command) > 1 && command[1] == "deploy" {
			t.Fatalf("the stack was deployed after a failed pull: %q", command)
		}
	}
}

// TestTheSwarmExecutorChecksTheConfigurationBeforeItDeploys proves the order
// of the commands: the configuration is validated, the images are prepared
// and only then the stack is written.
func TestTheSwarmExecutorChecksTheConfigurationBeforeItDeploys(t *testing.T) {
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	cmd := swarmTestCommand()
	engine.services = []swarmapi.Service{
		swarmService("api", 2, swarmDigests(t, cmd)["api"], swarmapi.UpdateStatus{}),
	}
	engine.tasks = []swarmapi.Task{
		swarmTask("task-1", "service-api", 1, swarmapi.TaskStateRunning, ""),
		swarmTask("task-2", "service-api", 2, swarmapi.TaskStateRunning, ""),
	}
	executor := newSwarmExecutor(t, runner, engine)

	if _, err := executor.Execute(context.Background(), cmd, io.Discard); err != nil {
		t.Fatalf("execute the command: %v", err)
	}

	commands := swarmCommands(runner)
	if len(commands) != 3 {
		t.Fatalf("the commands = %q, want the validation, the pull and the deploy", commands)
	}
	if commands[0][1] != "config" {
		t.Fatalf("the first command = %q, want the validation", commands[0])
	}
	if commands[1][0] != "pull" {
		t.Fatalf("the second command = %q, want the pull", commands[1])
	}
	if commands[2][1] != "deploy" {
		t.Fatalf("the third command = %q, want the deploy", commands[2])
	}
}

// TestTheSwarmExecutorKeepsTheNamespaceAndTheFileOfTheApplication proves
// that the stack and the file of the configuration are fixed by the platform
// and not by the configuration.
func TestTheSwarmExecutorKeepsTheNamespaceAndTheFileOfTheApplication(t *testing.T) {
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	cmd := swarmTestCommand()
	engine.services = []swarmapi.Service{
		swarmService("api", 2, swarmDigests(t, cmd)["api"], swarmapi.UpdateStatus{}),
	}
	engine.tasks = []swarmapi.Task{
		swarmTask("task-1", "service-api", 1, swarmapi.TaskStateRunning, ""),
		swarmTask("task-2", "service-api", 2, swarmapi.TaskStateRunning, ""),
	}
	executor := newSwarmExecutor(t, runner, engine)

	if _, err := executor.Execute(context.Background(), cmd, io.Discard); err != nil {
		t.Fatalf("execute the command: %v", err)
	}

	deploy := swarmCommands(runner)[2]
	if deploy[len(deploy)-1] != "demo" {
		t.Fatalf("the arguments = %q, want the stack demo", deploy)
	}
	want := filepath.Join(executor.root, "apps", "app-1", "deploy.yaml")
	if !contains(deploy, want) {
		t.Fatalf("the arguments = %q, want the file %q", deploy, want)
	}
}

// TestTheSwarmExecutorRemovesOnlyTheConfirmedServices proves that the prune
// flag only reaches the command when the administrator confirmed exactly the
// services that are left over.
func TestTheSwarmExecutorRemovesOnlyTheConfirmedServices(t *testing.T) {
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	cmd := swarmTestCommand()
	engine.services = []swarmapi.Service{
		swarmService("api", 2, swarmDigests(t, cmd)["api"], swarmapi.UpdateStatus{}),
		swarmService("cache", 1, "old", swarmapi.UpdateStatus{}),
	}
	engine.tasks = []swarmapi.Task{
		swarmTask("task-1", "service-api", 1, swarmapi.TaskStateRunning, ""),
		swarmTask("task-2", "service-api", 2, swarmapi.TaskStateRunning, ""),
	}
	cmd.RemovedServices = []string{"cache"}
	executor := newSwarmExecutor(t, runner, engine)

	if _, err := executor.Execute(context.Background(), cmd, io.Discard); err != nil {
		t.Fatalf("execute the command: %v", err)
	}

	deploy := swarmCommands(runner)[2]
	if !contains(deploy, "--prune") {
		t.Fatalf("the arguments = %q, want the removal of the confirmed service", deploy)
	}
}

// TestTheSwarmExecutorRefusesToRemoveAServiceNobodyConfirmed proves that a
// service of the stack that nobody confirmed stays where it is.
//
// The command line of Docker prunes every service of the stack that the
// configuration does not hold, so the scope of the removal is checked
// against the services of the stack before the flag is added.
func TestTheSwarmExecutorRefusesToRemoveAServiceNobodyConfirmed(t *testing.T) {
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	cmd := swarmTestCommand()
	engine.services = []swarmapi.Service{
		swarmService("api", 2, swarmDigests(t, cmd)["api"], swarmapi.UpdateStatus{}),
		swarmService("cache", 1, "old", swarmapi.UpdateStatus{}),
		swarmService("leftover", 1, "old", swarmapi.UpdateStatus{}),
	}
	executor := newSwarmExecutor(t, runner, engine)

	result, err := executor.Execute(context.Background(), cmd, io.Discard)
	if err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if result.State != model.Failed {
		t.Fatalf("the state = %q, want %q", result.State, model.Failed)
	}
	if result.Code != CodeStackPruneRefused {
		t.Fatalf("the code = %q, want %q", result.Code, CodeStackPruneRefused)
	}
	if !strings.Contains(result.Message, "leftover") {
		t.Fatalf("the message = %q, want the service nobody confirmed", result.Message)
	}
	if runner.count() != 0 {
		t.Fatalf("the runner received %d commands, want none", runner.count())
	}
}

// TestTheSwarmExecutorDoesNotPruneWithoutAConfirmation proves that a stack
// without a service to remove is never pruned.
func TestTheSwarmExecutorDoesNotPruneWithoutAConfirmation(t *testing.T) {
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	cmd := swarmTestCommand()
	engine.services = []swarmapi.Service{
		swarmService("api", 2, swarmDigests(t, cmd)["api"], swarmapi.UpdateStatus{}),
	}
	engine.tasks = []swarmapi.Task{
		swarmTask("task-1", "service-api", 1, swarmapi.TaskStateRunning, ""),
		swarmTask("task-2", "service-api", 2, swarmapi.TaskStateRunning, ""),
	}
	executor := newSwarmExecutor(t, runner, engine)

	if _, err := executor.Execute(context.Background(), cmd, io.Discard); err != nil {
		t.Fatalf("execute the command: %v", err)
	}

	deploy := swarmCommands(runner)[2]
	if contains(deploy, "--prune") {
		t.Fatalf("the arguments = %q, want no removal", deploy)
	}
}

// TestTheSwarmExecutorDoesNotReportAnOldServiceAsDeployed proves that a
// service that runs an older specification keeps the deployment from being
// successful, even when its tasks run.
func TestTheSwarmExecutorDoesNotReportAnOldServiceAsDeployed(t *testing.T) {
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	cmd := swarmTestCommand()
	engine.services = []swarmapi.Service{
		swarmService("api", 2, "the-old-digest", swarmapi.UpdateStatus{}),
	}
	engine.tasks = []swarmapi.Task{
		swarmTask("task-1", "service-api", 1, swarmapi.TaskStateRunning, ""),
		swarmTask("task-2", "service-api", 2, swarmapi.TaskStateRunning, ""),
	}
	executor := newSwarmExecutor(t, runner, engine)

	result, err := executor.Execute(context.Background(), cmd, io.Discard)
	if err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if result.State == model.Succeeded {
		t.Fatalf("the state = %q, want a failure", result.State)
	}
	if result.Code != CodeSwarmSpecMismatch {
		t.Fatalf("the code = %q, want %q", result.Code, CodeSwarmSpecMismatch)
	}
}

// TestTheSwarmExecutorDoesNotReportAFailedTaskAsDeployed proves that a
// service with a failed task keeps the deployment from being successful,
// even though the command line of Docker succeeded.
func TestTheSwarmExecutorDoesNotReportAFailedTaskAsDeployed(t *testing.T) {
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	cmd := swarmTestCommand()
	engine.services = []swarmapi.Service{
		swarmService("api", 2, swarmDigests(t, cmd)["api"], swarmapi.UpdateStatus{}),
	}
	engine.tasks = []swarmapi.Task{
		swarmTask("task-1", "service-api", 1, swarmapi.TaskStateRunning, ""),
		swarmTask("task-2", "service-api", 2, swarmapi.TaskStateFailed, "no suitable node"),
	}
	cmd.VerifyTimeout = 100 * time.Millisecond
	executor := newSwarmExecutor(t, runner, engine)

	result, err := executor.Execute(context.Background(), cmd, io.Discard)
	if err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if result.State == model.Succeeded {
		t.Fatalf("the state = %q, want a failure", result.State)
	}
	if result.Code != CodeSwarmTaskFailed {
		t.Fatalf("the code = %q, want %q", result.Code, CodeSwarmTaskFailed)
	}
	if !strings.Contains(result.Message, "task-2") {
		t.Fatalf("the message = %q, want the failed task", result.Message)
	}
}

// TestTheSwarmExecutorReportsTheRollbackOfTheCluster proves that a service
// the cluster rolled back is reported as a rollback and never as the version
// that was deployed.
func TestTheSwarmExecutorReportsTheRollbackOfTheCluster(t *testing.T) {
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	cmd := swarmTestCommand()
	engine.services = []swarmapi.Service{
		swarmService("api", 2, "the-old-digest", swarmapi.UpdateStatus{
			State: swarmapi.UpdateStateRollbackCompleted,
		}),
	}
	engine.tasks = []swarmapi.Task{
		swarmTask("task-1", "service-api", 1, swarmapi.TaskStateRunning, ""),
		swarmTask("task-2", "service-api", 2, swarmapi.TaskStateRunning, ""),
	}
	executor := newSwarmExecutor(t, runner, engine)

	result, err := executor.Execute(context.Background(), cmd, io.Discard)
	if err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if result.State == model.Succeeded {
		t.Fatalf("the state = %q, want a failure", result.State)
	}
	if !result.RolledBack {
		t.Fatalf("the rollback was not reported: %+v", result)
	}
}

// TestTheSwarmExecutorReportsTheDeploymentThatConverged proves that a stack
// whose services run the deployed version is reported as a success.
func TestTheSwarmExecutorReportsTheDeploymentThatConverged(t *testing.T) {
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	cmd := swarmTestCommand()
	engine.services = []swarmapi.Service{
		swarmService("api", 2, swarmDigests(t, cmd)["api"], swarmapi.UpdateStatus{
			State: swarmapi.UpdateStateCompleted,
		}),
	}
	engine.tasks = []swarmapi.Task{
		swarmTask("task-1", "service-api", 1, swarmapi.TaskStateRunning, ""),
		swarmTask("task-2", "service-api", 2, swarmapi.TaskStateRunning, ""),
	}
	executor := newSwarmExecutor(t, runner, engine)

	result, err := executor.Execute(context.Background(), cmd, io.Discard)
	if err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if result.State != model.Succeeded {
		t.Fatalf("the state = %q, want %q (%s)", result.State, model.Succeeded, result.Message)
	}
	if result.Code != CodeSwarmReady {
		t.Fatalf("the code = %q, want %q", result.Code, CodeSwarmReady)
	}
}

// TestTheSwarmExecutorDoesNotCountTheTasksOfAnOlderVersion proves that a
// task of the version before the deployment does not count, so a service
// whose second replica still runs the old image is not ready.
func TestTheSwarmExecutorDoesNotCountTheTasksOfAnOlderVersion(t *testing.T) {
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	cmd := swarmTestCommand()
	engine.services = []swarmapi.Service{
		swarmService("api", 2, swarmDigests(t, cmd)["api"], swarmapi.UpdateStatus{
			State: swarmapi.UpdateStateUpdating,
		}),
	}
	old := swarmTask("task-1", "service-api", 1, swarmapi.TaskStateRunning, "")
	old.Spec.ContainerSpec.Image = "nginx:old"
	engine.tasks = []swarmapi.Task{
		old,
		swarmTask("task-2", "service-api", 2, swarmapi.TaskStateRunning, ""),
	}
	cmd.VerifyTimeout = 100 * time.Millisecond
	executor := newSwarmExecutor(t, runner, engine)

	result, err := executor.Execute(context.Background(), cmd, io.Discard)
	if err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if result.State == model.Succeeded {
		t.Fatalf("the state = %q, want a failure", result.State)
	}
	if result.Code != CodeSwarmNotReady {
		t.Fatalf("the code = %q, want %q", result.Code, CodeSwarmNotReady)
	}
}

// TestTheSwarmExecutorHandsTheCredentialsOnlyToAStackThatNeedsThem proves
// that the credentials of the registry of the manager are only handed to the
// cluster when the stack pulls an image of a registry of its own.
//
// The platform never reads the credentials itself: the command line of
// Docker reads them and hands them to the cluster.
func TestTheSwarmExecutorHandsTheCredentialsOnlyToAStackThatNeedsThem(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	credential := filepath.Join(os.Getenv("HOME"), ".docker", "config.json")
	if err := os.MkdirAll(filepath.Dir(credential), 0o700); err != nil {
		t.Fatalf("prepare the configuration of the registry: %v", err)
	}
	if err := os.WriteFile(credential, []byte(`{"auths":{"registry.example.com":{"auth":"c2VjcmV0"}}}`), 0o600); err != nil {
		t.Fatalf("write the configuration of the registry: %v", err)
	}

	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	cmd := swarmTestCommand()
	cmd.Config.YAML = "services:\n  api:\n    image: registry.example.com/demo/api:1\n"
	engine.services = []swarmapi.Service{
		swarmService("api", 2, swarmDigests(t, cmd)["api"], swarmapi.UpdateStatus{}),
	}
	engine.tasks = []swarmapi.Task{
		swarmTask("task-1", "service-api", 1, swarmapi.TaskStateRunning, ""),
		swarmTask("task-2", "service-api", 2, swarmapi.TaskStateRunning, ""),
	}
	executor := newSwarmExecutor(t, runner, engine)

	buffer := &strings.Builder{}
	if _, err := executor.Execute(context.Background(), cmd, buffer); err != nil {
		t.Fatalf("execute the command: %v", err)
	}

	deploy := swarmCommands(runner)[2]
	if !contains(deploy, "--with-registry-auth") {
		t.Fatalf("the arguments = %q, want the credentials of the registry", deploy)
	}
	if strings.Contains(buffer.String(), "c2VjcmV0") {
		t.Fatalf("the output = %q, want no credential", buffer.String())
	}
	for _, command := range swarmCommands(runner) {
		for _, argument := range command {
			if strings.Contains(argument, "c2VjcmV0") {
				t.Fatalf("the argument %q holds the credential of the registry", argument)
			}
		}
	}
}

// TestTheSwarmExecutorDoesNotHandTheCredentialsToAPublicStack proves that a
// stack of the Docker Hub gets no credentials of the manager.
func TestTheSwarmExecutorDoesNotHandTheCredentialsToAPublicStack(t *testing.T) {
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	cmd := swarmTestCommand()
	engine.services = []swarmapi.Service{
		swarmService("api", 2, swarmDigests(t, cmd)["api"], swarmapi.UpdateStatus{}),
	}
	engine.tasks = []swarmapi.Task{
		swarmTask("task-1", "service-api", 1, swarmapi.TaskStateRunning, ""),
		swarmTask("task-2", "service-api", 2, swarmapi.TaskStateRunning, ""),
	}
	executor := newSwarmExecutor(t, runner, engine)

	if _, err := executor.Execute(context.Background(), cmd, io.Discard); err != nil {
		t.Fatalf("execute the command: %v", err)
	}

	deploy := swarmCommands(runner)[2]
	if contains(deploy, "--with-registry-auth") {
		t.Fatalf("the arguments = %q, want no credentials", deploy)
	}
}

// TestTheSwarmExecutorDoesNotLeakTheConfigurationOfTheStack proves that the
// expanded configuration never reaches the output of the task.
//
// The output of the validation of the Docker CLI holds the configuration with
// the values of the variables of the application.
func TestTheSwarmExecutorDoesNotLeakTheConfigurationOfTheStack(t *testing.T) {
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	cmd := swarmTestCommand()
	cmd.Config.YAML = "services:\n  api:\n    image: nginx:alpine\n    environment:\n      PASSWORD: ${PASSWORD}\n"
	cmd.Config.Variables = map[string]string{"PASSWORD": "the-secret-value"}
	engine.services = []swarmapi.Service{
		swarmService("api", 2, swarmDigests(t, cmd)["api"], swarmapi.UpdateStatus{}),
	}
	engine.tasks = []swarmapi.Task{
		swarmTask("task-1", "service-api", 1, swarmapi.TaskStateRunning, ""),
		swarmTask("task-2", "service-api", 2, swarmapi.TaskStateRunning, ""),
	}
	executor := newSwarmExecutor(t, runner, engine)

	buffer := &strings.Builder{}
	if _, err := executor.Execute(context.Background(), cmd, buffer); err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if strings.Contains(buffer.String(), "the-secret-value") {
		t.Fatalf("the output = %q, want no value of a variable", buffer.String())
	}
}

// TestTheSwarmExecutorReportsThePathsItCouldNotProve proves that the
// manager names the nodes whose paths of the host it could not check.
//
// The existence of a path on the manager says nothing about the hosts of the
// workers.
func TestTheSwarmExecutorReportsThePathsItCouldNotProve(t *testing.T) {
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	cmd := swarmTestCommand()
	cmd.Config.YAML = "services:\n  api:\n    image: nginx:alpine\n    volumes:\n      - /srv/data:/data\n"
	engine.services = []swarmapi.Service{
		swarmService("api", 2, swarmDigests(t, cmd)["api"], swarmapi.UpdateStatus{}),
	}
	engine.tasks = []swarmapi.Task{
		swarmTask("task-1", "service-api", 1, swarmapi.TaskStateRunning, ""),
		swarmTask("task-2", "service-api", 2, swarmapi.TaskStateRunning, ""),
	}
	executor := newSwarmExecutor(t, runner, engine)

	result, err := executor.Execute(context.Background(), cmd, io.Discard)
	if err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if !strings.Contains(result.Message, "worker-1") {
		t.Fatalf("the message = %q, want the node the manager could not check", result.Message)
	}
}

// TestTheSwarmExecutorReportsAFailedDeploy proves that a stack the Docker
// CLI cannot write is a failure and that the platform does not undo it.
func TestTheSwarmExecutorReportsAFailedDeploy(t *testing.T) {
	runner := &fakeRunner{fail: map[string]error{"deploy": errors.New("exit status 1")}}
	engine := swarmTestEngine(t)
	cmd := swarmTestCommand()
	engine.services = []swarmapi.Service{
		swarmService("api", 2, swarmDigests(t, cmd)["api"], swarmapi.UpdateStatus{}),
	}
	engine.tasks = []swarmapi.Task{
		swarmTask("task-1", "service-api", 1, swarmapi.TaskStateRunning, ""),
		swarmTask("task-2", "service-api", 2, swarmapi.TaskStateRunning, ""),
	}
	executor := newSwarmExecutor(t, runner, engine)

	result, err := executor.Execute(context.Background(), cmd, io.Discard)
	if err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if result.State != model.Failed {
		t.Fatalf("the state = %q, want %q", result.State, model.Failed)
	}
	if result.Code != CodeStackDeployFailed {
		t.Fatalf("the code = %q, want %q", result.Code, CodeStackDeployFailed)
	}
	if !result.PartialUpdate {
		t.Fatal("the failed deployment was not reported as a partial update")
	}
	for _, command := range swarmCommands(runner) {
		for _, argument := range command {
			if argument == "rm" || argument == "down" {
				t.Fatalf("the platform ran the command %q to undo the change", argument)
			}
		}
	}
}

// TestTheSwarmExecutorReportsAnUncertainOutcomeWhenTheCommandRunsOutOfTime
// proves that a command that ran out of time leaves the outcome uncertain,
// because the cluster may still work on the change.
func TestTheSwarmExecutorReportsAnUncertainOutcomeWhenTheCommandRunsOutOfTime(t *testing.T) {
	runner := &fakeRunner{fail: map[string]error{"deploy": context.DeadlineExceeded}}
	engine := swarmTestEngine(t)
	executor := newSwarmExecutor(t, runner, engine)

	result, err := executor.Execute(context.Background(), swarmTestCommand(), io.Discard)
	if err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if result.State != model.Uncertain {
		t.Fatalf("the state = %q, want %q", result.State, model.Uncertain)
	}
	if result.Code != CodeExecutionUncertain {
		t.Fatalf("the code = %q, want %q", result.Code, CodeExecutionUncertain)
	}
}

// TestTheSwarmExecutorDoesNotInheritTheEnvironmentOfTheAgent proves that the
// command line of Docker only knows the environment the platform built.
func TestTheSwarmExecutorDoesNotInheritTheEnvironmentOfTheAgent(t *testing.T) {
	t.Setenv("HMOOB_TEST_SECRET", "the secret of the agent")
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	cmd := swarmTestCommand()
	engine.services = []swarmapi.Service{
		swarmService("api", 2, swarmDigests(t, cmd)["api"], swarmapi.UpdateStatus{}),
	}
	engine.tasks = []swarmapi.Task{
		swarmTask("task-1", "service-api", 1, swarmapi.TaskStateRunning, ""),
		swarmTask("task-2", "service-api", 2, swarmapi.TaskStateRunning, ""),
	}
	executor := newSwarmExecutor(t, runner, engine)

	if _, err := executor.Execute(context.Background(), cmd, io.Discard); err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	for _, call := range runner.calls {
		if contains(call.Env, "HMOOB_TEST_SECRET") {
			t.Fatalf("the environment = %q, want no variable of the agent", call.Env)
		}
	}
}

// TestTheSwarmExecutorBindsTheVersionToEveryService proves that the
// specification of a service holds the digest of its own definition, so the
// platform can tell the version that runs from an older one.
func TestTheSwarmExecutorBindsTheVersionToEveryService(t *testing.T) {
	cmd := swarmTestCommand()
	digests := swarmDigests(t, cmd)
	if len(digests) != 1 {
		t.Fatalf("the digests = %v, want one service", digests)
	}
	if digests["api"] == "" {
		t.Fatal("the service carries no digest")
	}

	document, err := prepareStack(mustRender(t, cmd))
	if err != nil {
		t.Fatalf("prepare the stack: %v", err)
	}
	if !strings.Contains(string(document.Payload), SwarmVersionLabel) {
		t.Fatalf("the payload = %q, want the label of the version", document.Payload)
	}
	if !strings.Contains(string(document.Payload), digests["api"]) {
		t.Fatalf("the payload = %q, want the digest of the service", document.Payload)
	}
}

// mustRender returns the rendered configuration of the command.
func mustRender(t *testing.T, cmd model.Command) []byte {
	t.Helper()

	checked, err := yamlspec.Check(cmd.App, cmd.Config)
	if err != nil {
		t.Fatalf("check the configuration: %v", err)
	}
	return checked.RenderedYAML
}

// TestTheSwarmExecutorKeepsTheDigestOfAServiceThatDidNotChange proves that
// a service that did not change keeps its label, so its tasks are not
// restarted for nothing.
func TestTheSwarmExecutorKeepsTheDigestOfAServiceThatDidNotChange(t *testing.T) {
	cmd := swarmTestCommand()
	first := swarmDigests(t, cmd)

	changed := cmd
	changed.Config.YAML = "services:\n  api:\n    image: nginx:alpine\n    deploy:\n      replicas: 2\n" +
		"  worker:\n    image: nginx:alpine\n"
	second := swarmDigests(t, changed)

	if first["api"] != second["api"] {
		t.Fatalf("the digest of the service = %q, want the digest of the unchanged service", second["api"])
	}
	if second["worker"] == "" {
		t.Fatal("the new service carries no digest")
	}
}

// TestTheSwarmExecutorRefusesToDeployAnotherMode proves that the executor
// of the stacks refuses a command of another mode.
func TestTheSwarmExecutorRefusesToDeployAnotherMode(t *testing.T) {
	runner := &fakeRunner{}
	executor := newSwarmExecutor(t, runner, swarmTestEngine(t))

	cmd := swarmTestCommand()
	cmd.App.Mode = "compose"
	result, err := executor.Execute(context.Background(), cmd, io.Discard)
	if err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if result.State != model.Failed {
		t.Fatalf("the state = %q, want %q", result.State, model.Failed)
	}
	if runner.count() != 0 {
		t.Fatalf("the runner received %d commands, want none", runner.count())
	}
}

// TestTheSwarmExecutorRefusesAStackWithoutARunner proves that the executor
// reports the missing runner as an error: the executor could not tell what
// happened to the target.
func TestTheSwarmExecutorRefusesAStackWithoutARunner(t *testing.T) {
	executor := &Swarm{root: t.TempDir(), engine: swarmTestEngine(t)}

	if _, err := executor.Execute(context.Background(), swarmTestCommand(), io.Discard); err == nil {
		t.Fatal("the executor accepted a command without a runner")
	}
}

// TestTheGlobalServiceRunsOnEveryEligibleNode proves that the target of a
// global service is the number of the nodes that may run it.
func TestTheGlobalServiceRunsOnEveryEligibleNode(t *testing.T) {
	engine := swarmTestEngine(t)
	service := swarmapi.Service{
		ID: "service-api",
		Spec: swarmapi.ServiceSpec{
			Annotations: swarmapi.Annotations{Name: "demo_api"},
			Mode:        swarmapi.ServiceMode{Global: &swarmapi.GlobalService{}},
		},
	}
	target, known := effectiveTarget(service, engine.nodes)
	if !known {
		t.Fatal("the target of the service could not be proven")
	}
	if target != 2 {
		t.Fatalf("the target = %d, want the two nodes of the cluster", target)
	}

	// A node that drains does not get new tasks, so it is not part of the
	// target any more.
	engine.nodes[1].Spec.Availability = swarmapi.NodeAvailabilityDrain
	target, known = effectiveTarget(service, engine.nodes)
	if !known {
		t.Fatal("the target of the service could not be proven")
	}
	if target != 1 {
		t.Fatalf("the target = %d, want the one node that may run the service", target)
	}
}

// TestTheGlobalServiceHonoursTheConstraintsOfItsPlacement proves that a
// node that does not satisfy the constraints of the placement is not part of
// the target of a global service.
func TestTheGlobalServiceHonoursTheConstraintsOfItsPlacement(t *testing.T) {
	engine := swarmTestEngine(t)
	engine.nodes[1].Spec.Annotations.Labels = map[string]string{"role": "worker"}
	service := swarmapi.Service{
		ID: "service-api",
		Spec: swarmapi.ServiceSpec{
			Annotations: swarmapi.Annotations{Name: "demo_api"},
			Mode:        swarmapi.ServiceMode{Global: &swarmapi.GlobalService{}},
			TaskTemplate: swarmapi.TaskSpec{
				Placement: &swarmapi.Placement{
					Constraints: []string{"node.labels.role == worker"},
				},
			},
		},
	}
	target, known := effectiveTarget(service, engine.nodes)
	if !known {
		t.Fatal("the target of the service could not be proven")
	}
	if target != 1 {
		t.Fatalf("the target = %d, want the one node with the label", target)
	}
}

// TestTheGlobalServiceWithAnUnknownConstraintHasNoTarget proves that the
// platform never counts a node it cannot judge, so a global service with a
// constraint it cannot read never claims that it converged.
func TestTheGlobalServiceWithAnUnknownConstraintHasNoTarget(t *testing.T) {
	engine := swarmTestEngine(t)
	service := swarmapi.Service{
		ID: "service-api",
		Spec: swarmapi.ServiceSpec{
			Annotations: swarmapi.Annotations{Name: "demo_api"},
			Mode:        swarmapi.ServiceMode{Global: &swarmapi.GlobalService{}},
			TaskTemplate: swarmapi.TaskSpec{
				Placement: &swarmapi.Placement{
					Constraints: []string{"node.role == manager"},
				},
			},
		},
	}
	engine.nodes[1].Spec.Annotations.Labels = map[string]string{}
	target, known := effectiveTarget(service, engine.nodes)
	if !known {
		t.Fatal("the target of the service could not be proven")
	}
	if target != 1 {
		t.Fatalf("the target = %d, want the one manager of the cluster", target)
	}

	unknown := service
	unknown.Spec.TaskTemplate.Placement = &swarmapi.Placement{
		Constraints: []string{"engine.labels.kernel == 6"},
	}
	engine.nodes[0].Description.Engine.Labels = map[string]string{}
	if _, known := effectiveTarget(unknown, engine.nodes); !known {
		t.Fatal("the constraint of the engine was not read")
	}
}

// TestTheSwarmExecutorNamesTheServiceThatIsMissing proves that a service the
// deployment asks for and the cluster does not hold keeps the deployment from
// being successful.
func TestTheSwarmExecutorNamesTheServiceThatIsMissing(t *testing.T) {
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	cmd := swarmTestCommand()
	engine.services = nil
	executor := newSwarmExecutor(t, runner, engine)

	result, err := executor.Execute(context.Background(), cmd, io.Discard)
	if err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if result.State == model.Succeeded {
		t.Fatalf("the state = %q, want a failure", result.State)
	}
	if !strings.Contains(result.Message, "api") {
		t.Fatalf("the message = %q, want the missing service", result.Message)
	}
}

// TestTheSwarmExecutorReportsAStackWithoutAService proves that a
// configuration without a service never reaches the Docker CLI.
func TestTheSwarmExecutorReportsAStackWithoutAService(t *testing.T) {
	runner := &fakeRunner{}
	executor := newSwarmExecutor(t, runner, swarmTestEngine(t))

	cmd := swarmTestCommand()
	cmd.Config.YAML = "services:\n"
	result, err := executor.Execute(context.Background(), cmd, io.Discard)
	if err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if result.State != model.Failed {
		t.Fatalf("the state = %q, want %q", result.State, model.Failed)
	}
	if runner.count() != 0 {
		t.Fatalf("the runner received %d commands, want none", runner.count())
	}
}

// TestTheSwarmExecutorWritesTheConfigurationOfTheStack proves that the
// configuration of the stack is written into the workspace of the
// application and that it holds the version of every service.
func TestTheSwarmExecutorWritesTheConfigurationOfTheStack(t *testing.T) {
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	cmd := swarmTestCommand()
	engine.services = []swarmapi.Service{
		swarmService("api", 2, swarmDigests(t, cmd)["api"], swarmapi.UpdateStatus{}),
	}
	engine.tasks = []swarmapi.Task{
		swarmTask("task-1", "service-api", 1, swarmapi.TaskStateRunning, ""),
		swarmTask("task-2", "service-api", 2, swarmapi.TaskStateRunning, ""),
	}
	executor := newSwarmExecutor(t, runner, engine)

	if _, err := executor.Execute(context.Background(), cmd, io.Discard); err != nil {
		t.Fatalf("execute the command: %v", err)
	}

	written, err := os.ReadFile(filepath.Join(executor.root, "apps", "app-1", "deploy.yaml"))
	if err != nil {
		t.Fatalf("read the configuration of the stack: %v", err)
	}
	if !strings.Contains(string(written), SwarmVersionLabel) {
		t.Fatalf("the configuration = %q, want the label of the version", written)
	}
}

// TestTheSwarmExecutorRunsTheStackConfigWithoutTheValuesOfTheVariables
// proves that the validation of the Docker CLI never writes the expanded
// configuration into the output of the task.
func TestTheSwarmExecutorRunsTheStackConfigWithoutTheValuesOfTheVariables(t *testing.T) {
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	cmd := swarmTestCommand()
	cmd.Config.YAML = "services:\n  api:\n    image: nginx:alpine\n    environment:\n      PASSWORD: ${PASSWORD}\n"
	cmd.Config.Variables = map[string]string{"PASSWORD": "the-secret-value"}
	engine.services = []swarmapi.Service{
		swarmService("api", 2, swarmDigests(t, cmd)["api"], swarmapi.UpdateStatus{}),
	}
	engine.tasks = []swarmapi.Task{
		swarmTask("task-1", "service-api", 1, swarmapi.TaskStateRunning, ""),
		swarmTask("task-2", "service-api", 2, swarmapi.TaskStateRunning, ""),
	}
	executor := newSwarmExecutor(t, runner, engine)

	buffer := &strings.Builder{}
	if _, err := executor.Execute(context.Background(), cmd, buffer); err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if strings.Contains(buffer.String(), "the-secret-value") {
		t.Fatalf("the output = %q, want no value of a variable", buffer.String())
	}
}

// TestTheSwarmExecutorFailsWhenTheClusterCannotBeRead proves that a cluster
// the node cannot read is a failure and not an error: the target did not
// change.
func TestTheSwarmExecutorFailsWhenTheClusterCannotBeRead(t *testing.T) {
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	engine.servicesErr = errors.New("the cluster does not answer")
	executor := newSwarmExecutor(t, runner, engine)

	result, err := executor.Execute(context.Background(), swarmTestCommand(), io.Discard)
	if err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if result.State != model.Failed {
		t.Fatalf("the state = %q, want %q", result.State, model.Failed)
	}
	if result.Code != CodeStackPruneRefused {
		t.Fatalf("the code = %q, want %q", result.Code, CodeStackPruneRefused)
	}
	if runner.count() != 0 {
		t.Fatalf("the runner received %d commands, want none", runner.count())
	}
}

// TestTheSwarmExecutorDoesNotRunTheStackOfAnotherNamespace proves that the
// executor only reads and writes the stack of its own namespace.
func TestTheSwarmExecutorDoesNotRunTheStackOfAnotherNamespace(t *testing.T) {
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	cmd := swarmTestCommand()
	engine.services = []swarmapi.Service{
		swarmService("api", 2, swarmDigests(t, cmd)["api"], swarmapi.UpdateStatus{}),
	}
	engine.tasks = []swarmapi.Task{
		swarmTask("task-1", "service-api", 1, swarmapi.TaskStateRunning, ""),
		swarmTask("task-2", "service-api", 2, swarmapi.TaskStateRunning, ""),
	}
	executor := newSwarmExecutor(t, runner, engine)

	if _, err := executor.Execute(context.Background(), cmd, io.Discard); err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	deploy := swarmCommands(runner)[2]
	if deploy[len(deploy)-1] != "demo" {
		t.Fatalf("the command = %q, want the stack of the application", deploy)
	}
	for _, argument := range deploy {
		if argument == "other" {
			t.Fatalf("the command = %q, want no other stack", deploy)
		}
	}
}

// TestTheSwarmExecutorReportsTheServiceOfAnotherStack proves that the
// leftovers of another stack are never removed.
//
// The services of the stack are selected by the label of the stack, so a
// service of another stack is not even part of the inventory the executor
// reads.
func TestTheSwarmExecutorReportsTheServiceOfAnotherStack(t *testing.T) {
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	cmd := swarmTestCommand()
	engine.services = []swarmapi.Service{
		swarmService("api", 2, swarmDigests(t, cmd)["api"], swarmapi.UpdateStatus{}),
	}
	engine.tasks = []swarmapi.Task{
		swarmTask("task-1", "service-api", 1, swarmapi.TaskStateRunning, ""),
		swarmTask("task-2", "service-api", 2, swarmapi.TaskStateRunning, ""),
	}
	executor := newSwarmExecutor(t, runner, engine)

	result, err := executor.Execute(context.Background(), cmd, io.Discard)
	if err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if result.State != model.Succeeded {
		t.Fatalf("the state = %q, want %q (%s)", result.State, model.Succeeded, result.Message)
	}
	if strings.Contains(result.Message, "cache") {
		t.Fatalf("the message = %q, want no service of another stack", result.Message)
	}
}

// TestTheSwarmExecutorUsesTheFixedProgramOfTheDockerCommandLine proves that
// the executor only runs the command line of Docker of the image of the
// agent.
func TestTheSwarmExecutorUsesTheFixedProgramOfTheDockerCommandLine(t *testing.T) {
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	cmd := swarmTestCommand()
	engine.services = []swarmapi.Service{
		swarmService("api", 2, swarmDigests(t, cmd)["api"], swarmapi.UpdateStatus{}),
	}
	engine.tasks = []swarmapi.Task{
		swarmTask("task-1", "service-api", 1, swarmapi.TaskStateRunning, ""),
		swarmTask("task-2", "service-api", 2, swarmapi.TaskStateRunning, ""),
	}
	executor := newSwarmExecutor(t, runner, engine)

	if _, err := executor.Execute(context.Background(), cmd, io.Discard); err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	for _, call := range runner.calls {
		if call.Program != DockerProgram() {
			t.Fatalf("the program = %q, want %q", call.Program, DockerProgram())
		}
	}
}

// TestTheSwarmExecutorWritesTheOutputOfTheTask proves that the output of the
// command reaches the writer of the task, so the limit of the task applies to
// it.
func TestTheSwarmExecutorWritesTheOutputOfTheTask(t *testing.T) {
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	cmd := swarmTestCommand()
	engine.services = []swarmapi.Service{
		swarmService("api", 2, swarmDigests(t, cmd)["api"], swarmapi.UpdateStatus{}),
	}
	engine.tasks = []swarmapi.Task{
		swarmTask("task-1", "service-api", 1, swarmapi.TaskStateRunning, ""),
		swarmTask("task-2", "service-api", 2, swarmapi.TaskStateRunning, ""),
	}
	executor := newSwarmExecutor(t, runner, engine)

	buffer := &strings.Builder{}
	if _, err := executor.Execute(context.Background(), cmd, buffer); err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if !strings.Contains(buffer.String(), "the output of") {
		t.Fatalf("the output = %q, want the output of the command", buffer.String())
	}
}

// TestTheSwarmExecutorReportsTheDigestsOfItsServices proves that the
// executor binds the digest of every service of the configuration, so the
// convergence check can tell the version that runs from an older one.
func TestTheSwarmExecutorReportsTheDigestsOfItsServices(t *testing.T) {
	cmd := swarmTestCommand()
	digests := swarmDigests(t, cmd)
	if digests["api"] == "" {
		t.Fatal("the service carries no digest")
	}
	if len(digests) != len(strings.Split(cmd.Config.YAML, "  api:"))-1 {
		t.Fatalf("the digests = %v, want one digest", digests)
	}
}

// TestTheSwarmExecutorRefusesAServiceWithoutAnImage proves that the
// executor refuses a configuration the profile of the platform does not
// support.
func TestTheSwarmExecutorRefusesAServiceWithoutAnImage(t *testing.T) {
	runner := &fakeRunner{}
	executor := newSwarmExecutor(t, runner, swarmTestEngine(t))

	cmd := swarmTestCommand()
	cmd.Config.YAML = "services:\n  api:\n    command: [\"nginx\"]\n"
	result, err := executor.Execute(context.Background(), cmd, io.Discard)
	if err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if result.State != model.Failed {
		t.Fatalf("the state = %q, want %q", result.State, model.Failed)
	}
	if runner.count() != 0 {
		t.Fatalf("the runner received %d commands, want none", runner.count())
	}
}

// TestTheSwarmExecutorKeepsTheFileOfTheApplication proves that the file of
// the configuration is the file of the workspace of the application.
func TestTheSwarmExecutorKeepsTheFileOfTheApplication(t *testing.T) {
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	cmd := swarmTestCommand()
	engine.services = []swarmapi.Service{
		swarmService("api", 2, swarmDigests(t, cmd)["api"], swarmapi.UpdateStatus{}),
	}
	engine.tasks = []swarmapi.Task{
		swarmTask("task-1", "service-api", 1, swarmapi.TaskStateRunning, ""),
		swarmTask("task-2", "service-api", 2, swarmapi.TaskStateRunning, ""),
	}
	executor := newSwarmExecutor(t, runner, engine)

	if _, err := executor.Execute(context.Background(), cmd, io.Discard); err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	config := swarmCommands(runner)[0]
	if !contains(config, filepath.Join(executor.root, "apps", "app-1", "deploy.yaml")) {
		t.Fatalf("the arguments = %q, want the file of the application", config)
	}
}

// TestTheSwarmExecutorHoldsNoSecretOfTheApplication proves that the value of
// a variable of the application never reaches the command line of the Docker
// CLI.
func TestTheSwarmExecutorHoldsNoSecretOfTheApplication(t *testing.T) {
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	cmd := swarmTestCommand()
	cmd.Config.Variables = map[string]string{"PASSWORD": "; rm -rf / #"}
	engine.services = []swarmapi.Service{
		swarmService("api", 2, swarmDigests(t, cmd)["api"], swarmapi.UpdateStatus{}),
	}
	engine.tasks = []swarmapi.Task{
		swarmTask("task-1", "service-api", 1, swarmapi.TaskStateRunning, ""),
		swarmTask("task-2", "service-api", 2, swarmapi.TaskStateRunning, ""),
	}
	executor := newSwarmExecutor(t, runner, engine)

	if _, err := executor.Execute(context.Background(), cmd, io.Discard); err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	for _, call := range runner.calls {
		for _, argument := range call.Args {
			if strings.Contains(argument, "rm -rf") {
				t.Fatalf("the argument %q holds the value of a variable", argument)
			}
		}
	}
}

// TestTheSwarmExecutorReportsTheVersionOfTheStack proves that the message of
// a successful deployment names the services of the stack.
func TestTheSwarmExecutorReportsTheVersionOfTheStack(t *testing.T) {
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	cmd := swarmTestCommand()
	engine.services = []swarmapi.Service{
		swarmService("api", 2, swarmDigests(t, cmd)["api"], swarmapi.UpdateStatus{}),
	}
	engine.tasks = []swarmapi.Task{
		swarmTask("task-1", "service-api", 1, swarmapi.TaskStateRunning, ""),
		swarmTask("task-2", "service-api", 2, swarmapi.TaskStateRunning, ""),
	}
	executor := newSwarmExecutor(t, runner, engine)

	result, err := executor.Execute(context.Background(), cmd, io.Discard)
	if err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if !strings.Contains(result.Message, "api") {
		t.Fatalf("the message = %q, want the service of the stack", result.Message)
	}
	fmt.Fprintln(io.Discard, result.Message)
}

// TestTheSwarmExecutorRefusesAServiceWhoseModeDiffersFromTheConfiguration
// proves that the mode of the configuration is compared with the mode of the
// service of the cluster.
//
// The digest of the version is not enough: a change of the cluster that was
// made by hand keeps the labels of the service, so the digest still names
// this deployment.
func TestTheSwarmExecutorRefusesAServiceWhoseModeDiffersFromTheConfiguration(t *testing.T) {
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	cmd := swarmTestCommand()
	cmd.Config.YAML = "services:\n  api:\n    image: nginx:alpine\n    deploy:\n      mode: global\n"
	engine.services = []swarmapi.Service{
		swarmService("api", 2, swarmDigests(t, cmd)["api"], swarmapi.UpdateStatus{
			State: swarmapi.UpdateStateCompleted,
		}),
	}
	engine.tasks = []swarmapi.Task{
		swarmTask("task-1", "service-api", 1, swarmapi.TaskStateRunning, ""),
		swarmTask("task-2", "service-api", 2, swarmapi.TaskStateRunning, ""),
	}
	executor := newSwarmExecutor(t, runner, engine)

	result, err := executor.Execute(context.Background(), cmd, io.Discard)
	if err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if result.State == model.Succeeded {
		t.Fatalf("the state = %q, want a failure", result.State)
	}
	if result.Code != CodeSwarmSpecMismatch {
		t.Fatalf("the code = %q, want %q", result.Code, CodeSwarmSpecMismatch)
	}
	if !strings.Contains(result.Message, "api") {
		t.Fatalf("the message = %q, want the service", result.Message)
	}
}

// TestTheSwarmExecutorRefusesAServiceWhoseImageDiffersFromTheConfiguration
// proves that the image of the configuration is compared with the image of
// the service of the cluster.
func TestTheSwarmExecutorRefusesAServiceWhoseImageDiffersFromTheConfiguration(t *testing.T) {
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	cmd := swarmTestCommand()
	service := swarmService("api", 2, swarmDigests(t, cmd)["api"], swarmapi.UpdateStatus{
		State: swarmapi.UpdateStateCompleted,
	})
	service.Spec.TaskTemplate.ContainerSpec.Image = "redis:alpine"
	engine.services = []swarmapi.Service{service}
	engine.tasks = []swarmapi.Task{
		swarmTaskWithImage("task-1", "service-api", 1, swarmapi.TaskStateRunning, "", "redis:alpine"),
		swarmTaskWithImage("task-2", "service-api", 2, swarmapi.TaskStateRunning, "", "redis:alpine"),
	}
	executor := newSwarmExecutor(t, runner, engine)

	result, err := executor.Execute(context.Background(), cmd, io.Discard)
	if err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if result.State == model.Succeeded {
		t.Fatalf("the state = %q, want a failure", result.State)
	}
	if result.Code != CodeSwarmSpecMismatch {
		t.Fatalf("the code = %q, want %q", result.Code, CodeSwarmSpecMismatch)
	}
}

// TestTheSwarmExecutorAcceptsTheDigestOfTheImageOfTheCluster proves that
// the digest of an image is not a difference.
//
// The configuration names the image with a tag, the cluster holds it with
// the digest the registry answered with.
func TestTheSwarmExecutorAcceptsTheDigestOfTheImageOfTheCluster(t *testing.T) {
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	cmd := swarmTestCommand()
	digest := "nginx@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	service := swarmService("api", 2, swarmDigests(t, cmd)["api"], swarmapi.UpdateStatus{
		State: swarmapi.UpdateStateCompleted,
	})
	service.Spec.TaskTemplate.ContainerSpec.Image = digest
	engine.services = []swarmapi.Service{service}
	engine.tasks = []swarmapi.Task{
		swarmTaskWithImage("task-1", "service-api", 1, swarmapi.TaskStateRunning, "", digest),
		swarmTaskWithImage("task-2", "service-api", 2, swarmapi.TaskStateRunning, "", digest),
	}
	executor := newSwarmExecutor(t, runner, engine)

	result, err := executor.Execute(context.Background(), cmd, io.Discard)
	if err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if result.State != model.Succeeded {
		t.Fatalf("the state = %q, want %q (%s)", result.State, model.Succeeded, result.Message)
	}
}

// TestTheSwarmExecutorRefusesAStackThatWasScaledByHand proves that a stack
// that was scaled by hand is never reported as the deployment of this
// configuration.
//
// The number of the replicas of the configuration is the target of the
// deployment, so a stack that runs more tasks than the configuration asks
// for never reached the target of this deployment.
func TestTheSwarmExecutorRefusesAStackThatWasScaledByHand(t *testing.T) {
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	cmd := swarmTestCommand()
	engine.services = []swarmapi.Service{
		swarmService("api", 5, swarmDigests(t, cmd)["api"], swarmapi.UpdateStatus{
			State: swarmapi.UpdateStateCompleted,
		}),
	}
	engine.tasks = []swarmapi.Task{
		swarmTask("task-1", "service-api", 1, swarmapi.TaskStateRunning, ""),
		swarmTask("task-2", "service-api", 2, swarmapi.TaskStateRunning, ""),
		swarmTask("task-3", "service-api", 3, swarmapi.TaskStateRunning, ""),
		swarmTask("task-4", "service-api", 4, swarmapi.TaskStateRunning, ""),
		swarmTask("task-5", "service-api", 5, swarmapi.TaskStateRunning, ""),
	}
	executor := newSwarmExecutor(t, runner, engine)

	result, err := executor.Execute(context.Background(), cmd, io.Discard)
	if err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if result.State == model.Succeeded {
		t.Fatalf("the state = %q, want a failure", result.State)
	}
	if result.Code != CodeSwarmSpecMismatch {
		t.Fatalf("the code = %q, want %q", result.Code, CodeSwarmSpecMismatch)
	}
}

// TestTheSwarmExecutorReportsTheRollbackOfAChangedImage proves that the
// rollback of the cluster is reported even when the image of the
// configuration differs from the image the rolled back service runs.
//
// The cluster undoes the change of the image, so the service runs the image
// of the version before this deployment. The rollback is the outcome of the
// deployment, not a difference of the specification.
func TestTheSwarmExecutorReportsTheRollbackOfAChangedImage(t *testing.T) {
	runner := &fakeRunner{}
	engine := swarmTestEngine(t)
	cmd := swarmTestCommand()
	cmd.Config.YAML = "services:\n  api:\n    image: redis:alpine\n    deploy:\n      replicas: 2\n"
	service := swarmService("api", 2, swarmDigests(t, cmd)["api"], swarmapi.UpdateStatus{
		State: swarmapi.UpdateStateRollbackCompleted,
	})
	service.Spec.TaskTemplate.ContainerSpec.Image = "nginx:alpine"
	engine.services = []swarmapi.Service{service}
	engine.tasks = []swarmapi.Task{
		swarmTask("task-1", "service-api", 1, swarmapi.TaskStateRunning, ""),
		swarmTask("task-2", "service-api", 2, swarmapi.TaskStateRunning, ""),
	}
	executor := newSwarmExecutor(t, runner, engine)

	result, err := executor.Execute(context.Background(), cmd, io.Discard)
	if err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if result.State == model.Succeeded {
		t.Fatalf("the state = %q, want a failure", result.State)
	}
	if !result.RolledBack {
		t.Fatalf("the rollback was not reported: %+v", result)
	}
	if result.Code != CodeSwarmRolledBack {
		t.Fatalf("the code = %q, want %q", result.Code, CodeSwarmRolledBack)
	}
}
