package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"hmoob-dock/internal/agentlink"
	"hmoob-dock/internal/auth"
	"hmoob-dock/internal/protocol"
)

// logSubscription is one subscription of the logs of a container of the
// administrator. It belongs to the session that created it and ends with
// the session and with the last event stream of the session.
type logSubscription struct {
	id          string
	sessionID   string
	nodeID      string
	containerID string
	tail        int
	follow      bool
	expiresAt   time.Time
	timer       *time.Timer
	// lastOffset is the position after the last piece of the text that was
	// handed to the session. A frame that starts after it marks the gap of
	// a piece that did not arrive.
	lastOffset  uint64
	offsetKnown bool
}

// logSubscriptions owns the log subscriptions of the administrator.
//
// The subscriptions are held by the session and not by a connection of the
// browser: every page of a session uses the event stream of the
// administrator. The manager counts the streams of a session, so a page
// that closes does not end the subscriptions of the pages that stay open.
// The last stream ends them, and the expiry of the session ends them as
// well.
type logSubscriptions struct {
	hub    *agentlink.Hub
	events *eventBroker
	now    func() time.Time

	mu sync.Mutex
	// subs holds the subscription by its ID.
	subs map[string]*logSubscription
	// streams counts the open event streams of a session.
	streams map[string]int
}

// newLogSubscriptions builds the manager of the log subscriptions.
func newLogSubscriptions(hub *agentlink.Hub, events *eventBroker) *logSubscriptions {
	return &logSubscriptions{
		hub:     hub,
		events:  events,
		now:     time.Now,
		subs:    make(map[string]*logSubscription),
		streams: make(map[string]int),
	}
}

// create registers a subscription and asks the agent of the node to
// stream the logs.
func (m *logSubscriptions) create(ctx context.Context, session auth.Session, nodeID, containerID string, tail int, follow bool) (*logSubscription, error) {
	id, err := newLogSubscriptionID()
	if err != nil {
		return nil, err
	}

	subscription := &logSubscription{
		id:          id,
		sessionID:   session.ID,
		nodeID:      nodeID,
		containerID: containerID,
		tail:        tail,
		follow:      follow,
		expiresAt:   session.ExpiresAt,
	}

	m.mu.Lock()
	m.subs[id] = subscription
	m.mu.Unlock()

	payload, err := json.Marshal(protocol.LogRequest{
		SubscriptionID: id,
		ContainerID:    containerID,
		Tail:           tail,
		Follow:         follow,
	})
	if err != nil {
		m.delete("", id, false)
		return nil, fmt.Errorf("httpapi: encode the log subscription: %w", err)
	}
	envelope := protocol.Envelope{
		Version: protocol.WireVersion,
		Type:    protocol.TypeLogsSubscribe,
		Payload: payload,
	}
	if err := m.hub.Send(ctx, nodeID, envelope); err != nil {
		m.delete("", id, false)
		return nil, err
	}

	// The timer starts after the request reached the agent, so an expired
	// session unsubscribes the subscription after the agent subscribed to
	// it and never before.
	m.mu.Lock()
	if current, known := m.subs[id]; known && current == subscription && !subscription.expiresAt.IsZero() {
		wait := subscription.expiresAt.Sub(m.now())
		if wait < 0 {
			wait = 0
		}
		subscription.timer = time.AfterFunc(wait, func() { m.expire(id) })
	}
	m.mu.Unlock()
	return subscription, nil
}

// delete ends a subscription. A subscription of another session is not
// deleted, so a page cannot end the source of another page.
func (m *logSubscriptions) delete(sessionID, subscriptionID string, notify bool) (*logSubscription, bool) {
	m.mu.Lock()
	subscription, known := m.subs[subscriptionID]
	if !known || (sessionID != "" && subscription.sessionID != sessionID) {
		m.mu.Unlock()
		return nil, false
	}
	delete(m.subs, subscriptionID)
	if subscription.timer != nil {
		subscription.timer.Stop()
	}
	m.mu.Unlock()

	if notify {
		m.unsubscribe(subscription)
	}
	return subscription, true
}

