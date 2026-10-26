package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"hmoob-dock/internal/auth"
	"hmoob-dock/internal/deploy"
	"hmoob-dock/internal/model"
	"hmoob-dock/internal/yamlspec"
)

// The limits of the management API of the applications.
const (
	// maxDeploymentRequestBody is the largest body the endpoints of the
	// applications accept. The body holds a document of at most 1 MiB and
	// variables of at most 256 KiB, so it is bigger than the body of the
	// authentication endpoints.
	maxDeploymentRequestBody = 2 << 20
	// defaultVersionPageSize is the number of versions the list answers
	// when the request names no size.
	defaultVersionPageSize = 20
	// maxVersionPageSize is the largest number of versions the list
	// answers.
	maxVersionPageSize = 100
)

// registerApplicationRoutes mounts the management API of the applications.
//
// The API belongs to the administrator and uses the session of the
// administrator. Every request that changes something needs the CSRF token
// of the session as well.
func (s *Server) registerApplicationRoutes() {
	s.mux.HandleFunc("/api/v1/applications", s.handleApplications)
	s.mux.HandleFunc("/api/v1/applications/{id}", s.handleApplication)
	s.mux.HandleFunc("/api/v1/applications/{id}/preview", s.handleApplicationPreview)
	s.mux.HandleFunc("/api/v1/applications/{id}/deployments", s.handleApplicationDeploy)
	s.mux.HandleFunc("/api/v1/applications/{id}/versions", s.handleApplicationVersions)
	s.mux.HandleFunc("/api/v1/applications/{id}/versions/{version}", s.handleApplicationVersion)
}

// applicationPayload is an application with the configuration of its
// latest version.
type applicationPayload struct {
	model.App
	// Config is the configuration of the latest version of the
	// application. It is empty while the application has no version.
	Config model.Config `json:"config"`
}

// previewPayload is the answer of the preview of a deployment.
//
// The preview holds the warnings of the configuration and the services that
// the deployment removes. The flag tells the administrator whether the
// deployment needs a confirmation: a warning about a variable that was not
// provided does not need one.
type previewPayload struct {
	// Digest identifies the semantics of the configuration.
	Digest string `json:"digest"`
	// RemovedServices are the services the deployment removes.
	RemovedServices []string `json:"removed_services"`
	// Warnings are the notices of the configuration.
	Warnings []string `json:"warnings"`
	// RiskWarnings are the notices that need a confirmation.
	RiskWarnings []string `json:"risk_warnings"`
	// InventoryRevision identifies the state of the target the preview was
	// computed from.
	InventoryRevision string `json:"inventory_revision"`
	// RequiresConfirmation reports that the deployment needs an explicit
	// confirmation of the administrator.
	RequiresConfirmation bool `json:"requires_confirmation"`
}

// handleApplications answers the list and the creation of the
// applications.
func (s *Server) handleApplications(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.listApplications(w, r)
	case http.MethodPost:
		s.createApplication(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "该端点只接受 GET 和 POST 请求")
	}
}

// listApplications reports the applications of the control端.
func (s *Server) listApplications(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireSession(w, r); !ok {
		return
	}

	apps, err := s.apps.ListApps(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "无法读取应用列表")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"applications": apps})
}

// createApplication creates an application.
//
// The target of the application has to belong to the control端 and its state
// has to be readable: the platform never takes over a resource it cannot
// see. The adopt flag records that the administrator confirmed the takeover
// of the resources of the namespace that the platform does not manage.
func (s *Server) createApplication(w http.ResponseWriter, r *http.Request) {
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
		Name      string `json:"name"`
		Mode      string `json:"mode"`
		TargetID  string `json:"target_id"`
		Namespace string `json:"namespace"`
		Adopt     bool   `json:"adopt"`
	}
	if err := decodeDeploymentRequest(w, r, &body); err != nil {
		writeDeploymentRequestError(w, err)
		return
	}

	app, err := s.apps.CreateApp(r.Context(), model.App{
		Name:      body.Name,
		Mode:      body.Mode,
		TargetID:  body.TargetID,
		Namespace: body.Namespace,
	}, body.Adopt)
	if err != nil {
		writeDeployError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, applicationPayload{App: app})
}

