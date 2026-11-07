package protocol

import "time"

// The roles a node can have. The role comes from the Docker engine of the
// node: a manager of a Swarm cluster can answer for the cluster, a worker
// cannot.
const (
	// RoleManager marks a node that manages a Swarm cluster.
	RoleManager = "manager"
	// RoleWorker marks a node that runs the tasks of a Swarm cluster.
	RoleWorker = "worker"
	// RoleStandalone marks a node that is not part of a Swarm cluster.
	RoleStandalone = "standalone"
)

// NodeIdentity is the identity of a Docker engine.
//
// The engine ID anchors the identity: a node keeps the identity of its
// first enrollment, so a reported name alone never replaces a node.
type NodeIdentity struct {
	// EngineID is the ID of the Docker engine.
	EngineID string `json:"engine_id"`
	// SwarmID is the ID of the Swarm cluster the node belongs to.
	SwarmID string `json:"swarm_id,omitempty"`
	// SwarmNodeID is the ID of the node inside its Swarm cluster.
	SwarmNodeID string `json:"swarm_node_id,omitempty"`
	// Role is the role of the node inside its Swarm cluster.
	Role string `json:"role,omitempty"`
	// Hostname is the name of the host of the node.
	Hostname string `json:"hostname,omitempty"`
	// ServerVersion is the version of the Docker engine of the node, as
	// the engine reported it with its last inventory.
	ServerVersion string `json:"server_version,omitempty"`
	// OSType is the type of the operating system of the engine, for
	// example linux or windows.
	OSType string `json:"os_type,omitempty"`
	// Architecture is the architecture of the engine host, for example
	// x86_64.
	Architecture string `json:"architecture,omitempty"`
}

// PortMapping is one port of a container or of a service.
//
// A port that the node publishes has a public port; a port that is only
// exposed inside its network keeps the public port empty.
type PortMapping struct {
	// PrivatePort is the port inside the container or the service.
	PrivatePort uint16 `json:"private_port"`
	// PublicPort is the published port, zero when the port is not
	// published.
	PublicPort uint16 `json:"public_port,omitempty"`
	// Type is the protocol of the port, for example tcp or udp.
	Type string `json:"type,omitempty"`
	// IP is the address the port is published on, when the node names one.
	IP string `json:"ip,omitempty"`
}

// Mount is a mount of a container.
type Mount struct {
	// Type is the type of the mount, for example volume, bind or tmpfs.
	Type string `json:"type,omitempty"`
	// Source is the source of the mount on the node or the name of the
	// volume.
	Source string `json:"source,omitempty"`
	// Destination is the path inside the container.
	Destination string `json:"destination,omitempty"`
	// Mode is the mode of the mount, for example rw.
	Mode string `json:"mode,omitempty"`
	// ReadWrite reports whether the mount is writable.
	ReadWrite bool `json:"read_write,omitempty"`
}

// Container is a container of an inventory.
type Container struct {
	// ID is the ID of the container.
	ID string `json:"id"`
	// Name is the name of the container without the leading slash.
	Name string `json:"name"`
	// Image is the image the container runs.
	Image string `json:"image"`
	// State is the state of the container, for example running.
	State string `json:"state"`
	// Health is the state of the health check of the container. It is
	// empty when the container has no health check.
	Health string `json:"health,omitempty"`
	// Labels are the labels of the container.
	Labels map[string]string `json:"labels,omitempty"`
	// Service is the name of the Compose service the container belongs to.
	// It is empty when the container does not belong to a Compose project.
	Service string `json:"service,omitempty"`
	// ExitCode is the exit code of a container that ended. It is empty
	// while the container runs.
	ExitCode *int `json:"exit_code,omitempty"`
	// RestartCount is the number of times the container was restarted.
	RestartCount int `json:"restart_count,omitempty"`
	// Created is the time Docker created the container.
	Created string `json:"created,omitempty"`
	// StartedAt is the time the container last started.
	StartedAt string `json:"started_at,omitempty"`
	// IP is the address of the container in its first network.
	IP string `json:"ip,omitempty"`
	// Ports are the ports of the container.
	Ports []PortMapping `json:"ports,omitempty"`
	// Mounts are the mounts of the container.
	Mounts []Mount `json:"mounts,omitempty"`
}

