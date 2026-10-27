package httpapi

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"hmoob-dock/internal/alerts"
	"hmoob-dock/internal/auth"
	"hmoob-dock/internal/notify"
)

// maxWebhookRequestBody is the largest body that the endpoints of the
// webhook accept. The address of a target and a few networks fit into it.
const maxWebhookRequestBody = 8 * 1024

// The limits of the background sender of the notifications.
const (
	// notifyInterval is the time between two runs of the sender.
	notifyInterval = 10 * time.Second
	// notifyRunTimeout bounds one run of the sender, so a target that
	// does not answer cannot hold the sender forever.
	notifyRunTimeout = 60 * time.Second
)

// webhookRequest is the body of the endpoint that saves the target.
type webhookRequest struct {
	// URL is the address of the target. An empty address keeps the
	// address that is stored already.
	URL string `json:"url"`
	// AllowedPrivateCIDRs are the private networks the administrator
	// allows the target to live in.
	AllowedPrivateCIDRs []string `json:"allowed_private_cidrs"`
	// Enabled reports whether the target is used. An absent value keeps
	// the state that is stored already.
	Enabled *bool `json:"enabled"`
}

// webhookPayload is the answer of the endpoints of the target.
//
// The answer holds the masked form of the address only: the address of a
// target can carry a secret in its path, so it never leaves the control
// 端 in the clear.
type webhookPayload struct {
	// ID identifies the target.
	ID string `json:"id"`
	// Configured reports whether a target was configured already.
	Configured bool `json:"configured"`
	// URLMasked is the display form of the address of the target.
	URLMasked string `json:"url_masked"`
	// AllowedPrivateCIDRs are the private networks the administrator
	// allowed the target to live in.
	AllowedPrivateCIDRs []string `json:"allowed_private_cidrs"`
	// Enabled reports whether the target is used.
	Enabled bool `json:"enabled"`
	// UpdatedAt is the time of the last change of the target.
	UpdatedAt *string `json:"updated_at"`
}

// notificationStatus is the state of the queue of the notifications.
type notificationStatus struct {
	// Pending is the number of the notifications that wait for their
	// delivery.
	Pending int `json:"pending"`
	// Delivered is the number of the notifications that reached their
	// target.
	Delivered int `json:"delivered"`
	// Failed is the number of the notifications that gave up after their
	// last try.
	Failed int `json:"failed"`
	// Skipped is the number of the notifications that were not queued,
	// because the queue was full.
	Skipped int `json:"skipped"`
	// LastDeliveredAt is the time of the last delivery.
	LastDeliveredAt *string `json:"last_delivered_at"`
}

// registerWebhookRoutes mounts the endpoints of the notification target.
//
// The endpoints belong to the administrator: they use the session of the
// administrator, and the ones that change or use the target also the
// origin and the token of the session.
func (s *Server) registerWebhookRoutes() {
	s.mux.HandleFunc("/api/v1/webhook", s.handleWebhook)
	s.mux.HandleFunc("/api/v1/webhook/test", s.handleWebhookTest)
}

// handleWebhook answers the state of the target and saves it.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.showWebhook(w, r)
	case http.MethodPut:
		s.saveWebhook(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "该端点不接受这个方法")
	}
}

// showWebhook answers the masked configuration of the target and the
// state of the queue of the notifications.
func (s *Server) showWebhook(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSession(w, r); !ok {
		return
	}
	target, found, err := s.webhooks.Target(r.Context(), notify.DefaultTargetID)
	if err != nil {
		log.Printf("control: read the webhook target: %v", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "操作没有完成，请稍后再试")
		return
	}
	counts, err := s.alerts.Counts(r.Context())
	if err != nil {
		log.Printf("control: read the state of the notifications: %v", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "操作没有完成，请稍后再试")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"webhook":       newWebhookPayload(target, found),
		"notifications": newNotificationStatus(counts),
	})
}

// saveWebhook saves the target of the notifications.
//
// An address that the request does not carry keeps the address that is
// stored already, and the masked form of the address is refused: writing
// it back would replace a working address with a form that cannot be
// used.
func (s *Server) saveWebhook(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireWebhookWrite(w, r); !ok {
		return
	}
	var body webhookRequest
	if !s.readWebhookBody(w, r, &body) {
		return
	}

	target, found, err := s.webhooks.Target(r.Context(), notify.DefaultTargetID)
	if err != nil {
		log.Printf("control: read the webhook target: %v", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "操作没有完成，请稍后再试")
		return
	}

	fields := map[string]string{}
	address := strings.TrimSpace(body.URL)
	switch {
	case address == "":
		if !found {
			fields["url"] = "必须填写通知地址"
		}
	case found && address == target.MaskedURL:
		fields["url"] = "不能把脱敏地址写回配置，请填写完整的通知地址"
	default:
		if _, err := notify.ParseTargetURL(address); err != nil {
			fields["url"] = "通知地址必须是合法的 HTTP 或 HTTPS 地址，且不能包含凭据或片段"
		}
	}
	if err := notify.ValidateAllowedCIDRs(body.AllowedPrivateCIDRs); err != nil {
		fields["allowed_private_cidrs"] = "只允许私网 CIDR，例如 10.0.0.0/8"
	}
	if len(fields) > 0 {
		writeErrorFields(w, http.StatusBadRequest, "validation_failed", "通知目标的配置不合法", fields)
		return
	}

	saved := notify.Target{
		ID:                  notify.DefaultTargetID,
		URL:                 target.URL,
		AllowedPrivateCIDRs: body.AllowedPrivateCIDRs,
		Enabled:             true,
	}
	if address != "" {
		saved.URL = address
	}
	if body.Enabled != nil {
		saved.Enabled = *body.Enabled
	} else if found {
		saved.Enabled = target.Enabled
	}
	if err := s.webhooks.Save(r.Context(), saved); err != nil {
		log.Printf("control: write the webhook target: %v", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "操作没有完成，请稍后再试")
		return
	}

	written, found, err := s.webhooks.Target(r.Context(), notify.DefaultTargetID)
	if err != nil {
		log.Printf("control: read the webhook target: %v", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "操作没有完成，请稍后再试")
		return
	}
	writeJSON(w, http.StatusOK, newWebhookPayload(written, found))
}

