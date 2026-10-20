package policy

import (
	"testing"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/stretchr/testify/require"
)

// TestIsHostAllowedPort_IPv6CIDRWithPortScope pins the allow_private form the
// IPv6 acceptance test relies on: an exempted ULA range scoped to one port, so
// the per-VM gateway address (fd00:<octet>::1, only known after the VM exists)
// can be exempted without exempting every port. Both the bracketed and the bare
// form must compile to a network entry with that port.
func TestIsHostAllowedPort_IPv6CIDRWithPortScope(t *testing.T) {
	for _, entry := range []string{"[fd00::/8]:8080", "fd00::/8:8080"} {
		engine := NewEngine(&api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{entry},
		})

		require.True(t, engine.IsHostAllowedPort("fd00:37::1", 8080),
			"%s must exempt the per-VM gateway on its port", entry)
		require.False(t, engine.IsHostAllowedPort("fd00:37::1", 8081),
			"%s must not exempt another port on the same address", entry)
		require.True(t, engine.IsHostAllowedPort("fd00:37::2", 8080),
			"%s covers the whole range", entry)
		require.False(t, engine.IsHostAllowedPort("fc00:1::1", 8080),
			"%s must not exempt a ULA literal outside the range", entry)
		require.False(t, engine.IsHostAllowedPort("200:1234::1", 8080),
			"%s must not exempt the Yggdrasil range", entry)
		require.True(t, engine.IsHostAllowedPort("400::1", 8080),
			"400::/7 is public, so the allow_private scope does not apply (%s)", entry)
	}
}

// TestIsHostAllowedPort_IPv6CIDRWithoutPortScope is the port-agnostic half: a
// bare ULA range entry exempts the range on any port.
func TestIsHostAllowedPort_IPv6CIDRWithoutPortScope(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
		AllowPrivate:    []string{"fd00::/8"},
	})

	require.True(t, engine.IsHostAllowedPort("fd00:37::1", 8080))
	require.True(t, engine.IsHostAllowedPort("fd00:37::1", 443))
	require.False(t, engine.IsHostAllowedPort("fc00:1::1", 443),
		"another ULA prefix stays blocked")
}
