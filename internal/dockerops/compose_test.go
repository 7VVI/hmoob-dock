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

	"hmoob-dock/internal/model"
	"hmoob-dock/internal/protocol"
)

// testContainerID is the ID of the container the agent of the tests runs in.
const testContainerID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// fakeRunner records the commands of the tests.
//
// The runner fails on the action a test names, so a test can prove that a
// command does not run after another one failed.
type fakeRunner struct {
	mu    sync.Mutex
	calls []RunSpec
	fail  map[string]error
}

// Run records the command and reports the error of the action.
func (f *fakeRunner) Run(ctx context.Context, spec RunSpec, output io.Writer) error {
	f.mu.Lock()
	f.calls = append(f.calls, spec)
	f.mu.Unlock()

	if output != nil {
		fmt.Fprintf(output, "the output of %s\n", strings.Join(spec.Args, " "))
	}
	for _, arg := range spec.Args {
		if err, ok := f.fail[arg]; ok {
			return err
		}
	}
	return nil
}

// actions returns the action of every command the runner received.
func (f *fakeRunner) actions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	actions := make([]string, 0, len(f.calls))
	for _, call := range f.calls {
		actions = append(actions, actionOf(call))
	}
	return actions
}

// count returns the number of the commands the runner received.
func (f *fakeRunner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// last returns the last command the runner received.
func (f *fakeRunner) last(t *testing.T) RunSpec {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("the runner received no command")
	}
	return f.calls[len(f.calls)-1]
}

// actionOf returns the action of a command of Compose.
func actionOf(spec RunSpec) string {
	for i, arg := range spec.Args {
		if arg == "--file" && i+2 < len(spec.Args) {
			return spec.Args[i+2]
		}
	}
	return ""
}

// fakeEngine is the Docker engine of the tests.
//
// The engine holds the mounts of the container of the agent and the
// containers of the project, so a test can prove what the executor does with
// them.
type fakeEngine struct {
	mounts        []Mount
	containers    []protocol.Container
	mountErr      error
	containersErr error
}

// ContainerMounts returns the mounts of the container of the agent.
func (f *fakeEngine) ContainerMounts(ctx context.Context, id string) ([]Mount, error) {
	if f.mountErr != nil {
		return nil, f.mountErr
	}
	return f.mounts, nil
}

// ProjectContainers returns the containers of the project.
func (f *fakeEngine) ProjectContainers(ctx context.Context, namespace string) ([]protocol.Container, error) {
	if f.containersErr != nil {
		return nil, f.containersErr
	}
	return f.containers, nil
}

// Host returns the address of the engine of the tests.
func (f *fakeEngine) Host() string {
	return "unix:///var/run/docker.sock"
}

// newTestExecutor builds the executor of the tests.
//
// The work root of the agent is a temporary directory and the engine of the
// tests proves that the directory is mounted at the same path on the host.
func newTestExecutor(t *testing.T, runner Runner, engine *fakeEngine) (*Compose, string) {
	t.Helper()

	root := t.TempDir()
	t.Setenv(ContainerIDEnv, testContainerID)
	if engine == nil {
		engine = &fakeEngine{}
	}
	if engine.mounts == nil {
		engine.mounts = []Mount{{Type: "bind", Source: root, Destination: root}}
	}
	return &Compose{root: root, runner: runner, engine: engine}, root
}

// testCommand builds the command of the tests.
func testCommand() model.Command {
	return model.Command{
		Task: model.Task{ID: "task-1", AppID: "app-1", Digest: "digest-1"},
		App: model.App{
			ID:        "app-1",
			Name:      "demo",
			Mode:      "compose",
			TargetID:  "node-1",
			Namespace: "demo",
		},
		Config: model.Config{
			YAML:      "services:\n  api:\n    image: nginx:alpine\n",
			Variables: map[string]string{},
		},
		ExecutionTimeout: time.Minute,
		VerifyTimeout:    2 * time.Second,
	}
}