// SwarmNode is a node of a Swarm cluster.
//
// The fields are the ones the platform shows and reasons about. The object of
// the Docker SDK is not part of the protocol: it holds the resources of the
// node and the secrets of the engine.
type SwarmNode struct {
	// ID is the ID of the node inside of its Swarm cluster.
	ID string `json:"id"`
	// Hostname is the name of the host of the node.
	Hostname string `json:"hostname,omitempty"`
	// Role is the role of the node inside of the cluster, manager or worker.
	Role string `json:"role,omitempty"`
	// Availability is the state of the scheduler for the node, for example
	// active, pause or drain.
	Availability string `json:"availability,omitempty"`
	// Status is the state of the node as the manager sees it, for example
	// ready or down.
	Status string `json:"status,omitempty"`
	// Addr is the address the node reports to the cluster.
	Addr string `json:"addr,omitempty"`
	// CPUs is the number of the cores the node offers the cluster.
	CPUs float64 `json:"cpus"`
	// MemoryBytes is the memory the node offers the cluster, in bytes.
	MemoryBytes int64 `json:"memory_bytes,omitempty"`
	// EngineVersion is the version of the Docker engine of the node.
	EngineVersion string `json:"engine_version,omitempty"`
}

// SwarmService is a service of a Swarm stack.
//
// The fields are the ones the platform shows and reasons about. The object of
// the Docker SDK is not part of the protocol: it holds the specification of
// the service and the secrets it refers to.
type SwarmService struct {
	// ID is the ID of the service.
	ID string `json:"id"`
	// Name is the name of the service inside of its stack.
	Name string `json:"name"`
	// Mode is the mode of the service, replicated or global.
	Mode string `json:"mode,omitempty"`
	// UpdateState is the state of the update of the service, for example
	// updating, paused, completed or rollback_completed.
	UpdateState string `json:"update_state,omitempty"`
	// Desired is the number of the tasks the service wants to run.
	Desired int `json:"desired"`
	// Running is the number of the tasks that run.
	Running int `json:"running"`
	// Labels are the labels of the service.
	Labels map[string]string `json:"labels,omitempty"`
	// Image is the image the tasks of the service run.
	Image string `json:"image,omitempty"`
	// Ports are the ports the service publishes.
	Ports []PortMapping `json:"ports,omitempty"`
	// UpdatedAt is the time the service was updated last.
	UpdatedAt string `json:"updated_at,omitempty"`
}

// Network is a network of the Docker engine of a node.
type Network struct {
	// ID is the ID of the network.
	ID string `json:"id"`
	// Name is the name of the network.
	Name string `json:"name"`
	// Driver is the driver of the network, for example bridge or overlay.
	Driver string `json:"driver,omitempty"`
	// Scope is the scope of the network, for example local or swarm.
	Scope string `json:"scope,omitempty"`
	// Internal reports whether the network is internal.
	Internal bool `json:"internal,omitempty"`
	// Attachable reports whether a container may attach to the network.
	Attachable bool `json:"attachable,omitempty"`
	// Created is the time the network was created.
	Created string `json:"created,omitempty"`
	// Containers is the number of the containers attached to the network.
	Containers int `json:"containers"`
	// Labels are the labels of the network. The label of a Compose project
	// or a Swarm stack names the stack the network belongs to.
	Labels map[string]string `json:"labels,omitempty"`
	// IPAMDriver is the driver of the address management of the network.
	IPAMDriver string `json:"ipam_driver,omitempty"`
	// IPAMSubnet is the IPv4 subnet the network serves.
	IPAMSubnet string `json:"ipam_subnet,omitempty"`
	// IPAMGateway is the IPv4 gateway of the network.
	IPAMGateway string `json:"ipam_gateway,omitempty"`
	// IPAMIPv6Subnet is the IPv6 subnet the network serves.
	IPAMIPv6Subnet string `json:"ipam_ipv6_subnet,omitempty"`
	// IPAMIPv6Gateway is the IPv6 gateway of the network.
	IPAMIPv6Gateway string `json:"ipam_ipv6_gateway,omitempty"`
}

// Image is an image in the local store of a node.
//
// The fields are the ones the platform shows. The history and the build
// steps of the image stay on the node.
type Image struct {
	// ID is the ID of the image, for example sha256:....
	ID string `json:"id"`
	// Tags are the references the image carries, for example nginx:alpine.
	// An image that no reference names carries none.
	Tags []string `json:"tags,omitempty"`
	// Size is the size of the image on the disk of the node, in bytes.
	Size int64 `json:"size"`
	// Created is the time the image was built or pulled.
	Created string `json:"created,omitempty"`
	// Labels are the labels of the image.
	Labels map[string]string `json:"labels,omitempty"`
}

// Volume is a named volume in the local store of a node.
//
// The fields are the ones the platform shows. The content of the volume
// stays on the node.
type Volume struct {
	// Name is the name of the volume.
	Name string `json:"name"`
	// Driver is the driver of the volume, for example local.
	Driver string `json:"driver,omitempty"`
	// Mountpoint is the path on the node the volume lives at.
	Mountpoint string `json:"mountpoint,omitempty"`
	// Scope is the scope of the volume, for example local.
	Scope string `json:"scope,omitempty"`
	// Created is the time the volume was created.
	Created string `json:"created,omitempty"`
	// Labels are the labels of the volume. The label of a Compose project
	// or a Swarm stack names the stack the volume belongs to.
	Labels map[string]string `json:"labels,omitempty"`
}

