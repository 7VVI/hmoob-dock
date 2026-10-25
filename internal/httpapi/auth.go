package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"hmoob-dock/internal/auth"
	"hmoob-dock/internal/config"
)

const (
	// maxRequestBody is the largest request body that the authentication
	// endpoints accept.
	maxRequestBody = 8 * 1024
	// loginMaxAttempts is the number of failed logins that a source may
	// make within loginRateWindow.
	loginMaxAttempts = 5
	// loginRateWindow is the time the failed logins of a source are counted.
	loginRateWindow = 15 * time.Minute
	// bootstrapTimeout bounds the reading of the bootstrap credential at
	// the startup.
	bootstrapTimeout = 5 * time.Second
)

// The errors of the request body.
var (
	errRequestTooLarge = errors.New("httpapi: the request body is too large")
	errInvalidRequest  = errors.New("httpapi: the request body is not a JSON document")
)

// registerAuthRoutes mounts the authentication endpoints of the control端.
// The routes live under the versioned prefix of the API.
func (s *Server) registerAuthRoutes() {
	s.mux.HandleFunc("/api/v1/auth/initialize", s.handleAuthInitialize)
	s.mux.HandleFunc("/api/v1/auth/login", s.handleAuthLogin)
	s.mux.HandleFunc("/api/v1/auth/session", s.handleAuthSession)
	s.mux.HandleFunc("/api/v1/auth/logout", s.handleAuthLogout)
	s.mux.HandleFunc("/api/v1/auth/password", s.handleAuthPassword)
}

// seedBootstrap stores the bootstrap credential of the configuration. The
// credential is only read here and its content never reaches the log.
//
// The credential is only stored while no administrator exists, so a restart
// of the control端 cannot open a second initialization.
func (s *Server) seedBootstrap() error {
	if strings.TrimSpace(s.cfg.BootstrapFile) == "" {
		return nil
	}
	token, err := config.LoadBootstrapToken(s.cfg.BootstrapFile)
	if err != nil {
		return err
	}
	if token == "" {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), bootstrapTimeout)
	defer cancel()

	initialized, err := s.auth.Initialized(ctx)
	if err != nil {
		return err
	}
	if initialized {
		return nil
	}
	if err := s.auth.SeedBootstrap(ctx, token, time.Now().Add(auth.BootstrapTTL)); err != nil {
		return fmt.Errorf("httpapi: store the bootstrap credential: %w", err)
	}
	return nil
}

// handleAuthInitialize creates the administrator with the bootstrap
// credential of the deployment.
func (s *Server) handleAuthInitialize(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "该端点只接受 POST 请求")
		return
	}
	if !s.sameOrigin(r) {
		writeError(w, http.StatusForbidden, "cross_origin", "请求来源不被允许")
		return
	}

	var body struct {
		BootstrapToken string `json:"bootstrap_token"`
		Password       string `json:"password"`
	}
	if err := decodeRequest(w, r, &body); err != nil {
		writeRequestError(w, err)
		return
	}

	if err := s.auth.Initialize(r.Context(), body.BootstrapToken, body.Password); err != nil {
		switch {
		case errors.Is(err, auth.ErrAlreadyInitialized):
			writeError(w, http.StatusConflict, "already_initialized", "管理员已经初始化")
		case errors.Is(err, auth.ErrInvalidBootstrap):
			writeError(w, http.StatusUnauthorized, "invalid_bootstrap", "初始化凭据无效或已过期")
		case errors.Is(err, auth.ErrPasswordPolicy):
			writeErrorFields(w, http.StatusBadRequest, "validation_failed", "提交的密码不符合要求", passwordFields())
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", "初始化失败")
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"initialized": true})
}

