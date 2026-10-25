package httpapi_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hmoob-dock/internal/config"
	"hmoob-dock/internal/httpapi"
	"hmoob-dock/internal/secret"
	"hmoob-dock/internal/store"
)

// bootstrapCredential is the one time credential of the test deployments.
const bootstrapCredential = "bootstrap-token-of-the-test"

// adminPassword is the password of the administrator of the tests.
const adminPassword = "a-strong-test-password"

// authServer is a running control端 with the authentication endpoints.
type authServer struct {
	t      *testing.T
	server *httptest.Server
	client *http.Client
}

// newAuthServer starts a control端 that knows the bootstrap credential.
func newAuthServer(t *testing.T) *authServer {
	t.Helper()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "control.db")
	keyPath := filepath.Join(dir, "master.key")
	bootstrapPath := filepath.Join(dir, "bootstrap.token")
	if err := os.WriteFile(bootstrapPath, []byte(bootstrapCredential), 0o600); err != nil {
		t.Fatalf("write the bootstrap credential: %v", err)
	}

	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	box, err := secret.New(bytes.Repeat([]byte{5}, 32))
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}

	handler, err := httpapi.NewServer(db, box, config.Control{
		DBPath:        dbPath,
		KeyFile:       keyPath,
		BootstrapFile: bootstrapPath,
	})
	if err != nil {
		t.Fatalf("httpapi.NewServer: %v", err)
	}
	if closer, ok := handler.(io.Closer); ok {
		t.Cleanup(func() { closer.Close() })
	}

	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	client := server.Client()
	client.Jar = jar
	return &authServer{t: t, server: server, client: client}
}

// requestOption changes a request before it is sent.
type requestOption func(*http.Request)

// withOrigin sets the Origin header of the request.
func withOrigin(origin string) requestOption {
	return func(r *http.Request) { r.Header.Set("Origin", origin) }
}

// withHeader sets a header of the request.
func withHeader(name, value string) requestOption {
	return func(r *http.Request) { r.Header.Set(name, value) }
}

// do sends a request with the given method, path and JSON body.
func (s *authServer) do(method, path string, payload any, options ...requestOption) *http.Response {
	s.t.Helper()

	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			s.t.Fatalf("encode the payload: %v", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, s.server.URL+path, body)
	if err != nil {
		s.t.Fatalf("build the request: %v", err)
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for _, option := range options {
		option(request)
	}
	response, err := s.client.Do(request)
	if err != nil {
		s.t.Fatalf("send the request: %v", err)
	}
	return response
}

// post sends a request from the origin of the control端 itself.
func (s *authServer) post(path string, payload any, options ...requestOption) *http.Response {
	s.t.Helper()
	return s.do(http.MethodPost, path, payload, append([]requestOption{withOrigin(s.server.URL)}, options...)...)
}

// get sends a request without a body.
func (s *authServer) get(path string, options ...requestOption) *http.Response {
	s.t.Helper()
	return s.do(http.MethodGet, path, nil, options...)
}

// decode reads the JSON body of a response.
func (s *authServer) decode(response *http.Response) map[string]any {
	s.t.Helper()
	defer response.Body.Close()

	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		s.t.Fatalf("decode the response body: %v", err)
	}
	return payload
}

