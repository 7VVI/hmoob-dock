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
	"github.com/docker/docker/api/types/filters"
	swarmapi "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"

	"hmoob-dock/internal/protocol"
)

// The labels Compose puts on the resources of a project.
//
// The label of the project names the resources of the application, and the
// label of the service names the container of a service of it.
const (
	// ComposeProjectLabel is the label of the project of Compose.
	ComposeProjectLabel = "com.docker.compose.project"
	// ComposeServiceLabel is the label of the service of Compose.
	ComposeServiceLabel = "com.docker.compose.service"
)

// StackNamespaceLabel is the label Docker puts on the services and the tasks
// of a Swarm stack.
//
// The label names the stack, so the services of a stack are told apart from
// the services of another stack.
const StackNamespaceLabel = "com.docker.stack.namespace"

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
// The engine is asked for its info, for the list of its containers and for the
// details of every container, because the health of a container is only part
// of the list. A container that disappears while the list is read is left out,
// every other problem is reported.
//
// A node that belongs to a Swarm cluster reports the nodes, the services and
// the tasks of the cluster as well. The facts come from the Docker SDK and
// the protocol carries a narrow projection of them: the objects of the SDK
// hold the resources of the cluster and its secrets, so they never leave the
// node.
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

	inventory := protocol.Inventory{
		Identity:    identityOf(info),
		Containers:  containers,
		CollectedAt: time.Now().UTC(),
	}

	if info.Swarm.NodeID == "" {
		// The node does not belong to a cluster, so it has no swarm facts.
		return inventory, nil
	}

	nodes, err := l.SwarmNodes(ctx)
	if err != nil {
		return protocol.Inventory{}, err
	}
	services, err := l.SwarmServices(ctx, "")
	if err != nil {
		return protocol.Inventory{}, err
	}
	tasks, err := l.SwarmTasks(ctx, "")
	if err != nil {
		return protocol.Inventory{}, err
	}
	inventory.SwarmNodes = swarmNodesOf(nodes)
	inventory.SwarmServices = swarmServicesOf(services, tasks, nodes)
	inventory.SwarmTasks = swarmTasksOf(tasks)
	return inventory, nil
}

// SwarmNodes returns the nodes of the Swarm cluster of the local engine.
//
// The nodes are the facts of the Docker SDK. The platform reads them to know
// the role of a node and to know which nodes a stack may run on.
func (l *Local) SwarmNodes(ctx context.Context) ([]swarmapi.Node, error) {
	nodes, err := l.client.NodeList(ctx, swarmapi.NodeListOptions{})
	if err != nil {
		return nil, fmt.Errorf("dockerops: list the nodes of the cluster: %w", err)
	}
	return nodes, nil
}

// SwarmServices returns the services of the Swarm cluster of the local engine.
//
// An empty namespace returns the services of every stack, a namespace returns
// the services of that stack. The services are selected by the label of the
// stack, so a service of another stack is never part of the answer.
func (l *Local) SwarmServices(ctx context.Context, namespace string) ([]swarmapi.Service, error) {
	options := swarmapi.ServiceListOptions{}
	if namespace != "" {
		options.Filters = filters.NewArgs(
			filters.Arg("label", StackNamespaceLabel+"="+namespace))
	}
	services, err := l.client.ServiceList(ctx, options)
	if err != nil {
		return nil, fmt.Errorf("dockerops: list the services of the cluster: %w", err)
	}
	return services, nil
}

// SwarmTasks returns the tasks of the Swarm cluster of the local engine.
//
// An empty namespace returns the tasks of every stack, a namespace returns
// the tasks of that stack. The tasks are selected by the label of the stack,
// so a task of another stack is never part of the answer.
func (l *Local) SwarmTasks(ctx context.Context, namespace string) ([]swarmapi.Task, error) {
	options := swarmapi.TaskListOptions{}
	if namespace != "" {
		options.Filters = filters.NewArgs(
			filters.Arg("label", StackNamespaceLabel+"="+namespace))
	}
	tasks, err := l.client.TaskList(ctx, options)
	if err != nil {
		return nil, fmt.Errorf("dockerops: list the tasks of the cluster: %w", err)
	}
	return tasks, nil
}

// swarmNodesOf projects the nodes of the Docker SDK into the nodes of the
// protocol.
func swarmNodesOf(nodes []swarmapi.Node) []protocol.SwarmNode {
	projected := make([]protocol.SwarmNode, 0, len(nodes))
	for _, node := range nodes {
		projected = append(projected, protocol.SwarmNode{
			ID:           node.ID,
			Hostname:     node.Description.Hostname,
			Role:         string(node.Spec.Role),
			Availability: string(node.Spec.Availability),
			Status:       string(node.Status.State),
		})
	}
	return projected
}

