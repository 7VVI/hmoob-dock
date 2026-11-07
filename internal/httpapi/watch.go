package httpapi

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"

	"hmoob-dock/internal/alerts"
	"hmoob-dock/internal/dockerops"
	"hmoob-dock/internal/metrics"
	"hmoob-dock/internal/protocol"
)

// The limits of the observation of the nodes.
const (
	// agentOfflineLimit is the time a node may stay away before it counts
	// as offline. The decision is made by the control端, on the time it
	// received the last heartbeat, so a node with a wrong clock is not
	// offline.
	agentOfflineLimit = 60 * time.Second
	// agentOfflineCheckInterval is the interval at which the control端
	// looks at the heartbeats of the nodes.
	agentOfflineCheckInterval = 10 * time.Second
)

// The names of the metrics a resource rule may watch.
const (
	metricCPUPercent    = "cpu_percent"
	metricMemoryBytes   = "memory_bytes"
	metricNetRXPerSec   = "net_rx_per_sec"
	metricNetTXPerSec   = "net_tx_per_sec"
	metricIOReadPerSec  = "io_read_per_sec"
	metricIOWritePerSec = "io_write_per_sec"
)

// metricValue reads the value of a metric of a sample.
//
// The second answer reports whether the sample carries that metric at all:
// a rule that watches a metric the platform does not measure never fires.
func metricValue(values metrics.Values, metric string) (float64, bool) {
	switch metric {
	case metricCPUPercent:
		return values.CPUPercent, true
	case metricMemoryBytes:
		return values.MemoryBytes, true
	case metricNetRXPerSec:
		return values.NetRXPerSec, true
	case metricNetTXPerSec:
		return values.NetTXPerSec, true
	case metricIOReadPerSec:
		return values.IOReadPerSec, true
	case metricIOWritePerSec:
		return values.IOWritePerSec, true
	}
	return 0, false
}

// watchMetrics evaluates the samples of the containers against the rules of
// the resources.
//
// Only a valid sample opens or continues an event: a gap of the data is
// neither an anomaly nor a recovery.
func (s *Server) watchMetrics(point metrics.Point) {
	ctx := s.ctx
	sample := alerts.ResourceSample{
		ObjectID: point.ContainerID,
		At:       point.At,
		Valid:    point.Valid,
	}
	// Every rule of the container is evaluated with its own metric, so
	// the rules are read once per sample.
	rules, err := s.alerts.Rules(ctx)
	if err != nil {
		log.Printf("control: read the alert rules: %v", err)
		return
	}
	for _, rule := range rules {
		if rule.Kind != alerts.KindResource || !rule.Enabled {
			continue
		}
		if !s.resourceRuleMatches(rule, point) {
			continue
		}
		value, ok := metricValue(point.Values, rule.Metric)
		if !ok {
			continue
		}
		sample.Metric = rule.Metric
		sample.Value = value
		if _, err := s.alerts.Observe(ctx, rule.ID, alerts.Observation{
			ObjectID: sample.ObjectID,
			At:       sample.At,
			Value:    sample.Value,
			Valid:    sample.Valid,
		}); err != nil {
			log.Printf("control: evaluate the rule %s: %v", rule.ID, err)
		}
	}
}

// resourceRuleMatches reports whether a sample of a container falls into
// the scope of the rule.
//
// A rule of all watches every container the platform sees, a rule of one
// container watches the object of its scope, and a rule of a namespace
// watches the containers of the namespace the rule names. A container whose
// namespace was unknown never falls into the scope of a namespace rule,
// because the platform cannot tell what the container belonged to.
func (s *Server) resourceRuleMatches(rule alerts.Rule, point metrics.Point) bool {
	switch rule.ScopeKind {
	case alerts.ScopeAll:
		return true
	case alerts.ScopeNamespace:
		namespace := s.containerNamespace(point.NodeID, point.ContainerID)
		return namespace != "" && rule.ScopeID == namespace
	default:
		return rule.ScopeID != "" && rule.ScopeID == point.ContainerID
	}
}

// containerNamespace names the Compose project or Swarm stack a container
// belongs to, from the last inventory of its node.
//
// The inventory is the memory of the node: the read never reaches the agent
// and an agent that is away still answers with the inventory it reported
// last. An empty name means the inventory does not know the container.
func (s *Server) containerNamespace(nodeID, containerID string) string {
	inventory, err := s.hub.ReadInventory(s.ctx, "compose", nodeID)
	if err != nil {
		return ""
	}
	for _, container := range inventory.Containers {
		if container.ID != containerID {
			continue
		}
		if namespace := container.Labels[composeProjectLabel]; namespace != "" {
			return namespace
		}
		return container.Labels[dockerops.StackNamespaceLabel]
	}
	return ""
}