// initialize creates the administrator of the control端.
func (s *authServer) initialize() {
	s.t.Helper()

	response := s.post("/api/v1/auth/initialize", map[string]string{
		"bootstrap_token": bootstrapCredential,
		"password":        adminPassword,
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		s.t.Fatalf("initialize status = %d, want %d", response.StatusCode, http.StatusOK)
	}
}

// login logs the administrator in and returns the CSRF token of the session.
func (s *authServer) login() string {
	s.t.Helper()

	response := s.post("/api/v1/auth/login", map[string]string{"password": adminPassword})
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		s.t.Fatalf("login status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	payload := s.decode(response)
	csrf, _ := payload["csrf_token"].(string)
	if csrf == "" {
		s.t.Fatal("the login response holds no CSRF token")
	}
	return csrf
}

func TestTheAuthenticationFlowRunsFromInitializeToThePasswordChange(t *testing.T) {
	server := newAuthServer(t)

	server.initialize()

	response := server.post("/api/v1/auth/login", map[string]string{"password": adminPassword})
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	payload := server.decode(response)
	csrf, _ := payload["csrf_token"].(string)
	if csrf == "" {
		t.Fatal("the login response holds no CSRF token")
	}
	if _, ok := payload["expires_at"]; !ok {
		t.Fatalf("the login response = %v, want an expiry", payload)
	}

	response = server.get("/api/v1/auth/session")
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("session status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	payload = server.decode(response)
	if payload["csrf_token"] != csrf {
		t.Fatalf("session CSRF token = %v, want %q", payload["csrf_token"], csrf)
	}
	if _, ok := payload["expires_at"]; !ok {
		t.Fatalf("the session response = %v, want an expiry", payload)
	}

	response = server.post("/api/v1/auth/password", map[string]string{
		"current_password": adminPassword,
		"new_password":     "a-new-strong-password",
	}, withHeader("X-CSRF-Token", csrf))
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("password change status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}

	// The password change revoked every session, so the old cookie is dead.
	response = server.get("/api/v1/auth/session")
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}

	// The new password opens a new session.
	response = server.post("/api/v1/auth/login", map[string]string{"password": "a-new-strong-password"})
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want %d", response.StatusCode, http.StatusOK)
	}
}

func TestTheSessionCookieHasTheSafeAttributes(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()

	response := server.post("/api/v1/auth/login", map[string]string{"password": adminPassword})
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want %d", response.StatusCode, http.StatusOK)
	}

	cookies := response.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %d, want 1", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != "hmoob_session" {
		t.Fatalf("cookie name = %q, want hmoob_session", cookie.Name)
	}
	if !cookie.HttpOnly {
		t.Fatal("the session cookie is readable by scripts")
	}
	if !cookie.Secure {
		t.Fatal("the session cookie is sent over plain HTTP")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie same site = %v, want strict", cookie.SameSite)
	}
	if cookie.Path != "/" {
		t.Fatalf("cookie path = %q, want /", cookie.Path)
	}
	if cookie.Expires.IsZero() {
		t.Fatal("the session cookie has no expiry")
	}
	if cookie.Value == "" {
		t.Fatal("the session cookie has no value")
	}
}

func TestTheSessionResponseHoldsNoSessionToken(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()

	response := server.post("/api/v1/auth/login", map[string]string{"password": adminPassword})
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	loginBody := server.decode(response)
	cookies := response.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %d, want 1", len(cookies))
	}
	token := cookies[0].Value

	response = server.get("/api/v1/auth/session")
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("session status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	sessionBody := server.decode(response)

	if sessionBody["csrf_token"] != loginBody["csrf_token"] {
		t.Fatalf("session CSRF token = %v, want %q", sessionBody["csrf_token"], loginBody["csrf_token"])
	}
	if _, ok := sessionBody["session_token"]; ok {
		t.Fatalf("the session response = %v, want no session token", sessionBody)
	}
	encoded, err := json.Marshal(sessionBody)
	if err != nil {
		t.Fatalf("encode the session response: %v", err)
	}
	if strings.Contains(string(encoded), token) {
		t.Fatalf("the session response holds the session token: %s", encoded)
	}
}

func TestInitializeRejectsAWrongBootstrapCredential(t *testing.T) {
	server := newAuthServer(t)

	response := server.post("/api/v1/auth/initialize", map[string]string{
		"bootstrap_token": "wrong-credential",
		"password":        adminPassword,
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
	payload := server.decode(response)
	if payload["code"] == "" || payload["message"] == "" {
		t.Fatalf("error payload = %v, want code and message", payload)
	}
}

func TestInitializeAndLoginRejectACrossOriginRequest(t *testing.T) {
	server := newAuthServer(t)

	response := server.do(http.MethodPost, "/api/v1/auth/initialize", map[string]string{
		"bootstrap_token": bootstrapCredential,
		"password":        adminPassword,
	}, withOrigin("https://attacker.example"))
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("initialize status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}

	server.initialize()

	response = server.do(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"password": adminPassword,
	}, withOrigin("https://attacker.example"))
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("login status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}
}

func TestLoginRejectsARequestWithoutAnOrigin(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()

	response := server.do(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"password": adminPassword,
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}
}

func TestTheLoginFailureMessageDoesNotTellTheReason(t *testing.T) {
	server := newAuthServer(t)

	// Before the initialization there is no administrator at all.
	response := server.post("/api/v1/auth/login", map[string]string{"password": adminPassword})
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
	withoutAdmin := server.decode(response)

	server.initialize()

	response = server.post("/api/v1/auth/login", map[string]string{"password": "a-wrong-test-password"})
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
	wrongPassword := server.decode(response)

	if withoutAdmin["message"] != wrongPassword["message"] {
		t.Fatalf("the failure messages differ: %q and %q", withoutAdmin["message"], wrongPassword["message"])
	}
	if withoutAdmin["code"] != wrongPassword["code"] {
		t.Fatalf("the failure codes differ: %q and %q", withoutAdmin["code"], wrongPassword["code"])
	}
}

func TestTheLoginLimitsTheAttemptsOfASource(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()

	// The limiter counts the peer of the connection, so a forged forwarding
	// header does not give a new budget.
	for attempt := 0; attempt < 5; attempt++ {
		response := server.post("/api/v1/auth/login", map[string]string{
			"password": "a-wrong-test-password",
		}, withHeader("X-Forwarded-For", fmt.Sprintf("203.0.113.%d", attempt)))
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want %d", attempt, response.StatusCode, http.StatusUnauthorized)
		}
	}

	response := server.post("/api/v1/auth/login", map[string]string{
		"password": "a-wrong-test-password",
	}, withHeader("X-Forwarded-For", "203.0.113.99"))
	defer response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusTooManyRequests)
	}
	payload := server.decode(response)
	if payload["code"] == "" || payload["message"] == "" {
		t.Fatalf("error payload = %v, want code and message", payload)
	}
}

