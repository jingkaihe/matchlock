package darwin

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/jingkaihe/matchlock/pkg/vm"
)

// These tests carry no build tag: buildKernelArgs (and the shared IPv6
// derivation it consumes) is pure, so the darwin kernel-command-line
// regressions run in the ordinary unit suite on every platform, including the
// Linux CI host.

// darwinInterceptionConfig builds the shape the sandbox hands the backend for
// an intercepted sandbox with a wired IPv6 link.
func darwinInterceptionConfig(id string) *vm.VMConfig {
	return &vm.VMConfig{
		ID:              id,
		Hostname:        id,
		CPUs:            1,
		MemoryMB:        512,
		GatewayIP:       "192.168.100.1",
		GuestIP:         "192.168.100.2",
		SubnetCIDR:      "192.168.100.1/24",
		GatewayIPv6:     "fd00:100::1",
		GuestIPv6:       "fd00:100::2",
		Subnet6CIDR:     "fd00:100::/64",
		UseInterception: true,
		DNSServers:      []string{"8.8.8.8", "8.8.4.4"},
	}
}

// TestDarwinArgsIPv6InterceptionOnly pins the mode discipline of the IPv6
// field: it appears on the interception branch exactly when the config carries
// a complete IPv6 link, and never on the plain-NAT (dhcp) or --no-network
// branches even when the fields are set, so those modes stay byte-identical to
// the pre-IPv6 behavior.
func TestDarwinArgsIPv6InterceptionOnly(t *testing.T) {
	t.Run("interception with a link emits the field next to the ip= group", func(t *testing.T) {
		args := NewDarwinBackend().buildKernelArgs(darwinInterceptionConfig("vm-v6"))
		assert.Contains(t, args, " ip=192.168.100.2::192.168.100.1:255.255.255.0::eth0:off:8.8.8.8:8.8.4.4")
		assert.Contains(t, args, " matchlock.ipv6=fd00:100::2/64,fd00:100::1")
		// Placement: the IPv6 field rides directly after matchlock.mtu=, the
		// same position the Linux backend uses, so the two backends' generated
		// lines stay diffable.
		assert.Regexp(t, `matchlock\.mtu=1500 matchlock\.ipv6=fd00:100::2/64,fd00:100::1 matchlock\.cpus=1$`, args)
	})

	t.Run("interception without a link is byte-identical to the pre-IPv6 line", func(t *testing.T) {
		cfg := darwinInterceptionConfig("vm-v4")
		cfg.GatewayIPv6, cfg.GuestIPv6, cfg.Subnet6CIDR = "", "", ""
		const golden = "console=hvc0 root=/dev/vda rw init=/init reboot=k panic=1 " +
			"ip=192.168.100.2::192.168.100.1:255.255.255.0::eth0:off:8.8.8.8:8.8.4.4 hostname=vm-v4 " +
			"matchlock.dns=8.8.8.8,8.8.4.4 matchlock.mtu=1500 matchlock.cpus=1"
		assert.Equal(t, golden, NewDarwinBackend().buildKernelArgs(cfg))
	})

	t.Run("dhcp (plain NAT) never emits the IPv6 field", func(t *testing.T) {
		cfg := darwinInterceptionConfig("vm-nat")
		cfg.UseInterception = false
		found := false
		for _, f := range strings.Fields(NewDarwinBackend().buildKernelArgs(cfg)) {
			if strings.HasPrefix(f, "matchlock.ipv6=") {
				found = true
			}
		}
		assert.False(t, found, "plain NAT must keep its exact pre-IPv6 command line")
	})

	t.Run("--no-network never emits the IPv6 field", func(t *testing.T) {
		cfg := darwinInterceptionConfig("vm-nonet")
		cfg.NoNetwork = true
		args := NewDarwinBackend().buildKernelArgs(cfg)
		assert.NotContains(t, args, "matchlock.ipv6=")
		fields := strings.Fields(args)
		assert.Contains(t, fields, "ip=off", "no-network must keep booting with ip=off")
		assert.Contains(t, fields, "matchlock.no_network=1")
	})

	t.Run("a partial IPv6 triple emits no field", func(t *testing.T) {
		cfg := darwinInterceptionConfig("vm-partial")
		cfg.Subnet6CIDR = ""
		assert.NotContains(t, NewDarwinBackend().buildKernelArgs(cfg), "matchlock.ipv6=")
	})
}

// TestDarwinArgsIPv6AndExactMountsCoexist locks the regression the IPv6 work
// must not re-break: the exact-destinations argument (bead tamandua-03i,
// commit 2afbca3) keeps being emitted on the interception branch when the IPv6
// field is also present, and the workspace argument coexists with both.
func TestDarwinArgsIPv6AndExactMountsCoexist(t *testing.T) {
	cfg := darwinInterceptionConfig("vm-v6-exact")
	cfg.Workspace = "/workspace"
	cfg.ExactMounts = []string{"/home/me/work", "/home/me/.config"}
	args := NewDarwinBackend().buildKernelArgs(cfg)
	assert.Contains(t, args, " matchlock.workspace=/workspace")
	assert.Contains(t, args, " matchlock.exact.mounts=/home/me/work,/home/me/.config")
	assert.Contains(t, args, " matchlock.ipv6=fd00:100::2/64,fd00:100::1")
}

// TestDarwinArgsIPv6PrefixFromSubnet6CIDR pins that the prefix length in the
// emitted field comes from the shared derivation (Subnet6CIDR), not a
// hardcoded /64 — and that a network-form CIDR still yields the gateway.
func TestDarwinArgsIPv6PrefixFromSubnet6CIDR(t *testing.T) {
	cfg := darwinInterceptionConfig("vm-v6-prefix")
	cfg.Subnet6CIDR = "fd00:137::1/48"
	cfg.GatewayIPv6 = "fd00:137::1"
	cfg.GuestIPv6 = "fd00:137::2"
	assert.Contains(t, NewDarwinBackend().buildKernelArgs(cfg), " matchlock.ipv6=fd00:137::2/48,fd00:137::1")
}

// TestDarwinArgsCustomKernelArgsWinsVerbatim keeps the precedence rule pinned
// on the IPv6 branch too: custom KernelArgs return verbatim, dropping every
// generated matchlock.* argument including matchlock.ipv6=.
func TestDarwinArgsCustomKernelArgsWinsVerbatim(t *testing.T) {
	const custom = "console=hvc0 root=/dev/vda rw init=/init custom=1"
	cfg := darwinInterceptionConfig("vm-v6-custom")
	cfg.KernelArgs = custom
	assert.Equal(t, custom, NewDarwinBackend().buildKernelArgs(cfg))
}
