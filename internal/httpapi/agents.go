package httpapi

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"hmoob-dock/internal/agentlink"
	"hmoob-dock/internal/auth"
	"hmoob-dock/internal/protocol"
)

// maxAgentRequestBody is the largest body that the enrollment endpoint
// accepts. The body holds an identity and a token, nothing more.
const maxAgentRequestBody = 8 * 1024

const (
	// enrollMaxAttempts is the number of failed enrollments that a source
	// may make within enrollRateWindow.
	enrollMaxAttempts = 5
	// enrollRateWindow is the time the failed enrollments of a source are
	// counted.
	enrollRateWindow = 15 * time.Minute
)

// nodeSummary is the summary of a node in the API.
type nodeSummary struct {
	// ID identifies the node.
	ID string `json:"id"`
	// Hostname is the name of the host of the node.
	Hostname string `json:"hostname"`
	// Connected reports whether the agent of the node is connected.
	Connected bool `json:"connected"`
	// ContainerCount is the number of containers of the last complete
	// inventory. It is zero while the node sent no inventory.
	ContainerCount int `json:"container_count"`
	// LastSeen is the time the control端 received the last heartbeat of
	// the node. It is nil while the node never sent one.
	LastSeen *string `json:"last_seen"`
}

// registerAgentRoutes mounts the management API of the nodes and the
// machine endpoints of the agents.
//
// The management API belongs to the administrator and uses the session of
// the administrator. The machine endpoints belong to the agents and use
// their own credentials: the enrollment token and the credential of the
// node. The two never mix.
func (s *Server) registerAgentRoutes() {
	s.mux.HandleFunc("/api/v1/agent-tokens", s.handleAgentTokenCreate)
	s.mux.HandleFunc("/api/v1/nodes", s.handleNodesList)
	s.mux.HandleFunc("/api/v1/nodes/{id}/revoke", s.handleNodeRevoke)
	s.registerContainerRoutes()
	s.mux.HandleFunc("/api/v1/agent/enroll", s.handleAgentEnroll)
	s.mux.HandleFunc("/api/v1/agent/connect", s.handleAgentConnect)
}

// handleAgentTokenCreate hands out a one time enrollment token.
//
// The token is shown once, in the answer of this request. The database
// keeps only its digest, so the control端 cannot show it again.
func (s *Server) handleAgentTokenCreate(w http.ResponseWriter, r *http.Request) {
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

	token, err := s.enrollment.Issue(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "无法生成接入令牌")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"token":      token,
		"expires_at": formatTimestamp(time.Now().Add(agentlink.EnrollmentTTL)),
	})
}

// handleNodesList reports the enrolled nodes and their live state.
func (s *Server) handleNodesList(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "该端点只接受 GET 请求")
		return
	}
	if _, ok := s.requireSession(w, r); !ok {
		return
	}

	nodes, err := s.enrollment.ListNodes(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "无法读取节点列表")
		return
	}

	summaries := make([]nodeSummary, 0, len(nodes))
	for _, node := range nodes {
		summary := nodeSummary{ID: node.ID, Hostname: node.Hostname}
		status, live := s.hub.Status(node.ID)
		if live {
			summary.Connected = status.Connected
			// The name of the host is the name the Docker engine reports.
			// The record holds the name of the enrollment.
			if status.Identity.Hostname != "" {
				summary.Hostname = status.Identity.Hostname
			}
			// The number of containers comes from the last complete
			// inventory, so the page never shows a number of a node
			// that sent no inventory.
			summary.ContainerCount = status.ContainerCount
			if !status.LastSeen.IsZero() {
				lastSeen := formatTimestamp(status.LastSeen)
				summary.LastSeen = &lastSeen
			}
		}
		summaries = append(summaries, summary)
	}

	writeJSON(w, http.StatusOK, map[string]any{"nodes": summaries})
}