// expire ends a subscription whose session ended.
func (m *logSubscriptions) expire(subscriptionID string) {
	if _, ok := m.delete("", subscriptionID, true); ok {
		log.Printf("httpapi: the log subscription %s ended with the session", subscriptionID)
	}
}

// attachStream registers an event stream of a session.
func (m *logSubscriptions) attachStream(session auth.Session) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.streams[session.ID]++
}

// detachStream ends an event stream of a session. The last stream of a
// session ends the subscriptions of the session, so a browser that
// disappeared leaves no source of the logs open.
func (m *logSubscriptions) detachStream(session auth.Session) {
	if m == nil {
		return
	}

	m.mu.Lock()
	m.streams[session.ID]--
	if m.streams[session.ID] > 0 {
		m.mu.Unlock()
		return
	}
	delete(m.streams, session.ID)
	var ended []*logSubscription
	for id, subscription := range m.subs {
		if subscription.sessionID != session.ID {
			continue
		}
		delete(m.subs, id)
		if subscription.timer != nil {
			subscription.timer.Stop()
		}
		ended = append(ended, subscription)
	}
	m.mu.Unlock()

	for _, subscription := range ended {
		m.unsubscribe(subscription)
	}
}

// handleFrame hands a piece of the logs of an agent to the event stream of
// the browser.
func (m *logSubscriptions) handleFrame(ctx context.Context, nodeID string, envelope protocol.Envelope) error {
	frame, err := protocol.DecodeLogFrame(envelope.Payload)
	if err != nil {
		return err
	}

	m.mu.Lock()
	subscription, known := m.subs[frame.SubscriptionID]
	if !known {
		// The page deleted the subscription while the frame was on its
		// way. That is not an error.
		m.mu.Unlock()
		return nil
	}
	if subscription.nodeID != nodeID {
		m.mu.Unlock()
		return fmt.Errorf("httpapi: the node %s sent the logs of the subscription %s of another node", nodeID, frame.SubscriptionID)
	}
	expired := !subscription.expiresAt.IsZero() && m.now().After(subscription.expiresAt)
	truncated := frame.Truncated
	if frame.ErrorCode == "" && frame.Text != "" {
		if subscription.offsetKnown && frame.Offset > subscription.lastOffset {
			// A piece between the last one and this one did not arrive,
			// for example because a queue of the transport dropped it.
			truncated = true
		}
		if end := frame.Offset + uint64(len(frame.Text)); !subscription.offsetKnown || end > subscription.lastOffset {
			subscription.lastOffset = end
			subscription.offsetKnown = true
		}
	}
	id := subscription.id
	sessionID := subscription.sessionID
	m.mu.Unlock()

	if expired {
		m.expire(id)
		return nil
	}
	m.events.publish(Event{
		Type:      EventLog,
		ObjectID:  id,
		Offset:    frame.Offset,
		Text:      frame.Text,
		Truncated: truncated,
		ErrorCode: frame.ErrorCode,
		sessionID: sessionID,
	})
	return nil
}

// unsubscribe tells the agent that a subscription ended. A frame that does
// not reach the agent leaves only a read that ends with the connection.
func (m *logSubscriptions) unsubscribe(subscription *logSubscription) {
	if subscription == nil || m.hub == nil {
		return
	}
	payload, err := json.Marshal(protocol.LogRequest{SubscriptionID: subscription.id})
	if err != nil {
		log.Printf("httpapi: encode the end of the log subscription %s: %v", subscription.id, err)
		return
	}
	envelope := protocol.Envelope{
		Version: protocol.WireVersion,
		Type:    protocol.TypeLogsUnsubscribe,
		Payload: payload,
	}
	if err := m.hub.Send(context.Background(), subscription.nodeID, envelope); err != nil {
		log.Printf("httpapi: end the log subscription %s: %v", subscription.id, err)
	}
}

