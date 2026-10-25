package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

	return testServer{handler: handler, dbPath: dbPath, keyPath: keyPath, webDir: webDir}
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
