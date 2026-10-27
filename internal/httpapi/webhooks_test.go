package httpapi_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"hmoob-dock/internal/httpapi"
)

// webhookTransport answers the notifications of the control端 without a
// connection.
type webhookTransport struct {
	requests []string
	status   int
}

// RoundTrip records the request and answers it.
func (t *webhookTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	body := ""
	if request.Body != nil {
		content, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		body = string(content)
	}
	t.requests = append(t.requests, body)

	status := t.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     make(http.Header),
		Request:    request,
	}, nil
}

// putWebhook saves the target of the notifications.
func putWebhook(t *testing.T, server *authServer, csrf string, payload any) *http.Response {
	t.Helper()

	response := server.do(http.MethodPut, "/api/v1/webhook", payload,
		withOrigin(server.server.URL), withHeader("X-CSRF-Token", csrf))
	return response
}

// TestTheWebhookNeedsASession proves that the endpoints of the target
// belong to the administrator.
func TestTheWebhookNeedsASession(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()

	response := server.get("/api/v1/webhook")
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
}

// TestTheWebhookSavesTheTargetAndHidesTheSecret proves that the address
// of the target is stored encrypted and that the answer of the API never
// carries it in the clear.
func TestTheWebhookSavesTheTargetAndHidesTheSecret(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()

	const address = "https://hooks.example.com/services/T00/B00/secret"
	response := putWebhook(t, server, csrf, map[string]any{
		"url":                   address,
		"allowed_private_cidrs": []string{"10.0.0.0/8"},
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	body := readBody(t, response)
	if strings.Contains(body, "T00") || strings.Contains(body, "secret") {
		t.Fatalf("the answer carries the secret of the address: %s", body)
	}
	if !strings.Contains(body, "https://hooks.example.com/***") {
		t.Fatalf("the answer holds no masked address: %s", body)
	}

	// The database holds the address in the clear nowhere.
	var ciphertext []byte
	if err := server.db.QueryRow(`SELECT url_ciphertext FROM webhook_targets WHERE id = 'default'`).
		Scan(&ciphertext); err != nil {
		t.Fatalf("read the target: %v", err)
	}
	if strings.Contains(string(ciphertext), "hooks.example.com") {
		t.Fatal("the database holds the address in the clear")
	}

	// The list of the target holds the masked address only.
	response = server.get("/api/v1/webhook")
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	body = readBody(t, response)
	if strings.Contains(body, "T00") || strings.Contains(body, "/services/") {
		t.Fatalf("the list carries the secret of the address: %s", body)
	}
	if !strings.Contains(body, "https://hooks.example.com/***") {
		t.Fatalf("the list holds no masked address: %s", body)
	}
}

// TestTheWebhookKeepsTheAddressWhenTheRequestHasNone proves that a
// request without an address keeps the address that is stored already.
func TestTheWebhookKeepsTheAddressWhenTheRequestHasNone(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()

	const address = "https://hooks.example.com/services/T00/B00/secret"
	response := putWebhook(t, server, csrf, map[string]any{"url": address})
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}

	response = putWebhook(t, server, csrf, map[string]any{"enabled": false})
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	body := readBody(t, response)
	if !strings.Contains(body, "https://hooks.example.com/***") {
		t.Fatalf("the answer holds no masked address: %s", body)
	}
	if !strings.Contains(body, `"enabled":false`) {
		t.Fatalf("the answer does not report the disabled target: %s", body)
	}
}

// TestTheWebhookRefusesTheMaskedAddress proves that the display form of
// the address cannot be written back.
//
// Writing it back would replace a working address with a form that
// cannot be used.
func TestTheWebhookRefusesTheMaskedAddress(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()

	const address = "https://hooks.example.com/services/T00/B00/secret"
	response := putWebhook(t, server, csrf, map[string]any{"url": address})
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}

	response = putWebhook(t, server, csrf, map[string]any{"url": "https://hooks.example.com/***"})
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}
	payload := server.decode(response)
	if payload["code"] != "validation_failed" {
		t.Fatalf("the error = %v, want a validation failure", payload)
	}

	// The stored address is still the one of the administrator.
	response = server.get("/api/v1/webhook")
	defer response.Body.Close()
	body := readBody(t, response)
	if !strings.Contains(body, "https://hooks.example.com/***") {
		t.Fatalf("the list holds no masked address: %s", body)
	}
}

