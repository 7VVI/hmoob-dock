package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"hmoob-dock/internal/alerts"
	"hmoob-dock/internal/auth"
)

// maxAlertRequestBody is the largest body that the endpoints of the alerts
// accept. A rule and an acknowledgement hold a few fields, nothing more.
const maxAlertRequestBody = 8 * 1024

// ruleRequest is the body of the endpoints that write a rule.
type ruleRequest struct {
	// Name is the title the administrator reads.
	Name string `json:"name"`
	// Description is the sentence the administrator wrote for the rule.
	Description string `json:"description"`
	// Kind is the kind of the observation the rule watches.
	Kind string `json:"kind"`
	// Severity is the weight of the rule. An empty severity saves a
	// warning, so an old caller keeps the behaviour it had.
	Severity string `json:"severity"`
	// ScopeKind names the breadth the rule watches. An empty scope saves
	// the one object of the rule.
	ScopeKind string `json:"scope_kind"`
	// ScopeID is the object the rule watches.
	ScopeID string `json:"scope_id"`
	// Metric is the value of a resource rule.
	Metric string `json:"metric"`
	// Operator compares the observed value with the threshold. An empty
	// operator saves a comparison above the threshold.
	Operator string `json:"operator"`
	// Threshold is the limit of a resource rule.
	Threshold float64 `json:"threshold"`
	// ForSeconds is the time the condition has to hold, in seconds.
	ForSeconds int `json:"for_seconds"`
	// Enabled reports whether the rule is evaluated.
	Enabled *bool `json:"enabled"`
	// RecoverNotify reports whether the recovery of the rule speaks. An
	// empty field saves a recovery that speaks.
	RecoverNotify *bool `json:"recover_notify"`
}

// rulePayload is the answer of the endpoints of the rules.
type rulePayload struct {
	// ID identifies the rule.
	ID string `json:"id"`
	// Name is the title the administrator reads.
	Name string `json:"name"`
	// Description is the sentence the administrator wrote for the rule.
	Description string `json:"description"`
	// Kind is the kind of the observation the rule watches.
	Kind string `json:"kind"`
	// Severity is the weight of the rule.
	Severity string `json:"severity"`
	// ScopeKind names the breadth the rule watches.
	ScopeKind string `json:"scope_kind"`
	// ScopeID is the object the rule watches.
	ScopeID string `json:"scope_id"`
	// Metric is the value of a resource rule.
	Metric string `json:"metric"`
	// Operator compares the observed value with the threshold.
	Operator string `json:"operator"`
	// Threshold is the limit of a resource rule.
	Threshold float64 `json:"threshold"`
	// ForSeconds is the time the condition has to hold, in seconds.
	ForSeconds int `json:"for_seconds"`
	// Enabled reports whether the rule is evaluated.
	Enabled bool `json:"enabled"`
	// RecoverNotify reports whether the recovery of the rule speaks.
	RecoverNotify bool `json:"recover_notify"`
}

// eventPayload is the answer of the endpoints of the events.
type eventPayload struct {
	// ID identifies the event.
	ID string `json:"id"`
	// RuleID is the rule that opened the event.
	RuleID string `json:"rule_id"`
	// ObjectID is the object of the event.
	ObjectID string `json:"object_id"`
	// State is the state of the event.
	State string `json:"state"`
	// Summary describes the event.
	Summary string `json:"summary"`
	// StartedAt is the time the event opened.
	StartedAt string `json:"started_at"`
	// ResolvedAt is the time the event closed.
	ResolvedAt *string `json:"resolved_at"`
	// AcknowledgedAt is the time the administrator acknowledged the event.
	AcknowledgedAt *string `json:"acknowledged_at"`
}

// registerAlertRoutes mounts the API of the rules and of the events.
//
// The API belongs to the administrator: it uses the session of the
// administrator and the same origin and the token of the session.
func (s *Server) registerAlertRoutes() {
	s.mux.HandleFunc("/api/v1/alert-rules", s.handleAlertRules)
	s.mux.HandleFunc("/api/v1/alert-rules/{id}", s.handleAlertRule)
	s.mux.HandleFunc("/api/v1/alerts", s.handleAlerts)
	s.mux.HandleFunc("/api/v1/alerts/{id}/acknowledge", s.handleAlertAcknowledge)
}

