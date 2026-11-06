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
	// Namespace is the Compose project or the Swarm stack the container
	// belongs to. It is empty for a container that no application of the
	// platform manages.
	Namespace string `json:"namespace"`
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
	s.mux.HandleFunc("/api/v1/nodes/{id}/containers/{cid}/op", s.handleContainerOp)
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
			ID:        container.ID,
			Name:      container.Name,
			Image:     container.Image,
			State:     container.State,
			Health:    container.Health,
			Service:   container.Service,
			Namespace: namespaceOfContainer(container),
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
