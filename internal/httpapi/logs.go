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

// The reasons a stream of the logs ended without its text.
const (
	// logCodeNodeDisconnected reports a subscription that ended because the
	// agent of its node did not come back within the grace period.
	logCodeNodeDisconnected = "node_disconnected"
	// logCodeNodeRevoked reports a subscription that ended because the
	// administrator revoked the node.
	logCodeNodeRevoked = "node_revoked"
)

// The delays of the subscriptions.
//
// A frame of the control端 is refused while the queue of the node is full.
// The subscription keeps its state then and hands the frame over again, so
// the read of the logs of the agent is not left behind. A subscription whose
// node disappeared waits for the grace period before it ends with an error,
// so a short reconnect does not force the page to start over.
var (
	logRetryInterval  = time.Second
	logReconnectGrace = 30 * time.Second
)

// errNoEventStream reports a creation without an open event stream. The
// pieces of a subscription only reach a page through the event stream, so a
// subscription that nobody listens to would lose its tail.
var errNoEventStream = errors.New("httpapi: the session has no event stream")

// logSender hands the frames of the log subscriptions to the agents.
type logSender interface {
	// Send hands a frame to the agent of a node.
	Send(ctx context.Context, nodeID string, message protocol.Envelope) error
	// Connected reports whether a live connection of the node exists.
	Connected(nodeID string) bool
}

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
	// timer ends the subscription with the session.
	timer *time.Timer
	// graceTimer ends a subscription whose agent is gone.
	graceTimer *time.Timer
	// retryTimer hands a refused frame to the agent again.
	retryTimer *time.Timer
	// retryType is the type of the frame the retry sends.
	retryType string
	// ending reports that the subscription was ended and only waits for the
	// agent to accept the unsubscribe.
	ending bool
	// suspended reports that the agent of the node disappeared and the
	// subscription waits for its return or for the grace period.
	suspended bool
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
//
// A subscription is not dropped before the agent accepted its end. A queue
// of the node that is full refuses the frame, and the manager tries again
// instead of leaving a read of Docker behind. When the agent reconnects, the
// manager starts the subscriptions that are still alive again.
type logSubscriptions struct {
	hub    logSender
	events *eventBroker
	now    func() time.Time

	mu sync.Mutex
	// subs holds the subscription by its ID.
	subs map[string]*logSubscription
	// streams counts the open event streams of a session.
	streams map[string]int
}

