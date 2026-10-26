//go:build integration

package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/filters"
	swarmapi "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"

	"hmoob-dock/internal/dockerops"
	"hmoob-dock/internal/model"
	"hmoob-dock/internal/protocol"
)

// TestSwarmDeployment deploys an application as a Docker Stack, scales it,
// updates its image and proves that a failing update never reports a
// successful deployment.
//
// The test needs an authorized cluster with one manager and two workers. It
// never initializes a cluster and never changes the cluster of the user: a
// node that is not a manager of a cluster skips the test and reports the
// reason, so the acceptance of the real stack stays unproven instead of
// being faked.
func TestSwarmDeployment(t *testing.T) {
	host := dockerHost(t)

	local, err := dockerops.NewLocal(host)
	if err != nil {
		t.Fatalf("connect to the Docker engine %s: %v", host, err)
	}
	defer local.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	// The engine has to answer before the test deploys anything. A missing
	// engine blocks the acceptance, it is not a failure of the deployment.
	if err := local.Ping(ctx); err != nil {
		t.Skipf("the Docker engine %s is not available (%v): the acceptance of the real stack is blocked", host, err)
	}

	identity, err := local.Identity(ctx)
	if err != nil {
		t.Fatalf("read the identity of the node: %v", err)
	}
	if identity.Role != protocol.RoleManager {
		t.Skipf("the node %s is not a manager of a Swarm cluster (%s): the acceptance of the real stack is blocked",
			identity.Hostname, identity.Role)
	}
	if identity.SwarmID == "" {
		t.Skip("the node does not belong to a Swarm cluster: the acceptance of the real stack is blocked")
	}

	nodes, err := local.SwarmNodes(ctx)
	if err != nil {
		t.Fatalf("read the nodes of the cluster: %v", err)
	}
	workers := 0
	for _, node := range nodes {
		if node.Spec.Role == swarmapi.NodeRoleWorker {
			workers++
		}
	}
	if workers < 2 {
		t.Skipf("the cluster holds %d workers, the acceptance needs two: the acceptance of the real stack is blocked", workers)
	}

	// The command line of Docker runs on the machine of the test, so its
	// path is named here.
	program := dockerProgram(t)
	t.Setenv(dockerops.DockerProgramEnv, program)

	suffix := randomSuffix(t)
	namespace := "hmoobtest" + suffix
	appID := "apptest" + suffix
	root := t.TempDir()
	executor := dockerops.NewSwarm(root, dockerops.ExecRunner{}, local)

	api, err := client.NewClientWithOpts(client.WithHost(host), client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("connect to the Docker engine %s: %v", host, err)
	}
	defer api.Close()

	t.Cleanup(func() {
		cleanupStack(t, ctx, api, namespace)
	})

	// The first deployment starts a stack with two replicas of a service.
	first := swarmCommand(appID, namespace, identity.SwarmID, firstStack(namespace))
	result := deploy(t, ctx, executor, first)
	if result.State != model.Succeeded {
		t.Fatalf("the state of the first deployment = %q (%s), want %q", result.State, result.Message, model.Succeeded)
	}

	services := stackServices(t, ctx, local, namespace)
	service := services["demo_api"]
	if service.ID == "" {
		t.Fatalf("the stack holds no service api: %v", services)
	}
	if service.Desired != 2 {
		t.Fatalf("the target of the service api = %d, want 2", service.Desired)
	}
	if service.Running != 2 {
		t.Fatalf("the running tasks of the service api = %d, want 2", service.Running)
	}
	if service.Mode != "replicated" {
		t.Fatalf("the mode of the service api = %q, want %q", service.Mode, "replicated")
	}

	// The tasks of the service run on the nodes of the cluster, and the
	// control端 shows the node of every task.
	tasks := stackTasks(t, ctx, local, namespace)
	running := 0
	for _, task := range tasks {
		if task.ServiceID == service.ID && task.State == "running" {
			running++
			if task.NodeID == "" {
				t.Fatalf("the task %s runs on no node", task.ID)
			}
		}
	}
	if running != 2 {
		t.Fatalf("the running tasks of the service api = %d, want 2", running)
	}

	// The second deployment scales the service up.
	second := swarmCommand(appID, namespace, identity.SwarmID, secondStack(namespace, 3))
	result = deploy(t, ctx, executor, second)
	if result.State != model.Succeeded {
		t.Fatalf("the state of the second deployment = %q (%s), want %q", result.State, result.Message, model.Succeeded)
	}
	services = stackServices(t, ctx, local, namespace)
	if services["demo_api"].Running != 3 {
		t.Fatalf("the running tasks of the service api = %d, want 3", services["demo_api"].Running)
	}

	// The third deployment changes the image of the service. The stack runs
	// the new version, and the platform proves it with the digest of the
	// configuration in the labels of the service.
	third := swarmCommand(appID, namespace, identity.SwarmID, thirdStack(namespace))
	result = deploy(t, ctx, executor, third)
	if result.State != model.Succeeded {
		t.Fatalf("the state of the third deployment = %q (%s), want %q", result.State, result.Message, model.Succeeded)
	}
	services = stackServices(t, ctx, local, namespace)
	if services["demo_api"].Labels[dockerops.SwarmVersionLabel] == "" {
		t.Fatalf("the service api carries no version: %v", services["demo_api"].Labels)
	}

	// The fourth deployment runs a version that fails. The platform never
	// reports a failed version as a successful deployment, and the cluster
	// rolls the change back.
	fourth := swarmCommand(appID, namespace, identity.SwarmID, failingStack(namespace))
	result = deploy(t, ctx, executor, fourth)
	if result.State == model.Succeeded {
		t.Fatalf("the state of the fourth deployment = %q, want a failure", result.State)
	}
	if !result.RolledBack {
		t.Fatalf("the rollback of the cluster was not reported: %+v", result)
	}
}