// watchHeartbeats looks at the heartbeats of the nodes.
//
// The decision uses the time the control端 received the last heartbeat, not
// the time the agent claims. A node that comes back produces the recovery of
// its event.
func (s *Server) watchHeartbeats(ctx context.Context) {
	ticker := time.NewTicker(agentOfflineCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkHeartbeats(ctx)
		}
	}
}

// checkHeartbeats evaluates the heartbeat of every node once.
func (s *Server) checkHeartbeats(ctx context.Context) {
	now := time.Now().UTC()
	for _, node := range s.nodes(ctx) {
		status, ok := s.hub.Status(node)
		if !ok {
			continue
		}
		offline := status.LastSeen.IsZero() ||
			now.Sub(status.LastSeen) > agentOfflineLimit
		sample := alerts.OfflineSample{
			NodeID:  node,
			At:      now,
			Offline: offline,
		}
		if _, err := s.alerts.ObserveOffline(ctx, sample); err != nil {
			log.Printf("control: evaluate the heartbeat of %s: %v", node, err)
		}
	}
}

// nodes lists the identifiers of the enrolled nodes.
func (s *Server) nodes(ctx context.Context) []string {
	enrolled, err := s.enrollment.ListNodes(ctx)
	if err != nil {
		return nil
	}
	nodes := make([]string, 0, len(enrolled))
	for _, node := range enrolled {
		nodes = append(nodes, node.ID)
	}
	return nodes
}

// handleContainerEvent evaluates the end of a container.
//
// An end that Docker reports with an exit code of zero is not an anomaly
// when the platform replaced the container or when the container was a one
// shot task. Every other end reaches the administrator.
func (s *Server) handleContainerEvent(ctx context.Context, nodeID string, envelope protocol.Envelope) error {
	var event protocol.ContainerEvent
	if err := json.Unmarshal(envelope.Payload, &event); err != nil {
		return nil
	}
	if event.Action != "die" && event.Action != "kill" && event.Action != "stop" {
		return nil
	}
	objectID := s.objectOfContainer(nodeID, event)
	expected := false
	if event.ExitCode != nil && *event.ExitCode == 0 {
		expected = s.expectedExit(ctx, nodeID, event)
	}
	sample := alerts.ExitSample{
		NodeID:    nodeID,
		ObjectID:  objectID,
		Namespace: s.containerNamespace(nodeID, event.ContainerID),
		At:        event.OccurredAt,
		Expected:  expected,
	}
	if event.ExitCode != nil {
		sample.ExitCode = *event.ExitCode
	}
	if _, err := s.alerts.ObserveExit(ctx, sample); err != nil {
		log.Printf("control: evaluate the exit of %s: %v", event.ContainerID, err)
	}
	return nil
}

// objectOfContainer names the object of a container that stopped.
//
// The name of a swarm task survives its replacement, so a rolling update
// does not open and close an event for every replica.
func (s *Server) objectOfContainer(nodeID string, event protocol.ContainerEvent) string {
	if event.ServiceID == "" {
		return "container:" + event.ContainerID
	}
	if slot := strings.TrimSpace(event.Slot); slot != "" && slot != "0" {
		return "service:" + event.ServiceID + "@slot:" + slot
	}
	taskNode := event.NodeID
	if taskNode == "" {
		taskNode = nodeID
	}
	return "service:" + event.ServiceID + "@node:" + taskNode
}

// expectedExit reports whether the end of a container has an explanation
// the platform knows.
//
// Docker marks a container that runs once with the label
// "com.docker.compose.oneoff", and a container that a deployment replaces
// with the label "com.docker.compose.replace" and the value "old". Only
// such a container that ends with the exit code of zero is expected; every
// other end reaches the administrator.
func (s *Server) expectedExit(ctx context.Context, nodeID string, event protocol.ContainerEvent) bool {
	inventory, err := s.hub.ReadInventory(ctx, "compose", nodeID)
	if err != nil {
		return false
	}
	for _, container := range inventory.Containers {
		if container.ID != event.ContainerID {
			continue
		}
		if container.Labels["com.docker.compose.oneoff"] == "true" {
			return true
		}
		return container.Labels["com.docker.compose.replace"] == "old"
	}
	return false
}
