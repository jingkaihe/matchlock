package policy

import (
	"testing"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIsHostAllowedPort_LiteralAndPort covers IP-literal allow_private entries:
// a bare literal matches any port, while an entry carrying :port only matches
// that port, and an unlisted private literal stays blocked.
func TestIsHostAllowedPort_LiteralAndPort(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
		AllowPrivate:    []string{"192.168.107.74:8888"},
	})

	require.True(t, engine.IsHostAllowedPort("192.168.107.74", 8888))
	require.False(t, engine.IsHostAllowedPort("192.168.107.74", 8889),
		"a port-scoped entry must not match a different port")
	require.False(t, engine.IsHostAllowedPort("192.168.107.75", 8888),
		"an unlisted private literal stays blocked")

	// The port-agnostic wrapper parses the embedded port.
	require.True(t, engine.IsHostAllowed("192.168.107.74:8888"))
	require.False(t, engine.IsHostAllowed("192.168.107.74:8889"))

	// A bare entry matches any port.
	bare := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
		AllowPrivate:    []string{"10.0.0.5"},
	})
	require.True(t, bare.IsHostAllowedPort("10.0.0.5", 22))
	require.True(t, bare.IsHostAllowedPort("10.0.0.5", 443))
	require.False(t, bare.IsHostAllowedPort("10.0.0.6", 443))
}

// TestIsHostAllowedPort_CIDR covers CIDR entries with an optional port scope and
// the Yggdrasil 200::/7 range.
func TestIsHostAllowedPort_CIDR(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
		AllowPrivate:    []string{"192.168.1.0/24:443", "200::/7"},
	})

	require.True(t, engine.IsHostAllowedPort("192.168.1.10", 443))
	require.False(t, engine.IsHostAllowedPort("192.168.1.10", 444),
		"CIDR port scope must match")
	require.False(t, engine.IsHostAllowedPort("192.168.2.10", 443),
		"addresses outside the allowed CIDR stay blocked")

	// 200::/7 spans 200:: through 3ff:...; 400:: is public.
	require.True(t, engine.IsHostAllowedPort("200:1234::1", 12345),
		"a bare CIDR matches any port")
	require.True(t, engine.IsHostAllowedPort("300::1", 80))
	require.False(t, engine.IsHostAllowedPort("fc00::1", 80),
		"a private address outside the allowed CIDR stays blocked")
	require.True(t, engine.IsHostAllowedPort("400::1", 80),
		"400:: is public, outside 200::/7")

	// A Yggdrasil literal is allowed only when listed.
	noYggdrasil := NewEngine(&api.NetworkConfig{BlockPrivateIPs: true})
	require.False(t, noYggdrasil.IsHostAllowedPort("200:1234::1", 80))
}

// TestIsHostAllowedPort_IPv4Mapped covers IPv4-mapped IPv6 literals, which are
// private by default and must be explicitly listed.
func TestIsHostAllowedPort_IPv4Mapped(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
		AllowPrivate:    []string{"::ffff:192.168.1.1"},
	})

	require.True(t, engine.IsHostAllowedPort("::ffff:192.168.1.1", 80))
	require.True(t, engine.IsHostAllowedPort("192.168.1.1", 80),
		"the v4 and IPv4-mapped forms are equal")
	require.False(t, engine.IsHostAllowedPort("::ffff:192.168.1.2", 80))
	require.False(t, engine.IsHostAllowedPort("::ffff:10.0.0.1", 80))
}