// handleAuthLogin checks the password of the administrator and starts a
// session.
func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "该端点只接受 POST 请求")
		return
	}
	if !s.sameOrigin(r) {
		writeError(w, http.StatusForbidden, "cross_origin", "请求来源不被允许")
		return
	}

	var body struct {
		Password string `json:"password"`
	}
	if err := decodeRequest(w, r, &body); err != nil {
		writeRequestError(w, err)
		return
	}

	source := s.clientKey(r)
	if !s.loginLimiter.Allow(source) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "尝试次数过多，请稍后再试")
		return
	}

	session, err := s.auth.Login(r.Context(), body.Password)
	if err != nil {
		// Only a wrong password spends the budget of the source. A
		// control端 that is not initialized yet tells every visitor the
		// same thing, and that answer must not lock the administrator
		// out of the login.
		if errors.Is(err, auth.ErrInvalidPassword) {
			s.loginLimiter.Fail(source)
		}
		// The message is the same for every reason, so it does not tell an
		// attacker whether the administrator exists at all.
		if errors.Is(err, auth.ErrInvalidPassword) || errors.Is(err, auth.ErrNotInitialized) {
			writeError(w, http.StatusUnauthorized, "invalid_credentials", "登录凭据无效")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "登录失败")
		return
	}
	s.loginLimiter.Reset(source)

	setSessionCookie(w, session)
	writeJSON(w, http.StatusOK, map[string]any{
		"csrf_token": session.CSRF,
		"expires_at": formatTimestamp(session.ExpiresAt),
	})
}

// handleAuthSession reports the CSRF token and the expiry of the session of
// the request. It never returns the session token itself.
func (s *Server) handleAuthSession(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "该端点只接受 GET 请求")
		return
	}

	initialized, err := s.auth.Initialized(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "无法读取会话状态")
		return
	}
	if !initialized {
		writeError(w, http.StatusUnauthorized, "not_initialized", "管理员尚未初始化")
		return
	}

	session, err := s.auth.SessionFromRequest(r.Context(), r)
	if err != nil {
		if !errors.Is(err, auth.ErrInvalidSession) {
			writeError(w, http.StatusInternalServerError, "internal_error", "无法读取会话状态")
			return
		}
		writeError(w, http.StatusUnauthorized, "unauthenticated", "请先登录")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"csrf_token": session.CSRF,
		"expires_at": formatTimestamp(session.ExpiresAt),
	})
}

// handleAuthLogout revokes the session of the request.
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
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

	if err := s.auth.Logout(r.Context(), session.Token); err != nil {
		if errors.Is(err, auth.ErrInvalidSession) {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "请先登录")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "退出失败")
		return
	}

	clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// handleAuthPassword replaces the password of the administrator and revokes
