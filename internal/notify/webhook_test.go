package notify_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"hmoob-dock/internal/alerts"
	"hmoob-dock/internal/notify"
)

// recordingTransport answers the requests of a sender without a connection.
type recordingTransport struct {
	mu       sync.Mutex
	requests []recordedRequest
	status   int
	body     string
	delay    time.Duration
	// cancel ends the run of the worker while the target answers, so the
	// result of the try is written while the run is over.
	cancel context.CancelFunc
}

// recordedRequest is one request that the transport received.
type recordedRequest struct {
	Method string
	Path   string
	Body   string
	Header http.Header
}

// RoundTrip answers the request with the prepared answer.
func (t *recordingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	body := ""
	if request.Body != nil {
		content, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		body = string(content)
	}
	t.mu.Lock()
	t.requests = append(t.requests, recordedRequest{
		Method: request.Method,
		Path:   request.URL.Path,
		Body:   body,
		Header: request.Header.Clone(),
	})
	cancel := t.cancel
	t.mu.Unlock()

	if cancel != nil {
		// The run ends while the target answers.
		cancel()
	}

	if t.delay > 0 {
		select {
		case <-request.Context().Done():
			return nil, request.Context().Err()
		case <-time.After(t.delay):
		}
	}

	status := t.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(t.body)),
		Header:     make(http.Header),
		Request:    request,
	}, nil
}

// received returns the requests that the transport received.
func (t *recordingTransport) received() []recordedRequest {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]recordedRequest(nil), t.requests...)
}

// countingBody counts the bytes that were read from it.
type countingBody struct {
	reader io.Reader
	read   int
}

// Read reads from the body and counts the bytes.
func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.read += n
	return n, err
}

// Close closes the body.
func (b *countingBody) Close() error { return nil }

// fakeResolver answers the lookups of the sender with prepared answers.
type fakeResolver struct {
	mu      sync.Mutex
	calls   int
	answers [][]net.IPAddr
}

// LookupIPAddr answers with the next prepared answer.
func (r *fakeResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	index := r.calls - 1
	if index >= len(r.answers) {
		index = len(r.answers) - 1
	}
	return r.answers[index], nil
}

// lookups returns the number of the lookups the resolver answered.
func (r *fakeResolver) lookups() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// recordingConnect records the addresses the sender connects to.
type recordingConnect struct {
	mu        sync.Mutex
	addresses []string
	failure   error
}

// connect records the address and refuses the connection.
func (c *recordingConnect) connect(_ context.Context, _ string, address string) (net.Conn, error) {
	c.mu.Lock()
	c.addresses = append(c.addresses, address)
	c.mu.Unlock()
	if c.failure != nil {
		return nil, c.failure
	}
	return nil, errors.New("notify: the test does not open a connection")
}

// dialed returns the addresses the sender connected to.
func (c *recordingConnect) dialed() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.addresses...)
}

// testNotification is a notification of an event for the tests.
func testNotification() alerts.Notification {
	return alerts.Notification{
		ID:          "notification-1",
		EventID:     "event-1",
		Transition:  2,
		State:       alerts.StateActive,
		OccurredAt:  time.Unix(1800000000, 0).UTC(),
		Summary:     "对象 c1 的 cpu_percent 持续超过阈值 80",
		Environment: "rule-1",
		Object:      "c1",
	}
}

// publicResolver answers with a public address.
func publicResolver() *fakeResolver {
	return &fakeResolver{answers: [][]net.IPAddr{
		{{IP: net.ParseIP("93.184.216.34")}},
	}}
}

// TestNewSenderRefusesAnUnsafeAddress proves that only a plain address of
// HTTP is accepted.
//
// A file, a credential in the address and a fragment would all end up in
// the log of the target or in the address of the connection.
func TestNewSenderRefusesAnUnsafeAddress(t *testing.T) {
	for _, address := range []string{
		"",
		"ftp://hooks.example.com/notify",
		"file:///etc/passwd",
		"http://user:secret@hooks.example.com/notify",
		"https://hooks.example.com/notify#fragment",
		"https://hooks.example.com/" + strings.Repeat("a", 4096),
		"hooks.example.com/notify",
	} {
		if _, err := notify.NewSender(notify.WebhookConfig{ID: "default", URL: address}); err == nil {
			t.Fatalf("the unsafe address was accepted: %q", address)
		}
	}
}

