//go:build linux

package linux

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/vm"
)

// ipv6TestConfig builds a networked VMConfig with the IPv4 fields a real sandbox
// carries. The IPv6 fields are left to each test so the "unset stays identical"
// cases share exactly one baseline.
func ipv6TestConfig(id string) *vm.VMConfig {
	return &vm.VMConfig{
		ID:         id,
		Hostname:   id,
		CPUs:       1,
		MemoryMB:   512,
		KernelPath: "/kernel",
		RootfsPath: "/rootfs",
		GatewayIP:  "192.168.100.1",
		GuestIP:    "192.168.100.2",
		SubnetCIDR: "192.168.100.1/24",
		DNSServers: []string{"8.8.8.8", "8.8.4.4"},
	}
}

func TestParseIPv6Link(t *testing.T) {
	t.Run("derives the link from a gateway/guest/prefix triple", func(t *testing.T) {
		cfg := ipv6TestConfig("vm-ipv6-link")
		cfg.GatewayIPv6 = "fd00:100::1"
		cfg.GuestIPv6 = "fd00:100::2"
		cfg.Subnet6CIDR = "fd00:100::1/64"

		link, ok, err := ParseIPv6Link(cfg)
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, "fd00:100::1", link.Gateway)
		assert.Equal(t, "fd00:100::2", link.Guest)
		assert.Equal(t, 64, link.PrefixLen)
		assert.Equal(t, "fd00:100::1/64", link.TapCIDR())
		assert.Equal(t, " matchlock.ipv6=fd00:100::2/64,fd00:100::1", link.KernelArg())
	})

	t.Run("the TAP always carries the gateway, never a network address", func(t *testing.T) {
		// A subnet allocator reports the /64 in network form (fd00:100::/64);
		// configuring that literal on the TAP would install the network
		// address instead of the gateway the guest routes through.
		cfg := ipv6TestConfig("vm-ipv6-network-form")
		cfg.GatewayIPv6 = "fd00:100::1"
		cfg.GuestIPv6 = "fd00:100::2"
		cfg.Subnet6CIDR = "fd00:100::/64"

		link, ok, err := ParseIPv6Link(cfg)
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, "fd00:100::1/64", link.TapCIDR())
	})

	t.Run("non-64 prefix lengths are honored", func(t *testing.T) {
		cfg := ipv6TestConfig("vm-ipv6-prefix")
		cfg.GatewayIPv6 = "fd00:137::1"
		cfg.GuestIPv6 = "fd00:137::2"
		cfg.Subnet6CIDR = "fd00:137::1/48"

		link, ok, err := ParseIPv6Link(cfg)
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, 48, link.PrefixLen)
		assert.Equal(t, "fd00:137::1/48", link.TapCIDR())
		assert.Equal(t, " matchlock.ipv6=fd00:137::2/48,fd00:137::1", link.KernelArg())
	})

	t.Run("no IPv6 fields at all is not an error and configures nothing", func(t *testing.T) {
		cfg := ipv6TestConfig("vm-ipv6-absent")
		link, ok, err := ParseIPv6Link(cfg)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.Equal(t, IPv6Link{}, link)
		assert.Empty(t, IPv6KernelArg(cfg))
	})

	t.Run("partial IPv6 fields are not an error but configure nothing", func(t *testing.T) {
		partials := map[string]func(*vm.VMConfig){
			"gateway only":      func(c *vm.VMConfig) { c.GatewayIPv6 = "fd00:100::1" },
			"guest only":        func(c *vm.VMConfig) { c.GuestIPv6 = "fd00:100::2" },
			"prefix only":       func(c *vm.VMConfig) { c.Subnet6CIDR = "fd00:100::1/64" },
			"gateway and guest": func(c *vm.VMConfig) { c.GatewayIPv6 = "fd00:100::1"; c.GuestIPv6 = "fd00:100::2" },
		}
		for name, mutate := range partials {
			t.Run(name, func(t *testing.T) {
				cfg := ipv6TestConfig("vm-ipv6-partial")
				mutate(cfg)
				link, ok, err := ParseIPv6Link(cfg)
				require.NoError(t, err)
				assert.False(t, ok)
				assert.Equal(t, IPv6Link{}, link)
			})
		}
	})

	t.Run("nil config", func(t *testing.T) {
		link, ok, err := ParseIPv6Link(nil)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.Equal(t, IPv6Link{}, link)
	})

	t.Run("malformed IPv6 fields are rejected loudly", func(t *testing.T) {
		cases := []struct {
			name   string
			mutate func(*vm.VMConfig)
			want   error
		}{
			{name: "IPv4 gateway", mutate: func(c *vm.VMConfig) {
				c.GatewayIPv6 = "192.168.100.1"
				c.GuestIPv6 = "fd00:100::2"
				c.Subnet6CIDR = "fd00:100::1/64"
			}, want: ErrInvalidIPv6Address},
			{name: "garbage guest", mutate: func(c *vm.VMConfig) {
				c.GatewayIPv6 = "fd00:100::1"
				c.GuestIPv6 = "not-an-ip"
				c.Subnet6CIDR = "fd00:100::1/64"
			}, want: ErrInvalidIPv6Address},
			{name: "malformed prefix", mutate: func(c *vm.VMConfig) {
				c.GatewayIPv6 = "fd00:100::1"
				c.GuestIPv6 = "fd00:100::2"
				c.Subnet6CIDR = "fd00:100::1"
			}, want: ErrInvalidCIDR},
			{name: "IPv4 prefix", mutate: func(c *vm.VMConfig) {
				c.GatewayIPv6 = "fd00:100::1"
				c.GuestIPv6 = "fd00:100::2"
				c.Subnet6CIDR = "192.168.100.1/24"
			}, want: ErrInvalidCIDR},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				cfg := ipv6TestConfig("vm-ipv6-bad")
				tc.mutate(cfg)
				_, ok, err := ParseIPv6Link(cfg)
				require.Error(t, err)
				assert.ErrorIs(t, err, tc.want)
				assert.False(t, ok)
			})
		}
	})
}