// TestComposeUpNeverStopsWholeProjectOrDeletesVolumes proves that the
// command that starts a project never stops the project and never removes a
// volume.
func TestComposeUpNeverStopsWholeProjectOrDeletesVolumes(t *testing.T) {
	args, err := ComposeArgs("demo", "/opt/hmoob-dock/apps/a/deploy.yaml", "up", false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"compose", "--project-name", "demo", "--file", "/opt/hmoob-dock/apps/a/deploy.yaml", "up", "--detach"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args=%q", args)
	}
	for _, arg := range args {
		if arg == "down" || arg == "--volumes" {
			t.Fatalf("destructive argument %q", arg)
		}
	}
}

// TestComposeArgsRefusesAnUnknownAction proves that the platform cannot run
// a command of Compose that changes the resources of the node.
func TestComposeArgsRefusesAnUnknownAction(t *testing.T) {
	for _, action := range []string{"down", "rm", "stop", "kill", "restart", ""} {
		if _, err := ComposeArgs("demo", "/opt/hmoob-dock/apps/a/deploy.yaml", action, false); err == nil {
			t.Fatalf("the action %q was accepted", action)
		}
	}
}

// TestComposeArgsRefusesAnUnsafeNamespace proves that the name of the
// project is a fixed name and not a path of the host.
func TestComposeArgsRefusesAnUnsafeNamespace(t *testing.T) {
	for _, namespace := range []string{"", "..", "a/b", "a b", "Demo", "-demo"} {
		if _, err := ComposeArgs(namespace, "/opt/hmoob-dock/apps/a/deploy.yaml", "up", false); err == nil {
			t.Fatalf("the namespace %q was accepted", namespace)
		}
	}
}

// TestTheComposeExecutorStopsAfterAFailedConfiguration proves that a
// configuration the Docker CLI refuses never reaches the pull and never
// reaches the up.
func TestTheComposeExecutorStopsAfterAFailedConfiguration(t *testing.T) {
	runner := &fakeRunner{fail: map[string]error{ActionConfig: errors.New("exit status 1")}}
	executor, _ := newTestExecutor(t, runner, nil)

	result, err := executor.Execute(context.Background(), testCommand(), io.Discard)
	if err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if result.State != model.Failed {
		t.Fatalf("the state = %q, want %q", result.State, model.Failed)
	}
	if result.Code != CodeConfigInvalid {
		t.Fatalf("the code = %q, want %q", result.Code, CodeConfigInvalid)
	}
	if got := runner.actions(); !reflect.DeepEqual(got, []string{ActionConfig}) {
		t.Fatalf("the commands = %q, want only the config", got)
	}
}

// TestTheComposeExecutorStopsAfterAFailedPull proves that images the Docker
// CLI cannot pull keep the project from starting.
func TestTheComposeExecutorStopsAfterAFailedPull(t *testing.T) {
	runner := &fakeRunner{fail: map[string]error{ActionPull: errors.New("exit status 1")}}
	executor, _ := newTestExecutor(t, runner, nil)

	result, err := executor.Execute(context.Background(), testCommand(), io.Discard)
	if err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if result.State != model.Failed {
		t.Fatalf("the state = %q, want %q", result.State, model.Failed)
	}
	if result.Code != CodePullFailed {
		t.Fatalf("the code = %q, want %q", result.Code, CodePullFailed)
	}
	if result.PartialUpdate {
		t.Fatal("a failed pull changed the target")
	}
	if got := runner.actions(); !reflect.DeepEqual(got, []string{ActionConfig, ActionPull}) {
		t.Fatalf("the commands = %q, want the config and the pull", got)
	}
}

// TestTheComposeExecutorKeepsTheNamespaceAndTheFileOfTheApplication proves
// that the project and the file of the configuration are fixed by the
// platform and not by the configuration.
func TestTheComposeExecutorKeepsTheNamespaceAndTheFileOfTheApplication(t *testing.T) {
	runner := &fakeRunner{}
	executor, root := newTestExecutor(t, runner, nil)

	if _, err := executor.Execute(context.Background(), testCommand(), io.Discard); err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	last := runner.last(t)
	want := filepath.Join(root, "apps", "app-1", "deploy.yaml")
	if !contains(last.Args, want) {
		t.Fatalf("the arguments = %q, want the file %q", last.Args, want)
	}
	if !containsPair(last.Args, "--project-name", "demo") {
		t.Fatalf("the arguments = %q, want the project demo", last.Args)
	}
}

