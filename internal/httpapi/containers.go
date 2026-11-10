package httpapi

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"hmoob-dock/internal/agentlink"
	"hmoob-dock/internal/auth"
	"hmoob-dock/internal/protocol"
	"hmoob-dock/internal/store"
)

// containerSummary is one container of a node in the API.
//
// The page needs the identifier of a container to read its metrics and its
// logs, so the identifier travels with the name the administrator
// recognizes. The facts of the engine (the state, the address, the ports)
// are the ones the node reported with its last inventory.
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
	// Namespace is the Compose project or the Swarm stack the container
	// belongs to. It is empty for a container that no application of the
	// platform manages.
	Namespace string `json:"namespace"`
	// Created is the time Docker created the container.
	Created string `json:"created"`
	// StartedAt is the time the container last started.
	StartedAt string `json:"started_at"`
	// IP is the address of the container in its first network.
	IP string `json:"ip"`
	// Ports are the ports of the container.
	Ports []protocol.PortMapping `json:"ports"`
	// RestartCount is the number of times the container was restarted.
	RestartCount int `json:"restart_count"`
	// ExitCode is the exit code of a container that ended.
	ExitCode *int `json:"exit_code"`
}

// containerDetail is a container with the facts of its details.
type containerDetail struct {
	containerSummary
	// Mounts are the mounts of the container.
	Mounts []protocol.Mount `json:"mounts"`
	// Labels are the labels of the container.
	Labels map[string]string `json:"labels"`
}

// serviceSummary is one service of a node in the API.
//
// A service of a Swarm cluster carries its tasks; a service derived from
// the containers of a Compose project carries its containers.
type serviceSummary struct {
	ID          string                 `json:"id"`
	Name        string                 `json:"name"`
	Stack       string                 `json:"stack"`
	Image       string                 `json:"image"`
	Mode        string                 `json:"mode"`
	Desired     int                    `json:"desired"`
	Running     int                    `json:"running"`
	UpdateState string                 `json:"update_state"`
	Containers  int                    `json:"containers"`
	Ports       []protocol.PortMapping `json:"ports"`
	UpdatedAt   string                 `json:"updated_at"`
	Source      string                 `json:"source"`
}

// networkSummary is one network of a node in the API.
type networkSummary struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Stack           string `json:"stack,omitempty"`
	Driver          string `json:"driver"`
	Scope           string `json:"scope"`
	Internal        bool   `json:"internal"`
	Attachable      bool   `json:"attachable"`
	Created         string `json:"created"`
	Containers      int    `json:"containers"`
	IPAMDriver      string `json:"ipam_driver,omitempty"`
	IPAMSubnet      string `json:"ipam_subnet,omitempty"`
	IPAMGateway     string `json:"ipam_gateway,omitempty"`
	IPAMIPv6Subnet  string `json:"ipam_ipv6_subnet,omitempty"`
	IPAMIPv6Gateway string `json:"ipam_ipv6_gateway,omitempty"`
}

// imageSummary is one image of a node in the API.
type imageSummary struct {
	// ID is the ID of the image, for example sha256:....
	ID string `json:"id"`
	// Tags are the references the image carries. The field is always an
	// array, so a page can read its length without a special case.
	Tags []string `json:"tags"`
	// Size is the size of the image on the disk of the node, in bytes.
	Size int64 `json:"size"`
	// Created is the time the image was built or pulled.
	Created string `json:"created,omitempty"`
	// Labels are the labels of the image.
	Labels map[string]string `json:"labels,omitempty"`
	// Containers is the number of the containers of the node that run the
	// image.
	Containers int `json:"containers"`
	// Used reports whether a container of the node runs the image.
	Used bool `json:"used"`
}

// volumeSummary is one named volume of a node in the API.
type volumeSummary struct {
	// Name is the name of the volume.
	Name string `json:"name"`
	// Stack is the Compose project or the Swarm stack the volume belongs
	// to, when its labels name one.
	Stack string `json:"stack,omitempty"`
	// Driver is the driver of the volume, for example local.
	Driver string `json:"driver,omitempty"`
	// Scope is the scope of the volume, for example local.
	Scope string `json:"scope,omitempty"`
	// Mountpoint is the path on the node the volume lives at.
	Mountpoint string `json:"mountpoint,omitempty"`
	// Created is the time the volume was created.
	Created string `json:"created,omitempty"`
	// Containers is the number of the containers of the node that mount
	// the volume.
	Containers int `json:"containers"`
	// Used reports whether a container of the node mounts the volume.
	Used bool `json:"used"`
}