// TestNewSenderAcceptsAPlainAddress proves that a plain address of HTTP is
// accepted.
func TestNewSenderAcceptsAPlainAddress(t *testing.T) {
	for _, address := range []string{
		"http://hooks.example.com/notify",
		"https://hooks.example.com/notify",
		"https://hooks.example.com:8443/services/T00/B00/secret",
	} {
		if _, err := notify.NewSender(notify.WebhookConfig{ID: "default", URL: address}); err != nil {
			t.Fatalf("the address %q was refused: %v", address, err)
		}
	}
}

// TestTheSenderSendsOnlyTheNotification proves that the document of a
// notification carries the transition and nothing else.
//
// The document must not carry the YAML of an application, its variables,
// the logs of a container or a credential of the platform.
func TestTheSenderSendsOnlyTheNotification(t *testing.T) {
	transport := &recordingTransport{}
	sender, err := notify.NewSenderWithClient(
		notify.WebhookConfig{ID: "default", URL: "https://hooks.example.com/notify"},
		&http.Client{Transport: transport},
	)
	if err != nil {
		t.Fatalf("build the sender: %v", err)
	}

	if err := sender.Send(context.Background(), testNotification()); err != nil {
		t.Fatalf("send the notification: %v", err)
	}

	requests := transport.received()
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(requests))
	}
	request := requests[0]
	if request.Method != http.MethodPost {
		t.Fatalf("method = %q, want POST", request.Method)
	}
	if contentType := request.Header.Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("content type = %q, want application/json", contentType)
	}

	var document map[string]any
	if err := json.Unmarshal([]byte(request.Body), &document); err != nil {
		t.Fatalf("decode the document %q: %v", request.Body, err)
	}
	want := map[string]any{
		"notification_id": "notification-1",
		"event_id":        "event-1",
		"transition":      float64(2),
		"environment":     "rule-1",
		"object":          "c1",
		"state":           alerts.StateActive,
		"occurred_at":     "2027-01-15T08:00:00Z",
		"summary":         "对象 c1 的 cpu_percent 持续超过阈值 80",
	}
	if len(document) != len(want) {
		t.Fatalf("the document carries %d fields, want %d: %v", len(document), len(want), document)
	}
	for field, value := range want {
		got, ok := document[field]
		if !ok {
			t.Fatalf("the document has no field %q: %v", field, document)
		}
		if got != value {
			t.Fatalf("the field %q = %v, want %v", field, got, value)
		}
	}
	for _, forbidden := range []string{"yaml", "variables", "logs", "token", "password", "credential"} {
		if _, ok := document[forbidden]; ok {
			t.Fatalf("the document carries the field %q", forbidden)
		}
	}
}

// TestTheSenderRefusesARedirect proves that a target cannot move the
// notification to another address.
//
// The target of a notification is the address the administrator
// configured, so a redirect is refused instead of followed.
func TestTheSenderRefusesARedirect(t *testing.T) {
	redirected := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected++
		http.Redirect(w, r, "http://elsewhere.example.com/notify", http.StatusFound)
	}))
	defer server.Close()

	address := strings.TrimPrefix(server.URL, "http://")
	sender, err := notify.NewSender(notify.WebhookConfig{
		ID:  "default",
		URL: server.URL + "/notify",
	})
	if err != nil {
		t.Fatalf("build the sender: %v", err)
	}
	sender = sender.UseResolver(publicResolver())
	sender = sender.UseConnect(func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	})

	err = sender.Send(context.Background(), testNotification())
	if err == nil {
		t.Fatal("the redirect was followed")
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("the error does not report the redirect: %v", err)
	}
	if redirected != 1 {
		t.Fatalf("the target answered %d times, want 1", redirected)
	}
}