func TestGenerateFirecrackerConfigIPv6BootArg(t *testing.T) {
	// Golden IPv4-only boot args for ipv6TestConfig: the IPv6 work must not
	// change a single byte of this string.
	const goldenIPv4Args = "console=ttyS0 reboot=k panic=1 acpi=off init=/init hostname=vm-ipv6-args " +
		"matchlock.dns=8.8.8.8,8.8.4.4 ip=192.168.100.2::192.168.100.1:255.255.255.0::eth0:off:8.8.8.8:8.8.4.4 " +
		"matchlock.mtu=1500 matchlock.cpus=1"

	configArgs := func(t *testing.T, cfg *vm.VMConfig) string {
		t.Helper()
		m := &LinuxMachine{id: cfg.ID, config: cfg}
		return decodeFirecrackerConfigForTest(t, m.generateFirecrackerConfig()).BootSource.BootArgs
	}

	t.Run("IPv4-only args are unchanged", func(t *testing.T) {
		cfg := ipv6TestConfig("vm-ipv6-args")
		assert.Equal(t, goldenIPv4Args, configArgs(t, cfg))
	})

	t.Run("IPv6 fields add exactly one cmdline field next to ip=", func(t *testing.T) {
		cfg := ipv6TestConfig("vm-ipv6-args")
		cfg.GatewayIPv6 = "fd00:100::1"
		cfg.GuestIPv6 = "fd00:100::2"
		cfg.Subnet6CIDR = "fd00:100::1/64"

		got := configArgs(t, cfg)
		want := strings.Replace(goldenIPv4Args,
			"matchlock.mtu=1500",
			"matchlock.mtu=1500 matchlock.ipv6=fd00:100::2/64,fd00:100::1", 1)
		assert.Equal(t, want, got, "the IPv6 field is purely additive, right after the ip= field")
		assert.Contains(t, got, " ip=192.168.100.2::192.168.100.1:255.255.255.0::eth0:off")
	})

	t.Run("IPv6 prefix length comes from Subnet6CIDR", func(t *testing.T) {
		cfg := ipv6TestConfig("vm-ipv6-args")
		cfg.GatewayIPv6 = "fd00:137::1"
		cfg.GuestIPv6 = "fd00:137::2"
		cfg.Subnet6CIDR = "fd00:137::1/48"

		assert.Contains(t, configArgs(t, cfg), "matchlock.ipv6=fd00:137::2/48,fd00:137::1")
	})

	t.Run("--no-network emits no IPv6 field even with the fields set", func(t *testing.T) {
		withV6 := ipv6TestConfig("vm-ipv6-nonet")
		withV6.NoNetwork = true
		withV6.GatewayIPv6 = "fd00:100::1"
		withV6.GuestIPv6 = "fd00:100::2"
		withV6.Subnet6CIDR = "fd00:100::1/64"

		withoutV6 := ipv6TestConfig("vm-ipv6-nonet")
		withoutV6.NoNetwork = true

		withArgs := configArgs(t, withV6)
		assert.NotContains(t, withArgs, "matchlock.ipv6=")
		assert.Contains(t, withArgs, "ip=off matchlock.no_network=1")
		assert.Equal(t, configArgs(t, withoutV6), withArgs,
			"a --no-network sandbox must be byte-identical with or without IPv6 fields")
	})

	t.Run("a malformed IPv6 link emits no field", func(t *testing.T) {
		// Create rejects this config through the same derivation before any VM
		// exists; the boot-arg builder must not smuggle half a link into the
		// cmdline on the bypass path.
		cfg := ipv6TestConfig("vm-ipv6-bad-arg")
		cfg.GatewayIPv6 = "192.168.100.1"
		cfg.GuestIPv6 = "fd00:100::2"
		cfg.Subnet6CIDR = "fd00:100::1/64"

		assert.NotContains(t, configArgs(t, cfg), "matchlock.ipv6=")
	})
}

// TestVMConfigIPv6FieldsAreDocumented pins the naming the rest of the IPv6 work
// (guest-init parsing, sandbox wiring) depends on.
func TestVMConfigIPv6FieldsAreDocumented(t *testing.T) {
	cfg := &vm.VMConfig{
		GatewayIPv6: "fd00:100::1",
		GuestIPv6:   "fd00:100::2",
		Subnet6CIDR: "fd00:100::1/64",
	}
	assert.Equal(t, "fd00:100::1", cfg.GatewayIPv6)
	assert.Equal(t, "fd00:100::2", cfg.GuestIPv6)
	assert.Equal(t, "fd00:100::1/64", cfg.Subnet6CIDR)
	assert.Equal(t, 1, effectiveVCPUs(cfg.CPUs), "unrelated defaults stay untouched")
	assert.Equal(t, api.DefaultNetworkMTU, effectiveMTU(cfg.MTU))
}