// handleNodeRevoke withdraws the credential of a node.
//
// The connection of the node is closed and the node cannot authenticate
// again. The containers that run on the node keep running.
func (s *Server) handleNodeRevoke(w http.ResponseWriter, r *http.Request) {
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
	if nodeID == "" {
		writeError(w, http.StatusBadRequest, "validation_failed", "缺少节点标识")
		return
	}

	if err := s.enrollment.Revoke(r.Context(), nodeID); err != nil {
		switch {
		case errors.Is(err, agentlink.ErrUnknownNode):
			writeError(w, http.StatusNotFound, "node_not_found", "未找到该节点")
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", "撤销节点失败")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAgentEnroll registers a node with a one time enrollment token.
//
// The endpoint belongs to the machine, not to the administrator: it does
// not use the session of the administrator, it verifies the enrollment
// token instead.
//
// The endpoint counts the failed attempts of a source in a budget of its
// own. The budget is not the budget of the login, so a flood of anonymous
// requests neither uses up the budget of the administrator nor keeps the
// writes of the control端 waiting.
func (s *Server) handleAgentEnroll(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "该端点只接受 POST 请求")
		return
	}

	source := s.clientKey(r)
	if !s.enrollLimiter.Allow(source) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "尝试次数过多，请稍后再试")
		return
	}

	var body struct {
		Token    string                `json:"token"`
		Identity protocol.NodeIdentity `json:"identity"`
	}
	if err := decodeAgentRequest(w, r, &body); err != nil {
		writeRequestError(w, err)
		return
	}

	credential, err := s.enrollment.Redeem(r.Context(), body.Token, body.Identity)
	if err != nil {
		switch {
		case errors.Is(err, agentlink.ErrInvalidToken):
			// A refused token spends the budget of the source, because it
			// opens a transaction and writes an audit event.
			s.enrollLimiter.Fail(source)
			writeError(w, http.StatusUnauthorized, "invalid_token", "接入令牌无效或已过期")
		case errors.Is(err, agentlink.ErrEngineIDConflict):
			s.enrollLimiter.Fail(source)
			writeError(w, http.StatusConflict, "engine_conflict", "该 Docker 引擎已经接入")
		case errors.Is(err, agentlink.ErrInvalidIdentity):
			// An incomplete identity never reaches the database, so it does
			// not spend the budget of the source.
			writeError(w, http.StatusBadRequest, "validation_failed", "节点身份不完整")
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", "节点注册失败")
		}
		return
	}
	s.enrollLimiter.Reset(source)

	writeJSON(w, http.StatusCreated, map[string]string{
		"node_id":    credential.NodeID,
		"credential": credential.Token,
	})
}

// handleAgentConnect opens the connection of an agent.
//
// The credential of the node travels in a header, never in the URL, so it
// does not reach the log of a proxy. The control端 binds the connection to
// the node of the credential, so a report of an agent always belongs to
// the node that authenticated it.
func (s *Server) handleAgentConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "该端点只接受 GET 请求")
		return
	}

	token := bearerToken(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "invalid_credential", "节点凭据无效")
		return
	}
	credential, err := s.enrollment.Authenticate(r.Context(), token)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid_credential", "节点凭据无效")
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		// The handshake failed, the library answered the request.
		return
	}
	defer conn.CloseNow()

	if err := s.hub.Serve(r.Context(), credential.NodeID, conn); err != nil {
		// The end of a connection is the normal case: the agent opens a
		// new connection. Only a refusal is worth a report.
		if !errors.Is(err, agentlink.ErrConnectionEnded) {
			log.Printf("control: the connection of the node %s was refused: %v", credential.NodeID, err)
		}
	}
}

// decodeAgentRequest reads the JSON body of a machine request. The body is
// limited, so a huge body cannot make the control端 read it.
func decodeAgentRequest(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxAgentRequestBody)
	if err := json.NewDecoder(r.Body).Decode(target); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return errRequestTooLarge
		}
		return errInvalidRequest
	}
	return nil
}

// bearerToken returns the credential of a machine request.
func bearerToken(r *http.Request) string {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if header == "" {
		return ""
	}
	scheme, value, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(value)
}
