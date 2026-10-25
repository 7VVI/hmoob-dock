//go:build integration

// Package integration holds the tests that need a real Docker engine.
//
// The tests of this package only read: they never create, change or remove
// a container of the engine. They run with the tag "integration", so the
// ordinary test run does not need a Docker engine:
//
//	go test -tags=integration ./tests/integration -count=1
package integration

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"

	"hmoob-dock/internal/dockerops"
)

// TestAgentInventory reads the inventory of the local Docker engine and
// compares it with the engine itself.
//
// The test is the acceptance of the real inventory: the number of the
// containers, the identity of the engine and the state of every container
// have to be the ones of Docker. A mocked inventory does not prove
// anything here.
func TestAgentInventory(t *testing.T) {
	host := dockerHost(t)

	local, err := dockerops.NewLocal(host)
	if err != nil {
		t.Fatalf("connect to the Docker engine %s: %v", host, err)
	}
	defer local.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// The engine has to answer before the inventory is read. A missing
	// engine blocks the acceptance, it is not a failure of the inventory.
	if err := local.Ping(ctx); err != nil {
		t.Skipf("the Docker engine %s is not available (%v): the acceptance of the real inventory is blocked", host, err)
	}

	inventory, err := local.Inventory(ctx)
	if err != nil {
		t.Fatalf("read the inventory: %v", err)
	}

	oracle, err := client.NewClientWithOpts(client.WithHost(host), client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("connect to the Docker engine %s: %v", host, err)
	}
	defer oracle.Close()

	info, err := oracle.Info(ctx)
	if err != nil {
		t.Fatalf("read the info of the Docker engine: %v", err)
	}
	listed, err := oracle.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		t.Fatalf("list the containers of the Docker engine: %v", err)
	}

	// The identity of the node is the identity of the engine.
	if inventory.Identity.EngineID != info.ID {
		t.Fatalf("the engine ID = %q, want %q", inventory.Identity.EngineID, info.ID)
	}
	if inventory.Identity.Hostname != info.Name {
		t.Fatalf("the host name = %q, want %q", inventory.Identity.Hostname, info.Name)
	}
	if inventory.Identity.SwarmNodeID != info.Swarm.NodeID {
		t.Fatalf("the node ID of the cluster = %q, want %q", inventory.Identity.SwarmNodeID, info.Swarm.NodeID)
	}
	if info.Swarm.Cluster != nil && inventory.Identity.SwarmID != info.Swarm.Cluster.ID {
		t.Fatalf("the ID of the cluster = %q, want %q", inventory.Identity.SwarmID, info.Swarm.Cluster.ID)
	}

	// The number of the containers is the number of Docker.
	if len(inventory.Containers) != len(listed) {
		t.Fatalf("the inventory holds %d containers, want %d", len(inventory.Containers), len(listed))
	}

	// Every container of the inventory is a container of Docker and has
	// the same name, the same image and the same state.
	known := make(map[string]container.Summary, len(listed))
	for _, summary := range listed {
		known[summary.ID] = summary
	}
	for _, item := range inventory.Containers {
		summary, ok := known[item.ID]
		if !ok {
			t.Fatalf("the container %s is not a container of the Docker engine", item.ID)
		}
		name := ""
		if len(summary.Names) > 0 {
			name = strings.TrimPrefix(summary.Names[0], "/")
		}
		if item.Name != name {
			t.Fatalf("the name of the container %s = %q, want %q", item.ID, item.Name, name)
		}
		if item.State != summary.State {
			t.Fatalf("the state of the container %s = %q, want %q", item.ID, item.State, summary.State)
		}
		if item.Image == "" {
			t.Fatalf("the container %s has no image", item.ID)
		}
	}

	// The command line of Docker is the second opinion about the number
	// of the containers.
	if count, ok := containersOfTheCommandLine(t); ok && count != len(inventory.Containers) {
		t.Fatalf("the command line of Docker reports %d containers, want %d", count, len(inventory.Containers))
	}

	t.Logf("the engine %s holds %d containers", info.ID, len(inventory.Containers))
}

// dockerHost returns the address of the Docker engine of the test.
//
// The environment wins, then the context of the Docker command line, then
// the default of the client library.
func dockerHost(t *testing.T) string {
	t.Helper()

	if host := strings.TrimSpace(os.Getenv("DOCKER_HOST")); host != "" {
		return host
	}
	output, err := exec.Command("docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
	if err == nil {
		if host := strings.TrimSpace(string(output)); host != "" {
			return host
		}
	}
	return ""
}

// containersOfTheCommandLine counts the containers of the engine with the
// command line of Docker. The second return value reports whether the
// command line was available.
func containersOfTheCommandLine(t *testing.T) (int, bool) {
	t.Helper()

	if _, err := exec.LookPath("docker"); err != nil {
		return 0, false
	}
	output, err := exec.Command("docker", "ps", "-a", "--format", "{{.ID}}").Output()
	if err != nil {
		return 0, false
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count, true
}