// swarmCommand builds the command of a deployment of a stack.
func swarmCommand(appID, namespace, swarmID, document string) model.Command {
	return model.Command{
		Task: model.Task{ID: "task-" + namespace, AppID: appID, Digest: "digest-" + namespace},
		App: model.App{
			ID:        appID,
			Name:      "swarm test",
			Mode:      "swarm",
			TargetID:  swarmID,
			Namespace: namespace,
		},
		Config: model.Config{
			YAML:      document,
			Variables: map[string]string{},
		},
		ExecutionTimeout: 5 * time.Minute,
		VerifyTimeout:    5 * time.Minute,
	}
}

// firstStack is the configuration of the first deployment.
func firstStack(namespace string) string {
	return fmt.Sprintf(`name: %s
services:
  api:
    image: nginx:alpine
    deploy:
      replicas: 2
`, namespace)
}

// secondStack is the configuration of the second deployment: the service runs
// three replicas.
func secondStack(namespace string, replicas int) string {
	return fmt.Sprintf(`name: %s
services:
  api:
    image: nginx:alpine
    deploy:
      replicas: %d
`, namespace, replicas)
}

// thirdStack is the configuration of the third deployment: the image of the
// service changed.
func thirdStack(namespace string) string {
	return fmt.Sprintf(`name: %s
services:
  api:
    image: nginx:1.27-alpine
    deploy:
      replicas: 3
`, namespace)
}

// failingStack is the configuration of the fourth deployment: the task of
// the service ends with an error, so the cluster rolls the change back.
func failingStack(namespace string) string {
	return fmt.Sprintf(`name: %s
services:
  api:
    image: nginx:alpine
    command: ["sh", "-c", "exit 1"]
    deploy:
      replicas: 3
      update_config:
        parallelism: 1
        delay: 2s
        failure_action: pause
        monitor: 10s
        order: stop-first
      rollback_config:
        parallelism: 1
        delay: 2s
        failure_action: pause
        monitor: 10s
        order: stop-first
`, namespace)
}

// stackServices returns the services of the stack of the test.
//
// The services come from the inventory of the node, so the test proves the
// facts the control端 shows.
func stackServices(t *testing.T, ctx context.Context, local *dockerops.Local, namespace string) map[string]protocol.SwarmService {
	t.Helper()

	inventory, err := local.Inventory(ctx)
	if err != nil {
		t.Fatalf("read the inventory of the node: %v", err)
	}
	byName := map[string]protocol.SwarmService{}
	for _, service := range inventory.SwarmServices {
		if !strings.HasPrefix(service.Name, namespace+"_") {
			continue
		}
		byName[strings.TrimPrefix(service.Name, namespace+"_")] = service
	}
	return byName
}

// stackTasks returns the tasks of the stack of the test.
func stackTasks(t *testing.T, ctx context.Context, local *dockerops.Local, namespace string) []protocol.SwarmTask {
	t.Helper()

	inventory, err := local.Inventory(ctx)
	if err != nil {
		t.Fatalf("read the inventory of the node: %v", err)
	}
	return inventory.SwarmTasks
}

// cleanupStack removes the services of the stack of the test.
//
// Only the services of the stack of the test are touched: the stack carries
// the label of its own namespace, and the cluster of the user keeps its
// other stacks.
func cleanupStack(t *testing.T, ctx context.Context, api *client.Client, namespace string) {
	t.Helper()

	services, err := api.ServiceList(ctx, swarmapi.ServiceListOptions{
		Filters: filters.NewArgs(filters.Arg(
			"label", dockerops.StackNamespaceLabel+"="+namespace)),
	})
	if err != nil {
		t.Logf("read the services of the stack %s for the cleanup: %v", namespace, err)
		return
	}
	for _, service := range services {
		if err := api.ServiceRemove(ctx, service.ID); err != nil {
			t.Logf("remove the service %s of the test: %v", service.Spec.Name, err)
		}
	}
}
