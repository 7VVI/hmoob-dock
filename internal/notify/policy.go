// Package notify sends the notifications of the alerts to the targets of
// the administrator.
//
// The package is careful with the addresses it connects to. The control端
// may only reach the targets the administrator allowed, and the
// connection goes to the address that was checked, so a name that answers
// with another address cannot divert a notification. The address of a
// target can carry a secret, so it is stored encrypted and only its
// masked form is ever returned.
package notify

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// privateBlocks are the networks that need an explicit allowance of the
// administrator.
//
// The allowance has to name the network of the target: a list that
// covers the whole internet is not an allowance of a private network.
var privateBlocks = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fc00::/7"),
}

// reservedBlocks are the networks that never reach the internet.
var reservedBlocks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001:db8::/32"),
}

// metadataAddresses are the addresses of the metadata of the clouds.
//
// They are refused even when the administrator allowed their network:
// they carry the credential of the deployment of the host.
var metadataAddresses = []netip.Addr{
	netip.MustParseAddr("169.254.169.254"),
	netip.MustParseAddr("169.254.170.2"),
	netip.MustParseAddr("100.100.100.200"),
	netip.MustParseAddr("192.0.0.192"),
	netip.MustParseAddr("fd00:ec2::254"),
}

// The prefixes that carry an address of the IPv4.
var (
	// nat64Prefix is the prefix of the translation of the IPv4.
	nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")
	// sixToFourPrefix is the prefix of the tunnels of the IPv4.
	sixToFourPrefix = netip.MustParsePrefix("2002::/16")
)

// CheckAddress reports whether the control端 may connect to addr.
//
// The addresses that never reach the internet are always refused: the
// loopback, the link local addresses, the unspecified address, the
// multicast addresses, the reserved networks, the addresses of the
// metadata of the clouds and the addresses of the network cards of the
// control端 itself. A private address needs an explicit allowance of the
// administrator, and the allowance has to name the network of the target.
func CheckAddress(addr netip.Addr, privateAllowlist []netip.Prefix) error {
	return checkAddress(addr, privateAllowlist, localAddresses())
}

// checkAddress checks one address against the addresses of the host.
//
// The addresses of the host are passed in, so a name that answers with
// many addresses does not read the network cards of the host again and
// again.
func checkAddress(addr netip.Addr, privateAllowlist []netip.Prefix, own []netip.Addr) error {
	if !addr.IsValid() {
		return errors.New("notify: the address is not a valid address")
	}
	// An address of the IPv6 that carries an address of the IPv4 is
	// judged by the address it carries, otherwise the loopback would
	// sneak past its rule as ::ffff:127.0.0.1.
	addr = addr.Unmap()
	if addr.IsUnspecified() {
		return fmt.Errorf("notify: the address %s is the unspecified address", addr)
	}
	if addr.IsLoopback() {
		return fmt.Errorf("notify: the address %s is a loopback address", addr)
	}
	if addr.IsMulticast() {
		return fmt.Errorf("notify: the address %s is a multicast address", addr)
	}
	if addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() {
		return fmt.Errorf("notify: the address %s is a link local address", addr)
	}
	if isMetadata(addr) {
		return fmt.Errorf("notify: the address %s is the address of a metadata of a cloud", addr)
	}
	if isReserved(addr) {
		return fmt.Errorf("notify: the address %s belongs to a reserved network", addr)
	}
	for _, local := range own {
		if local == addr {
			return fmt.Errorf("notify: the address %s belongs to the control端 itself", addr)
		}
	}
	if isPrivate(addr) && !allowedPrivate(addr, privateAllowlist) {
		return fmt.Errorf("notify: the private address %s is not allowed by the administrator", addr)
	}
	// An address that carries an address of the IPv4 in a translation
	// prefix is judged by the address it carries, so a translation
	// cannot reach an address that is refused.
	if embedded, ok := embeddedIPv4(addr); ok {
		return checkAddress(embedded, privateAllowlist, own)
	}
	return nil
}

