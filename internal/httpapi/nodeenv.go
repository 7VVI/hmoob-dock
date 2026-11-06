package httpapi

import (
	"context"
	"encoding/json"
	"log"
	"strings"

	"hmoob-dock/internal/protocol"
)

// handleNodeEnvironment stores the environment a node reported with its
// inventory.
//
// The write keeps only the facts of the report and only when they differ
// from the stored ones: an inventory arrives in several batches and on
// every report of the agent, so a node that reports the same environment
// again writes nothing. A node that is away keeps the facts of its last
// report, which is what the page of the node shows.
func (s *Server) handleNodeEnvironment(_ context.Context, nodeID string, envelope protocol.Envelope) error {
	var batch protocol.InventoryBatch
	if err := json.Unmarshal(envelope.Payload, &batch); err != nil {
		return nil
	}
	// The role and the cluster of the connection are the ones of the
	// enrollment; the environment is the report of the engine.
	status, ok := s.hub.Status(nodeID)
	if !ok {
		return nil
	}
	identity := status.Identity
	if strings.TrimSpace(identity.ServerVersion) == "" &&
		strings.TrimSpace(identity.OSType) == "" &&
		strings.TrimSpace(identity.Architecture) == "" &&
		strings.TrimSpace(status.AgentVersion) == "" {
		return nil
	}

	record, err := s.enrollment.NodeRecord(s.ctx, nodeID)
	if err != nil {
		return nil
	}
	if record.DockerVersion == identity.ServerVersion &&
		record.OSType == identity.OSType &&
		record.OSArch == identity.Architecture &&
		record.AgentVersion == status.AgentVersion {
		return nil
	}
	if err := s.enrollment.UpdateNodeEnvironment(s.ctx, nodeID, identity, status.AgentVersion); err != nil {
		log.Printf("control: store the environment of the node %s: %v", nodeID, err)
	}
	return nil
}

// publishContainerEvent tells the pages of a node that its containers
// changed.
//
// The event carries no state of its own: the page reads the containers
// again when it receives the event. The object of the event is the node,
// so a page of one node is not woken by the changes of another one.
func (s *Server) publishContainerEvent(_ context.Context, nodeID string, envelope protocol.Envelope) error {
	var event protocol.ContainerEvent
	if err := json.Unmarshal(envelope.Payload, &event); err != nil {
		return nil
	}
	s.events.publish(Event{Type: EventContainer, ObjectID: nodeID})
	return nil
}
