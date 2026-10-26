//go:build integration

package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"

	"hmoob-dock/internal/dockerops"
	"hmoob-dock/internal/model"
	"hmoob-dock/internal/protocol"
)

// TestComposeDeployment deploys an application with Compose, changes it,
// removes a service and deploys the historical configuration again.
//
// The test proves that the platform touches nothing but the resources of
// its own project: the named volume of the application survives the removal
// of a service, and the cleanup of the test only touches the resources the
// test created.
func TestComposeDeployment(t *testing.T) {
	host := dockerHost(t)

	local, err := dockerops.NewLocal(host)
	if err != nil {
		t.Fatalf("connect to the Docker engine %s: %v", host, err)
	}
	defer local.Close()

	api, err := client.NewClientWithOpts(client.WithHost(host), client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("connect to the Docker engine %s: %v", host, err)
	}
	defer api.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// The engine has to answer before the test deploys anything. A missing
	// engine blocks the acceptance, it is not a failure of the deployment.
	if err := local.Ping(ctx); err != nil {
		t.Skipf("the Docker engine %s is not available (%v): the acceptance of the real deployment is blocked", host, err)
	}

	// The command line of Docker runs on the machine of the test, so its
	// path is named here.
	program := dockerProgram(t)
	t.Setenv(dockerops.DockerProgramEnv, program)
	// The agent of the test does not run in a container, so there is no
	// container of the agent to inspect. The operator of the test asserts
	// that the work root is the same path on the host as it is in the
	// container of the agent.
	t.Setenv(dockerops.MappingEnv, dockerops.MappingAsserted)

	suffix := randomSuffix(t)
	namespace := "hmoobtest" + suffix
	appID := "apptest" + suffix
	root := t.TempDir()
	executor := dockerops.NewCompose(root, dockerops.ExecRunner{}, local)

	t.Cleanup(func() {
		cleanupProject(t, api, local, namespace)
	})

	// The first deployment starts a service with a health check, a service
	// that runs once and exits, and a named volume.
	first := composeCommand(appID, namespace, firstConfiguration(namespace), map[string]string{
		"GREETING": "hello-first",
	})
	result := deploy(t, ctx, executor, first)
	if result.State != model.Succeeded {
		t.Fatalf("the state of the first deployment = %q (%s), want %q", result.State, result.Message, model.Succeeded)
	}
	if result.Code != dockerops.CodeComposeReady {
		t.Fatalf("the code of the first deployment = %q, want %q", result.Code, dockerops.CodeComposeReady)
	}

	containers := projectContainers(t, ctx, local, namespace)
	web := containerOfService(t, containers, "web")
	if web.State != "running" {
		t.Fatalf("the state of the service web = %q, want %q", web.State, "running")
	}
	if web.Health != "healthy" {
		t.Fatalf("the health of the service web = %q, want %q", web.Health, "healthy")
	}
	seed := containerOfService(t, containers, "seed")
	if seed.State != "exited" {
		t.Fatalf("the state of the service seed = %q, want %q", seed.State, "exited")
	}
	if seed.ExitCode == nil || *seed.ExitCode != 0 {
		t.Fatalf("the exit code of the service seed = %v, want 0", seed.ExitCode)
	}
	if web.RestartCount != 0 {
		t.Fatalf("the service web was restarted %d times, want none", web.RestartCount)
	}

	volumeName := namespace + "_data"
	if !volumeExists(t, ctx, api, volumeName) {
		t.Fatalf("the volume %s of the application does not exist", volumeName)
	}

	// The second deployment changes the image of the service and the value
	// of a variable.
	second := composeCommand(appID, namespace, secondConfiguration(namespace), map[string]string{
		"GREETING": "hello-second",
	})
	result = deploy(t, ctx, executor, second)
	if result.State != model.Succeeded {
		t.Fatalf("the state of the second deployment = %q (%s), want %q", result.State, result.Message, model.Succeeded)
	}

	containers = projectContainers(t, ctx, local, namespace)
	web = containerOfService(t, containers, "web")
	if !strings.Contains(web.Image, "redis:7-alpine") {
		t.Fatalf("the image of the service web = %q, want the image of the second deployment", web.Image)
	}
	if web.Health != "healthy" {
		t.Fatalf("the health of the service web = %q, want %q", web.Health, "healthy")
	}
	if greeting := containerEnvironment(t, ctx, api, namespace, "web"); greeting != "hello-second" {
		t.Fatalf("the value of the variable = %q, want %q", greeting, "hello-second")
	}

	// The third deployment removes the service that runs once. The removal
	// was confirmed, so the project cleans up the orphan. The named volume
	// of the application stays: the platform never removes a volume.
	third := composeCommand(appID, namespace, secondConfiguration(namespace), map[string]string{
		"GREETING": "hello-second",
	})
	third.RemovedServices = []string{"seed"}
	result = deploy(t, ctx, executor, third)
	if result.State != model.Succeeded {
		t.Fatalf("the state of the third deployment = %q (%s), want %q", result.State, result.Message, model.Succeeded)
	}

	containers = projectContainers(t, ctx, local, namespace)
	for _, item := range containers {
		if item.Service == "seed" {
			t.Fatal("the removed service still runs")
		}
	}
	if !volumeExists(t, ctx, api, volumeName) {
		t.Fatalf("the volume %s of the application was removed", volumeName)
	}

	// The fourth deployment is the historical configuration again. The
	// service that runs once comes back.
	fourth := composeCommand(appID, namespace, firstConfiguration(namespace), map[string]string{
		"GREETING": "hello-first",
	})
	result = deploy(t, ctx, executor, fourth)
	if result.State != model.Succeeded {
		t.Fatalf("the state of the fourth deployment = %q (%s), want %q", result.State, result.Message, model.Succeeded)
	}

	containers = projectContainers(t, ctx, local, namespace)
	seed = containerOfService(t, containers, "seed")
	if seed.ExitCode == nil || *seed.ExitCode != 0 {
		t.Fatalf("the exit code of the service seed = %v, want 0", seed.ExitCode)
	}
}