// every session.
func (s *Server) handleAuthPassword(w http.ResponseWriter, r *http.Request) {
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
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := decodeRequest(w, r, &body); err != nil {
		writeRequestError(w, err)
		return
	}

	if err := s.auth.ChangePassword(r.Context(), session.Token, body.CurrentPassword, body.NewPassword); err != nil {
		switch {
		case errors.Is(err, auth.ErrInvalidPassword):
			writeError(w, http.StatusUnauthorized, "invalid_credentials", "当前密码不正确")
		case errors.Is(err, auth.ErrPasswordPolicy):
			writeErrorFields(w, http.StatusBadRequest, "validation_failed", "提交的密码不符合要求", newPasswordFields())
		case errors.Is(err, auth.ErrInvalidSession):
			writeError(w, http.StatusUnauthorized, "unauthenticated", "请先登录")
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", "修改密码失败")
		}
		return
	}

	// Every session is revoked by the password change, so the cookie of
	// this request is dead as well.
	clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// requireSession returns the session of the request and answers 401 when the
// request carries no valid session.
func (s *Server) requireSession(w http.ResponseWriter, r *http.Request) (auth.Session, bool) {
	session, err := s.auth.SessionFromRequest(r.Context(), r)
	if err != nil {
		if !errors.Is(err, auth.ErrInvalidSession) {
			writeError(w, http.StatusInternalServerError, "internal_error", "无法读取会话状态")
			return auth.Session{}, false
		}
		writeError(w, http.StatusUnauthorized, "unauthenticated", "请先登录")
		return auth.Session{}, false
	}
	return session, true
}

// sameOrigin reports whether the request comes from the control端 itself.
// The Origin header of a browser request names the site that sent it, so a
// request of another site is rejected.
//
// Only the host of the origin is compared. The scheme is left out on
// purpose: a deployment behind a proxy that terminates the TLS answers
// plain HTTP while the browser sends the public HTTPS origin, and a
// comparison of the scheme would lock every administrator out. The
// session cookie is bound to its site by the SameSite attribute, so the
// scheme does not weaken the protection against cross site requests.
func (s *Server) sameOrigin(r *http.Request) bool {
	s.warnAboutMismatchingHost(r)

	origin := originHost(r.Header.Get("Origin"))
	if origin == "" {
		return false
	}
	expected := s.expectedHost(r)
	if expected == "" {
		return false
	}
	return origin == expected
}

// expectedHost returns the host that the authentication endpoints expect.
// The configured public address wins, because it names the address that
// the browser uses. Without it the host of the request is used.
func (s *Server) expectedHost(r *http.Request) string {
	if configured := originHost(s.cfg.PublicURL); configured != "" {
		return configured
	}
	return originHost(s.requestOrigin(r))
}

// requestOrigin returns the origin under which the control端 answered the
// request. The scheme follows the TLS state of the request and of the
// trusted proxy in front of it.
func (s *Server) requestOrigin(r *http.Request) string {
	scheme := "http"
	if s.requestIsHTTPS(r) {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	if host == "" {
		return ""
	}
	return scheme + "://" + host
}

// warnAboutMismatchingHost reports a request that reaches the control端
// under another host than the configured public address. Such a request
// is not rejected, because the origin of the browser decides that, but
// the mismatch points at the configuration of the deployment. The
// warning is written once, because the cause is the configuration and
// not the request.
func (s *Server) warnAboutMismatchingHost(r *http.Request) {
	configured := originHost(s.cfg.PublicURL)
	if configured == "" {
		return
	}
	host := normalizeHost(r.Host)
	if host == "" || host == configured {
		return
	}
	s.hostWarning.Do(func() {
		log.Printf(
			"control: the request host %q does not match the configured public host %q; check the public URL of the deployment",
			r.Host, configured,
		)
	})
}

// originHost returns the host of an origin in a comparable form: lower
// case and without the default ports of the web. A value that is no
// origin at all becomes an empty string.
func originHost(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.EqualFold(trimmed, "null") {
		return ""
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return ""
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		// A public address without a scheme names a host as well.
		parsed, err = url.Parse("https://" + trimmed)
		if err != nil || parsed.Host == "" {
			return ""
		}
	}
	return normalizeHost(parsed.Host)
}

// normalizeHost brings a host into a comparable form: lower case and
// without the default ports of the web.
func normalizeHost(host string) string {
	normalized := strings.ToLower(strings.TrimSpace(host))
	normalized = strings.TrimSuffix(normalized, ":443")
	normalized = strings.TrimSuffix(normalized, ":80")
	return normalized
}

// setSessionCookie writes the cookie that carries the session token. The
// cookie is only sent over HTTPS, is not readable by scripts and is not
// sent with a request of another site.
func setSessionCookie(w http.ResponseWriter, session auth.Session) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    session.Token,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		Expires:  session.ExpiresAt,
	})
}

// clearSessionCookie expires the cookie that carries the session token.
func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
	})
}

// decodeRequest reads the JSON body of a request. The body is limited, so a
// huge body cannot make the control端 read it.
func decodeRequest(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	if err := json.NewDecoder(r.Body).Decode(target); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return errRequestTooLarge
		}
		return errInvalidRequest
	}
	return nil
}

// writeRequestError answers a request body that could not be read.
func writeRequestError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errRequestTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "请求正文超过 8 KiB 上限")
	case errors.Is(err, errInvalidRequest):
		writeError(w, http.StatusBadRequest, "invalid_request", "请求正文必须是合法的 JSON")
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", "请求正文无法读取")
	}
}

// formatTimestamp writes a time in the format of the API.
func formatTimestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// passwordFields is the message of the password field when the password does
// not meet the policy.
func passwordFields() map[string]string {
	return map[string]string{"password": "密码长度需为 12 到 128 个字符"}
}

// newPasswordFields is the message of the new password field when the new
// password does not meet the policy.
func newPasswordFields() map[string]string {
	return map[string]string{"new_password": "密码长度需为 12 到 128 个字符"}
}

// noStore marks the answer of an authentication endpoint as one that no
// cache may keep. The answers carry the CSRF token of the session.
func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
}