// stackNamespaceLabel names the Swarm stack of a container.
const stackNamespaceLabel = "com.docker.stack.namespace"

// composeProjectLabel names the Compose project of a container.
const composeProjectLabel = "com.docker.compose.project"

// auditMaxEvents is the capacity of the audit table. A write beyond the
// capacity is dropped: the audit is a record, not a guard.
const auditMaxEvents = 10000

// maxContainerOpBody is the largest body a container operation accepts.
// The body names an action and nothing more.
const maxContainerOpBody = 4 * 1024

// containerOpTimeout is the time the control端 waits for the result of a
// container operation. A stop with its grace period fits into the timeout
// with room to spare, because the agent bounds its own operations.
const containerOpTimeout = 30 * time.Second

// registerContainerRoutes mounts the read of the containers of a node and
// the operations on one container of a node.
func (s *Server) registerContainerRoutes() {
	s.mux.HandleFunc("/api/v1/nodes/{id}/containers", s.handleNodeContainers)
	s.mux.HandleFunc("/api/v1/nodes/{id}/containers/{cid}", s.handleNodeContainerDetail)
	s.mux.HandleFunc("/api/v1/nodes/{id}/containers/{cid}/op", s.handleContainerOp)
	s.mux.HandleFunc("/api/v1/nodes/{id}/services", s.handleNodeServices)
	s.mux.HandleFunc("/api/v1/nodes/{id}/networks", s.handleNodeNetworks)
	s.mux.HandleFunc("/api/v1/nodes/{id}/images", s.handleNodeImages)
	s.mux.HandleFunc("/api/v1/nodes/{id}/volumes", s.handleNodeVolumes)
	s.mux.HandleFunc("/api/v1/nodes/{id}/swarm", s.handleNodeSwarm)
	s.mux.HandleFunc("/api/v1/nodes/{id}/events", s.handleNodeEvents)
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
		summaries = append(summaries, summaryOfContainer(container))
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

// summaryOfContainer projects a container of the inventory into the API.
func summaryOfContainer(container protocol.Container) containerSummary {
	return containerSummary{
		ID:           container.ID,
		Name:         container.Name,
		Image:        container.Image,
		State:        container.State,
		Health:       container.Health,
		Service:      container.Service,
		Namespace:    namespaceOfContainer(container),
		Created:      container.Created,
		StartedAt:    container.StartedAt,
		IP:           container.IP,
		Ports:        container.Ports,
		RestartCount: container.RestartCount,
		ExitCode:     container.ExitCode,
	}
}

// handleNodeContainerDetail answers with one container of one node.
func (s *Server) handleNodeContainerDetail(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "该端点只接受 GET 请求")
		return
	}
	if _, ok := s.requireSession(w, r); !ok {
		return
	}

	nodeID := strings.TrimSpace(r.PathValue("id"))
	containerID := strings.TrimSpace(r.PathValue("cid"))
	if nodeID == "" || containerID == "" {
		writeError(w, http.StatusBadRequest, "validation_failed", "必须指定节点与容器")
		return
	}

	inventory, err := s.hub.ReadInventory(r.Context(), "compose", nodeID)
	if err != nil {
		writeError(w, http.StatusConflict, "target_unavailable", "这个节点当前的清单不可用，请稍后重试")
		return
	}

	for _, container := range inventory.Containers {
		if container.ID != containerID {
			continue
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"node_id": nodeID,
			"container": containerDetail{
				containerSummary: summaryOfContainer(container),
				Mounts:           container.Mounts,
				Labels:           container.Labels,
			},
			"collected_at": inventory.CollectedAt.UTC().Format(time.RFC3339Nano),
		})
		return
	}
	writeError(w, http.StatusNotFound, "container_not_found", "这个节点上没有这个容器")
}

