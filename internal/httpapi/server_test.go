package httpapi_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
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

type testServer struct {
	handler http.Handler
	db      *sql.DB
	dbPath  string
	keyPath string
	webDir  string
}

func newTestServer(t *testing.T) testServer {
	t.Helper()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "control.db")
	keyPath := filepath.Join(dir, "master.key")
	webDir := filepath.Join(dir, "web")

	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	box, err := secret.New(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}

	handler, err := httpapi.NewServer(db, box, config.Control{
		DBPath:  dbPath,
		KeyFile: keyPath,
		WebDir:  webDir,
	})
	if err != nil {
		t.Fatalf("httpapi.NewServer: %v", err)
	}
	if closer, ok := handler.(io.Closer); ok {
		t.Cleanup(func() { closer.Close() })
	}

	return testServer{handler: handler, db: db, dbPath: dbPath, keyPath: keyPath, webDir: webDir}
}

// closeReadPool releases the read pool of the server, which makes the
// database unavailable for the query path.
func (s testServer) closeReadPool(t *testing.T) {
	t.Helper()

	server, ok := s.handler.(*httpapi.Server)
	if !ok {
		t.Fatal("the handler is not an *httpapi.Server")
	}
	if err := server.Close(); err != nil {
		t.Fatalf("close the read pool: %v", err)
	}
}

func TestHealthzReportsLiveness(t *testing.T) {
	server := newTestServer(t)

	recorder := httptest.NewRecorder()
	server.handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("content type = %q, want application/json", contentType)
	}

	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	if payload["status"] != "ok" {
		t.Fatalf("status field = %q, want ok", payload["status"])
	}
}

func TestHealthzDoesNotExposeConfiguration(t *testing.T) {
	server := newTestServer(t)

	recorder := httptest.NewRecorder()
	server.handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	body := recorder.Body.String()
	for name, secret := range map[string]string{
		"database path": server.dbPath,
		"key file":      server.keyPath,
		"web directory": server.webDir,
	} {
		if strings.Contains(body, secret) {
			t.Fatalf("health response exposes the %s", name)
		}
	}
}

func TestHealthzStaysAliveWhenTheDatabaseIsUnavailable(t *testing.T) {
	server := newTestServer(t)
	server.closeReadPool(t)

	recorder := httptest.NewRecorder()
	server.handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	if payload["status"] != "ok" {
		t.Fatalf("status field = %q, want ok", payload["status"])
	}
}

func TestHealthzRejectsOtherMethods(t *testing.T) {
	server := newTestServer(t)

	recorder := httptest.NewRecorder()
	server.handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/healthz", nil))

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusMethodNotAllowed)
	}
	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	if payload["code"] == "" || payload["message"] == "" {
		t.Fatalf("error payload = %v, want code and message", payload)
	}
}

func TestReadyzReportsReadyWhenTheDatabaseIsAvailable(t *testing.T) {
	server := newTestServer(t)

	recorder := httptest.NewRecorder()
	server.handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	if payload["status"] == "" {
		t.Fatalf("readiness payload = %v, want a status", payload)
	}
}

func TestReadyzReportsUnavailableDatabase(t *testing.T) {
	server := newTestServer(t)
	server.closeReadPool(t)

	recorder := httptest.NewRecorder()
	server.handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	if payload["code"] == "" || payload["message"] == "" {
		t.Fatalf("error payload = %v, want code and message", payload)
	}
}

func TestReadyzPerformsARealRead(t *testing.T) {
	server := newTestServer(t)

	// The connection stays alive, only the schema table disappears, so a
	// ping would still succeed while a real read fails.
	if _, err := server.db.Exec(`DROP TABLE schema_migrations`); err != nil {
		t.Fatalf("drop schema_migrations: %v", err)
	}

	recorder := httptest.NewRecorder()
	server.handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
}

func TestReadyzDoesNotExposeConfiguration(t *testing.T) {
	server := newTestServer(t)

	recorder := httptest.NewRecorder()
	server.handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	body := recorder.Body.String()
	for name, secret := range map[string]string{
		"database path": server.dbPath,
		"key file":      server.keyPath,
		"web directory": server.webDir,
	} {
		if strings.Contains(body, secret) {
			t.Fatalf("readiness response exposes the %s", name)
		}
	}
}

func TestUnknownRouteReturnsStructuredError(t *testing.T) {
	server := newTestServer(t)

	recorder := httptest.NewRecorder()
	server.handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/missing", nil))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	if payload["code"] == "" || payload["message"] == "" {
		t.Fatalf("error payload = %v, want code and message", payload)
	}
}

func TestNewServerRejectsInvalidDependencies(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()
	box, err := secret.New(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}

	if _, err := httpapi.NewServer(nil, box, config.Control{DBPath: filepath.Join(dir, "control.db")}); err == nil {
		t.Fatal("NewServer accepted a missing database handle")
	}
	if _, err := httpapi.NewServer(db, nil, config.Control{DBPath: filepath.Join(dir, "control.db")}); err == nil {
		t.Fatal("NewServer accepted a missing secret box")
	}
	if _, err := httpapi.NewServer(db, box, config.Control{}); err == nil {
		t.Fatal("NewServer accepted a configuration without a database path")
	}
}

func TestNewServerRejectsAMismatchedDatabasePath(t *testing.T) {
	dir := t.TempDir()
	writeDir := filepath.Join(dir, "write")
	readDir := filepath.Join(dir, "read")
	for _, path := range []string{writeDir, readDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatalf("create %s: %v", path, err)
		}
	}

	writeDB, err := store.Open(filepath.Join(writeDir, "control.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer writeDB.Close()
	readDB, err := store.Open(filepath.Join(readDir, "control.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer readDB.Close()

	box, err := secret.New(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}

	// The write pool addresses the database of writeDir while the
	// configuration points at the database of readDir.
	handler, err := httpapi.NewServer(writeDB, box, config.Control{DBPath: filepath.Join(readDir, "control.db")})
	if err == nil {
		if closer, ok := handler.(io.Closer); ok {
			closer.Close()
		}
		t.Fatal("NewServer accepted a read pool that addresses another database")
	}
}