func TestTheSessionRejectsAMissingOrUnknownCookie(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()

	response := server.get("/api/v1/auth/session")
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
	payload := server.decode(response)
	if payload["code"] != "unauthenticated" {
		t.Fatalf("error code = %q, want unauthenticated", payload["code"])
	}
}

func TestTheSessionReportsThatTheAdministratorIsMissing(t *testing.T) {
	server := newAuthServer(t)

	response := server.get("/api/v1/auth/session")
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
	payload := server.decode(response)
	if payload["code"] != "not_initialized" {
		t.Fatalf("error code = %q, want not_initialized", payload["code"])
	}
}

func TestTheModifyingRequestsNeedTheCSRFToken(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()

	response := server.post("/api/v1/auth/password", map[string]string{
		"current_password": adminPassword,
		"new_password":     "a-new-strong-password",
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("password change status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}

	response = server.post("/api/v1/auth/password", map[string]string{
		"current_password": adminPassword,
		"new_password":     "a-new-strong-password",
	}, withHeader("X-CSRF-Token", "not-the-csrf-token"))
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("password change status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}

	response = server.post("/api/v1/auth/logout", nil, withHeader("X-CSRF-Token", "not-the-csrf-token"))
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("logout status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}

	// The session still works, the rejected requests changed nothing.
	response = server.get("/api/v1/auth/session")
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("session status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	payload := server.decode(response)
	if payload["csrf_token"] != csrf {
		t.Fatalf("session CSRF token = %v, want %q", payload["csrf_token"], csrf)
	}
}

func TestTheLogoutRevokesTheSession(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()

	response := server.post("/api/v1/auth/logout", nil, withHeader("X-CSRF-Token", csrf))
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}

	response = server.get("/api/v1/auth/session")
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
}

func TestTheRequestBodyIsLimited(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()

	response := server.post("/api/v1/auth/login", map[string]string{
		"password": strings.Repeat("a", 9*1024),
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusRequestEntityTooLarge)
	}
}

func TestThePasswordPolicyIsReported(t *testing.T) {
	server := newAuthServer(t)

	response := server.post("/api/v1/auth/initialize", map[string]string{
		"bootstrap_token": bootstrapCredential,
		"password":        "short-passw",
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}
	payload := server.decode(response)
	if payload["code"] != "validation_failed" {
		t.Fatalf("error code = %q, want validation_failed", payload["code"])
	}
	fields, ok := payload["fields"].(map[string]any)
	if !ok {
		t.Fatalf("error payload = %v, want the fields of the password", payload)
	}
	if fields["password"] == nil {
		t.Fatalf("error payload = %v, want a message for the password", payload)
	}
}

func TestTheAuthenticationEndpointsRejectOtherMethods(t *testing.T) {
	server := newAuthServer(t)

	for path, method := range map[string]string{
		"/api/v1/auth/initialize": http.MethodGet,
		"/api/v1/auth/login":      http.MethodGet,
		"/api/v1/auth/session":    http.MethodPost,
		"/api/v1/auth/logout":     http.MethodGet,
		"/api/v1/auth/password":   http.MethodGet,
	} {
		response := server.do(method, path, nil)
		response.Body.Close()
		if response.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s status = %d, want %d", path, response.StatusCode, http.StatusMethodNotAllowed)
		}
	}
}

func TestTheLoginDoesNotWriteThePasswordToTheLog(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()

	var buffer bytes.Buffer
	writer := log.Writer()
	log.SetOutput(&buffer)
	defer log.SetOutput(writer)

	response := server.post("/api/v1/auth/login", map[string]string{"password": adminPassword})
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want %d", response.StatusCode, http.StatusOK)
	}

	if buffer.Len() != 0 {
		t.Fatalf("the login wrote to the log: %q", buffer.String())
	}
}