// handleNodeServices answers with the services of one node.
//
// A node that belongs to a Swarm cluster reports the services of the
// cluster. A node that runs Compose projects reports the services its
// containers belong to, so the page has an answer for both kinds of node.
func (s *Server) handleNodeServices(w http.ResponseWriter, r *http.Request) {
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

	inventory, err := s.hub.ReadInventory(r.Context(), "compose", nodeID)
	if err != nil {
		writeError(w, http.StatusConflict, "target_unavailable", "这个节点当前的清单不可用，请稍后重试")
		return
	}

	services := make([]serviceSummary, 0)
	if len(inventory.SwarmServices) > 0 {
		for _, service := range inventory.SwarmServices {
			services = append(services, serviceSummary{
				ID:          service.ID,
				Name:        service.Name,
				Stack:       service.Labels[stackNamespaceLabel],
				Image:       service.Image,
				Mode:        service.Mode,
				Desired:     service.Desired,
				Running:     service.Running,
				UpdateState: service.UpdateState,
				Ports:       service.Ports,
				UpdatedAt:   service.UpdatedAt,
				Source:      "swarm",
			})
		}
	} else {
		services = composeServices(inventory.Containers)
	}
	sort.SliceStable(services, func(i, j int) bool {
		if services[i].Stack != services[j].Stack {
			return services[i].Stack < services[j].Stack
		}
		return services[i].Name < services[j].Name
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"node_id":      nodeID,
		"services":     services,
		"collected_at": inventory.CollectedAt.UTC().Format(time.RFC3339Nano),
	})
}

// composeServices groups the containers of Compose projects by their
// project and service names. A container that belongs to no Compose project
// is not part of a service.
func composeServices(containers []protocol.Container) []serviceSummary {
	type key struct {
		namespace string
		service   string
	}
	groups := map[key][]protocol.Container{}
	order := make([]key, 0)
	for _, container := range containers {
		if container.Service == "" {
			continue
		}
		namespace := namespaceOfContainer(container)
		if namespace == "" {
			continue
		}
		item := key{namespace: namespace, service: container.Service}
		if _, known := groups[item]; !known {
			order = append(order, item)
		}
		groups[item] = append(groups[item], container)
	}

	services := make([]serviceSummary, 0, len(order))
	for _, item := range order {
		items := groups[item]
		summary := serviceSummary{
			ID:         item.namespace + "/" + item.service,
			Name:       item.service,
			Stack:      item.namespace,
			Mode:       "compose",
			Desired:    len(items),
			Containers: len(items),
			Source:     "compose",
		}
		ports := map[protocol.PortMapping]bool{}
		for _, container := range items {
			if container.State == "running" {
				summary.Running += 1
			}
			if summary.Image == "" {
				summary.Image = container.Image
			}
			for _, port := range container.Ports {
				ports[port] = true
			}
		}
		for port := range ports {
			summary.Ports = append(summary.Ports, port)
		}
		sort.SliceStable(summary.Ports, func(i, j int) bool {
			if summary.Ports[i].PrivatePort != summary.Ports[j].PrivatePort {
				return summary.Ports[i].PrivatePort < summary.Ports[j].PrivatePort
			}
			return summary.Ports[i].PublicPort < summary.Ports[j].PublicPort
		})
		services = append(services, summary)
	}
	return services
}

// handleNodeNetworks answers with the networks of one node.
func (s *Server) handleNodeNetworks(w http.ResponseWriter, r *http.Request) {
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

	inventory, err := s.hub.ReadInventory(r.Context(), "compose", nodeID)
	if err != nil {
		writeError(w, http.StatusConflict, "target_unavailable", "这个节点当前的清单不可用，请稍后重试")
		return
	}

	networks := make([]networkSummary, 0, len(inventory.Networks))
	for _, network := range inventory.Networks {
		networks = append(networks, networkSummary{
			ID:              network.ID,
			Name:            network.Name,
			Stack:           network.Labels[composeProjectLabel],
			Driver:          network.Driver,
			Scope:           network.Scope,
			Internal:        network.Internal,
			Attachable:      network.Attachable,
			Created:         network.Created,
			Containers:      network.Containers,
			IPAMDriver:      network.IPAMDriver,
			IPAMSubnet:      network.IPAMSubnet,
			IPAMGateway:     network.IPAMGateway,
			IPAMIPv6Subnet:  network.IPAMIPv6Subnet,
			IPAMIPv6Gateway: network.IPAMIPv6Gateway,
		})
	}
	sort.SliceStable(networks, func(i, j int) bool {
		return networks[i].Name < networks[j].Name
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"node_id":      nodeID,
		"networks":     networks,
		"collected_at": inventory.CollectedAt.UTC().Format(time.RFC3339Nano),
	})
}

