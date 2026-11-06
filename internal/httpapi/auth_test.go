package httpapi_test

import (
	"bytes"
	"context"
	"database/sql"
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

	"hmoob-dock/internal/auth"
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
	t       *testing.T
	handler http.Handler
	server  *httptest.Server
	client  *http.Client
	// db is the database of the control端. The tests use it to prepare a
	// state that the API cannot reach.
	db *sql.DB
}

// authServerConfig changes the configuration of a test control端.
type authServerConfig struct {
	// trustedProxies are the networks of the reverse proxies whose
	// forwarding headers the control端 believes.
	trustedProxies []string
	// publicURL is the externally reachable address of the control端.
	publicURL string
	// plain serves the control端 without TLS.
	plain bool
}

// newAuthServer starts a control端 that knows the bootstrap credential.
func newAuthServer(t *testing.T) *authServer {
	t.Helper()
	return newAuthServerWith(t, authServerConfig{})
}

// newAuthServerWith starts a control端 with a changed configuration.
func newAuthServerWith(t *testing.T, options authServerConfig) *authServer {
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
		DBPath:            dbPath,
		KeyFile:           keyPath,
		BootstrapFile:     bootstrapPath,
		PublicURL:         options.publicURL,
		TrustedProxyCIDRs: options.trustedProxies,
	})
	if err != nil {
		t.Fatalf("httpapi.NewServer: %v", err)
	}
	if closer, ok := handler.(io.Closer); ok {
		t.Cleanup(func() { closer.Close() })
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}

	var server *httptest.Server
	var client *http.Client
	if options.plain {
		server = httptest.NewServer(handler)
		client = &http.Client{Jar: jar}
	} else {
		server = httptest.NewTLSServer(handler)
		client = server.Client()
		client.Jar = jar
	}
	t.Cleanup(server.Close)

	return &authServer{t: t, handler: handler, server: server, client: client, db: db}
}

// fillAuditEvents fills the table of the audit events up with the given
// number of rows. It prepares the state of a control端 whose audit table
// reached its capacity.
func fillAuditEvents(t *testing.T, db *sql.DB, rows int) {
	t.Helper()

	if _, err := db.Exec(
		`INSERT INTO audit_events (id, occurred_at, action, target_id, result)
		 SELECT 'fill-' || value, 0, 'fill', 'admin', 'success'
		 FROM (WITH RECURSIVE seq(value) AS (SELECT 1 UNION ALL SELECT value + 1 FROM seq WHERE value < ?)
		       SELECT value FROM seq)`,
		rows,
	); err != nil {
		t.Fatalf("fill the audit events: %v", err)
	}
}

// host returns the host of the control端 without its scheme.
func (s *authServer) host() string {
	host := strings.TrimPrefix(s.server.URL, "https://")
	return strings.TrimPrefix(host, "http://")
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
	s.initializeWithOrigin(s.server.URL)
}

// initializeWithOrigin creates the administrator of the control端 with the
// given origin.
func (s *authServer) initializeWithOrigin(origin string) {
	s.t.Helper()

	response := s.do(http.MethodPost, "/api/v1/auth/initialize", map[string]string{
		"bootstrap_token": bootstrapCredential,
		"password":        adminPassword,
	}, withOrigin(origin))
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
		"password":        "short",
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

func TestTheServerReportsTheStateOfTheAdministrator(t *testing.T) {
	server := newAuthServer(t)

	state, ok := server.handler.(interface {
		Initialized(context.Context) (bool, error)
	})
	if !ok {
		t.Fatal("the handler of the control端 does not report the state of the administrator")
	}
	initialized, err := state.Initialized(context.Background())
	if err != nil {
		t.Fatalf("Initialized: %v", err)
	}
	if initialized {
		t.Fatal("the administrator is initialized before the initialization")
	}

	server.initialize()

	initialized, err = state.Initialized(context.Background())
	if err != nil {
		t.Fatalf("Initialized: %v", err)
	}
	if !initialized {
		t.Fatal("the administrator is not initialized after the initialization")
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

func TestTheLoginCountsTheAttemptsOfEveryClientBehindATrustedProxy(t *testing.T) {
	// The peer of the test server is 127.0.0.1, so it belongs to the
	// trusted network of the deployment.
	server := newAuthServerWith(t, authServerConfig{trustedProxies: []string{"127.0.0.0/8"}})
	server.initialize()

	// The first client uses up its budget.
	for attempt := 0; attempt < 5; attempt++ {
		response := server.post("/api/v1/auth/login", map[string]string{
			"password": "a-wrong-test-password",
		}, withHeader("X-Forwarded-For", "203.0.113.10"))
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want %d", attempt, response.StatusCode, http.StatusUnauthorized)
		}
	}
	response := server.post("/api/v1/auth/login", map[string]string{
		"password": "a-wrong-test-password",
	}, withHeader("X-Forwarded-For", "203.0.113.10"))
	defer response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusTooManyRequests)
	}

	// Another client behind the same proxy has its own budget.
	response = server.post("/api/v1/auth/login", map[string]string{
		"password": "a-wrong-test-password",
	}, withHeader("X-Forwarded-For", "203.0.113.11"))
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
}