// TestIsHostAllowedPort_NameExemptions exercises the DNS rebinding guard: a
// listed name is not enough on its own, every private address it resolves to
// must also be covered by an address entry (literal or CIDR) with a matching
// port scope.
func TestIsHostAllowedPort_NameExemptions(t *testing.T) {
	dns := newPolicySyntheticDNS(t, map[string][][]string{
		"allowed.test.":     {{"192.168.1.10"}},
		"sub.allowed.test.": {{"192.168.1.10"}},
		"mixed.test.":       {{"192.168.1.10", "192.168.1.99"}},
		"public.test.":      {{"203.0.113.5"}},
	})
	dns.install()
	defer dns.uninstall()

	t.Run("name entry with covering address entry is allowed", func(t *testing.T) {
		engine := NewEngine(&api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{"allowed.test", "192.168.1.10"},
		})
		require.True(t, engine.IsHostAllowedPort("allowed.test", 80))
	})

	t.Run("name entry without covering address entry is refused", func(t *testing.T) {
		engine := NewEngine(&api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{"allowed.test"},
		})
		require.False(t, engine.IsHostAllowedPort("allowed.test", 80),
			"a listed name alone must not authorize its private address")
	})

	t.Run("address entry without name entry is refused", func(t *testing.T) {
		engine := NewEngine(&api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{"192.168.1.10"},
		})
		require.False(t, engine.IsHostAllowedPort("allowed.test", 80),
			"an address entry alone must not authorize an unlisted name")
	})

	t.Run("name entry with covering CIDR is allowed", func(t *testing.T) {
		engine := NewEngine(&api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{"allowed.test", "192.168.1.0/24"},
		})
		require.True(t, engine.IsHostAllowedPort("allowed.test", 80))
	})

	t.Run("wildcard name entry with covering CIDR is allowed", func(t *testing.T) {
		engine := NewEngine(&api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{"*.allowed.test", "192.168.1.0/24"},
		})
		require.True(t, engine.IsHostAllowedPort("sub.allowed.test", 80))
	})

	t.Run("mixed allowed and unlisted private address is refused", func(t *testing.T) {
		engine := NewEngine(&api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{"mixed.test", "192.168.1.10"},
		})
		require.False(t, engine.IsHostAllowedPort("mixed.test", 80),
			"any unlisted private address in the answer set refuses the name")
	})

	t.Run("name and address entries are both port scoped", func(t *testing.T) {
		engine := NewEngine(&api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{"allowed.test:8080", "192.168.1.10:8080"},
		})
		require.True(t, engine.IsHostAllowedPort("allowed.test", 8080))
		require.False(t, engine.IsHostAllowedPort("allowed.test", 8081))
	})

	t.Run("name port scope is enforced even when the address entry is bare", func(t *testing.T) {
		engine := NewEngine(&api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{"allowed.test:8080", "192.168.1.10"},
		})
		require.True(t, engine.IsHostAllowedPort("allowed.test", 8080))
		require.False(t, engine.IsHostAllowedPort("allowed.test", 8081),
			"a name entry port mismatch must refuse even when the address entry is bare")
	})

	t.Run("public name is allowed without any entry", func(t *testing.T) {
		engine := NewEngine(&api.NetworkConfig{BlockPrivateIPs: true})
		require.True(t, engine.IsHostAllowedPort("public.test", 80))
	})
}

// TestAllowedHostIPsPort_ReturnsAllowedPrivateIP verifies the interceptor-facing
// path resolves once and returns the verified private address set for an exempt
// name, while an answer set containing an unlisted private address is refused.
func TestAllowedHostIPsPort_ReturnsAllowedPrivateIP(t *testing.T) {
	dns := newPolicySyntheticDNS(t, map[string][][]string{
		"allowed.test.": {{"192.168.1.10"}},
		"mixed.test.":   {{"192.168.1.10", "192.168.1.99"}},
	})
	dns.install()
	defer dns.uninstall()

	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
		AllowPrivate:    []string{"allowed.test", "192.168.1.0/24"},
	})
	ips, ok := engine.AllowedHostIPsPort("allowed.test", 80)
	require.True(t, ok)
	require.Len(t, ips, 1)
	assert.Equal(t, "192.168.1.10", ips[0].String())
	assert.Equal(t, 1, dns.aCount("allowed.test."),
		"AllowedHostIPsPort must resolve the hostname exactly once")

	// An unlisted private address in the answer set refuses the name outright.
	mixed := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
		AllowPrivate:    []string{"mixed.test", "192.168.1.10"},
	})
	ips, ok = mixed.AllowedHostIPsPort("mixed.test", 80)
	require.False(t, ok)
	require.Nil(t, ips)
}