// handleNodeImages answers with the images in the local store of one node.
func (s *Server) handleNodeImages(w http.ResponseWriter, r *http.Request) {
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

	inventory, err := s.hub.ReadInventory(r.Context(), "compose", nodeID)
	if err != nil {
		writeError(w, http.StatusConflict, "target_unavailable", "这个节点当前的清单不可用，请稍后重试")
		return
	}

	images := make([]imageSummary, 0, len(inventory.Images))
	for _, image := range inventory.Images {
		images = append(images, imageSummaryOf(image, inventory.Containers))
	}
	// The newest image comes first, an image without a build time last.
	sort.SliceStable(images, func(i, j int) bool {
		return images[i].Created > images[j].Created
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"node_id":      nodeID,
		"images":       images,
		"collected_at": inventory.CollectedAt.UTC().Format(time.RFC3339Nano),
	})
}

// imageSummaryOf projects one image of the inventory and counts the
// containers of the same inventory that run it.
//
// The count is a correspondence of the reference a container was created
// with and the references the image carries: a container that runs an image
// by an ID the inventory does not carry is not counted.
func imageSummaryOf(image protocol.Image, containers []protocol.Container) imageSummary {
	running := 0
	for _, container := range containers {
		if imageInUseBy(container.Image, image) {
			running++
		}
	}
	// An image nobody runs is unused, whether it carries a tag or not.
	used := running > 0
	// The tags are always an array in the answer: a nil slice would become
	// null, and the page reads the length of the field.
	tags := image.Tags
	if tags == nil {
		tags = []string{}
	}
	return imageSummary{
		ID:         image.ID,
		Tags:       tags,
		Size:       image.Size,
		Created:    image.Created,
		Labels:     image.Labels,
		Containers: running,
		Used:       used,
	}
}

// imageInUseBy reports whether a container runs on the image.
//
// A container names the image it was created with, a tag or an ID. The
// answer is a heuristic: a container that runs by a digest the local store
// names differently is not counted.
func imageInUseBy(reference string, image protocol.Image) bool {
	if reference == "" {
		return false
	}
	if reference == image.ID {
		return true
	}
	if strings.HasPrefix(image.ID, reference) || strings.HasPrefix(reference, image.ID) {
		return true
	}
	for _, tag := range image.Tags {
		if reference == tag {
			return true
		}
		// A reference without a tag, for example nginx, names the latest
		// tag of the repository in the store.
		if tag == reference+":latest" {
			return true
		}
	}
	return false
}

// handleNodeVolumes answers with the named volumes of one node.
func (s *Server) handleNodeVolumes(w http.ResponseWriter, r *http.Request) {
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

	inventory, err := s.hub.ReadInventory(r.Context(), "compose", nodeID)
	if err != nil {
		writeError(w, http.StatusConflict, "target_unavailable", "这个节点当前的清单不可用，请稍后重试")
		return
	}

	volumes := make([]volumeSummary, 0, len(inventory.Volumes))
	for _, volume := range inventory.Volumes {
		volumes = append(volumes, volumeSummaryOf(volume, inventory.Containers))
	}
	sort.SliceStable(volumes, func(i, j int) bool {
		return volumes[i].Name < volumes[j].Name
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"node_id":      nodeID,
		"volumes":      volumes,
		"collected_at": inventory.CollectedAt.UTC().Format(time.RFC3339Nano),
	})
}

