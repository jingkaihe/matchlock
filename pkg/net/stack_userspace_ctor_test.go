package net

import (
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/policy"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
)

// This file has no build tag: the userspace stack is pure Go (gVisor + os.File),
// so the dual-stack bring-up its constructor performs is asserted here on every
// host, including the Linux unit suite. Production wires the stack to a VZ
// file-handle socketpair; these tests use the same syscall.Socketpair shape.

// ctorSocketPair returns the two ends of a datagram socketpair matching the one
// the darwin backend hands the stack (host end = the file the stack reads).
func ctorSocketPair(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	require.NoError(t, err)
	host := os.NewFile(uintptr(fds[0]), "host-net")
	guest := os.NewFile(uintptr(fds[1]), "guest-net")
	t.Cleanup(func() { _ = guest.Close() })
	require.NotNil(t, host)
	return host, guest
}

func ctorConfig(t *testing.T) *Config {
	t.Helper()
	host, _ := ctorSocketPair(t)
	return &Config{
		File:       host,
		GatewayIP:  "192.168.100.1",
		GuestIP:    "192.168.100.2",
		MTU:        1500,
		Policy:     policy.NewEngine(&api.NetworkConfig{}),
		Events:     make(chan api.Event, 10),
		DNSServers: []string{"192.0.2.53"},
	}
}

func hasRoute(routes []tcpip.Route, subnet tcpip.Subnet) bool {
	for _, r := range routes {
		if r.NIC == 1 && r.Destination.Equal(subnet) {
			return true
		}
	}
	return false
}

// TestNewNetworkStackIPv4Only pins the unchanged shape: a config without IPv6
// fields brings up an IPv4-only NIC (gateway address assigned, exactly one
// route entry) and never answers for the IPv6 gateway.
func TestNewNetworkStackIPv4Only(t *testing.T) {
	cfg := ctorConfig(t)
	ns, err := NewNetworkStack(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ns.Close()) })

	addr, tcpipErr := ns.Stack().GetMainNICAddress(1, header.IPv4ProtocolNumber)
	require.Nil(t, tcpipErr)
	assert.Equal(t, "192.168.100.1", addr.Address.String())

	routes := ns.Stack().GetRouteTable()
	require.Len(t, routes, 1, "an IPv4-only stack installs exactly the IPv4 route")
	assert.True(t, hasRoute(routes, header.IPv4EmptySubnet))
	assert.False(t, hasRoute(routes, header.IPv6EmptySubnet))
	// NOTE: Stack.CheckLocalAddress cannot discriminate here — it returns true
	// for ANY address while the NIC is in spoofing mode (gVisor nic.go), and
	// the stack enables spoofing deliberately. The IPv6 bring-up evidence is
	// the route table plus GetMainNICAddress below; the guest-side proof
	// (address/route install, NDP) is the mac runtime validation.
}

// TestNewNetworkStackDualStackBringUp is the core M1 assertion: when the config
// carries the full per-VM ULA triple, the constructor assigns the SAME gateway
// the guest installs as its default route to NIC 1 (with the lease prefix
// length) and installs the ::/0 route next to the IPv4 one, so guest IPv6
// packets terminate in the stack instead of being dropped for lack of an
// address.
func TestNewNetworkStackDualStackBringUp(t *testing.T) {
	cfg := ctorConfig(t)
	cfg.GatewayIPv6 = "fd00:100::1"
	cfg.GuestIPv6 = "fd00:100::2"
	cfg.Subnet6CIDR = "fd00:100::/64"

	ns, err := NewNetworkStack(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ns.Close()) })

	addr, tcpipErr := ns.Stack().GetMainNICAddress(1, ipv6.ProtocolNumber)
	require.Nil(t, tcpipErr)
	assert.Equal(t, "fd00:100::1", addr.Address.String())
	assert.Equal(t, 64, addr.PrefixLen, "the NIC address carries the lease prefix length")

	routes := ns.Stack().GetRouteTable()
	assert.True(t, hasRoute(routes, header.IPv4EmptySubnet), "the IPv4 route must survive")
	assert.True(t, hasRoute(routes, header.IPv6EmptySubnet), "the IPv6 default route must be installed")

	// A non-64 prefix from the lease is honored on the NIC.
	cfg2 := ctorConfig(t)
	cfg2.GatewayIPv6 = "fd00:137::1"
	cfg2.GuestIPv6 = "fd00:137::2"
	cfg2.Subnet6CIDR = "fd00:137::1/48"
	ns2, err := NewNetworkStack(cfg2)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ns2.Close()) })
	addr2, tcpipErr2 := ns2.Stack().GetMainNICAddress(1, ipv6.ProtocolNumber)
	require.Nil(t, tcpipErr2)
	assert.Equal(t, 48, addr2.PrefixLen)
}

// TestNewNetworkStackFailClosed pins every constructor rejection:
//   - a missing policy engine (the stack exists to enforce policy);
//   - a partially-set IPv6 triple (must be set together; never half a link);
//   - a malformed triple (loud, before any host resource exists);
//   - an unsupported dns_priority (never a silent verbatim degradation).
func TestNewNetworkStackFailClosed(t *testing.T) {
	t.Run("missing policy engine", func(t *testing.T) {
		cfg := ctorConfig(t)
		cfg.Policy = nil
		_, err := NewNetworkStack(cfg)
		require.Error(t, err)
		require.ErrorIs(t, err, ErrStackNoPolicy)
	})

	t.Run("partial ipv6 triple", func(t *testing.T) {
		cfg := ctorConfig(t)
		cfg.GatewayIPv6 = "fd00:100::1" // guest + prefix missing
		_, err := NewNetworkStack(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be set together")
	})

	t.Run("malformed ipv6 gateway", func(t *testing.T) {
		cfg := ctorConfig(t)
		cfg.GatewayIPv6 = "192.168.100.1"
		cfg.GuestIPv6 = "fd00:100::2"
		cfg.Subnet6CIDR = "fd00:100::/64"
		_, err := NewNetworkStack(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid ipv6 link")
	})

	t.Run("unsupported dns_priority", func(t *testing.T) {
		cfg := ctorConfig(t)
		cfg.DNSPriority = "IPv6" // exact lower-case wire values only
		_, err := NewNetworkStack(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "IPv6")
	})

	t.Run("ipv4 default with allow_private matcher wired", func(t *testing.T) {
		// The production ipv4 default needs the policy engine's
		// allow_private matcher; with a real engine present the constructor
		// must succeed (this is the default path every intercepted darwin
		// sandbox takes).
		cfg := ctorConfig(t)
		cfg.Policy = policy.NewEngine(&api.NetworkConfig{AllowPrivate: []string{"ai.iasylum.net"}})
		cfg.DNSPriority = "ipv4"
		ns, err := NewNetworkStack(cfg)
		require.NoError(t, err)
		require.NoError(t, ns.Close())
	})
}
