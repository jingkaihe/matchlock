//go:build linux

package qemu

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jingkaihe/matchlock/pkg/vm"
)

// ipv6WiredConfig is a QEMU VMConfig as the sandbox hands it to the backend for
// an intercepted guest: the IPv4 link plus the per-VM IPv6 unique-local /64 the
// proxy, the DNS forwarder and the ip6 nftables table are all provisioned from.
func ipv6WiredConfig() *vm.VMConfig {
	return &vm.VMConfig{
		ID:          "vm-qemu-ipv6",
		Hostname:    "vm-qemu-ipv6",
		CPUs:        1,
		MemoryMB:    512,
		KernelPath:  "/kernel",
		RootfsPath:  "/rootfs",
		GatewayIP:   "192.168.100.1",
		GuestIP:     "192.168.100.2",
		SubnetCIDR:  "192.168.100.1/24",
		GatewayIPv6: "fd00:100::1",
		GuestIPv6:   "fd00:100::2",
		Subnet6CIDR: "fd00:100::1/64",
		DNSServers:  []string{"8.8.8.8"},
	}
}

// The QEMU TAP must carry the IPv6 gateway the sandbox already bound the
// interception proxy and the DNS forwarder to. Without it every intercepted
// sandbox fails to launch under MATCHLOCK_BACKEND=qemu with EADDRNOTAVAIL.
func TestIPv6TapLinkDerivesGatewayAndBootArg(t *testing.T) {
	tapCIDR, kernelArg, hasLink, err := ipv6TapLink(ipv6WiredConfig())
	require.NoError(t, err)
	require.True(t, hasLink)
	assert.Equal(t, "fd00:100::1/64", tapCIDR)
	assert.Equal(t, " matchlock.ipv6=fd00:100::2/64,fd00:100::1", kernelArg)
}

// A subnet reported in network form must still put the GATEWAY on the TAP: the
// guest routes through the gateway address, and the proxy binds it.
func TestIPv6TapLinkAlwaysCarriesTheGateway(t *testing.T) {
	cfg := ipv6WiredConfig()
	cfg.Subnet6CIDR = "fd00:100::/64"

	tapCIDR, kernelArg, hasLink, err := ipv6TapLink(cfg)
	require.NoError(t, err)
	require.True(t, hasLink)
	assert.Equal(t, "fd00:100::1/64", tapCIDR)
	assert.Equal(t, " matchlock.ipv6=fd00:100::2/64,fd00:100::1", kernelArg)
}

// An IPv4-only QEMU config (NAT sandbox, or an explicitly IPv4-only one) must
// leave the TAP configuration and the boot line exactly as they were.
func TestIPv6TapLinkAbsentForIPv4OnlyConfig(t *testing.T) {
	cfg := ipv6WiredConfig()
	cfg.GatewayIPv6, cfg.GuestIPv6, cfg.Subnet6CIDR = "", "", ""

	tapCIDR, kernelArg, hasLink, err := ipv6TapLink(cfg)
	require.NoError(t, err)
	assert.False(t, hasLink)
	assert.Empty(t, tapCIDR)
	assert.Empty(t, kernelArg)
	require.NotContains(t, (&Machine{config: cfg}).networkBootArgs(), "matchlock.ipv6")
}

func TestIPv6TapLinkNilConfig(t *testing.T) {
	tapCIDR, kernelArg, hasLink, err := ipv6TapLink(nil)
	require.NoError(t, err)
	assert.False(t, hasLink)
	assert.Empty(t, tapCIDR)
	assert.Empty(t, kernelArg)
}