// handleApplication reports an application with its latest configuration.
func (s *Server) handleApplication(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "该端点只接受 GET 请求")
		return
	}
	if _, ok := s.requireSession(w, r); !ok {
		return
	}

	appID := strings.TrimSpace(r.PathValue("id"))
	app, err := s.apps.GetApp(r.Context(), appID)
	if err != nil {
		writeDeployError(w, err)
		return
	}
	payload := applicationPayload{App: app}
	if app.LatestVersionID != "" {
		version, err := s.apps.GetVersion(r.Context(), appID, app.LatestVersionID)
		if err != nil {
			writeDeployError(w, err)
			return
		}
		payload.Config = version.Config
	}
	writeJSON(w, http.StatusOK, payload)
}

// handleApplicationPreview reports the impact of a configuration on the
// target of an application.
//
// The preview only reads: it changes neither the database nor the target.
func (s *Server) handleApplicationPreview(w http.ResponseWriter, r *http.Request) {
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
		BaseVersionID string       `json:"base_version_id"`
		Config        model.Config `json:"config"`
	}
	if err := decodeDeploymentRequest(w, r, &body); err != nil {
		writeDeploymentRequestError(w, err)
		return
	}

	appID := strings.TrimSpace(r.PathValue("id"))
	preview, err := s.apps.Preview(r.Context(), appID, body.BaseVersionID, body.Config)
	if err != nil {
		writeDeployError(w, err)
		return
	}
	risks := deploy.RiskWarnings(preview.Warnings)
	writeJSON(w, http.StatusOK, previewPayload{
		Digest:               preview.Digest,
		RemovedServices:      preview.RemovedServices,
		Warnings:             preview.Warnings,
		RiskWarnings:         risks,
		InventoryRevision:    preview.InventoryRevision,
		RequiresConfirmation: len(risks) > 0 || len(preview.RemovedServices) > 0,
	})
}

// handleApplicationDeploy writes the version and the task of a deployment.
func (s *Server) handleApplicationDeploy(w http.ResponseWriter, r *http.Request) {
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
		IdempotencyKey    string                     `json:"idempotency_key"`
		BaseVersionID     string                     `json:"base_version_id"`
		PreviewDigest     string                     `json:"preview_digest"`
		InventoryRevision string                     `json:"inventory_revision"`
		Config            model.Config               `json:"config"`
		ConfirmedRemoval  deploy.RemovalConfirmation `json:"confirmed_removal"`
		AcceptWarnings    bool                       `json:"accept_warnings"`
	}
	if err := decodeDeploymentRequest(w, r, &body); err != nil {
		writeDeploymentRequestError(w, err)
		return
	}

	appID := strings.TrimSpace(r.PathValue("id"))
	task, err := s.apps.Submit(r.Context(), deploy.SubmitRequest{
		AppID:             appID,
		BaseVersionID:     body.BaseVersionID,
		IdempotencyKey:    body.IdempotencyKey,
		PreviewDigest:     body.PreviewDigest,
		InventoryRevision: body.InventoryRevision,
		Config:            body.Config,
		ConfirmedRemoval:  body.ConfirmedRemoval,
		AcceptWarnings:    body.AcceptWarnings,
	})
	if err != nil {
		writeDeployError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, task)
}

// handleApplicationVersions reports the versions of an application.
//
// The list holds no configuration: the history is read often and the
// configuration of a version is decrypted only when the administrator opens
// it. The list is paged, so it never returns the whole history at once.
func (s *Server) handleApplicationVersions(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "该端点只接受 GET 请求")
		return
	}
	if _, ok := s.requireSession(w, r); !ok {
		return
	}

	appID := strings.TrimSpace(r.PathValue("id"))
	versions, err := s.apps.ListVersions(r.Context(), appID)
	if err != nil {
		writeDeployError(w, err)
		return
	}
	limit, offset := pageOf(r)
	total := len(versions)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"versions": versions[offset:end],
		"total":    total,
		"limit":    limit,
		"offset":   offset,
	})
}

// handleApplicationVersion reports one version of an application with its
// configuration.
func (s *Server) handleApplicationVersion(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "该端点只接受 GET 请求")
		return
	}
	if _, ok := s.requireSession(w, r); !ok {
		return
	}

	appID := strings.TrimSpace(r.PathValue("id"))
	versionID := strings.TrimSpace(r.PathValue("version"))
	version, err := s.apps.GetVersion(r.Context(), appID, versionID)
	if err != nil {
		writeDeployError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, version)
}