// isPrivate reports whether the address belongs to a private network.
func isPrivate(addr netip.Addr) bool {
	_, ok := privateBlock(addr)
	return ok
}

// privateBlock returns the private network the address belongs to.
func privateBlock(addr netip.Addr) (netip.Prefix, bool) {
	for _, block := range privateBlocks {
		if block.Contains(addr) {
			return block, true
		}
	}
	return netip.Prefix{}, false
}

// isReserved reports whether the address belongs to a reserved network.
func isReserved(addr netip.Addr) bool {
	for _, block := range reservedBlocks {
		if block.Contains(addr) {
			return true
		}
	}
	return false
}

// isMetadata reports whether the address is the address of the metadata of
// a cloud.
func isMetadata(addr netip.Addr) bool {
	for _, metadata := range metadataAddresses {
		if metadata == addr {
			return true
		}
	}
	return false
}

// allowedPrivate reports whether the administrator allowed the private
// address.
//
// The allowance has to stay inside the private network of the address, so
// a list of 0.0.0.0/0 does not open the private addresses.
func allowedPrivate(addr netip.Addr, privateAllowlist []netip.Prefix) bool {
	block, ok := privateBlock(addr)
	if !ok {
		return false
	}
	for _, prefix := range privateAllowlist {
		if !prefix.Contains(addr) {
			continue
		}
		if block.Contains(prefix.Addr()) && prefix.Bits() >= block.Bits() {
			return true
		}
	}
	return false
}

// embeddedIPv4 returns the address of the IPv4 that an address of a
// translation prefix carries.
func embeddedIPv4(addr netip.Addr) (netip.Addr, bool) {
	if addr.Is4() {
		return netip.Addr{}, false
	}
	bytes := addr.As16()
	switch {
	case nat64Prefix.Contains(addr):
		return netip.AddrFrom4([4]byte{bytes[12], bytes[13], bytes[14], bytes[15]}), true
	case sixToFourPrefix.Contains(addr):
		return netip.AddrFrom4([4]byte{bytes[2], bytes[3], bytes[4], bytes[5]}), true
	}
	return netip.Addr{}, false
}

// localAddresses returns the addresses of the network cards of the host.
//
// The control端 must not notify itself, so its own addresses are refused
// even when the administrator allowed their network.
func localAddresses() []netip.Addr {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var addresses []netip.Addr
	for _, iface := range interfaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			parsed, ok := interfaceAddress(addr)
			if !ok {
				continue
			}
			addresses = append(addresses, parsed)
		}
	}
	return addresses
}

// interfaceAddress reads the address of a network card.
func interfaceAddress(addr net.Addr) (netip.Addr, bool) {
	var raw net.IP
	switch value := addr.(type) {
	case *net.IPNet:
		raw = value.IP
	case *net.IPAddr:
		raw = value.IP
	default:
		return netip.Addr{}, false
	}
	parsed, ok := netip.AddrFromSlice(raw)
	if !ok {
		return netip.Addr{}, false
	}
	return parsed.Unmap(), true
}

// parseAllowlist reads the private networks the administrator allowed.
//
// Only the private networks are accepted, and the network has to name
// the range of its addresses: a network that reaches beyond the private
// range covers the internet as well, so it is not an allowance of a
// private network.
func parseAllowlist(values []string) ([]netip.Prefix, error) {
	var prefixes []netip.Prefix
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(trimmed)
		if err != nil {
			return nil, fmt.Errorf("notify: the network %q is malformed: %w", value, err)
		}
		prefix = prefix.Masked()
		block, ok := privateBlock(prefix.Addr())
		if !ok || prefix.Bits() < block.Bits() {
			return nil, fmt.Errorf("notify: the network %q is not a private network", value)
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes, nil
}

// ValidateAllowedCIDRs checks the private networks the administrator
// allows.
//
// The list is checked before it is stored, so a network that would open
// the internet is reported instead of saved.
func ValidateAllowedCIDRs(values []string) error {
	_, err := parseAllowlist(values)
	return err
}