// swarmServicesOf projects the services of the Docker SDK into the services
// of the protocol.
//
// The projection keeps the counts and the labels a stack is made of. The
// specification of the service and the secrets it refers to stay on the node.
func swarmServicesOf(services []swarmapi.Service, tasks []swarmapi.Task, nodes []swarmapi.Node) []protocol.SwarmService {
	projected := make([]protocol.SwarmService, 0, len(services))
	for _, service := range services {
		labels := map[string]string{}
		for key, value := range service.Spec.Labels {
			labels[key] = value
		}
		target, known := effectiveTarget(service, nodes)
		if !known {
			// The target of the service cannot be proven, so the protocol
			// reports the replicas the service asks for and nothing more.
			target = replicasOf(service)
		}
		projected = append(projected, protocol.SwarmService{
			ID:          service.ID,
			Name:        service.Spec.Name,
			Mode:        modeOf(service),
			UpdateState: updateStateOf(service),
			Desired:     target,
			Running:     runningTasksOf(tasks, service.ID),
			Labels:      labels,
		})
	}
	return projected
}

// swarmTasksOf projects the tasks of the Docker SDK into the tasks of the
// protocol.
func swarmTasksOf(tasks []swarmapi.Task) []protocol.SwarmTask {
	projected := make([]protocol.SwarmTask, 0, len(tasks))
	for _, task := range tasks {
		projected = append(projected, protocol.SwarmTask{
			ID:           task.ID,
			ServiceID:    task.ServiceID,
			NodeID:       task.NodeID,
			DesiredState: string(task.DesiredState),
			State:        string(task.Status.State),
			Error:        task.Status.Err,
			Slot:         task.Slot,
		})
	}
	return projected
}

// Host returns the address of the Docker engine the client talks to.
//
// The address belongs to the command line of Docker as well: the command
// line has to reach the same engine as the client of the node.
func (l *Local) Host() string {
	return l.client.DaemonHost()
}

// ContainerMounts returns the mounts of a container.
//
// The mounts of the container of the agent itself prove that the work root
// of the agent is the same path on the host as it is in the container.
func (l *Local) ContainerMounts(ctx context.Context, id string) ([]Mount, error) {
	details, err := l.client.ContainerInspect(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("dockerops: inspect the container %s: %w", id, err)
	}
	mounts := make([]Mount, 0, len(details.Mounts))
	for _, mount := range details.Mounts {
		mounts = append(mounts, Mount{
			Type:        string(mount.Type),
			Source:      mount.Source,
			Destination: mount.Destination,
		})
	}
	return mounts, nil
}

// ProjectContainers returns the containers of a Compose project.
//
// The containers are selected by the label of the project, so a container of
// another project is never part of the answer.
func (l *Local) ProjectContainers(ctx context.Context, namespace string) ([]protocol.Container, error) {
	filter := filters.NewArgs(filters.Arg("label", ComposeProjectLabel+"="+namespace))
	listed, err := l.client.ContainerList(ctx, container.ListOptions{All: true, Filters: filter})
	if err != nil {
		return nil, fmt.Errorf("dockerops: list the containers of the project %s: %w", namespace, err)
	}

	containers := make([]protocol.Container, 0, len(listed))
	for _, summary := range listed {
		details, err := l.client.ContainerInspect(ctx, summary.ID)
		if err != nil {
			if errdefs.IsNotFound(err) {
				// The container was removed while the list is read.
				continue
			}
			return nil, fmt.Errorf("dockerops: inspect the container %s: %w", summary.ID, err)
		}
		containers = append(containers, containerOf(summary, details))
	}
	return containers, nil
}

// identityOf turns the info of the engine into the identity of a node.
func identityOf(info system.Info) protocol.NodeIdentity {
	identity := protocol.NodeIdentity{
		EngineID:      info.ID,
		SwarmNodeID:   info.Swarm.NodeID,
		Role:          roleOf(info),
		Hostname:      info.Name,
		ServerVersion: info.ServerVersion,
		OSType:        info.OSType,
		Architecture:  info.Architecture,
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
	restarts := 0
	var exitCode *int
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
		if state == "exited" || state == "dead" {
			// The exit code only means something when the container
			// ended.
			code := details.State.ExitCode
			exitCode = &code
		}
	}
	if details.ContainerJSONBase != nil {
		restarts = details.RestartCount
	}

	return protocol.Container{
		ID:           summary.ID,
		Name:         name,
		Image:        image,
		State:        state,
		Health:       health,
		Labels:       labels,
		Service:      labels[ComposeServiceLabel],
		ExitCode:     exitCode,
		RestartCount: restarts,
	}
}
