package notify_test

import (
	"net"
	"net/netip"
	"testing"

	"hmoob-dock/internal/notify"
)

// TestWebhookNeverConnectsToLoopbackOrMetadata proves that the allowlist of
// the administrator cannot open the addresses that are never allowed.
//
// The allowlist of the test covers every address of the internet, so a
// rule that only looked at the list would accept the loopback and the
// address of the metadata of the cloud.
func TestWebhookNeverConnectsToLoopbackOrMetadata(t *testing.T) {
	allowed := []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}
	for _, address := range []string{"127.0.0.1", "169.254.169.254", "::1", "::ffff:127.0.0.1"} {
		if err := notify.CheckAddress(netip.MustParseAddr(address), allowed); err == nil {
			t.Fatalf("unsafe address accepted: %s", address)
		}
	}
}

// TestCheckAddressRefusesTheUnsafeRanges proves that every range that
// never reaches the internet is refused, no matter what the administrator
// allowed.
func TestCheckAddressRefusesTheUnsafeRanges(t *testing.T) {
	allowed := []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/0"),
		netip.MustParsePrefix("::/0"),
	}
	for _, address := range []string{
		"0.0.0.0",
		"::",
		"127.0.0.1",
		"127.10.20.30",
		"::1",
		"169.254.1.1",
		"fe80::1",
		"224.0.0.1",
		"ff02::1",
		"239.255.255.250",
		"255.255.255.255",
		"169.254.169.254",
		"169.254.170.2",
		"100.100.100.200",
		"fd00:ec2::254",
	} {
		if err := notify.CheckAddress(netip.MustParseAddr(address), allowed); err == nil {
			t.Fatalf("unsafe address accepted: %s", address)
		}
	}
}

// TestCheckAddressAllowsAPublicAddress proves that an address of the
// internet is allowed without an allowance of the administrator.
func TestCheckAddressAllowsAPublicAddress(t *testing.T) {
	for _, address := range []string{"93.184.216.34", "2606:2800:220:1:248:1893:25c8:1946"} {
		if err := notify.CheckAddress(netip.MustParseAddr(address), nil); err != nil {
			t.Fatalf("the public address %s was refused: %v", address, err)
		}
	}
}

// TestCheckAddressRefusesAPrivateAddress proves that a private address
// needs an explicit allowance of the administrator.
func TestCheckAddressRefusesAPrivateAddress(t *testing.T) {
	for _, address := range []string{"10.0.0.5", "172.16.0.1", "192.168.1.1", "100.64.0.1", "fc00::1", "fd12:3456::1"} {
		if err := notify.CheckAddress(netip.MustParseAddr(address), nil); err == nil {
			t.Fatalf("the private address %s was accepted without an allowance", address)
		}
	}
}

// TestCheckAddressAllowsTheAllowedPrivateNetwork proves that a private
// address is allowed when the administrator allowed its network.
func TestCheckAddressAllowsTheAllowedPrivateNetwork(t *testing.T) {
	allowed := []netip.Prefix{
		netip.MustParsePrefix("10.1.2.0/24"),
		netip.MustParsePrefix("192.168.0.0/16"),
	}
	for _, address := range []string{"10.1.2.3", "192.168.7.8"} {
		if err := notify.CheckAddress(netip.MustParseAddr(address), allowed); err != nil {
			t.Fatalf("the allowed address %s was refused: %v", address, err)
		}
	}
}

// TestCheckAddressRefusesABroadAllowance proves that a list that covers
// the whole internet is not an allowance of a private network.
//
// The administrator has to name the network of the target exactly, so a
// list of 0.0.0.0/0 does not open the private addresses.
func TestCheckAddressRefusesABroadAllowance(t *testing.T) {
	allowed := []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}
	if err := notify.CheckAddress(netip.MustParseAddr("10.1.2.3"), allowed); err == nil {
		t.Fatal("the private address was accepted by a list that covers the whole internet")
	}
}

// TestCheckAddressUnmapsTheMappedAddresses proves that an address of the
// IPv6 that carries an IPv4 address is judged by the address it carries.
//
// Otherwise 127.0.0.1 would sneak past the rule of the loopback as
// ::ffff:127.0.0.1.
func TestCheckAddressUnmapsTheMappedAddresses(t *testing.T) {
	if err := notify.CheckAddress(netip.MustParseAddr("::ffff:127.0.0.1"), nil); err == nil {
		t.Fatal("the mapped loopback was accepted")
	}
	allowed := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	if err := notify.CheckAddress(netip.MustParseAddr("::ffff:10.1.2.3"), allowed); err != nil {
		t.Fatalf("the mapped private address was refused: %v", err)
	}
}

// TestCheckAddressRefusesTheAddressOfTheControl proves that the control端
// does not notify itself.
//
// The address of a network card of the host belongs to the deployment of
// the control端, so it is refused even when the administrator allowed its
// network.
func TestCheckAddressRefusesTheAddressOfTheControl(t *testing.T) {
	own := ownAddresses(t)
	if len(own) == 0 {
		t.Skip("the host has no address of its own")
	}
	allowed := []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}
	for _, address := range own {
		if err := notify.CheckAddress(address, allowed); err == nil {
			t.Fatalf("the address of the control %s was accepted", address)
		}
	}
}

// TestCheckAddressRefusesAnInvalidAddress proves that an address that is
// no address at all is refused.
func TestCheckAddressRefusesAnInvalidAddress(t *testing.T) {
	if err := notify.CheckAddress(netip.Addr{}, nil); err == nil {
		t.Fatal("the invalid address was accepted")
	}
}

// ownAddresses returns the addresses of the network cards of the host.
func ownAddresses(t *testing.T) []netip.Addr {
	t.Helper()

	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("list the interfaces: %v", err)
	}
	var addresses []netip.Addr
	for _, iface := range interfaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err != nil {
				continue
			}
			parsed, ok := netip.AddrFromSlice(ip)
			if !ok {
				continue
			}
			parsed = parsed.Unmap()
			// The loopback and the unspecified address are refused by
			// their own rule, so they prove nothing here.
			if parsed.IsLoopback() || parsed.IsUnspecified() {
				continue
			}
			addresses = append(addresses, parsed)
		}
	}
	return addresses
}
