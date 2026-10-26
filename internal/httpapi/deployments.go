package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"hmoob-dock/internal/auth"
	"hmoob-dock/internal/deploy"
	"hmoob-dock/internal/protocol"
)

// registerDeploymentRoutes mounts the API of the deployments.
//
// The API belongs to the administrator and uses the session of the
// administrator. Every request that changes something needs the CSRF token
// of the session as well.
func (s *Server) registerDeploymentRoutes() {
	s.mux.HandleFunc("/api/v1/deployments/{id}", s.handleDeployment)
	s.mux.HandleFunc("/api/v1/deployments/{id}/output", s.handleDeploymentOutput)
	s.mux.HandleFunc("/api/v1/deployments/{id}/reconcile", s.handleDeploymentReconcile)
	s.mux.HandleFunc("/api/v1/deployments/{id}/resolve", s.handleDeploymentResolve)
}

// handleDeployment reports the state of a deployment.
//
// The answer holds no configuration: the configuration of a version is read
// when the administrator opens the version.
func (s *Server) handleDeployment(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "该端点只接受 GET 请求")
		return
	}
	if _, ok := s.requireSession(w, r); !ok {
		return
	}

	taskID := strings.TrimSpace(r.PathValue("id"))
	deployment, err := s.apps.Deployment(r.Context(), taskID)
	if err != nil {
		writeDeployError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, deployment)
}

// handleDeploymentOutput reports the output of a deployment.
//
// The output holds the fragments of the command line of Docker in the order
// of their offset, so a reader skips the fragments it has already seen.
func (s *Server) handleDeploymentOutput(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "该端点只接受 GET 请求")
		return
	}
	if _, ok := s.requireSession(w, r); !ok {
		return
	}

	taskID := strings.TrimSpace(r.PathValue("id"))
	output, err := s.apps.Output(r.Context(), taskID)
	if err != nil {
		writeDeployError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, output)
}

// handleDeploymentReconcile compares a deployment with the ledger of the
// node that executes it.
//
// The request only reads: it asks the node about the task and applies its
// answer. It changes neither the target nor the ledger of the node.
func (s *Server) handleDeploymentReconcile(w http.ResponseWriter, r *http.Request) {
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

	taskID := strings.TrimSpace(r.PathValue("id"))
	task, err := s.dispatcher.Reconcile(r.Context(), taskID)
	if err != nil {
		if errors.Is(err, deploy.ErrDivergence) {
			// The two sides disagree about the outcome of the task. The
			// answer of the node wins, and the disagreement is reported
			// instead of being hidden.
			writeError(w, http.StatusConflict, "divergence", err.Error())
			return
		}
		writeDeployError(w, err)
		return
	}
	deployment, err := s.apps.Deployment(r.Context(), task.ID)
	if err != nil {
		writeDeployError(w, err)
		return
	}
	s.events.publish(Event{Type: EventDeployment, ObjectID: task.ID})
	writeJSON(w, http.StatusOK, deployment)
}

// handleDeploymentResolve ends a deployment whose outcome is unknown.
//
// The administrator has to confirm that the executor of the task stopped and
// has to name the reason of the release. The release is only complete when
// the node confirms it, so the answer tells the administrator that the
// release waits for the confirmation of the node.
func (s *Server) handleDeploymentResolve(w http.ResponseWriter, r *http.Request) {
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

	var body struct {
		// ExpectedSeq is the sequence number the task holds.
		ExpectedSeq uint64 `json:"expected_seq"`
		// ExecutorStopped confirms that the executor of the task stopped.
		ExecutorStopped bool `json:"executor_stopped"`
		// Reason is the reason of the administrator.
		Reason string `json:"reason"`
	}
	if err := decodeDeploymentRequest(w, r, &body); err != nil {
		writeDeploymentRequestError(w, err)
		return
	}

	taskID := strings.TrimSpace(r.PathValue("id"))
	err := s.dispatcher.Resolve(r.Context(), taskID, body.ExpectedSeq, body.ExecutorStopped, body.Reason)
	switch {
	case err == nil:
	case errors.Is(err, deploy.ErrResolvePending):
		// The node does not answer. The release waits for its
		// confirmation, and the task holds its application until then.
		log.Printf("control: the release of the task %s waits for the node", taskID)
	default:
		writeDeployError(w, err)
		return
	}

	deployment, err := s.apps.Deployment(r.Context(), taskID)
	if err != nil {
		writeDeployError(w, err)
		return
	}
	s.events.publish(Event{Type: EventDeployment, ObjectID: taskID})
	writeJSON(w, http.StatusAccepted, deployment)
}

// subscribeDeployments hands the frames of the agents to the dispatcher.
//
// The callbacks run in the dispatcher of the connection of the node, so they
// have to be quick: a slow callback delays the frames of its own connection
// only. The control端 never blocks a connection of an agent with the work of
// a deployment.
func (s *Server) subscribeDeployments() {
	s.hub.Subscribe(protocol.TypeDeployReceipt, func(ctx context.Context, nodeID string, envelope protocol.Envelope) error {
		var message protocol.DeployReceipt
		if err := json.Unmarshal(envelope.Payload, &message); err != nil {
			return fmt.Errorf("httpapi: decode the report of the node %s: %w", nodeID, err)
		}
		if err := s.dispatcher.ApplyReceipt(ctx, nodeID, message.Receipt); err != nil {
			return err
		}
		s.events.publish(Event{Type: EventDeployment, ObjectID: message.Receipt.TaskID})
		return nil
	})

	s.hub.Subscribe(protocol.TypeDeployOutput, func(ctx context.Context, nodeID string, envelope protocol.Envelope) error {
		var message protocol.DeployOutput
		if err := json.Unmarshal(envelope.Payload, &message); err != nil {
			return fmt.Errorf("httpapi: decode the output of the node %s: %w", nodeID, err)
		}
		if err := s.dispatcher.ApplyOutput(ctx, nodeID, message); err != nil {
			return err
		}
		s.events.publish(Event{Type: EventOutput, ObjectID: message.TaskID})
		return nil
	})

	s.hub.Subscribe(protocol.TypeDeployQueryResult, func(ctx context.Context, nodeID string, envelope protocol.Envelope) error {
		var message protocol.DeployQueryResult
		if err := json.Unmarshal(envelope.Payload, &message); err != nil {
			return fmt.Errorf("httpapi: decode the answer of the node %s: %w", nodeID, err)
		}
		if err := s.dispatcher.ApplyQueryResult(ctx, nodeID, message); err != nil {
			return err
		}
		s.events.publish(Event{Type: EventDeployment, ObjectID: message.TaskID})
		return nil
	})

	// A node that comes back is asked about the tasks it holds: a
	// connection that ended may have swallowed the reports of the node.
	s.hub.OnConnect(func(nodeID string) {
		go func() {
			if err := s.dispatcher.RecoverNode(s.ctx, nodeID); err != nil {
				log.Printf("control: the deployments of the node %s were not recovered: %v", nodeID, err)
			}
		}()
	})
}
