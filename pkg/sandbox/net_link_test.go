package sandbox

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jingkaihe/matchlock/pkg/state"
)

// leasedIPv6SubnetInfo is the shape pkg/state hands out for a VM: an IPv4 /24
// plus the matching per-VM IPv6 unique-local /64 (see state.ipv6ForOctet).
func leasedIPv6SubnetInfo(octet int) *state.SubnetInfo {
	return &state.SubnetInfo{
		Octet:       octet,
		GatewayIP:   "192.168.100.1",
		GuestIP:     "192.168.100.2",
		Subnet:      "192.168.100.0/24",
		GatewayIPv6: "fd00:100::1",
		GuestIPv6:   "fd00:100::2",
		Subnet6:     "fd00:100::/64",
		VMID:        "vm-netlink",
	}
}

// TestSandboxNetLinkGatesIPv6OnInterception pins the gate every IPv6 consumer
// relies on: an intercepted sandbox gets the lease's IPv6 triple exactly as
// recorded, while a plain-NAT sandbox (intercept=false) and a --no-network
// sandbox (nil lease) get an empty IPv6 half so their backend config and boot
// args stay byte-identical to the pre-IPv6 behavior.
func TestSandboxNetLinkGatesIPv6OnInterception(t *testing.T) {
	t.Run("intercepted sandbox carries the full lease", func(t *testing.T) {
		link := newSandboxNetLink(leasedIPv6SubnetInfo(100), true)
		require.Equal(t, "192.168.100.1", link.GatewayIPv4)
		require.Equal(t, "192.168.100.2", link.GuestIPv4)
		require.Equal(t, "192.168.100.1/24", link.SubnetCIDR)
		require.Equal(t, "fd00:100::1", link.GatewayIPv6)
		require.Equal(t, "fd00:100::2", link.GuestIPv6)
		require.Equal(t, "fd00:100::/64", link.Subnet6CIDR)
	})

	t.Run("plain NAT sandbox keeps the IPv4-only shape", func(t *testing.T) {
		link := newSandboxNetLink(leasedIPv6SubnetInfo(101), false)
		require.Equal(t, "192.168.100.1", link.GatewayIPv4)
		require.Equal(t, "192.168.100.2", link.GuestIPv4)
		require.Equal(t, "192.168.100.1/24", link.SubnetCIDR)
		assert.Empty(t, link.GatewayIPv6)
		assert.Empty(t, link.GuestIPv6)
		assert.Empty(t, link.Subnet6CIDR)
	})

	t.Run("no-network sandbox (nil lease) is fully inert", func(t *testing.T) {
		link := newSandboxNetLink(nil, true)
		assert.Equal(t, sandboxNetLink{}, link)
	})
}