// handleNodeSwarm answers with the Swarm cluster the node belongs to.
//
// A node that belongs to no cluster answers with an empty list, so the
// console shows the page of a standalone node without a cluster.
func (s *Server) handleNodeSwarm(w http.ResponseWriter, r *http.Request) {
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

	inventory, err := s.hub.ReadInventory(r.Context(), "compose", nodeID)
	if err != nil {
		writeError(w, http.StatusConflict, "target_unavailable", "这个节点当前的清单不可用，请稍后重试")
		return
	}

	nodes := make([]protocol.SwarmNode, 0, len(inventory.SwarmNodes))
	for _, node := range inventory.SwarmNodes {
		nodes = append(nodes, node)
	}
	sort.SliceStable(nodes, func(i, j int) bool {
		if nodes[i].Role != nodes[j].Role {
			return nodes[i].Role > nodes[j].Role
		}
		return nodes[i].Hostname < nodes[j].Hostname
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"node_id":      nodeID,
		"swarm_nodes":  nodes,
		"collected_at": inventory.CollectedAt.UTC().Format(time.RFC3339Nano),
	})
}

// volumeSummaryOf projects one volume of the inventory and counts the
// containers of the same inventory that mount it.
//
// A container mounts a volume by its name, so the count reads the source
// of every mount of the container.
func volumeSummaryOf(volume protocol.Volume, containers []protocol.Container) volumeSummary {
	mountedBy := 0
	for _, container := range containers {
		for _, mount := range container.Mounts {
			if mount.Source == volume.Name && (mount.Type == "" || mount.Type == "volume") {
				mountedBy++
				break
			}
		}
	}
	stack := volume.Labels[composeProjectLabel]
	if stack == "" {
		stack = volume.Labels[stackNamespaceLabel]
	}
	return volumeSummary{
		Name:       volume.Name,
		Stack:      stack,
		Driver:     volume.Driver,
		Scope:      volume.Scope,
		Mountpoint: volume.Mountpoint,
		Created:    volume.Created,
		Containers: mountedBy,
		Used:       mountedBy > 0,
	}
}

// namespaceOfContainer returns the namespace an application of the platform
// would use for the container: the name of its Compose project or of its
// Swarm stack. A container without such a label is not managed by any
// application.
func namespaceOfContainer(container protocol.Container) string {
	if name := container.Labels[composeProjectLabel]; name != "" {
		return name
	}
	return container.Labels[stackNamespaceLabel]
}

