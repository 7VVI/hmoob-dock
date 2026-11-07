package httpapi

import (
	"net/http"
	"strings"
	"sync"
	"time"

)

// eventLogCapacity is the number of the container events a node keeps in
// the memory of the control端. The log is a window of the recent past, not
// a history: the pages read it to see what changed since they looked last.
const eventLogCapacity = 200

// containerEventRecord is one container event of a node, in the form the
// console shows it.
type containerEventRecord struct {
	// ContainerID is the container the event belongs to.
	ContainerID string `json:"container_id"`
	// Service is the Compose service or the Swarm service the container
	// belongs to, when the event names one.
	Service string `json:"service,omitempty"`
	// Action is what happened to the container, for example start or die.
	Action string `json:"action"`
	// ExitCode is the exit code of a container that ended.
	ExitCode *int `json:"exit_code,omitempty"`
	// OccurredAt is the time Docker reported for the event.
	OccurredAt time.Time `json:"occurred_at"`
}

// eventLog keeps the recent container events of every node in the memory of
// the control端. The log of one node is a ring: an event beyond the
// capacity drops the oldest one.
type eventLog struct {
	mu     sync.Mutex
	cap    int
	byNode map[string][]containerEventRecord
}

// newEventLog opens an empty log of container events.
func newEventLog(capacity int) *eventLog {
	return &eventLog{cap: capacity, byNode: map[string][]containerEventRecord{}}
}

// append records one event of a node.
func (l *eventLog) append(nodeID string, record containerEventRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	events := append(l.byNode[nodeID], record)
	if len(events) > l.cap {
		events = events[len(events)-l.cap:]
	}
	l.byNode[nodeID] = events
}

// recent returns the events of a node, the newest first.
func (l *eventLog) recent(nodeID string) []containerEventRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	events := l.byNode[nodeID]
	recent := make([]containerEventRecord, len(events))
	for index, event := range events {
		recent[len(events)-1-index] = event
	}
	return recent
}

// handleNodeEvents answers with the recent container events of one node.
//
// A namespace selects only the events of the containers that belong to it.
// The namespace of an event is resolved from the last inventory of the
// node, so an event of a container that the inventory does not know any
// more is filtered out when a namespace is asked for.
func (s *Server) handleNodeEvents(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "该端点只接受 GET 请求")
		return
	}
	if _, ok := s.requireSession(w, r); !ok {
		return
	}

	nodeID := strings.TrimSpace(r.PathValue("id"))
	if nodeID == "" {
		writeError(w, http.StatusBadRequest, "validation_failed", "必须指定节点")
		return
	}
	namespace := strings.TrimSpace(r.URL.Query().Get("namespace"))

	records := s.containerEvents.recent(nodeID)
	events := make([]containerEventRecord, 0, len(records))
	for _, record := range records {
		if namespace != "" {
			if s.containerNamespace(nodeID, record.ContainerID) != namespace {
				continue
			}
		}
		events = append(events, record)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"node_id":      nodeID,
		"namespace":    namespace,
		"events":       events,
		"generated_at": time.Now().UTC().Format(time.RFC3339Nano),
	})
}
