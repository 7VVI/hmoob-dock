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
}

// Inventory is the state of a node at a point in time.
type Inventory struct {
	// Identity is the identity of the node the inventory belongs to.
	Identity NodeIdentity `json:"identity"`
	// Containers are the containers of the node.
	Containers []Container `json:"containers"`
	// CollectedAt is the time the inventory was collected.
	CollectedAt time.Time `json:"collected_at"`
}

// Hello opens the conversation of a connection. It names the identity of
// the agent, so the control端 can check it against the enrolled node.
type Hello struct {
	// Identity is the identity of the agent.
	Identity NodeIdentity `json:"identity"`
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
	// CollectedAt is the time the snapshot was collected.
	CollectedAt time.Time `json:"collected_at"`
}
