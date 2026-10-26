package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"hmoob-dock/internal/httpapi"
)

// TestTheHandlerReportsTheStateOfTheAdministrator proves that the handler of
// the control端 can tell the command whether the administrator exists
// already. The command uses it to report the state of the deployment, so it
// does not have to read the bootstrap credential a second time.
func TestTheHandlerReportsTheStateOfTheAdministrator(t *testing.T) {
	var handler initializationState = (*httpapi.Server)(nil)
	if handler == nil {
		t.Fatal("the handler of the control端 does not report the state of the administrator")
	}
}

// TestTheRouterAnswersTheAPIAndTheFrontend proves that the control端 serves
// the API and the frontend on one origin. The API answers the paths below
// /api and the two health endpoints, the frontend answers every other path.
func TestTheRouterAnswersTheAPIAndTheFrontend(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("api"))
	})
	web := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("web"))
	})

	handler := newHandler(api, web)

	for path, want := range map[string]string{
		"/api/v1/nodes":        "api",
		"/api/v1/agent/enroll": "api",
		"/healthz":             "api",
		"/readyz":              "api",
		"/":                    "web",
		"/settings":            "web",
		"/assets/app.js":       "web",
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))

		if got := recorder.Body.String(); got != want {
			t.Fatalf("the path %s was answered by %q, want %q", path, got, want)
		}
	}
}