// newLogSubscriptionID returns a random identifier of a subscription.
func newLogSubscriptionID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("httpapi: generate the log subscription: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

// registerLogRoutes mounts the subscriptions of the logs.
func (s *Server) registerLogRoutes() {
	s.mux.HandleFunc("/api/v1/log-subscriptions", s.handleLogSubscriptions)
	s.mux.HandleFunc("/api/v1/log-subscriptions/{id}", s.handleLogSubscription)
}

// handleLogSubscriptions creates a subscription of the logs of a container.
//
// The browser names the node and the container; the control端 checks that
// the node is enrolled and that the container belongs to the inventory of
// the node. The browser never names an address of an agent, so it cannot
// choose a target of its own.
func (s *Server) handleLogSubscriptions(w http.ResponseWriter, r *http.Request) {
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
		NodeID      string `json:"node_id"`
		ContainerID string `json:"container_id"`
		Tail        *int   `json:"tail"`
		Follow      *bool  `json:"follow"`
	}
	if err := decodeRequest(w, r, &body); err != nil {
		writeRequestError(w, err)
		return
	}

	nodeID := strings.TrimSpace(body.NodeID)
	containerID := strings.TrimSpace(body.ContainerID)
	fields := map[string]string{}
	if nodeID == "" {
		fields["node_id"] = "必须指定节点"
	}
	if containerID == "" {
		fields["container_id"] = "必须指定容器"
	}
	tail := protocol.DefaultLogTail
	if body.Tail != nil {
		if *body.Tail < 0 || *body.Tail > protocol.MaxLogTail {
			fields["tail"] = fmt.Sprintf("行数必须在 0 到 %d 之间", protocol.MaxLogTail)
		} else {
			tail = protocol.NormalizeLogTail(*body.Tail)
		}
	}
	if len(fields) > 0 {
		writeErrorFields(w, http.StatusBadRequest, "validation_failed", "日志订阅的参数无效", fields)
		return
	}
	follow := true
	if body.Follow != nil {
		follow = *body.Follow
	}

	record, err := s.enrollment.NodeRecord(r.Context(), nodeID)
	if err != nil {
		if errors.Is(err, agentlink.ErrUnknownNode) || errors.Is(err, agentlink.ErrNodeRevoked) {
			writeError(w, http.StatusNotFound, "node_not_found", "未找到该节点")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "无法读取节点")
		return
	}
	if record.RevokedAt != nil {
		writeError(w, http.StatusNotFound, "node_not_found", "未找到该节点")
		return
	}

	inventory, err := s.hub.ReadInventory(r.Context(), "compose", nodeID)
	if err != nil {
		switch {
		case errors.Is(err, agentlink.ErrNodeNotConnected),
			errors.Is(err, agentlink.ErrNoInventory),
			errors.Is(err, agentlink.ErrStaleInventory):
			writeError(w, http.StatusServiceUnavailable, "node_unavailable", "节点当前不可用")
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", "无法读取节点的容器")
		}
		return
	}
	found := false
	for _, container := range inventory.Containers {
		if container.ID == containerID {
			found = true
			break
		}
	}
	if !found {
		writeError(w, http.StatusNotFound, "container_not_found", "该容器不属于指定节点")
		return
	}

	subscription, err := s.logs.create(r.Context(), session, nodeID, containerID, tail, follow)
	if err != nil {
		switch {
		case errors.Is(err, agentlink.ErrNodeNotConnected), errors.Is(err, agentlink.ErrQueueFull):
			writeError(w, http.StatusServiceUnavailable, "node_unavailable", "节点当前不可用")
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", "无法创建日志订阅")
		}
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":           subscription.id,
		"node_id":      subscription.nodeID,
		"container_id": subscription.containerID,
		"tail":         subscription.tail,
		"follow":       subscription.follow,
		"expires_at":   formatTimestamp(subscription.expiresAt),
	})
}

// handleLogSubscription ends a subscription of the logs.
func (s *Server) handleLogSubscription(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "该端点只接受 DELETE 请求")
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

	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusNotFound, "subscription_not_found", "未找到该日志订阅")
		return
	}
	if _, known := s.logs.delete(session.ID, id, true); !known {
		writeError(w, http.StatusNotFound, "subscription_not_found", "未找到该日志订阅")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