// TestTheComposeExecutorNeverPutsAValueIntoTheCommandLine proves that a
// value of a variable never reaches the command line of the shell.
func TestTheComposeExecutorNeverPutsAValueIntoTheCommandLine(t *testing.T) {
	runner := &fakeRunner{}
	executor, _ := newTestExecutor(t, runner, nil)

	cmd := testCommand()
	cmd.Config.YAML = "services:\n  api:\n    image: nginx:alpine\n    environment:\n      PASSWORD: ${PASSWORD}\n"
	cmd.Config.Variables = map[string]string{"PASSWORD": "; rm -rf / #"}
	if _, err := executor.Execute(context.Background(), cmd, io.Discard); err != nil {
		t.Fatalf("execute the command: %v", err)
	}

	for _, call := range runner.calls {
		for _, arg := range call.Args {
			if strings.Contains(arg, "rm -rf") {
				t.Fatalf("the argument %q holds the value of a variable", arg)
			}
		}
	}
}

// TestTheComposeExecutorDoesNotInheritTheEnvironmentOfTheAgent proves that
// the command line of Docker only knows the environment the platform built.
func TestTheComposeExecutorDoesNotInheritTheEnvironmentOfTheAgent(t *testing.T) {
	t.Setenv("HMOOB_TEST_SECRET", "the secret of the agent")
	runner := &fakeRunner{}
	executor, _ := newTestExecutor(t, runner, nil)

	if _, err := executor.Execute(context.Background(), testCommand(), io.Discard); err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	env := runner.last(t).Env
	if contains(env, "HMOOB_TEST_SECRET") {
		t.Fatalf("the environment = %q, want no variable of the agent", env)
	}
	if !contains(env, DisableEnvFileEnv+"=1") {
		t.Fatalf("the environment = %q, want %s", env, DisableEnvFileEnv)
	}
}

// TestTheComposeExecutorWritesTheOutputIntoTheWriterOfTheCaller proves that
// the output of the command reaches the writer of the task, so the limit of
// the task applies to it.
func TestTheComposeExecutorWritesTheOutputIntoTheWriterOfTheCaller(t *testing.T) {
	runner := &fakeRunner{}
	executor, _ := newTestExecutor(t, runner, nil)

	buffer := &strings.Builder{}
	if _, err := executor.Execute(context.Background(), testCommand(), buffer); err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if !strings.Contains(buffer.String(), "the output of compose") {
		t.Fatalf("the output = %q, want the output of the command", buffer.String())
	}
}

// TestTheComposeExecutorChangesNothingWithoutAProofOfTheMapping proves that
// a work root the platform cannot prove runs no command at all.
func TestTheComposeExecutorChangesNothingWithoutAProofOfTheMapping(t *testing.T) {
	runner := &fakeRunner{}
	engine := &fakeEngine{mounts: []Mount{{Type: "bind", Source: "/srv/hmoob", Destination: "/opt/hmoob-dock"}}}
	executor, _ := newTestExecutor(t, runner, engine)

	result, err := executor.Execute(context.Background(), testCommand(), io.Discard)
	if err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if result.State != model.Failed {
		t.Fatalf("the state = %q, want %q", result.State, model.Failed)
	}
	if result.Code != CodePreflightFailed {
		t.Fatalf("the code = %q, want %q", result.Code, CodePreflightFailed)
	}
	if runner.count() != 0 {
		t.Fatalf("the runner received %d commands, want none", runner.count())
	}
}

// TestTheComposeExecutorRemovesTheConfirmedServices proves that the
// confirmed removals reach the up and that the scope of the removal is
// checked again.
func TestTheComposeExecutorRemovesTheConfirmedServices(t *testing.T) {
	runner := &fakeRunner{}
	executor, _ := newTestExecutor(t, runner, nil)

	cmd := testCommand()
	cmd.RemovedServices = []string{"cache"}
	if _, err := executor.Execute(context.Background(), cmd, io.Discard); err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if !contains(runner.last(t).Args, "--remove-orphans") {
		t.Fatalf("the arguments = %q, want the removal of the orphans", runner.last(t).Args)
	}
}

