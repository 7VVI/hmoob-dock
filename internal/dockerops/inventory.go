// Package dockerops reads the state of the local Docker engine.
//
// The package only reads: it asks the engine for its info, lists the
// containers and inspects the containers it needs to know. The agent of a
// node uses it to report the inventory of its node to the control端.
package dockerops

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"

	"hmoob-dock/internal/protocol"
)

// Local reads the state of the local Docker engine.
type Local struct {
	client *client.Client
}

// NewLocal connects to the Docker engine that listens at the given socket.
//
// An empty socket uses the address of the environment. A path without a
// scheme names a Unix socket, a path with a scheme is used as it is, so a
// named pipe of Windows works as well.
func NewLocal(socketPath string) (*Local, error) {
	options := []client.Opt{client.WithAPIVersionNegotiation()}

	host := strings.TrimSpace(socketPath)
	switch {
	case host == "":
		// The engine of the environment wins, the client falls back to
		// the default socket of the platform.
		options = append(options, client.WithHostFromEnv())
	case strings.Contains(host, "://"):
		options = append(options, client.WithHost(host))
	case runtime.GOOS == "windows":
		options = append(options, client.WithHost("npipe://"+filepath.ToSlash(host)))
	default:
		options = append(options, client.WithHost("unix://"+host))
	}

	api, err := client.NewClientWithOpts(options...)
	if err != nil {
		return nil, fmt.Errorf("dockerops: connect to the Docker engine: %w", err)
	}
	return &Local{client: api}, nil
}

// Close releases the connection to the Docker engine.
func (l *Local) Close() error {
	return l.client.Close()
}

// Ping reports whether the Docker engine answers.
func (l *Local) Ping(ctx context.Context) error {
	if _, err := l.client.Ping(ctx); err != nil {
		return fmt.Errorf("dockerops: the Docker engine does not answer: %w", err)
	}
	return nil
}

// Identity returns the identity of the local Docker engine.
//
// The engine ID anchors the identity of the node in the control端. The role
// and the cluster come from the Swarm information of the engine: only a
// manager of a cluster can answer for the cluster.
func (l *Local) Identity(ctx context.Context) (protocol.NodeIdentity, error) {
	info, err := l.client.Info(ctx)
	if err != nil {
		return protocol.NodeIdentity{}, fmt.Errorf("dockerops: read the info of the Docker engine: %w", err)
	}
	return identityOf(info), nil
}

// Inventory reads the containers of the local Docker engine.
//
// The engine is asked for its info, for the list of its containers and for
// the details of every container, because the health of a container is
// only part of the list. A container that disappears while the list is
// read is left out, every other problem is reported.
func (l *Local) Inventory(ctx context.Context) (protocol.Inventory, error) {
	info, err := l.client.Info(ctx)
	if err != nil {
		return protocol.Inventory{}, fmt.Errorf("dockerops: read the info of the Docker engine: %w", err)
	}

	listed, err := l.client.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return protocol.Inventory{}, fmt.Errorf("dockerops: list the containers: %w", err)
	}

	containers := make([]protocol.Container, 0, len(listed))
	for _, summary := range listed {
		details, err := l.client.ContainerInspect(ctx, summary.ID)
		if err != nil {
			if errdefs.IsNotFound(err) {
				// The container was removed while the list was read.
				continue
			}
			return protocol.Inventory{}, fmt.Errorf(
				"dockerops: inspect the container %s: %w", summary.ID, err)
		}
		containers = append(containers, containerOf(summary, details))
	}

	return protocol.Inventory{
		Identity:    identityOf(info),
		Containers:  containers,
		CollectedAt: time.Now().UTC(),
	}, nil
}

// identityOf turns the info of the engine into the identity of a node.
func identityOf(info system.Info) protocol.NodeIdentity {
	identity := protocol.NodeIdentity{
		EngineID:    info.ID,
		SwarmNodeID: info.Swarm.NodeID,
		Role:        roleOf(info),
		Hostname:    info.Name,
	}
	if info.Swarm.Cluster != nil {
		identity.SwarmID = info.Swarm.Cluster.ID
	}
	return identity
}

// roleOf returns the role of the node inside its Swarm cluster. A node
// that does not belong to a cluster is a standalone node.
func roleOf(info system.Info) string {
	switch {
	case info.Swarm.ControlAvailable:
		return protocol.RoleManager
	case info.Swarm.NodeID != "":
		return protocol.RoleWorker
	default:
		return protocol.RoleStandalone
	}
}

// containerOf turns the summary of the list and the details of the
// inspection into a container of the inventory.
func containerOf(summary container.Summary, details container.InspectResponse) protocol.Container {
	name := ""
	if len(summary.Names) > 0 {
		name = strings.TrimPrefix(summary.Names[0], "/")
	}

	image := summary.Image
	state := summary.State
	labels := map[string]string{}
	health := ""
	if details.Config != nil {
		if details.Config.Image != "" {
			image = details.Config.Image
		}
		for key, value := range details.Config.Labels {
			labels[key] = value
		}
	}
	if details.State != nil {
		state = details.State.Status
		if details.State.Health != nil {
			health = details.State.Health.Status
		}
	}

	return protocol.Container{
		ID:     summary.ID,
		Name:   name,
		Image:  image,
		State:  state,
		Health: health,
		Labels: labels,
	}
}