func TestTheOriginIsComparedByItsHostOnly(t *testing.T) {
	// The control端 answers plain HTTP while the browser sends the public
	// HTTPS origin, which is the case of a proxy that terminates the TLS.
	server := newAuthServerWith(t, authServerConfig{plain: true})
	server.initialize()

	response := server.do(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"password": adminPassword,
	}, withOrigin("https://"+server.host()))
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
}

func TestTheOriginOfAnotherHostIsRejected(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()

	// The host of the origin is the one of the control端, but the port
	// is another one.
	response := server.do(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"password": adminPassword,
	}, withOrigin("https://"+server.host()+":8443"))
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}
	payload := server.decode(response)
	if payload["code"] != "cross_origin" {
		t.Fatalf("error code = %q, want cross_origin", payload["code"])
	}
}

func TestTheModifyingRequestsRejectACrossOriginRequest(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()

	response := server.do(http.MethodPost, "/api/v1/auth/password", map[string]string{
		"current_password": adminPassword,
		"new_password":     "a-new-strong-password",
	}, withOrigin("https://attacker.example"), withHeader("X-CSRF-Token", csrf))
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("password change status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}
	payload := server.decode(response)
	if payload["code"] != "cross_origin" {
		t.Fatalf("error code = %q, want cross_origin", payload["code"])
	}

	response = server.do(http.MethodPost, "/api/v1/auth/logout", nil,
		withOrigin("https://attacker.example"), withHeader("X-CSRF-Token", csrf))
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("logout status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}
	payload = server.decode(response)
	if payload["code"] != "cross_origin" {
		t.Fatalf("error code = %q, want cross_origin", payload["code"])
	}

	// The rejected requests changed nothing.
	response = server.get("/api/v1/auth/session")
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("session status = %d, want %d", response.StatusCode, http.StatusOK)
	}
}

func TestThePasswordChangeReportsTheFieldOfTheNewPassword(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()

	response := server.post("/api/v1/auth/password", map[string]string{
		"current_password": adminPassword,
		"new_password":     "short",
	}, withHeader("X-CSRF-Token", csrf))
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
		t.Fatalf("error payload = %v, want the fields of the new password", payload)
	}
	if fields["new_password"] == nil {
		t.Fatalf("error payload = %v, want a message for the new password", payload)
	}
	if fields["password"] != nil {
		t.Fatalf("error payload = %v, want no message for the password", payload)
	}
}