// handleContainerOp performs one operation on one container of a node.
//
// The operations are the fixed verbs of the protocol. An operation on a
// container that an application of the platform manages is refused while
// the application holds a task that did not finish, and a removal is
// refused altogether: the configuration of the application is the only
// source of its containers, so a removal belongs to its YAML. The removal
// of a container that no application manages is allowed, and it never
// removes the volumes of the container.
//
// The result of the node is the result of the request. A timeout is
// reported as it is: the operation may still run on the node, so the
// control端 never claims an outcome it does not know.
func (s *Server) handleContainerOp(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "该端点只接受 POST 请求")
		return
	}
	if !s.sameOrigin(r) {
		writeError(w, http.StatusForbidden, "cross_origin", "请求来源不被允许")
		return
	}
	session, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	if !auth.CheckCSRF(r, session) {
		writeError(w, http.StatusForbidden, "csrf_invalid", "CSRF 令牌无效")
		return
	}

	nodeID := strings.TrimSpace(r.PathValue("id"))
	containerID := strings.TrimSpace(r.PathValue("cid"))
	if nodeID == "" {
		writeError(w, http.StatusBadRequest, "validation_failed", "必须指定节点")
		return
	}
	if !protocol.ValidContainerID(containerID) {
		writeError(w, http.StatusBadRequest, "validation_failed", "容器标识无效")
		return
	}

	var body struct {
		Action string `json:"action"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxContainerOpBody)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", "请求体不是有效的 JSON")
		return
	}
	if !protocol.IsContainerAction(body.Action) {
		writeError(w, http.StatusBadRequest, "validation_failed", "不支持的操作类型")
		return
	}

	node, err := s.enrollment.NodeRecord(r.Context(), nodeID)
	if err != nil {
		if errors.Is(err, agentlink.ErrUnknownNode) {
			writeError(w, http.StatusNotFound, "node_not_found", "未找到该节点")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "无法读取节点信息")
		return
	}
	if node.RevokedAt != nil {
		writeError(w, http.StatusConflict, "node_revoked", "该节点的接入已被撤销")
		return
	}
	if !s.hub.Connected(nodeID) {
		writeError(w, http.StatusConflict, "target_unavailable", "节点当前没有连接，操作不可用")
		return
	}

	// The container has to belong to the node: an identifier that the
	// last inventory does not hold is either gone or never was one of
	// this node.
	inventory, err := s.hub.ReadInventory(r.Context(), "compose", nodeID)
	if err != nil {
		writeError(w, http.StatusConflict, "target_unavailable", "这个节点当前的清单不可用，请稍后重试")
		return
	}
	var target *protocol.Container
	for index := range inventory.Containers {
		if inventory.Containers[index].ID == containerID {
			target = &inventory.Containers[index]
			break
		}
	}
	if target == nil {
		writeError(w, http.StatusNotFound, "container_not_found", "节点最近的清单里没有这个容器")
		return
	}

	// A container of an application belongs to the configuration of that
	// application, so the operations around a deployment are guarded.
	if namespace := namespaceOfContainer(*target); namespace != "" {
		var appID string
		err := s.db.QueryRowContext(r.Context(),
			`SELECT id FROM apps WHERE target_id = ? AND namespace = ?`, nodeID, namespace).Scan(&appID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// The application was deleted; its containers are no longer
			// guarded by a configuration.
		case err != nil:
			writeError(w, http.StatusInternalServerError, "internal_error", "无法读取应用信息")
			return
		default:
			if body.Action == protocol.ContainerActionRemove {
				writeError(w, http.StatusConflict, "managed_by_application",
					"这个容器由平台的站点管理，删除请编辑站点 YAML 并重新部署")
				return
			}
			var openTask string
			err := s.db.QueryRowContext(r.Context(),
				`SELECT id FROM tasks
				 WHERE app_id = ? AND state IN ('queued','preflight','running','verifying','uncertain')`,
				appID).Scan(&openTask)
			switch {
			case errors.Is(err, sql.ErrNoRows):
			case err != nil:
				writeError(w, http.StatusInternalServerError, "internal_error", "无法读取部署任务")
				return
			default:
				writeError(w, http.StatusConflict, "deployment_in_progress",
					"该站点有一个未结束的部署任务，容器操作暂不可用")
				return
			}
		}
	}

	result, err := s.hub.Request(r.Context(), nodeID, body.Action, containerID, containerOpTimeout)
	s.recordContainerOpAudit(r.Context(), nodeID, body.Action, containerID, result, err)
	switch {
	case errors.Is(err, agentlink.ErrNodeNotConnected):
		writeError(w, http.StatusConflict, "target_unavailable", "节点当前没有连接，操作不可用")
		return
	case errors.Is(err, agentlink.ErrRequestTimeout):
		writeError(w, http.StatusGatewayTimeout, "operation_timeout",
			"节点没有在期限内回答，操作可能仍在节点上执行，请刷新清单核对实际状态")
		return
	case errors.Is(err, agentlink.ErrRequestRefused):
		writeError(w, http.StatusConflict, result.ErrorCode, result.Message)
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal_error", "操作请求发送失败")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}

// recordContainerOpAudit writes the audit event of a container operation.
//
// The event records what was asked and what the node answered. A failure
// of the audit write never fails the operation: the audit is a record, not
// a guard.
func (s *Server) recordContainerOpAudit(ctx context.Context, nodeID, action, containerID string, result protocol.ContainerResult, requestErr error) {
	outcome := "success"
	detail := map[string]any{
		"action":    action,
		"container": containerID,
	}
	if requestErr != nil {
		outcome = "failure"
		detail["error"] = requestErr.Error()
	} else if !result.OK {
		outcome = "failure"
		detail["error_code"] = result.ErrorCode
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		return
	}

	identifier := make([]byte, 16)
	if _, err := rand.Read(identifier); err != nil {
		return
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO audit_events (id, occurred_at, action, target_id, result, detail)
		 SELECT ?, ?, ?, ?, ?, ? WHERE (SELECT count(*) FROM audit_events) < ?`,
		hex.EncodeToString(identifier), store.TimeToMillis(time.Now()), "container_op", nodeID,
		outcome, string(encoded), auditMaxEvents)
	if err != nil {
		return
	}
}