// deploy runs a deployment and fails the test when the executor reports an
// error of the agent itself.
func deploy(t *testing.T, ctx context.Context, executor dockerops.Executor, cmd model.Command) model.Result {
	t.Helper()

	output := &strings.Builder{}
	result, err := executor.Execute(ctx, cmd, output)
	if err != nil {
		t.Fatalf("execute the deployment: %v\n%s", err, output.String())
	}
	t.Logf("the deployment ended as %s (%s)\n%s", result.State, result.Message, output.String())
	return result
}

// composeCommand builds the command of a deployment.
func composeCommand(appID, namespace, document string, variables map[string]string) model.Command {
	return model.Command{
		Task: model.Task{ID: "task-" + namespace, AppID: appID, Digest: "digest-" + namespace},
		App: model.App{
			ID:        appID,
			Name:      "compose test",
			Mode:      "compose",
			TargetID:  "node-1",
			Namespace: namespace,
		},
		Config: model.Config{
			YAML:      document,
			Variables: variables,
		},
		ExecutionTimeout: 5 * time.Minute,
		VerifyTimeout:    3 * time.Minute,
	}
}

// firstConfiguration is the configuration of the first deployment.
//
// The service web has a health check, the service seed runs once and exits
// and the application has a named volume.
func firstConfiguration(namespace string) string {
	return fmt.Sprintf(`name: %s
services:
  web:
    image: nginx:alpine
    environment:
      GREETING: ${GREETING}
    volumes:
      - data:/usr/share/nginx/html
    healthcheck:
      test: ["CMD-SHELL", "wget -q -O /dev/null http://127.0.0.1/ || exit 1"]
      interval: 2s
      timeout: 3s
      retries: 15
  seed:
    image: nginx:alpine
    x-hmoob-lifecycle: oneshot
    restart: "no"
    command: ["sh", "-c", "echo ready > /tmp/ready"]
volumes:
  data:
`, namespace)
}