func TestTheLoginBeforeTheInitializationIsLimitedByItsOwnBudget(t *testing.T) {
	server := newAuthServer(t)

	// Before the initialization there is no administrator at all, so every
	// attempt fails. The attempts are counted in the budget of this phase,
	// because an anonymous client must not write an endless number of audit
	// events.
	for attempt := 0; attempt < 5; attempt++ {
		response := server.post("/api/v1/auth/login", map[string]string{"password": adminPassword})
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want %d", attempt, response.StatusCode, http.StatusUnauthorized)
		}
	}

	response := server.post("/api/v1/auth/login", map[string]string{"password": adminPassword})
	defer response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusTooManyRequests)
	}
	payload := server.decode(response)
	if payload["code"] != "rate_limited" {
		t.Fatalf("error code = %q, want rate_limited", payload["code"])
	}

	// The budget of this phase does not keep the administrator out: the
	// correct password opens a session after the initialization.
	server.initialize()

	response = server.post("/api/v1/auth/login", map[string]string{"password": adminPassword})
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want %d", response.StatusCode, http.StatusOK)
	}
}

func TestTheBudgetOfTheCredentialsIsNotSpentBeforeTheInitialization(t *testing.T) {
	server := newAuthServer(t)

	// The attempts before the initialization are counted in their own
	// budget.
	for attempt := 0; attempt < 5; attempt++ {
		response := server.post("/api/v1/auth/login", map[string]string{"password": adminPassword})
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want %d", attempt, response.StatusCode, http.StatusUnauthorized)
		}
	}

	server.initialize()

	// The budget of the credentials is untouched by those attempts, so the
	// wrong passwords are counted from the beginning.
	for attempt := 0; attempt < 5; attempt++ {
		response := server.post("/api/v1/auth/login", map[string]string{"password": "a-wrong-test-password"})
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want %d", attempt, response.StatusCode, http.StatusUnauthorized)
		}
	}

	response := server.post("/api/v1/auth/login", map[string]string{"password": "a-wrong-test-password"})
	defer response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusTooManyRequests)
	}
}

func TestTheLoginAnswersWhenTheAuditTableIsFull(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()

	// The audit table reached its capacity, so no further audit event can
	// be stored.
	fillAuditEvents(t, server.db, auth.AuditMaxEvents-1)

	var buffer bytes.Buffer
	writer := log.Writer()
	log.SetOutput(&buffer)
	defer log.SetOutput(writer)

	// A wrong password is answered with the result of the authentication
	// and not with an error of the server.
	response := server.post("/api/v1/auth/login", map[string]string{"password": "a-wrong-test-password"})
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("login status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}

	// The correct password opens a session as well.
	response = server.post("/api/v1/auth/login", map[string]string{"password": adminPassword})
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want %d", response.StatusCode, http.StatusOK)
	}

	// The problem is reported, so it does not pass unnoticed.
	if !strings.Contains(buffer.String(), "audit table is full") {
		t.Fatalf("the log = %q, want a report about the full audit table", buffer.String())
	}
}

func TestTheAuthenticationResponsesAreNotStored(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()

	// The login answer holds the CSRF token.
	response := server.post("/api/v1/auth/login", map[string]string{"password": adminPassword})
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if got := response.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache control = %q, want no-store", got)
	}

	response = server.get("/api/v1/auth/session")
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("session status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if got := response.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache control = %q, want no-store", got)
	}

	// The error answers are not stored either.
	response = server.post("/api/v1/auth/login", map[string]string{"password": "a-wrong-test-password"})
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("login status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
	if got := response.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache control = %q, want no-store", got)
	}
}

func TestTheConfiguredPublicHostWarnsAboutAMismatchingRequestHost(t *testing.T) {
	server := newAuthServerWith(t, authServerConfig{publicURL: "https://public.example.com"})

	var buffer bytes.Buffer
	writer := log.Writer()
	log.SetOutput(&buffer)
	defer log.SetOutput(writer)

	server.initializeWithOrigin("https://public.example.com")

	// The origin of the browser matches the configured public address, so
	// the request is not rejected although the host of the request is
	// another one.
	response := server.do(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"password": adminPassword,
	}, withOrigin("https://public.example.com"))
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}

	warning := buffer.String()
	if !strings.Contains(warning, "public.example.com") {
		t.Fatalf("the log = %q, want a warning about the configured public host", warning)
	}
	if !strings.Contains(warning, server.host()) {
		t.Fatalf("the log = %q, want a warning about the host of the request", warning)
	}
}
