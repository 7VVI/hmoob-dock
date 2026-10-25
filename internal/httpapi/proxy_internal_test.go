package httpapi

import (
	"net"
	"net/http"
	"testing"
)

// TestTheTrustedProxyNetworksHoldBothAddressFamilies proves that the trust
// decision tells IPv4 and IPv6 apart: a network of one family never
// covers an address of the other one.
func TestTheTrustedProxyNetworksHoldBothAddressFamilies(t *testing.T) {
	networks, err := parseTrustedProxies([]string{"10.0.0.0/8", "2001:db8::/32"})
	if err != nil {
		t.Fatalf("parseTrustedProxies: %v", err)
	}
	trusted := trustedProxyNetworks(networks)

	for _, address := range []string{"10.1.2.3", "10.255.255.255", "2001:db8::1", "::ffff:10.1.2.3"} {
		if !trusted.contains(net.ParseIP(address)) {
			t.Fatalf("the address %s is not trusted", address)
		}
	}
	for _, address := range []string{"192.168.0.1", "11.0.0.1", "2001:db9::1", "2001:db7::1"} {
		if trusted.contains(net.ParseIP(address)) {
			t.Fatalf("the address %s is trusted", address)
		}
	}
	if trusted.contains(nil) {
		t.Fatal("an unknown address is trusted")
	}
}

// TestParseTrustedProxiesRejectsAMalformedNetwork proves that a typo in
// the configuration is reported instead of silently ignored.
func TestParseTrustedProxiesRejectsAMalformedNetwork(t *testing.T) {
	if _, err := parseTrustedProxies([]string{"10.0.0.0/8", "not-a-network"}); err == nil {
		t.Fatal("parseTrustedProxies accepted a malformed network")
	}
}

// TestTheForwardedForTakesTheFirstUntrustedAddressFromTheRight proves that
// the client is the address that the closest trusted proxy did not
// trust itself.
func TestTheForwardedForTakesTheFirstUntrustedAddressFromTheRight(t *testing.T) {
	networks, err := parseTrustedProxies([]string{"10.0.0.0/8", "2001:db8::/32"})
	if err != nil {
		t.Fatalf("parseTrustedProxies: %v", err)
	}
	trusted := trustedProxyNetworks(networks)

	cases := []struct {
		header string
		want   string
	}{
		{header: "198.51.100.7, 10.0.0.1", want: "198.51.100.7"},
		{header: "198.51.100.7, 10.0.0.1, 10.0.0.2", want: "198.51.100.7"},
		{header: "10.0.0.5, 10.0.0.1", want: "10.0.0.5"},
		{header: "2001:db8::5, 10.0.0.1", want: "2001:db8::5"},
		{header: "198.51.100.7", want: "198.51.100.7"},
		{header: "not-an-address", want: ""},
		{header: "", want: ""},
	}
	for _, testCase := range cases {
		got := trusted.forwardedFor(testCase.header)
		if got != testCase.want {
			t.Fatalf("forwardedFor(%q) = %q, want %q", testCase.header, got, testCase.want)
		}
	}
}

// TestTheForwardedForKeepsThePortOfAnAddress proves that an entry with a
// port is still understood as an address.
func TestTheForwardedForKeepsThePortOfAnAddress(t *testing.T) {
	networks, err := parseTrustedProxies([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatalf("parseTrustedProxies: %v", err)
	}
	trusted := trustedProxyNetworks(networks)

	if got := trusted.forwardedFor("198.51.100.7:41234, 10.0.0.1"); got != "198.51.100.7" {
		t.Fatalf("forwardedFor = %q, want %q", got, "198.51.100.7")
	}
}

// TestTheRequestIsHTTPSFollowsTheTrustedProxy proves that only a trusted
// proxy may tell the control端 that the request arrived over HTTPS. The
// header of any other peer is never believed.
func TestTheRequestIsHTTPSFollowsTheTrustedProxy(t *testing.T) {
	networks, err := parseTrustedProxies([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatalf("parseTrustedProxies: %v", err)
	}
	server := &Server{trustedProxies: trustedProxyNetworks(networks)}

	request := &http.Request{RemoteAddr: "10.0.0.1:41234", Header: http.Header{}}
	request.Header.Set("X-Forwarded-Proto", "https")
	if !server.requestIsHTTPS(request) {
		t.Fatal("the request of a trusted proxy is not an HTTPS request")
	}

	request = &http.Request{RemoteAddr: "192.0.2.1:41234", Header: http.Header{}}
	request.Header.Set("X-Forwarded-Proto", "https")
	if server.requestIsHTTPS(request) {
		t.Fatal("the header of an untrusted peer was believed")
	}

	request = &http.Request{RemoteAddr: "10.0.0.1:41234", Header: http.Header{}}
	if server.requestIsHTTPS(request) {
		t.Fatal("a request without a TLS state is an HTTPS request")
	}
}

// TestThePeerAddressDropsThePort proves that the key of a source holds the
// address of the peer without its port.
func TestThePeerAddressDropsThePort(t *testing.T) {
	request := &http.Request{RemoteAddr: "192.0.2.10:54321"}
	if got := peerAddress(request); got != "192.0.2.10" {
		t.Fatalf("peerAddress = %q, want %q", got, "192.0.2.10")
	}
	request = &http.Request{RemoteAddr: "[2001:db8::1]:54321"}
	if got := peerAddress(request); got != "2001:db8::1" {
		t.Fatalf("peerAddress = %q, want %q", got, "2001:db8::1")
	}
}
