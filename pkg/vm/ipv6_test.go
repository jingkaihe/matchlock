package vm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ipv6TestConfig builds a networked VMConfig with the IPv4 fields a real
// sandbox carries. The IPv6 fields are left to each test so the "unset stays
// identical" cases share exactly one baseline.
func ipv6TestConfig(id string) *VMConfig {
	return &VMConfig{
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

// TestParseIPv6LinkSharedDerivation pins the shared derivation every backend
// consumes. These are the same shapes the Linux backend always promised; the
// move into pkg/vm exists so the darwin backend derives its IPv6 link from the
// identical code path instead of a second copy.
func TestParseIPv6LinkSharedDerivation(t *testing.T) {
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
		// The kernel-arg spelling is load-bearing: guest-init parses exactly
		// this field format, on both platforms.
		assert.Equal(t, " matchlock.ipv6=fd00:100::2/64,fd00:100::1", link.KernelArg())
		assert.Equal(t, "fd00:100::1/64", link.TapCIDR())
	})

	t.Run("network-form CIDR still yields the gateway address", func(t *testing.T) {
		cfg := ipv6TestConfig("vm-ipv6-network-form")
		cfg.GatewayIPv6 = "fd00:100::1"
		cfg.GuestIPv6 = "fd00:100::2"
		cfg.Subnet6CIDR = "fd00:100::/64"

		link, ok, err := ParseIPv6Link(cfg)
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, "fd00:100::1/64", link.TapCIDR())
	})

	t.Run("no IPv6 fields configures nothing", func(t *testing.T) {
		link, ok, err := ParseIPv6Link(ipv6TestConfig("vm-ipv6-absent"))
		require.NoError(t, err)
		assert.False(t, ok)
		assert.Equal(t, IPv6Link{}, link)
		assert.Empty(t, IPv6KernelArg(ipv6TestConfig("vm-ipv6-absent-arg")))
	})

	t.Run("partially-set fields configure nothing and are not an error", func(t *testing.T) {
		// Pinned Linux semantics: only a fully-set link is acted on. A partial
		// triple is the pre-existing "IPv4-only" shape, not a boot failure.
		partials := map[string]func(*VMConfig){
			"gateway only":      func(c *VMConfig) { c.GatewayIPv6 = "fd00:100::1" },
			"guest only":        func(c *VMConfig) { c.GuestIPv6 = "fd00:100::2" },
			"prefix only":       func(c *VMConfig) { c.Subnet6CIDR = "fd00:100::1/64" },
			"gateway and guest": func(c *VMConfig) { c.GatewayIPv6 = "fd00:100::1"; c.GuestIPv6 = "fd00:100::2" },
		}
		for name, mutate := range partials {
			t.Run(name, func(t *testing.T) {
				cfg := ipv6TestConfig("vm-ipv6-partial")
				mutate(cfg)
				link, ok, err := ParseIPv6Link(cfg)
				require.NoError(t, err)
				assert.False(t, ok)
				assert.Equal(t, IPv6Link{}, link)
				assert.Empty(t, IPv6KernelArg(cfg))
			})
		}
	})

	t.Run("nil config configures nothing", func(t *testing.T) {
		link, ok, err := ParseIPv6Link(nil)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.Equal(t, IPv6Link{}, link)
	})

	t.Run("malformed fully-set links are rejected loudly", func(t *testing.T) {
		cases := []struct {
			name   string
			mutate func(*VMConfig)
			want   error
		}{
			{name: "IPv4 gateway", mutate: func(c *VMConfig) {
				c.GatewayIPv6 = "192.168.100.1"
				c.GuestIPv6 = "fd00:100::2"
				c.Subnet6CIDR = "fd00:100::1/64"
			}, want: ErrInvalidIPv6Address},
			{name: "garbage guest", mutate: func(c *VMConfig) {
				c.GatewayIPv6 = "fd00:100::1"
				c.GuestIPv6 = "not-an-ip"
				c.Subnet6CIDR = "fd00:100::1/64"
			}, want: ErrInvalidIPv6Address},
			{name: "malformed prefix", mutate: func(c *VMConfig) {
				c.GatewayIPv6 = "fd00:100::1"
				c.GuestIPv6 = "fd00:100::2"
				c.Subnet6CIDR = "fd00:100::1"
			}, want: ErrInvalidCIDR},
			{name: "IPv4 prefix", mutate: func(c *VMConfig) {
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
				// A malformed link must never reach a kernel cmdline.
				assert.Empty(t, IPv6KernelArg(cfg))
			})
		}
	})
}
