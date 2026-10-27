package httpapi

import (
	"net/http"
	"sort"
	"strings"
	"time"
)

// containerSummary is one container of a node in the API.
//
// The page needs the identifier of a container to read its metrics and its
// logs, so the identifier travels with the name the administrator
// recognizes. Nothing else of the container leaves the node: the labels and
// the configuration stay there.
type containerSummary struct {
	// ID is the identifier Docker gave the container.
	ID string `json:"id"`
	// Name is the name of the container.
	Name string `json:"name"`
	// Image is the image the container runs.
	Image string `json:"image"`
	// State is the state of the container.
	State string `json:"state"`
	// Health is the state of the health check, empty when there is none.
	Health string `json:"health"`
	// Service is the Compose service of the container, empty when it has
	// none.
	Service string `json:"service"`
}

// composeProjectLabel names the Compose project of a container.
const composeProjectLabel = "com.docker.compose.project"

// registerContainerRoutes mounts the read of the containers of a node.
func (s *Server) registerContainerRoutes() {
	s.mux.HandleFunc("/api/v1/nodes/{id}/containers", s.handleNodeContainers)
}

// handleNodeContainers answers with the containers of one node.
//
// The list comes from the last complete inventory of the node, so an agent
// that is away answers with the containers it reported last and the page
// shows the age of the report. A node that never reported has no
// containers to show.
func (s *Server) handleNodeContainers(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "该端点只接受 GET 请求")
		return
	}
	if _, ok := s.requireSession(w, r); !ok {
		return
	}

	nodeID := r.PathValue("id")
	if strings.TrimSpace(nodeID) == "" {
		writeError(w, http.StatusBadRequest, "validation_failed", "必须指定节点")
		return
	}

	// A namespace filters the list to the containers of one Compose
	// project, so a page of an application shows its own containers and
	// not every container of the node.
	namespace := strings.TrimSpace(r.URL.Query().Get("namespace"))

	inventory, err := s.hub.ReadInventory(r.Context(), "compose", nodeID)
	if err != nil {
		writeError(w, http.StatusConflict, "target_unavailable", "这个节点当前的清单不可用，请稍后重试")
		return
	}

	summaries := make([]containerSummary, 0, len(inventory.Containers))
	for _, container := range inventory.Containers {
		if namespace != "" && container.Labels[composeProjectLabel] != namespace {
			continue
		}
		summaries = append(summaries, containerSummary{
			ID:      container.ID,
			Name:    container.Name,
			Image:   container.Image,
			State:   container.State,
			Health:  container.Health,
			Service: container.Service,
		})
	}
	// The running containers come first, because a container that runs is
	// the one whose metrics tell the administrator something.
	sort.SliceStable(summaries, func(i, j int) bool {
		left := summaries[i].State == "running"
		right := summaries[j].State == "running"
		if left != right {
			return left
		}
		return summaries[i].Name < summaries[j].Name
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"node_id":      nodeID,
		"containers":   summaries,
		"collected_at": inventory.CollectedAt.UTC().Format(time.RFC3339Nano),
	})
}