// TestTheSenderIgnoresTheEnvironmentProxy proves that the environment of
// the control端 cannot divert the notifications to another host.
//
// The proxy of the environment would be asked first, so the address the
// sender connects to names the host of the target and not the proxy.
func TestTheSenderIgnoresTheEnvironmentProxy(t *testing.T) {
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer proxyServer.Close()
	t.Setenv("HTTP_PROXY", proxyServer.URL)
	t.Setenv("http_proxy", proxyServer.URL)

	connect := &recordingConnect{}
	sender, err := notify.NewSender(notify.WebhookConfig{
		ID:  "default",
		URL: "http://hooks.example.com/notify",
	})
	if err != nil {
		t.Fatalf("build the sender: %v", err)
	}
	sender = sender.UseResolver(publicResolver())
	sender = sender.UseConnect(connect.connect)

	if err := sender.Send(context.Background(), testNotification()); err == nil {
		t.Fatal("the notification reached a host that does not exist")
	}
	dialed := connect.dialed()
	if len(dialed) != 1 {
		t.Fatalf("the sender connected %d times, want 1", len(dialed))
	}
	if !strings.HasPrefix(dialed[0], "93.184.216.34:") {
		t.Fatalf("the sender connected to %q, want the address of the target", dialed[0])
	}
	if strings.Contains(dialed[0], "127.0.0.1") {
		t.Fatalf("the sender connected to the proxy: %q", dialed[0])
	}
}

// TestTheSenderStopsAfterTheTimeout proves that a target that does not
// answer cannot hold the worker forever.
//
// The client of the production path waits ten seconds at most.
func TestTheSenderStopsAfterTheTimeout(t *testing.T) {
	sender, err := notify.NewSender(notify.WebhookConfig{
		ID:  "default",
		URL: "https://hooks.example.com/notify",
	})
	if err != nil {
		t.Fatalf("build the sender: %v", err)
	}
	if sender.Timeout() != notify.RequestTimeout {
		t.Fatalf("the timeout = %s, want %s", sender.Timeout(), notify.RequestTimeout)
	}
	if notify.RequestTimeout != 10*time.Second {
		t.Fatalf("the timeout of the production path = %s, want 10s", notify.RequestTimeout)
	}

	transport := &recordingTransport{delay: 2 * time.Second}
	short, err := notify.NewSenderWithClient(
		notify.WebhookConfig{ID: "default", URL: "https://hooks.example.com/notify"},
		&http.Client{Timeout: 100 * time.Millisecond, Transport: transport},
	)
	if err != nil {
		t.Fatalf("build the sender: %v", err)
	}

	err = short.Send(context.Background(), testNotification())
	if err == nil {
		t.Fatal("the request did not stop")
	}
	var timeout interface{ Timeout() bool }
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("the error is not a timeout: %v", err)
	}
}

// TestTheSenderReadsAtMostTheBodyLimit proves that a target cannot make
// the control端 read an endless answer.
func TestTheSenderReadsAtMostTheBodyLimit(t *testing.T) {
	body := &countingBody{reader: strings.NewReader(strings.Repeat("x", 4*1024*1024))}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       body,
			Header:     make(http.Header),
			Request:    request,
		}, nil
	})}

	sender, err := notify.NewSenderWithClient(
		notify.WebhookConfig{ID: "default", URL: "https://hooks.example.com/notify"},
		client,
	)
	if err != nil {
		t.Fatalf("build the sender: %v", err)
	}
	if err := sender.Send(context.Background(), testNotification()); err != nil {
		t.Fatalf("send the notification: %v", err)
	}
	if body.read > notify.MaxResponseBody {
		t.Fatalf("the sender read %d bytes, want at most %d", body.read, notify.MaxResponseBody)
	}
	if body.read == 0 {
		t.Fatal("the sender did not read the answer at all")
	}
	if notify.MaxResponseBody != 64*1024 {
		t.Fatalf("the limit of the answer = %d, want 64 KiB", notify.MaxResponseBody)
	}
}

// roundTripFunc turns a function into a transport.
type roundTripFunc func(request *http.Request) (*http.Response, error)

// RoundTrip calls the function.
func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

// TestTheSenderReportsTheStatusOfTheReceiver proves that a target that
// refuses the notification is reported as a failure.
//
// The answer of the target decides whether the notification was
// delivered, so every status outside of 2xx is a failure.
func TestTheSenderReportsTheStatusOfTheReceiver(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusTooManyRequests, http.StatusUnauthorized} {
		transport := &recordingTransport{status: status}
		sender, err := notify.NewSenderWithClient(
			notify.WebhookConfig{ID: "default", URL: "https://hooks.example.com/notify"},
			&http.Client{Transport: transport},
		)
		if err != nil {
			t.Fatalf("build the sender: %v", err)
		}
		err = sender.Send(context.Background(), testNotification())
		if err == nil {
			t.Fatalf("the status %d was accepted", status)
		}
		if !strings.Contains(err.Error(), strconv.Itoa(status)) {
			t.Fatalf("the error does not report the status %d: %v", status, err)
		}
	}
}