// TestIsHostAllowedPort_AllowPrivateDoesNotBypassAllowedHosts verifies
// allow_private only lifts the private block: a non-empty allowed_hosts still
// governs the public side and covers IP-literal destinations too.
func TestIsHostAllowedPort_AllowPrivateDoesNotBypassAllowedHosts(t *testing.T) {
	dns := newPolicySyntheticDNS(t, map[string][][]string{
		"api.example.com.": {{"203.0.113.7"}},
		"listed.test.":     {{"192.168.50.50"}},
		"public2.test.":    {{"203.0.113.6"}},
	})
	dns.install()
	defer dns.uninstall()

	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
		AllowedHosts:    []string{"api.example.com"},
		AllowPrivate:    []string{"listed.test", "192.168.50.0/24", "public2.test"},
	})

	require.True(t, engine.IsHostAllowedPort("api.example.com", 443),
		"an allowed public host still works")
	require.False(t, engine.IsHostAllowedPort("listed.test", 80),
		"allow_private must not bypass a non-empty allowed_hosts")
	require.False(t, engine.IsHostAllowedPort("public2.test", 80),
		"allow_private must not admit a public host missing from allowed_hosts")
	require.False(t, engine.IsHostAllowedPort("192.168.50.50", 80),
		"an exempted private IP literal must still satisfy allowed_hosts")
}

// TestIsHostAllowedPort_NoNetworkWins verifies NoNetwork refuses everything,
// even when allow_private and allowed_hosts would otherwise permit it.
func TestIsHostAllowedPort_NoNetworkWins(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
		NoNetwork:       true,
		AllowedHosts:    []string{"api.example.com"},
		AllowPrivate:    []string{"192.168.1.10", "8.8.8.8", "api.example.com"},
	})

	require.False(t, engine.IsHostAllowedPort("8.8.8.8", 443))
	require.False(t, engine.IsHostAllowedPort("192.168.1.10", 443))
	require.False(t, engine.IsHostAllowedPort("api.example.com", 443))
	require.False(t, engine.IsHostAllowed("8.8.8.8:443"))

	ips, ok := engine.AllowedHostIPsPort("192.168.1.10", 443)
	require.False(t, ok)
	require.Nil(t, ips)
}

// TestIsHostAllowedPort_BracketAndCIDRPortParsing verifies the bracket form
// ([v6]:port) is recognized before colon splitting and a CIDR may also carry a
// port scope.
func TestIsHostAllowedPort_BracketAndCIDRPortParsing(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
		AllowPrivate:    []string{"[200::1]:8443", "10.0.0.0/8:9000"},
	})

	require.True(t, engine.IsHostAllowedPort("200::1", 8443))
	require.True(t, engine.IsHostAllowed("[200::1]:8443"))
	require.False(t, engine.IsHostAllowedPort("200::1", 8444))
	require.True(t, engine.IsHostAllowedPort("10.1.2.3", 9000))
	require.False(t, engine.IsHostAllowedPort("10.1.2.3", 9001))
}

// TestCompileAllowPrivate_MalformedEntriesNeverAllow verifies blank/malformed
// entries are treated as inert names and cannot accidentally exempt a private
// destination.
func TestCompileAllowPrivate_MalformedEntriesNeverAllow(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
		AllowPrivate:    []string{"", "   ", "192.168.1.0/33", "not a cidr/24"},
	})

	require.False(t, engine.IsHostAllowedPort("192.168.1.5", 80))
	require.False(t, engine.IsHostAllowedPort("10.0.0.1", 80))
}