// handleAlertRules answers the list and the creation of the rules.
func (s *Server) handleAlertRules(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSession(w, r); !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.listAlertRules(w, r)
	case http.MethodPost:
		s.createAlertRule(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "该端点不接受这个方法")
	}
}

// handleAlertRule answers the change and the removal of one rule.
func (s *Server) handleAlertRule(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSession(w, r); !ok {
		return
	}
	ruleID := r.PathValue("id")
	switch r.Method {
	case http.MethodGet:
		s.showAlertRule(w, r, ruleID)
	case http.MethodPut:
		s.updateAlertRule(w, r, ruleID)
	case http.MethodDelete:
		s.deleteAlertRule(w, r, ruleID)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "该端点不接受这个方法")
	}
}

// handleAlerts answers the list of the events.
func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSession(w, r); !ok {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "该端点只接受 GET 请求")
		return
	}
	activeOnly := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("active")), "true")
	events, err := s.alerts.Events(r.Context(), activeOnly)
	if err != nil {
		s.writeAlertError(w, err)
		return
	}
	payload := make([]eventPayload, 0, len(events))
	for _, event := range events {
		payload = append(payload, newEventPayload(event))
	}
	writeJSON(w, http.StatusOK, map[string]any{"alerts": payload})
}

// handleAlertAcknowledge marks an event as seen.
//
// Acknowledging is not a recovery: the event keeps its state until the
// observation shows that the anomaly is over.
func (s *Server) handleAlertAcknowledge(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAlertWrite(w, r); !ok {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "该端点只接受 POST 请求")
		return
	}
	eventID := r.PathValue("id")
	if err := s.alerts.Acknowledge(r.Context(), eventID, time.Now().UTC()); err != nil {
		s.writeAlertError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "acknowledged"})
}