// handleWebhookTest sends a test notification to the target.
//
// The test is an outgoing action: it leaves the control端 and reaches the
// target of the administrator. It only happens when the administrator
// asks for it.
func (s *Server) handleWebhookTest(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireWebhookWrite(w, r); !ok {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "该端点只接受 POST 请求")
		return
	}

	target, found, err := s.webhooks.Target(r.Context(), notify.DefaultTargetID)
	if err != nil {
		log.Printf("control: read the webhook target: %v", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "操作没有完成，请稍后再试")
		return
	}
	if !found || strings.TrimSpace(target.URL) == "" {
		writeError(w, http.StatusBadRequest, "not_configured", "尚未配置通知地址")
		return
	}

	sender, err := s.webhookSender(target)
	if err != nil {
		log.Printf("control: build the sender of the webhook: %v", err)
		writeError(w, http.StatusBadRequest, "invalid_target", "通知地址无法使用，请检查通知地址的配置")
		return
	}
	if err := sender.Send(r.Context(), notify.TestNotification()); err != nil {
		// The reason of the failure is written without the address of
		// the target: the address can carry a secret in its path.
		log.Printf("control: the test notification was not delivered: %s", notify.DescribeFailure(err))
		writeError(w, http.StatusBadGateway, "delivery_failed", "测试通知发送失败，请检查通知地址与网络")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

// webhookSender builds the sender of the target.
func (s *Server) webhookSender(target notify.Target) (*notify.Sender, error) {
	config := notify.WebhookConfig{
		ID:                  target.ID,
		URL:                 target.URL,
		AllowedPrivateCIDRs: target.AllowedPrivateCIDRs,
		Enabled:             target.Enabled,
	}
	if s.webhookClient != nil {
		return notify.NewSenderWithClient(config, s.webhookClient)
	}
	return notify.NewSender(config)
}

// requireWebhookWrite checks the session, the origin and the token of a
// request that changes the target or uses it.
//
// A request of another site and a request without the token of the
// session are refused with a reason, so the caller of the API knows
// what went wrong.
func (s *Server) requireWebhookWrite(w http.ResponseWriter, r *http.Request) (auth.Session, bool) {
	session, ok := s.requireSession(w, r)
	if !ok {
		return session, false
	}
	if !s.sameOrigin(r) {
		writeError(w, http.StatusForbidden, "cross_origin", "请求来源不被允许")
		return session, false
	}
	if !auth.CheckCSRF(r, session) {
		writeError(w, http.StatusForbidden, "csrf_invalid", "CSRF 令牌无效")
		return session, false
	}
	return session, true
}

// readWebhookBody reads the body of a request of the target.
func (s *Server) readWebhookBody(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookRequestBody)
	if err := json.NewDecoder(r.Body).Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "请求的内容不是合法的 JSON")
		return false
	}
	return true
}

// newWebhookPayload builds the answer of the target.
func newWebhookPayload(target notify.Target, found bool) webhookPayload {
	payload := webhookPayload{
		ID:                  target.ID,
		Configured:          found,
		URLMasked:           target.MaskedURL,
		AllowedPrivateCIDRs: target.AllowedPrivateCIDRs,
		Enabled:             target.Enabled,
	}
	if payload.ID == "" {
		payload.ID = notify.DefaultTargetID
	}
	if payload.AllowedPrivateCIDRs == nil {
		payload.AllowedPrivateCIDRs = []string{}
	}
	if found && !target.UpdatedAt.IsZero() {
		value := target.UpdatedAt.UTC().Format(time.RFC3339Nano)
		payload.UpdatedAt = &value
	}
	return payload
}

// newNotificationStatus builds the answer of the state of the queue.
func newNotificationStatus(counts alerts.Counts) notificationStatus {
	status := notificationStatus{
		Pending:   counts.Pending,
		Delivered: counts.Delivered,
		Failed:    counts.Failed,
		Skipped:   counts.Skipped,
	}
	if !counts.LastDeliveredAt.IsZero() {
		value := counts.LastDeliveredAt.UTC().Format(time.RFC3339Nano)
		status.LastDeliveredAt = &value
	}
	return status
}

// startNotifyWorker starts the background sender of the notifications.
//
// The worker claims every notification before it sends it, so a restart
// of the control端 neither loses a notification nor sends one twice at
// the same time.
func (s *Server) startNotifyWorker() {
	if s.notifyWorker == nil {
		return
	}
	s.backgroundWorkers.Add(1)
	go func() {
		defer s.backgroundWorkers.Done()
		ticker := time.NewTicker(notifyInterval)
		defer ticker.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(s.ctx, notifyRunTimeout)
				err := s.notifyWorker.RunOnce(ctx)
				cancel()
				if err != nil && s.ctx.Err() == nil {
					log.Printf("control: send the notifications: %v", err)
				}
			}
		}
	}()
}
