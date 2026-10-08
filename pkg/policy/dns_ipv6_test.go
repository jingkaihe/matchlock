package policy

import (
	"testing"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPrivateIPv6OnlyNameRefused qualifies that a name whose ONLY answer is an
// unlisted private IPv6 address is treated exactly like an unlisted private IPv4
// one: the address ranges apply to IPv6 (fc00::/7 unique-local and 200::/7
// Yggdrasil), and the guest's connection to that name is refused before any
// dial. The synthetic resolver serves the name over AAAA only, so the decision
// can only come from the IPv6 answer.
func TestPrivateIPv6OnlyNameRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		address string
	}{
		{"unique-local", "fc00::7"},
		{"yggdrasil", "200::9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dns := newPolicySyntheticDNSWithAAAA(t,
				nil, // no A answer at all
				map[string][][]string{"v6only.test.": {{tc.address}}},
			)
			dns.install()
			defer dns.uninstall()

			engine := NewEngine(&api.NetworkConfig{BlockPrivateIPs: true})

			assert.True(t, isPrivateIP("v6only.test"),
				"a name resolving only to %s must be considered private", tc.address)
			assert.False(t, engine.IsHostAllowedPort("v6only.test", 443),
				"an unlisted private IPv6 destination must be refused")

			ips, ok := engine.AllowedHostIPsPort("v6only.test", 443)
			assert.False(t, ok, "no verified address set may be returned for a refused host")
			assert.Nil(t, ips)

			assert.NotZero(t, dns.aAAAACount("v6only.test."),
				"the decision must come from the AAAA answer set")
		})
	}
}

// TestPublicIPv6OnlyNameAllowed is the positive control: a name resolving only
// to a public IPv6 address (2001:db8::/32 documentation space is not in the
// private ranges) is allowed, and its verified AAAA address set is returned so
// the caller can dial that literal.
func TestPublicIPv6OnlyNameAllowed(t *testing.T) {
	dns := newPolicySyntheticDNSWithAAAA(t,
		nil,
		map[string][][]string{"publicv6.test.": {{"2001:db8::1"}}},
	)
	dns.install()
	defer dns.uninstall()

	engine := NewEngine(&api.NetworkConfig{BlockPrivateIPs: true})

	assert.False(t, isPrivateIP("publicv6.test"))
	assert.True(t, engine.IsHostAllowedPort("publicv6.test", 443),
		"a public IPv6-only name must stay reachable")

	ips, ok := engine.AllowedHostIPsPort("publicv6.test", 443)
	require.True(t, ok)
	require.Len(t, ips, 1)
	assert.Equal(t, "2001:db8::1", ips[0].String())
}

// TestAllowPrivateIPv6NameRequiresBothEntries proves the allow_private
// exemptions apply to IPv6 exactly as they do to IPv4: an extended private range
// (200::/7, the Yggdrasil overlay) covered by an allow_private address entry
// still needs the matching name entry from the guest's connection, and the pair
// together authorizes the destination.
func TestAllowPrivateIPv6NameRequiresBothEntries(t *testing.T) {
	newResolver := func(t *testing.T) {
		t.Helper()
		dns := newPolicySyntheticDNSWithAAAA(t,
			nil,
			map[string][][]string{"ygg.test.": {{"200::9"}}},
		)
		dns.install()
		t.Cleanup(func() { dns.uninstall() })
	}

	t.Run("range alone is not enough", func(t *testing.T) {
		newResolver(t)
		engine := NewEngine(&api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{"200::/7"},
		})
		assert.False(t, engine.IsHostAllowedPort("ygg.test", 443),
			"an address entry alone must not authorize an unlisted name")
	})

	t.Run("name and range together allow it", func(t *testing.T) {
		newResolver(t)
		engine := NewEngine(&api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{"ygg.test", "200::/7"},
		})
		assert.True(t, engine.IsHostAllowedPort("ygg.test", 443),
			"the Yggdrasil name is authorized once both the name and the range are allowed")

		ips, ok := engine.AllowedHostIPsPort("ygg.test", 443)
		require.True(t, ok)
		require.Len(t, ips, 1)
		assert.Equal(t, "200::9", ips[0].String())
	})

	t.Run("an IPv4 entry does not cover an IPv6 address", func(t *testing.T) {
		newResolver(t)
		engine := NewEngine(&api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{"ygg.test", "10.0.0.0/8"},
		})
		assert.False(t, engine.IsHostAllowedPort("ygg.test", 443),
			"allow_private entries are family-scoped")
	})
}

// TestPrivateIPv6NameRefusedWithoutAllowPrivate is the legacy-behavior control:
// with no allow_private list at all, any name resolving to a private IPv6
// address is denied through the fast path.
func TestPrivateIPv6NameRefusedWithoutAllowPrivate(t *testing.T) {
	dns := newPolicySyntheticDNSWithAAAA(t,
		nil,
		map[string][][]string{"ual.test.": {{"fd00:100::2"}}},
	)
	dns.install()
	defer dns.uninstall()

	engine := NewEngine(&api.NetworkConfig{BlockPrivateIPs: true})
	assert.False(t, engine.IsHostAllowedPort("ual.test", 80))
}