// requireAlertWrite checks the session, the origin and the token of a
// request that changes the alerts.
//
// A rejected request answers with its reason: an empty answer would look
// like a success to the caller, although nothing was written.
func (s *Server) requireAlertWrite(w http.ResponseWriter, r *http.Request) (auth.Session, bool) {
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

// listAlertRules answers with the rules.
func (s *Server) listAlertRules(w http.ResponseWriter, r *http.Request) {
	rules, err := s.alerts.Rules(r.Context())
	if err != nil {
		s.writeAlertError(w, err)
		return
	}
	payload := make([]rulePayload, 0, len(rules))
	for _, rule := range rules {
		payload = append(payload, newRulePayload(rule))
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": payload})
}

// showAlertRule answers with one rule.
func (s *Server) showAlertRule(w http.ResponseWriter, r *http.Request, ruleID string) {
	rules, err := s.alerts.Rules(r.Context())
	if err != nil {
		s.writeAlertError(w, err)
		return
	}
	for _, rule := range rules {
		if rule.ID == ruleID {
			writeJSON(w, http.StatusOK, newRulePayload(rule))
			return
		}
	}
	writeError(w, http.StatusNotFound, "rule_not_found", "未找到这条告警规则")
}

// createAlertRule writes a new rule.
//
// A resource rule needs a metric and a limit. The other kinds watch an end
// or a heartbeat and have no limit of their own.
func (s *Server) createAlertRule(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAlertWrite(w, r); !ok {
		return
	}
	var body ruleRequest
	if !s.readAlertBody(w, r, &body) {
		return
	}
	fields := validateRule(body)
	if len(fields) > 0 {
		writeErrorFields(w, http.StatusBadRequest, "validation_failed", "这条告警规则的内容不合法", fields)
		return
	}
	rule := alerts.Rule{
		ID:           alerts.NewRuleID(),
		Name:         strings.TrimSpace(body.Name),
		Description:  strings.TrimSpace(body.Description),
		Kind:         body.Kind,
		Severity:     severityOf(body.Severity),
		ScopeKind:    scopeKindOf(body.ScopeKind),
		ScopeID:      strings.TrimSpace(body.ScopeID),
		Metric:       body.Metric,
		Operator:     operatorOf(body.Operator),
		Threshold:    body.Threshold,
		For:          time.Duration(body.ForSeconds) * time.Second,
		Enabled:      true,
		RecoverQuiet: false,
	}
	if body.Enabled != nil {
		rule.Enabled = *body.Enabled
	}
	if body.RecoverNotify != nil {
		rule.RecoverQuiet = !*body.RecoverNotify
	}
	if err := s.alerts.PutRule(r.Context(), rule); err != nil {
		s.writeAlertError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, newRulePayload(rule))
}

// updateAlertRule changes one rule.
func (s *Server) updateAlertRule(w http.ResponseWriter, r *http.Request, ruleID string) {
	if _, ok := s.requireAlertWrite(w, r); !ok {
		return
	}
	var body ruleRequest
	if !s.readAlertBody(w, r, &body) {
		return
	}
	fields := validateRule(body)
	if len(fields) > 0 {
		writeErrorFields(w, http.StatusBadRequest, "validation_failed", "这条告警规则的内容不合法", fields)
		return
	}
	rule := alerts.Rule{
		ID:           ruleID,
		Name:         strings.TrimSpace(body.Name),
		Description:  strings.TrimSpace(body.Description),
		Kind:         body.Kind,
		Severity:     severityOf(body.Severity),
		ScopeKind:    scopeKindOf(body.ScopeKind),
		ScopeID:      strings.TrimSpace(body.ScopeID),
		Metric:       body.Metric,
		Operator:     operatorOf(body.Operator),
		Threshold:    body.Threshold,
		For:          time.Duration(body.ForSeconds) * time.Second,
		Enabled:      true,
		RecoverQuiet: false,
	}
	if body.Enabled != nil {
		rule.Enabled = *body.Enabled
	}
	if body.RecoverNotify != nil {
		rule.RecoverQuiet = !*body.RecoverNotify
	}
	if err := s.alerts.PutRule(r.Context(), rule); err != nil {
		s.writeAlertError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newRulePayload(rule))
}

// deleteAlertRule removes one rule.
//
// The events of the rule stay in the history: the administrator still reads
// what happened. A removed rule opens no event and sends no notification.
func (s *Server) deleteAlertRule(w http.ResponseWriter, r *http.Request, ruleID string) {
	if _, ok := s.requireAlertWrite(w, r); !ok {
		return
	}
	if err := s.alerts.DeleteRule(r.Context(), ruleID); err != nil {
		s.writeAlertError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// readAlertBody reads the body of a request of the alerts.
func (s *Server) readAlertBody(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxAlertRequestBody)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "请求的内容不是合法的 JSON")
		return false
	}
	return true
}

// maxRuleNameLength bounds the name of a rule. The name is a title, not a
// description.
const maxRuleNameLength = 64

// maxRuleDescriptionLength bounds the description of a rule.
const maxRuleDescriptionLength = 200

// validateRule checks the body of a rule.
func validateRule(body ruleRequest) map[string]string {
	fields := map[string]string{}
	if strings.TrimSpace(body.Name) == "" {
		fields["name"] = "这条规则需要一个名称"
	}
	if utf8.RuneCountInString(strings.TrimSpace(body.Name)) > maxRuleNameLength {
		fields["name"] = "名称不能超过 64 个字符"
	}
	if utf8.RuneCountInString(strings.TrimSpace(body.Description)) > maxRuleDescriptionLength {
		fields["description"] = "描述不能超过 200 个字符"
	}
	switch body.Severity {
	case "", alerts.SeverityInfo, alerts.SeverityWarning, alerts.SeverityCritical:
	default:
		fields["severity"] = "这个级别无法识别"
	}
	switch body.ScopeKind {
	case "", alerts.ScopeAll, alerts.ScopeContainer, alerts.ScopeNamespace:
	default:
		fields["scope_kind"] = "这个监控范围无法识别"
	}
	switch body.Operator {
	case "", alerts.OperatorGreaterThan, alerts.OperatorGreaterOrEqual,
		alerts.OperatorLessThan, alerts.OperatorLessOrEqual:
	default:
		fields["operator"] = "这个触发条件无法识别"
	}

	// The breadth of the rule decides the object it names. A rule of all
	// watches every container and names no object; the other scopes name
	// one. A rule of a lost agent always names its node.
	scopeKind := scopeKindOf(body.ScopeKind)
	needsObject := scopeKind != alerts.ScopeAll || body.Kind == alerts.KindAgentOffline
	if needsObject && strings.TrimSpace(body.ScopeID) == "" {
		fields["scope_id"] = "这条规则必须指定它监视的对象"
	}

	switch body.Kind {
	case alerts.KindResource:
		if strings.TrimSpace(body.Metric) == "" {
			fields["metric"] = "资源告警必须指定指标"
		}
		if body.Threshold <= 0 {
			fields["threshold"] = "资源告警必须指定一个大于零的阈值"
		}
	case alerts.KindContainerExit, alerts.KindAgentOffline:
	default:
		fields["kind"] = "这条规则的类型无法识别"
	}
	if body.ForSeconds < 0 {
		fields["for_seconds"] = "持续时间不能是负数"
	}
	return fields
}

// severityOf turns the severity of a request into the severity of a rule.
// An empty severity is a warning, so an old caller keeps its behaviour.
func severityOf(severity string) string {
	if strings.TrimSpace(severity) == "" {
		return alerts.SeverityWarning
	}
	return severity
}

// scopeKindOf turns the scope of a request into the scope of a rule.
// An empty scope watches the one object of the rule, so an old caller keeps
// its behaviour.
func scopeKindOf(scopeKind string) string {
	if strings.TrimSpace(scopeKind) == "" {
		return alerts.ScopeContainer
	}
	return scopeKind
}

// operatorOf turns the operator of a request into the operator of a rule.
// An empty operator compares above the threshold, so an old caller keeps
// its behaviour.
func operatorOf(operator string) string {
	if strings.TrimSpace(operator) == "" {
		return alerts.OperatorGreaterThan
	}
	return operator
}

// newRulePayload builds the answer of a rule.
func newRulePayload(rule alerts.Rule) rulePayload {
	return rulePayload{
		ID:            rule.ID,
		Name:          rule.Name,
		Description:   rule.Description,
		Kind:          rule.Kind,
		Severity:      rule.Severity,
		ScopeKind:     rule.ScopeKind,
		ScopeID:       rule.ScopeID,
		Metric:        rule.Metric,
		Operator:      rule.Operator,
		Threshold:     rule.Threshold,
		ForSeconds:    rule.ForSeconds,
		Enabled:       rule.Enabled,
		RecoverNotify: !rule.RecoverQuiet,
	}
}

// newEventPayload builds the answer of an event.
func newEventPayload(event alerts.Event) eventPayload {
	payload := eventPayload{
		ID:         event.ID,
		RuleID:     event.RuleID,
		ObjectID:   event.ObjectID,
		State:      event.State,
		Summary:    event.Summary,
		StartedAt:  event.StartedAt.UTC().Format(time.RFC3339Nano),
		ResolvedAt: nil,
	}
	if event.ResolvedAt != nil {
		value := event.ResolvedAt.UTC().Format(time.RFC3339Nano)
		payload.ResolvedAt = &value
	}
	if event.AcknowledgedAt != nil {
		value := event.AcknowledgedAt.UTC().Format(time.RFC3339Nano)
		payload.AcknowledgedAt = &value
	}
	return payload
}

// writeAlertError answers with the error of the alerts.
func (s *Server) writeAlertError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, alerts.ErrRuleNotFound), errors.Is(err, alerts.ErrEventNotFound):
		writeError(w, http.StatusNotFound, "not_found", "未找到请求的资源")
	default:
		writeError(w, http.StatusInternalServerError, "internal_error", "操作没有完成，请稍后再试")
	}
}