// TestTheSenderDialsTheCheckedAddress proves that the connection uses the
// address that was checked.
//
// The name is resolved once and the connection goes to the answer that
// was checked, so a name that answers with another address after the
// check cannot divert the notification.
func TestTheSenderDialsTheCheckedAddress(t *testing.T) {
	resolver := &fakeResolver{answers: [][]net.IPAddr{
		{{IP: net.ParseIP("93.184.216.34")}, {IP: net.ParseIP("198.51.100.7")}},
		{{IP: net.ParseIP("127.0.0.1")}},
	}}
	connect := &recordingConnect{}
	sender, err := notify.NewSender(notify.WebhookConfig{
		ID:  "default",
		URL: "https://hooks.example.com/notify",
	})
	if err != nil {
		t.Fatalf("build the sender: %v", err)
	}
	sender = sender.UseResolver(resolver)
	sender = sender.UseConnect(connect.connect)

	if err := sender.Send(context.Background(), testNotification()); err == nil {
		t.Fatal("the request to the unreachable address succeeded")
	}
	dialed := connect.dialed()
	if len(dialed) == 0 {
		t.Fatal("the sender did not connect at all")
	}
	// The connection goes to the addresses that were checked, and the
	// address of the second answer of the name is never used.
	for _, address := range dialed {
		if address != "93.184.216.34:443" && address != "198.51.100.7:443" {
			t.Fatalf("the sender connected to %q, want a checked address", address)
		}
	}
	if lookups := resolver.lookups(); lookups != 1 {
		t.Fatalf("the name was resolved %d times, want 1", lookups)
	}
}

// TestTheSenderRefusesAnUnsafeAnswerOfTheResolver proves that every
// answer of the name is checked.
//
// A name that answers with a loopback address and with a public address
// is refused as a whole, so the target cannot choose the answer that
// happens to be allowed.
func TestTheSenderRefusesAnUnsafeAnswerOfTheResolver(t *testing.T) {
	resolver := &fakeResolver{answers: [][]net.IPAddr{
		{
			{IP: net.ParseIP("93.184.216.34")},
			{IP: net.ParseIP("127.0.0.1")},
		},
	}}
	connect := &recordingConnect{}
	sender, err := notify.NewSender(notify.WebhookConfig{
		ID:  "default",
		URL: "https://hooks.example.com/notify",
	})
	if err != nil {
		t.Fatalf("build the sender: %v", err)
	}
	sender = sender.UseResolver(resolver)
	sender = sender.UseConnect(connect.connect)

	err = sender.Send(context.Background(), testNotification())
	if err == nil {
		t.Fatal("the unsafe answer was accepted")
	}
	if !strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("the error does not name the unsafe address: %v", err)
	}
	if dialed := connect.dialed(); len(dialed) != 0 {
		t.Fatalf("the sender connected to %v, want no connection", dialed)
	}
}

// TestTheSenderRefusesTheUnsafeTarget proves that the sender of the
// production path refuses a target that resolves to a loopback address.
//
// The allowlist of the administrator cannot open the loopback, so a
// target of the local host is never reached.
func TestTheSenderRefusesTheUnsafeTarget(t *testing.T) {
	resolver := &fakeResolver{answers: [][]net.IPAddr{
		{{IP: net.ParseIP("127.0.0.1")}},
	}}
	connect := &recordingConnect{}
	sender, err := notify.NewSender(notify.WebhookConfig{
		ID:  "default",
		URL: "https://hooks.example.com/notify",
	})
	if err != nil {
		t.Fatalf("build the sender: %v", err)
	}
	sender = sender.UseResolver(resolver)
	sender = sender.UseConnect(connect.connect)

	err = sender.Send(context.Background(), testNotification())
	if err == nil {
		t.Fatal("the loopback was reached")
	}
	if dialed := connect.dialed(); len(dialed) != 0 {
		t.Fatalf("the sender connected to %v, want no connection", dialed)
	}
}