// newLogSubscriptions builds the manager of the log subscriptions.
func newLogSubscriptions(hub logSender, events *eventBroker) *logSubscriptions {
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
	// The pieces of the subscription only reach the page through an event
	// stream. Without one the agent would read a tail that nobody receives,
	// so the creation is refused instead of losing the first frames.
	if m.streams[session.ID] == 0 {
		m.mu.Unlock()
		return nil, errNoEventStream
	}
	m.subs[id] = subscription
	m.mu.Unlock()

	if err := m.send(ctx, subscription, protocol.TypeLogsSubscribe); err != nil {
		m.forget(subscription)
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
//
// The state stays until the agent accepted the end: a queue that is full
// does not lose the unsubscribe, so the read of the logs does not outlive
// the page.
func (m *logSubscriptions) delete(sessionID, subscriptionID string, notify bool) (*logSubscription, bool) {
	m.mu.Lock()
	subscription, known := m.subs[subscriptionID]
	if !known || (sessionID != "" && subscription.sessionID != sessionID) {
		m.mu.Unlock()
		return nil, false
	}
	if !notify {
		m.forgetLocked(subscription)
		m.mu.Unlock()
		return subscription, true
	}
	if !subscription.ending {
		subscription.ending = true
		m.stopTimer(&subscription.graceTimer)
		m.stopTimer(&subscription.retryTimer)
	}
	m.mu.Unlock()

	m.end(subscription)
	return subscription, true
}

// expire ends a subscription whose session ended.
func (m *logSubscriptions) expire(subscriptionID string) {
	m.mu.Lock()
	subscription, known := m.subs[subscriptionID]
	if !known {
		m.mu.Unlock()
		return
	}
	ending := subscription.ending
	if ending {
		// The session is over and the subscription already waits for the
		// agent; no page reads it any more, so the state is dropped.
		m.forgetLocked(subscription)
	}
	m.mu.Unlock()

	if !ending {
		m.delete("", subscriptionID, true)
	}
	log.Printf("httpapi: the log subscription %s ended with the session", subscriptionID)
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
	for _, subscription := range m.subs {
		if subscription.sessionID != session.ID {
			continue
		}
		if !subscription.ending {
			subscription.ending = true
			m.stopTimer(&subscription.graceTimer)
			m.stopTimer(&subscription.retryTimer)
		}
		ended = append(ended, subscription)
	}
	m.mu.Unlock()

	for _, subscription := range ended {
		m.end(subscription)
	}
}

// handleConnect replays the subscriptions of a node whose agent opened a
// connection.
//
// Every connection of an agent owns its own reads of Docker, so the
// subscriptions that survived the old connection have to be started again.
// A subscription keeps its ID, so the frames of the new read stay with the
// page that created it.
func (m *logSubscriptions) handleConnect(nodeID string) {
	if m == nil {
		return
	}

	m.mu.Lock()
	var active, ending []*logSubscription
	for _, subscription := range m.subs {
		if subscription.nodeID != nodeID {
			continue
		}
		if subscription.ending {
			ending = append(ending, subscription)
			continue
		}
		subscription.suspended = false
		m.stopTimer(&subscription.graceTimer)
		subscription.graceTimer = nil
		active = append(active, subscription)
	}
	m.mu.Unlock()

	for _, subscription := range ending {
		m.end(subscription)
	}
	for _, subscription := range active {
		m.resume(subscription)
	}
}

// handleDisconnect suspends the subscriptions of a node whose agent
// disappeared.
//
// A subscription waits for the grace period. A node that comes back within
// it keeps its subscriptions; a node that stays away ends them with an
// error event, so a page never waits for pieces that cannot arrive.
func (m *logSubscriptions) handleDisconnect(nodeID string) {
	if m == nil {
		return
	}
	// A newer connection that already replaced the lost one owns the
	// subscriptions now; its connect callback replays them.
	if m.hub != nil && m.hub.Connected(nodeID) {
		return
	}

	m.mu.Lock()
	// The connection may have been replaced while the callback waited for
	// the lock. The check under the lock orders it with the connect
	// callback, so a subscription is not suspended while its node is alive.
	if m.hub != nil && m.hub.Connected(nodeID) {
		m.mu.Unlock()
		return
	}
	for _, subscription := range m.subs {
		if subscription.nodeID != nodeID {
			continue
		}
		if subscription.ending {
			// The agent read ended with the connection. The state waits
			// for the return of the node or for the end of the session.
			m.stopTimer(&subscription.retryTimer)
			subscription.retryTimer = nil
			continue
		}
		if subscription.suspended {
			continue
		}
		subscription.suspended = true
		m.stopTimer(&subscription.retryTimer)
		subscription.retryTimer = nil
		subscription.graceTimer = time.AfterFunc(logReconnectGrace, func() { m.endSuspended(subscription) })
	}
	m.mu.Unlock()
}

// revokeNode ends every subscription of a revoked node with an error event.
//
// The credential of the node is not accepted any more, so the node cannot
// come back and the reads of its containers are gone.
func (m *logSubscriptions) revokeNode(nodeID string) {
	if m == nil {
		return
	}

	m.mu.Lock()
	var ended []*logSubscription
	for _, subscription := range m.subs {
		if subscription.nodeID != nodeID {
			continue
		}
		m.forgetLocked(subscription)
		ended = append(ended, subscription)
	}
	m.mu.Unlock()

	for _, subscription := range ended {
		m.publishError(subscription, logCodeNodeRevoked)
	}
}

// endSuspended ends a subscription whose agent did not return within the
// grace period.
func (m *logSubscriptions) endSuspended(subscription *logSubscription) {
	m.mu.Lock()
	if current, known := m.subs[subscription.id]; !known || current != subscription || !subscription.suspended {
		m.mu.Unlock()
		return
	}
	m.forgetLocked(subscription)
	m.mu.Unlock()
	m.publishError(subscription, logCodeNodeDisconnected)
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
	if !known || subscription.ending {
		// The page deleted the subscription while the frame was on its
		// way. That is not an error.
		m.mu.Unlock()
		return nil
	}
	if subscription.nodeID != nodeID {
		m.mu.Unlock()
		return fmt.Errorf("httpapi: the node %s sent the logs of the subscription %s of another node", nodeID, frame.SubscriptionID)
	}
	if subscription.suspended {
		// The agent of the frame belongs to a connection that is gone.
		m.mu.Unlock()
		return nil
	}
	if m.streams[subscription.sessionID] == 0 {
		// No page listens to the subscription. The frame is not counted as
		// delivered, so the positions of the next page still show the gap.
		m.mu.Unlock()
		return nil
	}
	expired := !subscription.expiresAt.IsZero() && m.now().After(subscription.expiresAt)
	truncated := frame.Truncated
	if frame.ErrorCode == "" && frame.Text != "" {
		if !subscription.offsetKnown {
			// The subscription starts in the middle of its stream, for
			// example because it joined a shared read of the agent after
			// the head of the tail left the buffer of the read.
			if frame.Offset > 0 {
				truncated = true
			}
		} else if frame.Offset > subscription.lastOffset {
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

// resume starts a subscription again after the agent of its node opened a
// new connection.
func (m *logSubscriptions) resume(subscription *logSubscription) {
	m.mu.Lock()
	if current, known := m.subs[subscription.id]; !known || current != subscription || subscription.ending {
		m.mu.Unlock()
		return
	}
	gap := subscription.offsetKnown || subscription.lastOffset > 0
	// The new read of the agent starts at its own tail, so the positions of
	// the page start over as well.
	subscription.lastOffset = 0
	subscription.offsetKnown = false
	sessionID := subscription.sessionID
	m.mu.Unlock()

	if err := m.send(context.Background(), subscription, protocol.TypeLogsSubscribe); err != nil {
		if errors.Is(err, agentlink.ErrQueueFull) || errors.Is(err, agentlink.ErrNodeNotConnected) {
			m.retryLater(subscription, protocol.TypeLogsSubscribe)
			return
		}
		log.Printf("httpapi: restart the log subscription %s: %v", subscription.id, err)
		m.fail(subscription, "logs_unavailable")
		return
	}
	if gap {
		// The pieces between the old stream and the new tail are lost.
		m.events.publish(Event{Type: EventLog, ObjectID: subscription.id, Truncated: true, sessionID: sessionID})
	}
}

// end hands the end of a subscription to the agent.
func (m *logSubscriptions) end(subscription *logSubscription) {
	m.mu.Lock()
	_, known := m.subs[subscription.id]
	m.mu.Unlock()
	if !known {
		return
	}

	err := m.send(context.Background(), subscription, protocol.TypeLogsUnsubscribe)
	switch {
	case err == nil:
		m.forget(subscription)
	case errors.Is(err, agentlink.ErrQueueFull):
		// The queue of the node was full. The read of the agent still
		// exists, so the end is tried again instead of dropping the state.
		m.retryLater(subscription, protocol.TypeLogsUnsubscribe)
	case errors.Is(err, agentlink.ErrNodeNotConnected):
		// The connection of the node ended and its reads ended with it.
		// The state waits for the return of the node, so the end still
		// reaches an agent that comes back within the session.
	default:
		log.Printf("httpapi: end the log subscription %s: %v", subscription.id, err)
		m.forget(subscription)
	}
}

// retryLater hands a refused frame to the agent again after the retry
// interval.
func (m *logSubscriptions) retryLater(subscription *logSubscription, messageType string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if current, known := m.subs[subscription.id]; !known || current != subscription {
		return
	}
	subscription.retryType = messageType
	if subscription.retryTimer != nil {
		return
	}
	subscription.retryTimer = time.AfterFunc(logRetryInterval, func() { m.retry(subscription) })
}

// retry repeats the frame that the queue of the node refused.
func (m *logSubscriptions) retry(subscription *logSubscription) {
	m.mu.Lock()
	if current, known := m.subs[subscription.id]; !known || current != subscription {
		m.mu.Unlock()
		return
	}
	subscription.retryTimer = nil
	messageType := subscription.retryType
	ending := subscription.ending
	suspended := subscription.suspended
	m.mu.Unlock()

	switch {
	case messageType == protocol.TypeLogsUnsubscribe:
		if ending {
			m.end(subscription)
		}
	case ending || suspended:
		// The subscription waits for the return of the node; the connect
		// callback starts it again.
	default:
		m.resume(subscription)
	}
}

// fail ends a subscription with an error event.
func (m *logSubscriptions) fail(subscription *logSubscription, code string) {
	m.forget(subscription)
	m.publishError(subscription, code)
}

// publishError tells the session of a subscription why its stream ended.
func (m *logSubscriptions) publishError(subscription *logSubscription, code string) {
	m.events.publish(Event{
		Type:      EventLog,
		ObjectID:  subscription.id,
		ErrorCode: code,
		sessionID: subscription.sessionID,
	})
}

// forget drops the state of a subscription.
func (m *logSubscriptions) forget(subscription *logSubscription) {
	m.mu.Lock()
	m.forgetLocked(subscription)
	m.mu.Unlock()
}

// forgetLocked drops the state of a subscription. The caller holds the
// lock.
func (m *logSubscriptions) forgetLocked(subscription *logSubscription) {
	if current, known := m.subs[subscription.id]; known && current == subscription {
		delete(m.subs, subscription.id)
	}
	m.stopTimer(&subscription.timer)
	m.stopTimer(&subscription.graceTimer)
	m.stopTimer(&subscription.retryTimer)
}

// stopTimer stops a timer if one waits.
func (m *logSubscriptions) stopTimer(timer **time.Timer) {
	if *timer == nil {
		return
	}
	(*timer).Stop()
	*timer = nil
}

// send hands one frame of a subscription to the agent of its node.
func (m *logSubscriptions) send(ctx context.Context, subscription *logSubscription, messageType string) error {
	if subscription == nil || m.hub == nil {
		return errors.New("httpapi: the hub of the agents is not available")
	}
	request := protocol.LogRequest{SubscriptionID: subscription.id}
	switch messageType {
	case protocol.TypeLogsSubscribe:
		request.ContainerID = subscription.containerID
		request.Tail = subscription.tail
		request.Follow = subscription.follow
	case protocol.TypeLogsUnsubscribe:
	default:
		return fmt.Errorf("httpapi: the log frame %q is not supported", messageType)
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("httpapi: encode the %s: %w", messageType, err)
	}
	return m.hub.Send(ctx, subscription.nodeID, protocol.Envelope{
		Version: protocol.WireVersion,
		Type:    messageType,
		Payload: payload,
	})
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
// choose a target of its own. The session needs an open event stream,
// because the pieces of the subscription travel in it.
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
		case errors.Is(err, errNoEventStream):
			writeError(w, http.StatusConflict, "event_stream_required", "会话没有活跃的事件流，无法接收日志")
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