// decodeDeploymentRequest reads the body of a request of the management
// API of the applications.
//
// The body is limited, so a huge body cannot make the control端 read it.
func decodeDeploymentRequest(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxDeploymentRequestBody)
	if err := json.NewDecoder(r.Body).Decode(target); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return errRequestTooLarge
		}
		return errInvalidRequest
	}
	return nil
}

// writeDeploymentRequestError answers a request body that could not be
// read.
func writeDeploymentRequestError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errRequestTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "请求正文超过 2 MiB 上限")
	case errors.Is(err, errInvalidRequest):
		writeError(w, http.StatusBadRequest, "invalid_request", "请求正文必须是合法的 JSON")
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", "请求正文无法读取")
	}
}

// writeDeployError answers an error of the deployment service.
//
// The errors of the service name the kind of the failure, so the API can
// answer with the status code and the message that belong to it.
func writeDeployError(w http.ResponseWriter, err error) {
	var configError *yamlspec.Error
	if errors.As(err, &configError) {
		writeConfigError(w, configError)
		return
	}

	switch {
	case errors.Is(err, deploy.ErrBusy):
		writeError(w, http.StatusConflict, "app_busy", "该应用有一个未结束的部署任务")
	case errors.Is(err, deploy.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "部署请求与应用的当前状态不一致，请重新预览")
	case errors.Is(err, deploy.ErrIdempotencyConflict):
		writeError(w, http.StatusConflict, "idempotency_conflict", "该幂等键已经用于另一个部署请求")
	case errors.Is(err, deploy.ErrConfirmationRequired):
		writeError(w, http.StatusUnprocessableEntity, "confirmation_required", "该部署需要管理员明确确认后才能执行")
	case errors.Is(err, deploy.ErrUnknownApp):
		writeError(w, http.StatusNotFound, "app_not_found", "未找到该应用")
	case errors.Is(err, deploy.ErrUnknownVersion):
		writeError(w, http.StatusNotFound, "version_not_found", "未找到该版本")
	case errors.Is(err, deploy.ErrUnknownTask):
		writeError(w, http.StatusNotFound, "deployment_not_found", "未找到该部署任务")
	case errors.Is(err, deploy.ErrUnknownTarget):
		writeError(w, http.StatusNotFound, "target_not_found", "未找到该目标节点或集群")
	case errors.Is(err, deploy.ErrTargetUnavailable):
		writeError(w, http.StatusServiceUnavailable, "target_unavailable", "目标当前不可用，请恢复与目标的连接后重试")
	case errors.Is(err, deploy.ErrNamespaceConflict):
		writeError(w, http.StatusConflict, "namespace_conflict", "该目标上已经有应用在使用这个命名空间")
	case errors.Is(err, deploy.ErrPlatformResource):
		writeError(w, http.StatusUnprocessableEntity, "platform_resource", "该命名空间包含平台自身的容器，不能纳入受管理应用")
	case errors.Is(err, deploy.ErrInvalidApp), errors.Is(err, deploy.ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, "validation_failed", "提交的内容不合法")
	default:
		writeError(w, http.StatusInternalServerError, "internal_error", "部署请求失败")
	}
}

// writeConfigError answers an invalid configuration with the fields that
// failed the validation.
//
// Every field carries the path and the line of the document, so the
// administrator sees where the document has to change.
func writeConfigError(w http.ResponseWriter, failure *yamlspec.Error) {
	payload := map[string]any{
		"code":    failure.Code,
		"message": failure.Message,
	}
	if len(failure.Fields) > 0 {
		fields := make([]map[string]any, 0, len(failure.Fields))
		for _, field := range failure.Fields {
			entry := map[string]any{"path": field.Path, "message": field.Message}
			if field.Line > 0 {
				entry["line"] = field.Line
			}
			fields = append(fields, entry)
		}
		payload["fields"] = fields
	}
	writeJSON(w, http.StatusBadRequest, payload)
}

// pageOf reads the size and the offset of a page from the query of the
// request.
func pageOf(r *http.Request) (int, int) {
	limit := defaultVersionPageSize
	if value, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && value > 0 {
		limit = value
	}
	if limit > maxVersionPageSize {
		limit = maxVersionPageSize
	}
	offset := 0
	if value, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && value > 0 {
		offset = value
	}
	return limit, offset
}