// secondConfiguration is the configuration of the second deployment.
//
// The image of the service changed and the service that runs once is gone.
func secondConfiguration(namespace string) string {
	return fmt.Sprintf(`name: %s
services:
  web:
    image: redis:7-alpine
    environment:
      GREETING: ${GREETING}
    volumes:
      - data:/data
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 2s
      timeout: 3s
      retries: 15
volumes:
  data:
`, namespace)
}

// randomSuffix returns a random suffix for the names of the test.
func randomSuffix(t *testing.T) string {
	t.Helper()

	buffer := make([]byte, 6)
	if _, err := rand.Read(buffer); err != nil {
		t.Fatalf("generate the suffix of the test: %v", err)
	}
	// The name of a project of Compose holds lower case letters only.
	return hex.EncodeToString(buffer)
}

// dockerProgram returns the path of the command line of Docker.
func dockerProgram(t *testing.T) string {
	t.Helper()

	path, err := exec.LookPath("docker")
	if err != nil {
		t.Skipf("the command line of Docker is not available (%v): the acceptance of the real deployment is blocked", err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("resolve the path of the command line of Docker: %v", err)
	}
	return absolute
}

// projectContainers returns the containers of the project of the test.
func projectContainers(t *testing.T, ctx context.Context, local *dockerops.Local, namespace string) []protocol.Container {
	t.Helper()

	containers, err := local.ProjectContainers(ctx, namespace)
	if err != nil {
		t.Fatalf("read the containers of the project %s: %v", namespace, err)
	}
	return containers
}

// containerOfService returns the container of a service of the project.
func containerOfService(t *testing.T, containers []protocol.Container, service string) protocol.Container {
	t.Helper()

	for _, item := range containers {
		if item.Service == service {
			return item
		}
	}
	t.Fatalf("the project has no container of the service %s", service)
	return protocol.Container{}
}

// containerEnvironment returns the value of a variable of the container of a
// service.
func containerEnvironment(t *testing.T, ctx context.Context, api *client.Client, namespace, service string) string {
	t.Helper()

	listed, err := api.ContainerList(ctx, container.ListOptions{
		All: true,
		Filters: filters.NewArgs(filters.Arg(
			"label", dockerops.ComposeProjectLabel+"="+namespace)),
	})
	if err != nil {
		t.Fatalf("list the containers of the project %s: %v", namespace, err)
	}
	for _, summary := range listed {
		details, err := api.ContainerInspect(ctx, summary.ID)
		if err != nil {
			t.Fatalf("inspect the container %s: %v", summary.ID, err)
		}
		if details.Config == nil || details.Config.Labels[dockerops.ComposeServiceLabel] != service {
			continue
		}
		for _, entry := range details.Config.Env {
			name, value, found := strings.Cut(entry, "=")
			if found && name == "GREETING" {
				return value
			}
		}
	}
	t.Fatalf("the container of the service %s holds no value of the variable", service)
	return ""
}

// volumeExists reports whether a volume of the engine exists.
func volumeExists(t *testing.T, ctx context.Context, api *client.Client, name string) bool {
	t.Helper()

	_, err := api.VolumeInspect(ctx, name)
	return err == nil
}

// cleanupProject removes the resources the test created.
//
// Only the resources of the project of the test are touched. The volume of
// the test is removed as well: the test created it and the application of
// the test is gone. A volume that the engine refuses to remove is kept and
// reported.
func cleanupProject(t *testing.T, api *client.Client, local *dockerops.Local, namespace string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	containers, err := local.ProjectContainers(ctx, namespace)
	if err != nil {
		t.Logf("read the containers of the project %s for the cleanup: %v", namespace, err)
	}
	for _, item := range containers {
		if err := api.ContainerRemove(ctx, item.ID, container.RemoveOptions{Force: true}); err != nil {
			t.Logf("remove the container %s of the test: %v", item.ID, err)
		}
	}
	if err := api.NetworkRemove(ctx, namespace+"_default"); err != nil {
		t.Logf("remove the network of the test: %v", err)
	}
	if err := api.VolumeRemove(ctx, namespace+"_data", false); err != nil {
		// The volume of the test is kept and reported: the platform never
		// removes a volume and the test does not remove one that the
		// engine refuses.
		t.Logf("the volume %s_data of the test was kept: %v", namespace, err)
	}
}
