package httpapi

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// trustedProxyNetworks holds the networks of the reverse proxies whose
// forwarding headers the control端 believes.
//
// The list is empty by default. A peer outside of these networks is
// never believed, so a forwarding header of such a peer cannot forge
// the address of a client.
type trustedProxyNetworks []*net.IPNet

// parseTrustedProxies turns the configured networks into their parsed
// form. A malformed network is reported as an error, so a typo in the
// configuration cannot silently change the trust.
func parseTrustedProxies(cidrs []string) ([]*net.IPNet, error) {
	networks := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		trimmed := strings.TrimSpace(cidr)
		if trimmed == "" {
			continue
		}
		_, network, err := net.ParseCIDR(trimmed)
		if err == nil {
			networks = append(networks, network)
			continue
		}
		// A single address without a mask names one host.
		address := net.ParseIP(trimmed)
		if address == nil {
			return nil, fmt.Errorf("httpapi: the trusted proxy network %q is malformed", trimmed)
		}
		bits := 8 * net.IPv6len
		if address.To4() != nil {
			address = address.To4()
			bits = 8 * net.IPv4len
		}
		networks = append(networks, &net.IPNet{IP: address, Mask: net.CIDRMask(bits, bits)})
	}
	return networks, nil
}

// contains reports whether the address belongs to one of the networks.
// An address of one family never belongs to a network of the other
// family.
func (n trustedProxyNetworks) contains(address net.IP) bool {
	if address == nil {
		return false
	}
	for _, network := range n {
		if network.Contains(address) {
			return true
		}
	}
	return false
}

// forwardedFor returns the address of the client that a trusted proxy
// reports in the given forwarding header.
//
// The header holds the addresses in the order they were seen, so the
// rightmost entry was added by the closest proxy. The first address
// from the right that no trusted network covers is the client. When
// every address is trusted, the leftmost one is used.
//
// A header without a usable address yields an empty string, so the
// caller falls back to the address of the peer.
func (n trustedProxyNetworks) forwardedFor(header string) string {
	entries := strings.Split(header, ",")
	addresses := make([]net.IP, 0, len(entries))
	for _, entry := range entries {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			continue
		}
		address := net.ParseIP(trimmed)
		if address == nil {
			// Some proxies write the address with its port.
			if host, _, err := net.SplitHostPort(trimmed); err == nil {
				address = net.ParseIP(host)
			}
		}
		if address == nil {
			continue
		}
		addresses = append(addresses, address)
	}
	if len(addresses) == 0 {
		return ""
	}
	for index := len(addresses) - 1; index >= 0; index-- {
		if !n.contains(addresses[index]) {
			return addresses[index].String()
		}
	}
	return addresses[0].String()
}

// peerAddress returns the address of the peer of the connection without
// its port.
func peerAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}

// peerIP returns the address of the peer of the connection. An address
// that is no IP at all is reported as nil.
func peerIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	return net.ParseIP(host)
}

// clientKey returns the key that counts the attempts of a source.
//
// A request that arrives from a trusted proxy is attributed to the
// client that the proxy reports, so the clients behind one proxy do
// not share a single budget. A request from any other peer is
// attributed to the address of the peer, because a forwarding header
// of an untrusted peer can be forged by anybody.
func (s *Server) clientKey(r *http.Request) string {
	peer := peerAddress(r)
	if !s.trustedProxies.contains(peerIP(r)) {
		return peer
	}
	if client := s.trustedProxies.forwardedFor(r.Header.Get("X-Forwarded-For")); client != "" {
		return client
	}
	return peer
}

// requestIsHTTPS reports whether the request arrived in an HTTPS
// context. A trusted proxy may terminate the TLS, so the forwarding
// header of that proxy decides the answer. The header of any other
// peer is never believed.
func (s *Server) requestIsHTTPS(r *http.Request) bool {
	if s.trustedProxies.contains(peerIP(r)) {
		proto := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")))
		if proto != "" {
			return proto == "https"
		}
	}
	return r.TLS != nil
}