// TestTheWebhookRefusesAnUnsafeAddress proves that the address of the
// target is checked before it is stored.
func TestTheWebhookRefusesAnUnsafeAddress(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()

	for _, address := range []string{
		"ftp://hooks.example.com/notify",
		"http://user:secret@hooks.example.com/notify",
		"https://hooks.example.com/notify#fragment",
	} {
		response := putWebhook(t, server, csrf, map[string]any{"url": address})
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("the address %q was accepted: %d", address, response.StatusCode)
		}
		response.Body.Close()
	}
}

// TestTheWebhookRefusesABroadNetwork proves that the allowed networks are
// private networks.
//
// A list that covers the whole internet is not an allowance of a private
// network, so it is refused.
func TestTheWebhookRefusesABroadNetwork(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()

	response := putWebhook(t, server, csrf, map[string]any{
		"url":                   "https://hooks.example.com/notify",
		"allowed_private_cidrs": []string{"0.0.0.0/0"},
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}
}

// TestTheWebhookNeedsTheOriginAndTheToken proves that a request of
// another site cannot change the target.
func TestTheWebhookNeedsTheOriginAndTheToken(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()

	// The request of another site is refused.
	response := server.do(http.MethodPut, "/api/v1/webhook",
		map[string]any{"url": "https://hooks.example.com/notify"},
		withOrigin("https://elsewhere.example.com"), withHeader("X-CSRF-Token", csrf))
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}
	response.Body.Close()

	// The request without the token of the session is refused.
	response = server.do(http.MethodPut, "/api/v1/webhook",
		map[string]any{"url": "https://hooks.example.com/notify"},
		withOrigin(server.server.URL))
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}
	response.Body.Close()
}

// TestTheWebhookTestSendsANotification proves that the manual test sends
// a notification to the target of the administrator.
func TestTheWebhookTestSendsANotification(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()

	transport := &webhookTransport{}
	if err := httpapi.UseWebhookClientForTest(server.handler, &http.Client{Transport: transport}); err != nil {
		t.Fatalf("use the client of the notifications: %v", err)
	}

	response := putWebhook(t, server, csrf, map[string]any{"url": "https://hooks.example.com/notify"})
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}

	response = server.post("/api/v1/webhook/test", nil, withHeader("X-CSRF-Token", csrf))
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if len(transport.requests) != 1 {
		t.Fatalf("the target received %d notifications, want 1", len(transport.requests))
	}
	document := transport.requests[0]
	for _, field := range []string{"notification_id", "event_id", "transition", "state", "occurred_at", "summary"} {
		if !strings.Contains(document, field) {
			t.Fatalf("the document has no field %q: %s", field, document)
		}
	}
	for _, forbidden := range []string{"yaml", "variables", "logs", "token"} {
		if strings.Contains(document, forbidden) {
			t.Fatalf("the document carries %q: %s", forbidden, document)
		}
	}
}

// TestTheWebhookTestNeedsATarget proves that a test without a target is
// refused instead of reaching an address of its own.
func TestTheWebhookTestNeedsATarget(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()

	transport := &webhookTransport{}
	if err := httpapi.UseWebhookClientForTest(server.handler, &http.Client{Transport: transport}); err != nil {
		t.Fatalf("use the client of the notifications: %v", err)
	}

	response := server.post("/api/v1/webhook/test", nil, withHeader("X-CSRF-Token", csrf))
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}
	if len(transport.requests) != 0 {
		t.Fatalf("the target received %d notifications, want none", len(transport.requests))
	}
}

// TestTheWebhookTestNeedsTheToken proves that the manual test is an
// action of the administrator.
func TestTheWebhookTestNeedsTheToken(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	server.login()

	response := server.post("/api/v1/webhook/test", nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}
}

// TestTheWebhookReportsTheStateOfTheNotifications proves that the answer
// of the target carries the state of the queue of the notifications.
func TestTheWebhookReportsTheStateOfTheNotifications(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	server.login()

	response := server.get("/api/v1/webhook")
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	body := readBody(t, response)
	for _, field := range []string{`"webhook"`, `"notifications"`, `"pending"`, `"delivered"`, `"failed"`} {
		if !strings.Contains(body, field) {
			t.Fatalf("the answer has no field %q: %s", field, body)
		}
	}
}

// readBody reads the body of a response.
func readBody(t *testing.T, response *http.Response) string {
	t.Helper()

	content, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the body: %v", err)
	}
	return string(content)
}
