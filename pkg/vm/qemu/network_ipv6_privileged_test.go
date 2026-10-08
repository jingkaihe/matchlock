//go:build linux

package qemu

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// requirePrivileged skips unless this test can create a TAP device. It is the
// same gate the linux-backend TAP tests use (a skip is not a pass, so the
// privileged battery must be run explicitly):
//
//	CGO_ENABLED=0 go test -c -o /tmp/vmqemu.test ./pkg/vm/qemu/
//	docker run --rm --privileged -v /tmp/vmqemu.test:/vmqemu.test:ro \
//	  -e MATCHLOCK_PRIVILEGED_TESTS=1 alpine:latest \
//	  /vmqemu.test -test.run TestSetupNetworkInstallsIPv6Gateway -test.v
func requirePrivileged(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires root (CAP_NET_ADMIN) to create TAP devices")
	}
	if os.Getenv("MATCHLOCK_PRIVILEGED_TESTS") != "1" {
		t.Skip("set MATCHLOCK_PRIVILEGED_TESTS=1 to run privileged kernel TAP tests")
	}
}

// TestSetupNetworkInstallsIPv6Gateway is the real-kernel regression lock for the
// QEMU-backend launch failure: an intercepted sandbox binds its HTTP/HTTPS proxy
// and its DNS forwarder to the TAP's IPv6 gateway BEFORE the VM exists, so a TAP
// that never received that address makes every intercepted QEMU sandbox fail at
// creation with
//
//	create transparent proxy: listen failed on HTTP port [fd00:100::1]:PORT:
//	bind: cannot assign requested address
//
// and leaves the fail-closed ip6 table in front of a guest with no IPv6. The
// address must also be usable IMMEDIATELY (IFA_F_NODAD): bind(2) rejects a
// tentative address, and DAD never completes on a carrier-less TAP.
func TestSetupNetworkInstallsIPv6Gateway(t *testing.T) {
	requirePrivileged(t)

	cfg := ipv6WiredConfig()
	cfg.ID = "vm-qemu6tap"
	m := &Machine{id: cfg.ID, config: cfg}

	net6, err := m.setupNetwork()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, net6.teardownNetwork()) })

	// The TAP is non-persistent: it lives as long as this fd does, so it must be
	// observable (and gone) inside this test.
	require.Equal(t, "qm-qemu6tap", net6.tapName)

	tapIface, err := net.InterfaceByName(net6.tapName)
	require.NoError(t, err, "the TAP must exist after setupNetwork")

	var gotGateway, gotLinkLocal bool
	addrs, err := tapIface.Addrs()
	require.NoError(t, err)
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if ipnet.IP.String() == cfg.GatewayIPv6 {
			gotGateway = true
		}
		if ipnet.IP.IsLinkLocalUnicast() {
			gotLinkLocal = true
		}
	}
	require.True(t, gotGateway, "TAP %s must carry the IPv6 gateway %s (addresses: %v)",
		net6.tapName, cfg.GatewayIPv6, addrs)
	t.Logf("TAP %s addresses after setupNetwork: %v (link-local present: %v)", net6.tapName, addrs, gotLinkLocal)

	// The prefix the KERNEL holds for the address is ground truth here (Go's
	// interface table reports its own mask size): /proc/net/if_inet6 lists
	// <address> <ifindex> <prefixlen> <scope> <flags> <name>, and the prefix is
	// what creates the fd00:…::/64 connected route the guest's replies come back
	// on.
	prefixLen, err := ifInet6PrefixLen(net6.tapName, cfg.GatewayIPv6)
	require.NoError(t, err, "the gateway must appear in /proc/net/if_inet6")
	require.Equal(t, 64, prefixLen, "the gateway must be installed with the allocated /64 prefix")

	// The exact operation the proxy performs at sandbox creation.
	ln, err := net.Listen("tcp", net.JoinHostPort(cfg.GatewayIPv6, "0"))
	require.NoError(t, err, "the TAP gateway must be bindable immediately (IFA_F_NODAD)")
	require.NoError(t, ln.Close())
}

// ifInet6PrefixLen reads the prefix length the kernel holds for addr on iface
// from /proc/net/if_inet6 (the same source `ip -6 addr` reports). The prefix
// column of that file is HEX (<address> <ifindex> <prefixlen-hex> <scope-hex>
// <flags-hex> <name>), so 0x40 is a /64.
func ifInet6PrefixLen(iface, addr string) (int, error) {
	want := net.ParseIP(addr)
	if want == nil {
		return 0, fmt.Errorf("%q is not an IP address", addr)
	}
	raw, err := os.ReadFile("/proc/net/if_inet6")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 || fields[5] != iface {
			continue
		}
		hexAddr := fields[0]
		ip := make(net.IP, net.IPv6len)
		for i := 0; i < net.IPv6len; i++ {
			b, err := strconv.ParseUint(hexAddr[2*i:2*i+2], 16, 8)
			if err != nil {
				return 0, err
			}
			ip[i] = byte(b)
		}
		if ip.Equal(want) {
			n, err := strconv.ParseUint(fields[2], 16, 8)
			if err != nil {
				return 0, err
			}
			return int(n), nil
		}
	}
	return 0, fmt.Errorf("%s not found on %s in /proc/net/if_inet6", addr, iface)
}

// TestSetupNetworkLeavesIPv4OnlyConfigAlone pins the other half of the contract:
// a QEMU config without the IPv6 fields (an IPv4-only or --no-network hand-off)
// gets no IPv6 address on its TAP, so the IPv6 path cannot leak into runs that
// never provisioned the proxy/ip6 side.
func TestSetupNetworkLeavesIPv4OnlyConfigAlone(t *testing.T) {
	requirePrivileged(t)

	cfg := ipv6WiredConfig()
	cfg.ID = "vm-qemu4tap"
	cfg.GatewayIPv6, cfg.GuestIPv6, cfg.Subnet6CIDR = "", "", ""
	m := &Machine{id: cfg.ID, config: cfg}

	net6, err := m.setupNetwork()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, net6.teardownNetwork()) })

	tapIface, err := net.InterfaceByName(net6.tapName)
	require.NoError(t, err)
	addrs, err := tapIface.Addrs()
	require.NoError(t, err)
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if ip6 := ipnet.IP.To4(); ip6 == nil && !ipnet.IP.IsLinkLocalUnicast() {
			t.Fatalf("IPv4-only config must install no global IPv6 address on the TAP, got %s (all: %v)", ipnet.IP, addrs)
		}
	}

	require.Contains(t, m.networkBootArgs(), "ip=192.168.100.2::192.168.100.1")
	require.NotContains(t, m.networkBootArgs(), "matchlock.ipv6")
}