// TestTheComposeExecutorRefusesToRemoveAServiceTheConfigurationHolds
// proves that a service the configuration still holds is never removed.
func TestTheComposeExecutorRefusesToRemoveAServiceTheConfigurationHolds(t *testing.T) {
	runner := &fakeRunner{}
	executor, _ := newTestExecutor(t, runner, nil)

	cmd := testCommand()
	cmd.RemovedServices = []string{"api"}
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

// TestTheComposeExecutorReportsAPartialUpdateWithoutACompensation proves
// that a service that fails after the update leaves the application partly
// changed and that the platform never stops the project to undo it.
func TestTheComposeExecutorReportsAPartialUpdateWithoutACompensation(t *testing.T) {
	runner := &fakeRunner{}
	engine := &fakeEngine{containers: []protocol.Container{
		{Service: "api", State: "running", Health: "healthy"},
		{Service: "worker", State: "running", Health: "unhealthy"},
	}}
	executor, _ := newTestExecutor(t, runner, engine)

	cmd := testCommand()
	cmd.Config.YAML = "services:\n  api:\n    image: nginx:alpine\n  worker:\n    image: nginx:alpine\n"
	result, err := executor.Execute(context.Background(), cmd, io.Discard)
	if err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if result.State != model.Failed {
		t.Fatalf("the state = %q, want %q", result.State, model.Failed)
	}
	if result.Code != CodeComposeUnhealthy {
		t.Fatalf("the code = %q, want %q", result.Code, CodeComposeUnhealthy)
	}
	if !result.PartialUpdate {
		t.Fatal("the update was not reported as partial")
	}
	for _, action := range runner.actions() {
		if action == "down" || action == "stop" || action == "rm" {
			t.Fatalf("the platform ran the command %q to undo the update", action)
		}
	}
}

// TestTheComposeExecutorReportsAPartialUpdateWhenAServiceIsNotReady proves
// that a deployment that started the project but did not reach the expected
// state is a partial update: the target changed.
func TestTheComposeExecutorReportsAPartialUpdateWhenAServiceIsNotReady(t *testing.T) {
	runner := &fakeRunner{}
	engine := &fakeEngine{containers: []protocol.Container{
		{Service: "api", State: "running", Health: "starting"},
	}}
	executor, _ := newTestExecutor(t, runner, engine)

	cmd := testCommand()
	cmd.VerifyTimeout = 100 * time.Millisecond
	result, err := executor.Execute(context.Background(), cmd, io.Discard)
	if err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if result.State != model.Failed {
		t.Fatalf("the state = %q, want %q", result.State, model.Failed)
	}
	if result.Code != CodeComposeNotReady {
		t.Fatalf("the code = %q, want %q", result.Code, CodeComposeNotReady)
	}
	if !result.PartialUpdate {
		t.Fatal("the started project was not reported as a partial update")
	}
}

// TestTheComposeExecutorReportsAnUncertainOutcomeWhenTheCommandRunsOutOfTime// proves that a command that ran out of time leaves the outcome uncertain,
// because the daemon may still work on the change.
func TestTheComposeExecutorReportsAnUncertainOutcomeWhenTheCommandRunsOutOfTime(t *testing.T) {
	runner := &fakeRunner{fail: map[string]error{ActionUp: context.DeadlineExceeded}}
	executor, _ := newTestExecutor(t, runner, nil)

	result, err := executor.Execute(context.Background(), testCommand(), io.Discard)
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

// TestTheComposeExecutorFailsWhenAServiceIsMissing proves that the number of
// the services of the configuration decides whether the deployment is ready.
func TestTheComposeExecutorFailsWhenAServiceIsMissing(t *testing.T) {
	runner := &fakeRunner{}
	engine := &fakeEngine{containers: []protocol.Container{
		{Service: "api", State: "running"},
	}}
	executor, _ := newTestExecutor(t, runner, engine)

	cmd := testCommand()
	cmd.Config.YAML = "services:\n  api:\n    image: nginx:alpine\n  worker:\n    image: nginx:alpine\n"
	result, err := executor.Execute(context.Background(), cmd, io.Discard)
	if err != nil {
		t.Fatalf("execute the command: %v", err)
	}
	if result.State != model.Failed {
		t.Fatalf("the state = %q, want %q", result.State, model.Failed)
	}
	if !strings.Contains(result.Message, "worker") {
		t.Fatalf("the message = %q, want the missing service", result.Message)
	}
}

// contains reports whether the list holds the value.
func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// containsPair reports whether the list holds the two values in a row.
func containsPair(list []string, first, second string) bool {
	for i := 0; i+1 < len(list); i++ {
		if list[i] == first && list[i+1] == second {
			return true
		}
	}
	return false
}

// TestTheExecRunnerRefusesAProgramThatIsNotTheDockerCommandLine proves that
// the executor only runs the fixed path of the Docker command line.
func TestTheExecRunnerRefusesAProgramThatIsNotTheDockerCommandLine(t *testing.T) {
	runner := ExecRunner{}
	for _, program := range []string{"", "docker", "./docker", "/bin/sh"} {
		err := runner.Run(context.Background(), RunSpec{Program: program}, io.Discard)
		if err == nil {
			t.Fatalf("the program %q was accepted", program)
		}
	}
}

// TestTheExecRunnerRunsTheProgramItIsGiven proves that the runner runs the
// program with the environment and the directory it is given and with
// nothing else.
func TestTheExecRunnerRunsTheProgramItIsGiven(t *testing.T) {
	program, err := os.Executable()
	if err != nil {
		t.Fatalf("find the program of the test: %v", err)
	}
	t.Setenv(DockerProgramEnv, program)
	t.Setenv("HMOOB_TEST_SECRET", "the secret of the agent")

	dir := t.TempDir()
	buffer := &strings.Builder{}
	runner := ExecRunner{}
	err = runner.Run(context.Background(), RunSpec{
		Program: program,
		Dir:     dir,
		Args:    []string{"-test.run=^TestExecRunnerHelper$"},
		Env:     append([]string{"HMOOB_TEST_HELPER=1", "HMOOB_TEST_VALUE=value"}, systemEnv()...),
	}, buffer)
	if err != nil {
		t.Fatalf("run the program: %v", err)
	}
	if !strings.Contains(buffer.String(), "value=value") {
		t.Fatalf("the output = %q, want the environment of the command", buffer.String())
	}
	if strings.Contains(buffer.String(), "the secret of the agent") {
		t.Fatalf("the output = %q, want no variable of the agent", buffer.String())
	}
	if !strings.Contains(buffer.String(), "dir="+dir) && !strings.Contains(buffer.String(), "dir="+resolvedDir(dir)) {
		t.Fatalf("the output = %q, want the directory of the command", buffer.String())
	}
}

// TestExecRunnerHelper is the program the test of the runner runs.
//
// The test binary is the only program that exists on every platform, so it
// stands in for the Docker command line of the test. The helper does
// nothing when the environment does not ask for it.
func TestExecRunnerHelper(t *testing.T) {
	if os.Getenv("HMOOB_TEST_HELPER") != "1" {
		return
	}
	directory, err := os.Getwd()
	if err != nil {
		fmt.Printf("dir=error\n")
		os.Exit(1)
	}
	fmt.Printf("dir=%s\n", directory)
	fmt.Printf("value=%s\n", os.Getenv("HMOOB_TEST_VALUE"))
	os.Exit(0)
}

// systemEnv returns the variables of the system the program of the test
// needs. The variables of the agent are not part of them.
func systemEnv() []string {
	var env []string
	for _, name := range []string{"PATH", "SYSTEMROOT", "SystemRoot", "HOME", "TMPDIR", "TEMP", "TMP"} {
		if value := os.Getenv(name); value != "" {
			env = append(env, name+"="+value)
		}
	}
	return env
}