// SwarmTask is a task of a Swarm service.
//
// The fields are the ones the platform shows and reasons about. The object of
// the Docker SDK is not part of the protocol: it holds the specification of
// the task and the environment of its container.
type SwarmTask struct {
	// ID is the ID of the task.
	ID string `json:"id"`
	// ServiceID is the ID of the service the task belongs to.
	ServiceID string `json:"service_id"`
	// NodeID is the ID of the node the task runs on.
	NodeID string `json:"node_id,omitempty"`
	// DesiredState is the state the cluster wants the task to reach.
	DesiredState string `json:"desired_state,omitempty"`
	// State is the state of the task.
	State string `json:"state,omitempty"`
	// Error is the message of the failure of the task.
	Error string `json:"error,omitempty"`
	// Slot is the position of the task inside of its service.
	Slot int `json:"slot,omitempty"`
}

// Inventory is the state of a node at a point in time.
type Inventory struct {
	// Identity is the identity of the node the inventory belongs to.
	Identity NodeIdentity `json:"identity"`
	// Containers are the containers of the node.
	Containers []Container `json:"containers"`
	// SwarmNodes are the nodes of the Swarm cluster the node belongs to.
	SwarmNodes []SwarmNode `json:"swarm_nodes,omitempty"`
	// SwarmServices are the services of the Swarm cluster the node belongs
	// to.
	SwarmServices []SwarmService `json:"swarm_services,omitempty"`
	// SwarmTasks are the tasks of the Swarm cluster the node belongs to.
	SwarmTasks []SwarmTask `json:"swarm_tasks,omitempty"`
	// Networks are the networks of the Docker engine of the node.
	Networks []Network `json:"networks,omitempty"`
	// Images are the images in the local store of the node.
	Images []Image `json:"images,omitempty"`
	// Volumes are the named volumes in the local store of the node.
	Volumes []Volume `json:"volumes,omitempty"`
	// CollectedAt is the time the inventory was collected.
	CollectedAt time.Time `json:"collected_at"`
}

// Hello opens the conversation of a connection. It names the identity of
// the agent, so the control端 can check it against the enrolled node.
type Hello struct {
	// Identity is the identity of the agent.
	Identity NodeIdentity `json:"identity"`
	// AgentVersion is the version of the agent that reports. It is
	// informational: the control端 stores it with the node record.
	AgentVersion string `json:"agent_version,omitempty"`
}

// Heartbeat reports that the agent is alive.
type Heartbeat struct {
	// SentAt is the time the agent sent the heartbeat.
	SentAt time.Time `json:"sent_at"`
}

// InventoryBatch is one batch of an inventory snapshot.
//
// A snapshot that does not fit into a single message is sent in several
// batches. The control端 holds the snapshot as incomplete until every
// batch of it arrived, so a truncated inventory never looks like a
// complete one.
type InventoryBatch struct {
	// SnapshotID names the snapshot the batch belongs to.
	SnapshotID string `json:"snapshot_id"`
	// BatchIndex is the position of the batch in the snapshot.
	BatchIndex int `json:"batch_index"`
	// BatchCount is the number of batches of the snapshot.
	BatchCount int `json:"batch_count"`
	// Complete marks the last batch of the snapshot.
	Complete bool `json:"complete"`
	// Identity is the identity of the node the snapshot belongs to.
	Identity NodeIdentity `json:"identity"`
	// Containers are the containers of the batch.
	Containers []Container `json:"containers"`
	// SwarmNodes are the nodes of the Swarm cluster. They travel in the
	// first batch of the snapshot, so the other batches stay small.
	SwarmNodes []SwarmNode `json:"swarm_nodes,omitempty"`
	// SwarmServices are the services of the Swarm cluster. They travel in
	// the first batch of the snapshot.
	SwarmServices []SwarmService `json:"swarm_services,omitempty"`
	// SwarmTasks are the tasks of the Swarm cluster. They travel in the
	// first batch of the snapshot.
	SwarmTasks []SwarmTask `json:"swarm_tasks,omitempty"`
	// Networks are the networks of the engine. They travel in the first
	// batch of the snapshot.
	Networks []Network `json:"networks,omitempty"`
	// Images are the images of the engine. They travel in the first batch
	// of the snapshot.
	Images []Image `json:"images,omitempty"`
	// Volumes are the named volumes of the engine. They travel in the
	// first batch of the snapshot.
	Volumes []Volume `json:"volumes,omitempty"`
	// CollectedAt is the time the snapshot was collected.
	CollectedAt time.Time `json:"collected_at"`
}
