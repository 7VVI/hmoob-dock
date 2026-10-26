package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hmoob-dock/internal/httpapi"
)

// writeBuild writes a small frontend build into a temporary directory. The
// build holds the index of the application and one asset.
func writeBuild(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<main>app</main>"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte("console.log(1)"), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestMissingAPIRouteIsNotHTML proves that a request of the API never falls
// into the fallback of the frontend. The frontend parses the answer of the
// API as JSON, so an HTML answer would be reported as a broken API instead
// of as a missing route.
func TestMissingAPIRouteIsNotHTML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<main>app</main>"), 0600); err != nil {
		t.Fatal(err)
	}
	handler := httpapi.StaticHandler(dir)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/missing", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d", w.Code)
	}
}

// TestMissingAPIRouteAnswersWithTheCommonErrorFormat proves that the
// refused API path is answered in the error format of the API and not with
// the index of the frontend.
func TestMissingAPIRouteAnswersWithTheCommonErrorFormat(t *testing.T) {
	handler := httpapi.StaticHandler(writeBuild(t))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/missing", nil))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("content type = %q, want JSON", contentType)
	}
	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	if payload["code"] == "" || payload["message"] == "" {
		t.Fatalf("error payload = %v, want code and message", payload)
	}
	if strings.Contains(recorder.Body.String(), "<main>") {
		t.Fatal("the answer of the API holds the index of the frontend")
	}
}

// TestNavigationIsAnsweredWithTheIndex proves that a path of the frontend
// that names no file is answered with the index of the build. The frontend
// is a single page application, so its navigation needs the index.
func TestStaticNavigationIsAnsweredWithTheIndex(t *testing.T) {
	handler := httpapi.StaticHandler(writeBuild(t))

	for _, path := range []string{"/", "/settings", "/login", "/settings/"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))

		if recorder.Code != http.StatusOK {
			t.Fatalf("the path %s returned %d, want %d", path, recorder.Code, http.StatusOK)
		}
		if body := recorder.Body.String(); body != "<main>app</main>" {
			t.Fatalf("the path %s returned %q, want the index of the build", path, body)
		}
	}
}

// TestAssetIsAnsweredWithTheFile proves that a file of the build is
// answered with the file itself and not with the index.
func TestStaticAssetIsAnsweredWithTheFile(t *testing.T) {
	handler := httpapi.StaticHandler(writeBuild(t))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if body := recorder.Body.String(); body != "console.log(1)" {
		t.Fatalf("body = %q, want the file of the build", body)
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.Contains(contentType, "javascript") {
		t.Fatalf("content type = %q, want the type of the file", contentType)
	}
}

// TestMissingAssetIsNotAnsweredWithTheIndex proves that a file that the
// build does not hold is a 404. The index would answer with an empty page
// and the browser would report a broken script instead of a missing file.
func TestStaticMissingAssetIsNotAnsweredWithTheIndex(t *testing.T) {
	handler := httpapi.StaticHandler(writeBuild(t))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/assets/missing.js", nil))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
	if strings.Contains(recorder.Body.String(), "<main>") {
		t.Fatal("the missing asset was answered with the index of the frontend")
	}
}

// TestPathTraversalIsRefused proves that a path that leaves the directory
// of the build is refused. The answer must never be a file of the host.
func TestStaticPathTraversalIsRefused(t *testing.T) {
	dir := writeBuild(t)
	outside := filepath.Join(filepath.Dir(dir), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}

	handler := httpapi.StaticHandler(dir)
	for _, path := range []string{
		"/../secret.txt",
		"/assets/../../secret.txt",
		"/%2e%2e/secret.txt",
		"/%2e%2e%2fsecret.txt",
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))

		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("the path %s returned %d, want %d", path, recorder.Code, http.StatusBadRequest)
		}
		if strings.Contains(recorder.Body.String(), "secret") {
			t.Fatalf("the path %s was answered with the content of a file outside the build", path)
		}
	}
}

// TestMissingDirectoryIsReported proves that a frontend directory that
// does not exist is reported with an error. An empty page would hide the
// misconfiguration from the administrator.
func TestStaticMissingDirectoryIsReported(t *testing.T) {
	handler := httpapi.StaticHandler(filepath.Join(t.TempDir(), "missing"))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("content type = %q, want JSON", contentType)
	}
	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	if payload["code"] == "" || payload["message"] == "" {
		t.Fatalf("error payload = %v, want code and message", payload)
	}
}

// TestWithoutAFrontendEveryPathIsMissing proves that a control端 without a
// frontend answers every path of the frontend with a 404 in the error
// format. The API stays the only thing such a deployment offers.
func TestStaticWithoutAFrontendEveryPathIsMissing(t *testing.T) {
	handler := httpapi.StaticHandler("")

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/settings", nil))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
	if strings.Contains(recorder.Body.String(), "<main>") {
		t.Fatal("a control端 without a frontend answered with an index")
	}
}

// TestOnlyReadMethodsAreAccepted proves that the static resources are read
// only. A write request never touches a file of the build.
func TestStaticOnlyReadMethodsAreAccepted(t *testing.T) {
	handler := httpapi.StaticHandler(writeBuild(t))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/assets/app.js", nil))

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusMethodNotAllowed)
	}
	if allow := recorder.Header().Get("Allow"); !strings.Contains(allow, http.MethodGet) {
		t.Fatalf("the Allow header = %q, want it to name the accepted methods", allow)
	}
}
