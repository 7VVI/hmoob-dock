package main

import (
	"flag"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hmoob-dock/internal/config"
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

// TestTheHealthCheckProbesTheSameAddressAsTheServer proves that the health
// check of the control端 asks the listener that the server opens.
//
// The check and the server have to agree about the address and about the
// switch of TLS. A check that probes another address reports the health of
// a service that does not answer at all.
func TestTheHealthCheckProbesTheSameAddressAsTheServer(t *testing.T) {
	for _, testCase := range []struct {
		addr   string
		cert   string
		scheme string
	}{
		{addr: "", cert: "", scheme: "http"},
		{addr: "", cert: "/etc/hmoob-dock/secrets/tls.crt", scheme: "https"},
		{addr: ":8443", cert: "/etc/hmoob-dock/secrets/tls.crt", scheme: "https"},
		{addr: ":8443", cert: "", scheme: "http"},
		{addr: "127.0.0.1:9000", cert: "", scheme: "http"},
	} {
		cfg := config.Control{TLSCert: testCase.cert}

		target, err := healthTargetFor(testCase.addr, cfg)
		if err != nil {
			t.Fatalf("the target of the health check for %q failed: %v", testCase.addr, err)
		}
		if target.scheme != testCase.scheme {
			t.Fatalf(
				"the health check for %q uses %q, want %q",
				testCase.addr, target.scheme, testCase.scheme,
			)
		}

		// The port of the probe is the port of the listener of the server.
		listen := listenAddress(testCase.addr, cfg)
		_, wantPort, err := net.SplitHostPort(listen)
		if err != nil {
			t.Fatalf("the listen address %q is malformed: %v", listen, err)
		}
		_, gotPort, err := net.SplitHostPort(target.address)
		if err != nil {
			t.Fatalf("the address of the probe %q is malformed: %v", target.address, err)
		}
		if gotPort != wantPort {
			t.Fatalf(
				"the health check for %q probes the port %s, the server listens on %s",
				testCase.addr, gotPort, wantPort,
			)
		}
	}
}

// TestTheHealthCheckProbesTheLoopbackOfAWildcardAddress proves that the
// health check asks the container itself when the listener is a wildcard.
//
// The address ":8443" names every address of the host, so the check cannot
// ask it by name. It asks the loopback, which the container reaches.
func TestTheHealthCheckProbesTheLoopbackOfAWildcardAddress(t *testing.T) {
	for _, addr := range []string{":8443", "0.0.0.0:8443"} {
		target, err := healthTargetFor(addr, config.Control{})
		if err != nil {
			t.Fatalf("the target of the health check for %q failed: %v", addr, err)
		}
		if !strings.HasPrefix(target.address, "127.0.0.1:") {
			t.Fatalf("the health check for %q probes %q, want the loopback", addr, target.address)
		}
	}
}

// TestTheHealthCheckAnswersTheHealthEndpointWithoutTLS proves that the
// health check asks a plain control端 over HTTP.
//
// A deployment without a certificate serves plain HTTP, so a check that
// insists on TLS would report a healthy deployment as broken.
func TestTheHealthCheckAnswersTheHealthEndpointWithoutTLS(t *testing.T) {
	server := httptest.NewServer(healthHandler())
	defer server.Close()

	target, err := healthTargetFor(server.Listener.Addr().String(), config.Control{})
	if err != nil {
		t.Fatalf("the target of the health check failed: %v", err)
	}
	if target.scheme != "http" {
		t.Fatalf("the health check uses %q, want %q", target.scheme, "http")
	}
	if err := probeHealth(target); err != nil {
		t.Fatalf("the health check failed: %v", err)
	}
}

// TestTheHealthCheckAnswersTheHealthEndpointWithTLS proves that the health
// check asks a control端 with a certificate over HTTPS.
//
// The certificate of the deployment is not verified: the check proves that
// the process answers and says nothing about the trust of the certificate.
func TestTheHealthCheckAnswersTheHealthEndpointWithTLS(t *testing.T) {
	server := httptest.NewTLSServer(healthHandler())
	defer server.Close()

	target, err := healthTargetFor(
		server.Listener.Addr().String(),
		config.Control{TLSCert: "/etc/hmoob-dock/secrets/tls.crt"},
	)
	if err != nil {
		t.Fatalf("the target of the health check failed: %v", err)
	}
	if target.scheme != "https" {
		t.Fatalf("the health check uses %q, want %q", target.scheme, "https")
	}
	if err := probeHealth(target); err != nil {
		t.Fatalf("the health check failed: %v", err)
	}
}

// TestTheHealthCheckFailsWhenTheEndpointIsNotHealthy proves that the health
// check reports a control端 that does not answer with a healthy status.
//
// A check that accepts every answer would report a broken deployment as
// healthy.
func TestTheHealthCheckFailsWhenTheEndpointIsNotHealthy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	target, err := healthTargetFor(server.Listener.Addr().String(), config.Control{})
	if err != nil {
		t.Fatalf("the target of the health check failed: %v", err)
	}
	if err := probeHealth(target); err == nil {
		t.Fatal("the health check accepted a control端 that is not healthy")
	}
}

// TestTheHealthCheckFailsWhenNothingAnswers proves that the health check
// reports a control端 that is not listening.
func TestTheHealthCheckFailsWhenNothingAnswers(t *testing.T) {
	server := httptest.NewServer(healthHandler())
	address := server.Listener.Addr().String()
	server.Close()

	target, err := healthTargetFor(address, config.Control{})
	if err != nil {
		t.Fatalf("the target of the health check failed: %v", err)
	}
	if err := probeHealth(target); err == nil {
		t.Fatal("the health check accepted a control端 that does not listen")
	}
}

// TestTheCommandHasAHealthCheckFlag proves that the command can ask its own
// health endpoint.
//
// The image of the control端 uses the flag for its health check, so the flag
// is part of the delivery of the command.
func TestTheCommandHasAHealthCheckFlag(t *testing.T) {
	if flag.Lookup("health-check") == nil {
		t.Fatal("the command has no flag -health-check")
	}
}

// healthHandler answers the health endpoint of the control端.
func healthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
}