// A half-set triple routes no IPv6 link (nothing is installed and no boot arg is
// emitted), matching the shared derivation's "no IPv6 fields" behaviour.
func TestIPv6TapLinkPartialTripleConfiguresNothing(t *testing.T) {
	partials := map[string]func(*vm.VMConfig){
		"gateway only":      func(c *vm.VMConfig) { c.GuestIPv6, c.Subnet6CIDR = "", "" },
		"guest only":        func(c *vm.VMConfig) { c.GatewayIPv6, c.Subnet6CIDR = "", "" },
		"subnet only":       func(c *vm.VMConfig) { c.GatewayIPv6, c.GuestIPv6 = "", "" },
		"gateway and guest": func(c *vm.VMConfig) { c.Subnet6CIDR = "" },
	}
	for name, mutate := range partials {
		t.Run(name, func(t *testing.T) {
			cfg := ipv6WiredConfig()
			mutate(cfg)
			tapCIDR, kernelArg, hasLink, err := ipv6TapLink(cfg)
			require.NoError(t, err)
			assert.False(t, hasLink)
			assert.Empty(t, tapCIDR)
			assert.Empty(t, kernelArg)
			assert.NotContains(t, (&Machine{config: cfg}).networkBootArgs(), "matchlock.ipv6")
		})
	}
}

// A fully-set but malformed link is an error, not a silently missing IPv6 path:
// setupNetwork turns it into ErrTAPConfigureIPv6 and Create fails, instead of
// launching a TAP the already-provisioned proxy cannot bind.
func TestIPv6TapLinkRejectsMalformedLink(t *testing.T) {
	cases := map[string]func(*vm.VMConfig){
		"IPv4 guest address": func(c *vm.VMConfig) { c.GuestIPv6 = "192.168.100.2" },
		"IPv4 gateway":       func(c *vm.VMConfig) { c.GatewayIPv6 = "192.168.100.1" },
		"garbage guest":      func(c *vm.VMConfig) { c.GuestIPv6 = "not-an-ip" },
		"IPv4 subnet":        func(c *vm.VMConfig) { c.Subnet6CIDR = "192.168.100.1/24" },
		"unparsable subnet":  func(c *vm.VMConfig) { c.Subnet6CIDR = "not-a-cidr" },
		"prefix out of range": func(c *vm.VMConfig) {
			c.Subnet6CIDR = "fd00:100::1/129"
		},
		"missing prefix length": func(c *vm.VMConfig) { c.Subnet6CIDR = "fd00:100::1" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := ipv6WiredConfig()
			mutate(cfg)
			_, _, _, err := ipv6TapLink(cfg)
			require.Error(t, err)
		})
	}
}

// The guest side of the link reaches the guest only through the QEMU kernel
// command line: guest-init parses matchlock.ipv6=<guest>/<prefixlen>,<gateway>
// and installs the address plus its ::/0 route (the kernel ip= argument is
// IPv4-only). This pins the field into bootArgs, not just into the helper.
func TestQEMUBootArgsCarryIPv6GuestLink(t *testing.T) {
	m := &Machine{config: ipv6WiredConfig()}
	args := m.bootArgs()
	assert.Contains(t, args, " ip=192.168.100.2::192.168.100.1:255.255.255.0::eth0:off:8.8.8.8")
	assert.Contains(t, args, " matchlock.ipv6=fd00:100::2/64,fd00:100::1")
}

// An IPv4-only boot line stays byte-identical to what it was before the IPv6
// path existed.
func TestQEMUBootArgsWithoutIPv6LinkAreUnchanged(t *testing.T) {
	m := &Machine{config: &vm.VMConfig{
		ID: "vm-qemu-ipv4", Hostname: "vm-qemu-ipv4", CPUs: 1, MTU: 1400,
		GatewayIP: "192.168.50.1", GuestIP: "192.168.50.2",
		DNSServers: []string{"1.1.1.1"},
	}}
	assert.Contains(t, m.bootArgs(),
		" ip=192.168.50.2::192.168.50.1:255.255.255.0::eth0:off:1.1.1.1 matchlock.mtu=1400")
	assert.NotContains(t, m.bootArgs(), "matchlock.ipv6")
}