// TestTheSenderRefusesAPrivateTargetWithoutAnAllowance proves that a
// private target needs the allowance of the administrator.
func TestTheSenderRefusesAPrivateTargetWithoutAnAllowance(t *testing.T) {
	resolver := &fakeResolver{answers: [][]net.IPAddr{
		{{IP: net.ParseIP("10.1.2.3")}},
	}}
	connect := &recordingConnect{}
	sender, err := notify.NewSender(notify.WebhookConfig{
		ID:  "default",
		URL: "https://hooks.example.com/notify",
	})
	if err != nil {
		t.Fatalf("build the sender: %v", err)
	}
	sender = sender.UseResolver(resolver)
	sender = sender.UseConnect(connect.connect)

	if err := sender.Send(context.Background(), testNotification()); err == nil {
		t.Fatal("the private address was reached without an allowance")
	}
	if dialed := connect.dialed(); len(dialed) != 0 {
		t.Fatalf("the sender connected to %v, want no connection", dialed)
	}
}

// TestTheSenderReachesAnAllowedPrivateTarget proves that a private target
// is reached when the administrator allowed its network.
func TestTheSenderReachesAnAllowedPrivateTarget(t *testing.T) {
	resolver := &fakeResolver{answers: [][]net.IPAddr{
		{{IP: net.ParseIP("10.1.2.3")}},
	}}
	connect := &recordingConnect{}
	sender, err := notify.NewSender(notify.WebhookConfig{
		ID:                  "default",
		URL:                 "https://hooks.example.com/notify",
		AllowedPrivateCIDRs: []string{"10.0.0.0/8"},
	})
	if err != nil {
		t.Fatalf("build the sender: %v", err)
	}
	sender = sender.UseResolver(resolver)
	sender = sender.UseConnect(connect.connect)

	// The connection is refused by the test, but the address was allowed.
	if err := sender.Send(context.Background(), testNotification()); err == nil {
		t.Fatal("the request to the unreachable address succeeded")
	}
	dialed := connect.dialed()
	if len(dialed) != 1 || dialed[0] != "10.1.2.3:443" {
		t.Fatalf("the sender connected to %v, want the allowed address", dialed)
	}
}

// TestMaskURLHidesTheSecret proves that the display form of an address
// carries no credential.
//
// The path of an address of a service of a chat holds a token, so the
// display form keeps the host and hides everything behind it.
func TestMaskURLHidesTheSecret(t *testing.T) {
	cases := map[string]string{
		"https://hooks.example.com/services/T00/B00/secret": "https://hooks.example.com/***",
		"https://user:secret@hooks.example.com/notify":      "https://hooks.example.com/***",
		"https://hooks.example.com":                         "https://hooks.example.com",
		"http://hooks.example.com:8080/notify?token=abc":    "http://hooks.example.com:8080/***",
	}
	for address, want := range cases {
		if got := notify.MaskURL(address); got != want {
			t.Fatalf("the mask of %q is %q, want %q", address, got, want)
		}
	}
}

// TestValidateAllowedCIDRs proves that only the networks of a private
// range may be allowed.
//
// A list that covers the whole internet is not an allowance of a private
// network, so it is refused.
func TestValidateAllowedCIDRs(t *testing.T) {
	if err := notify.ValidateAllowedCIDRs([]string{"10.0.0.0/8", "192.168.1.0/24"}); err != nil {
		t.Fatalf("the private networks were refused: %v", err)
	}
	for _, values := range [][]string{
		{"0.0.0.0/0"},
		{"::/0"},
		{"not a network"},
		{"10.0.0.0/33"},
	} {
		if err := notify.ValidateAllowedCIDRs(values); err == nil {
			t.Fatalf("the networks %v were accepted", values)
		}
	}
}

// TestCheckAddressUsesTheAllowedNetworks proves that the allowed networks
// of the policy open a private address.
func TestCheckAddressUsesTheAllowedNetworks(t *testing.T) {
	allowed := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	if err := notify.CheckAddress(netip.MustParseAddr("10.1.2.3"), allowed); err != nil {
		t.Fatalf("the allowed address was refused: %v", err)
	}
}
