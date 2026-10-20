package policy

import (
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover the passthrough defect shape: pkg/net/proxy.go
// handlePassthrough only ever sees the pre-DNAT destination IP:port, so an
// allow_private NAME entry can only lift the private block if the entry covers
// the address set the name resolves to (allowPrivateNameAddress, reached through
// allowPrivateAddress for an IP-literal destination). The documented DNS
// rebinding guard for a NAME destination is deliberately unchanged: it still
// requires address-entry coverage of every private address in the answer set
// (privateBlockedResolved uses allowPrivateAddressEntry) plus a matching name
// entry.
//
// They use the package's process-local synthetic resolver (dnssynth_test.go),
// which replaces net.DefaultResolver, so they must NOT run with t.Parallel().

// TestAllowPrivateAddress_NameEntryCoversLiteralDestination is the regression
// test for the coordinator's reproduction: '--allow-private ai.iasylum.net:8888'
// must admit the literal destination the passthrough path observes, exactly like
// the literal and CIDR entry forms do.
func TestAllowPrivateAddress_NameEntryCoversLiteralDestination(t *testing.T) {
	t.Run("IPv4 literal", func(t *testing.T) {
		dns := newPolicySyntheticDNS(t, map[string][][]string{
			"priv-name.test.": {{"192.168.107.74"}},
		})
		dns.install()
		defer dns.uninstall()

		engine := NewEngine(&api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{"priv-name.test:8888"},
		})

		require.True(t, engine.IsHostAllowedPort("192.168.107.74", 8888),
			"the name entry must cover the address the name resolves to")
		require.True(t, engine.IsHostAllowed("192.168.107.74:8888"))

		ips, ok := engine.AllowedHostIPsPort("192.168.107.74", 8888)
		require.True(t, ok)
		require.Len(t, ips, 1)
		assert.Equal(t, "192.168.107.74", ips[0].String())

		// The same private literal without the entry stays blocked, and an
		// unrelated private address is not covered by the entry.
		control := NewEngine(&api.NetworkConfig{BlockPrivateIPs: true})
		require.False(t, control.IsHostAllowedPort("192.168.107.74", 8888))
		require.False(t, engine.IsHostAllowedPort("192.168.107.1", 80),
			"an address outside the resolved set stays blocked")
	})

	t.Run("IPv6 Yggdrasil literal", func(t *testing.T) {
		const yggLiteral = "219:c447:5629:62b7:4e7:9db8:2a3f:8ca9"
		dns := newPolicySyntheticDNSWithAAAA(t,
			nil,
			map[string][][]string{"ygg-name.test.": {{yggLiteral}}},
		)
		dns.install()
		defer dns.uninstall()

		engine := NewEngine(&api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{"ygg-name.test:8888"},
		})

		require.True(t, engine.IsHostAllowedPort(yggLiteral, 8888),
			"the name entry must cover its resolved Yggdrasil address (200::/7)")

		ips, ok := engine.AllowedHostIPsPort(yggLiteral, 8888)
		require.True(t, ok)
		require.Len(t, ips, 1)
		assert.Equal(t, yggLiteral, ips[0].String())

		control := NewEngine(&api.NetworkConfig{BlockPrivateIPs: true})
		require.False(t, control.IsHostAllowedPort(yggLiteral, 8888))
	})

	t.Run("add_hosts mapping needs no DNS", func(t *testing.T) {
		dns := newPolicySyntheticDNS(t, map[string][][]string{
			// A deliberately different answer: any DNS use would be observable
			// both in the covered address and in aCount.
			"fixture-name.test.": {{"10.1.1.1"}},
		})
		dns.install()
		defer dns.uninstall()

		engine := NewEngine(&api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{"fixture-name.test:8888"},
			AddHosts:        []api.HostIPMapping{{Host: "fixture-name.test", IP: "192.168.107.74"}},
		})

		require.True(t, engine.IsHostAllowedPort("192.168.107.74", 8888),
			"the static add_hosts mapping is authoritative for policy evaluation")
		assert.Equal(t, 0, dns.aCount("fixture-name.test."),
			"an add_hosts mapping must cover the address with zero DNS queries")
	})
}

// TestAllowPrivateAddress_NameEntryPortScope verifies the port scope of a NAME
// entry is enforced on the address it covers, so '--allow-private name:8888'
// does not authorize the same address on another port.
func TestAllowPrivateAddress_NameEntryPortScope(t *testing.T) {
	dns := newPolicySyntheticDNS(t, map[string][][]string{
		"scoped.test.": {{"10.3.3.3"}},
		"bare.test.":   {{"10.4.4.4"}},
	})
	dns.install()
	defer dns.uninstall()

	scoped := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
		AllowPrivate:    []string{"scoped.test:9443"},
	})
	require.True(t, scoped.IsHostAllowedPort("10.3.3.3", 9443))
	require.False(t, scoped.IsHostAllowedPort("10.3.3.3", 8443),
		"a port-scoped name entry must not cover the address on another port")
	require.False(t, scoped.IsHostAllowedPort("10.3.3.3", 0),
		"a port-scoped entry does not match an unknown (0) port")

	ips, ok := scoped.AllowedHostIPsPort("10.3.3.3", 9443)
	require.True(t, ok)
	require.Len(t, ips, 1)
	ips, ok = scoped.AllowedHostIPsPort("10.3.3.3", 8443)
	require.False(t, ok)
	require.Nil(t, ips)

	// A bare entry (no port) covers its address on any port, and the bracketed
	// name form parses to the same entry.
	bare := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
		AllowPrivate:    []string{"[bare.test]:443"},
	})
	require.True(t, bare.IsHostAllowedPort("10.4.4.4", 443))
	require.False(t, bare.IsHostAllowedPort("10.4.4.4", 444))

	anyPort := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
		AllowPrivate:    []string{"bare.test"},
	})
	require.True(t, anyPort.IsHostAllowedPort("10.4.4.4", 22))
	require.True(t, anyPort.IsHostAllowedPort("10.4.4.4", 8888))
}

// TestAllowPrivateAddress_UnresolvableNameEntryNeverLifts verifies an entry that
// does not resolve covers nothing, never panics, and is reported exactly once.
func TestAllowPrivateAddress_UnresolvableNameEntryNeverLifts(t *testing.T) {
	dns := newPolicySyntheticDNS(t, map[string][][]string{
		"resolve.test.": {{"10.1.2.3"}},
	})
	dns.install()
	defer dns.uninstall()

	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
		AllowPrivate:    []string{"missing.test:8888"},
	})

	var warns []string
	engine.nameWarn = func(name string) { warns = append(warns, name) }

	require.False(t, engine.IsHostAllowedPort("10.1.2.3", 8888),
		"an unresolvable name entry must not lift the block")

	ips, ok := engine.AllowedHostIPsPort("10.1.2.3", 8888)
	require.False(t, ok)
	require.Nil(t, ips)

	require.True(t, engine.IsHostAllowedPort("203.0.113.60", 80),
		"a public destination is unaffected")
	require.Equal(t, []string{"missing.test"}, warns,
		"an unresolvable name entry is reported exactly once")

	// A second decision neither re-resolves nor warns again inside the TTL.
	require.False(t, engine.IsHostAllowedPort("10.1.2.3", 8888))
	require.Equal(t, []string{"missing.test"}, warns)
}

// TestAllowPrivateAddress_GlobNameEntryIsNotResolved documents that a glob name
// entry is matched by allowPrivateName only: it is not a host name, so it is
// never resolved, contributes no address, and does not warn.
func TestAllowPrivateAddress_GlobNameEntryIsNotResolved(t *testing.T) {
	dns := newPolicySyntheticDNS(t, map[string][][]string{
		"sub.priv.test.": {{"192.168.5.5"}},
	})
	dns.install()
	defer dns.uninstall()

	engine := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
		AllowPrivate:    []string{"*.priv.test"},
	})

	assert.Empty(t, engine.allowPrivateNames,
		"a glob pattern is not a resolvable name entry")

	var warns []string
	engine.nameWarn = func(name string) { warns = append(warns, name) }

	// The glob entry does not cover a literal destination...
	require.False(t, engine.IsHostAllowedPort("192.168.5.5", 80),
		"a glob entry contributes no resolved address set")
	// ...but it still matches the destination name itself.
	require.False(t, engine.IsHostAllowedPort("sub.priv.test", 80),
		"a glob entry alone still refuses an uncovered private address")

	// A glob entry + an address entry authorizes the name.
	withAddr := NewEngine(&api.NetworkConfig{
		BlockPrivateIPs: true,
		AllowPrivate:    []string{"*.priv.test", "192.168.5.5"},
	})
	require.True(t, withAddr.IsHostAllowedPort("sub.priv.test", 80))

	assert.Empty(t, warns, "a glob entry must not warn that it does not resolve")
	assert.Equal(t, 0, dns.aCount("*.priv.test."),
		"a glob pattern must never be queried")
}

// TestAllowPrivateAddress_RebindingGuardUnchanged keeps the documented guard
// shape intact: a name destination needs a matching name entry AND full
// coverage of every private address in its answer set; an address entry alone
// authorizes no name, and a name entry authorizes no other name.
func TestAllowPrivateAddress_RebindingGuardUnchanged(t *testing.T) {
	t.Run("address entry alone does not authorize a name", func(t *testing.T) {
		dns := newPolicySyntheticDNS(t, map[string][][]string{
			"allowed.test.": {{"192.168.1.10"}},
		})
		dns.install()
		defer dns.uninstall()

		engine := NewEngine(&api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{"192.168.1.0/24"},
		})
		require.False(t, engine.IsHostAllowedPort("allowed.test", 80))
		require.True(t, engine.IsHostAllowedPort("192.168.1.10", 80),
			"the literal is covered by the address entry")
	})

	t.Run("name entry does not authorize another name", func(t *testing.T) {
		dns := newPolicySyntheticDNS(t, map[string][][]string{
			"allowed.test.": {{"192.168.1.10"}},
			"evil.test.":    {{"192.168.1.10"}},
		})
		dns.install()
		defer dns.uninstall()

		engine := NewEngine(&api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{"allowed.test"},
		})
		require.False(t, engine.IsHostAllowedPort("evil.test", 80),
			"a different name resolving to the same private address stays refused")
	})

	t.Run("mixed public/private answer with an uncovered private address is refused", func(t *testing.T) {
		dns := newPolicySyntheticDNS(t, map[string][][]string{
			"gw.guard.test.": {{"203.0.113.9", "192.168.1.99"}},
		})
		dns.install()
		defer dns.uninstall()

		engine := NewEngine(&api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{"*.guard.test", "203.0.113.9"},
		})
		require.False(t, engine.IsHostAllowedPort("gw.guard.test", 80),
			"the unlisted private address refuses the whole answer set")
	})

	t.Run("a name destination still needs a covering address entry", func(t *testing.T) {
		// The documented guard is unchanged: a NAME destination's private
		// addresses must be covered by an address entry (literal/CIDR) in the
		// same list. The name entry authorizes the name; it does not by itself
		// pin the address the proxy resolved for it.
		dns := newPolicySyntheticDNS(t, map[string][][]string{
			"allowed.test.": {{"192.168.1.10"}},
		})
		dns.install()
		defer dns.uninstall()

		nameOnly := NewEngine(&api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{"allowed.test"},
		})
		require.False(t, nameOnly.IsHostAllowedPort("allowed.test", 80))

		withAddress := NewEngine(&api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{"allowed.test", "192.168.1.0/24"},
		})
		require.True(t, withAddress.IsHostAllowedPort("allowed.test", 80))
	})

	t.Run("a refreshed name entry covers only its current answer set", func(t *testing.T) {
		dns := newPolicySyntheticDNS(t, map[string][][]string{
			"rebind.test.": {
				{"192.168.1.10"}, // first resolution
				{"192.168.1.99"}, // after the TTL elapsed
			},
		})
		dns.install()
		defer dns.uninstall()

		engine := NewEngine(&api.NetworkConfig{
			BlockPrivateIPs: true,
			AllowPrivate:    []string{"rebind.test"},
		})

		current := time.Now()
		engine.now = func() time.Time { return current }

		// The literal destination is what the passthrough path observes.
		require.True(t, engine.IsHostAllowedPort("192.168.1.10", 80))
		require.False(t, engine.IsHostAllowedPort("192.168.1.99", 80),
			"an address the name does not (yet) resolve to is not covered")
		assert.Equal(t, 1, dns.aCount("rebind.test."),
			"the entry must resolve at most once inside the TTL")

		// Past the TTL the entry is re-resolved: the new address set is the one
		// covered and the stale address is no longer authorized.
		current = current.Add(engine.nameTTLValue() + time.Second)
		require.True(t, engine.IsHostAllowedPort("192.168.1.99", 80),
			"the refreshed answer set is covered")
		require.False(t, engine.IsHostAllowedPort("192.168.1.10", 80),
			"a stale address outside the current answer set is no longer covered")
		assert.Equal(t, 2, dns.aCount("rebind.test."))
	})
}

// TestAllowPrivateAddress_NoExceptionFastPathUnchanged is the control for the
// legacy path: with no allow_private list at all nothing about the decision
// changes and the NAME resolver does no work.
func TestAllowPrivateAddress_NoExceptionFastPathUnchanged(t *testing.T) {
	dns := newPolicySyntheticDNS(t, map[string][][]string{
		"priv.test.":   {{"192.168.1.10"}},
		"public.test.": {{"203.0.113.5"}},
	})
	dns.install()
	defer dns.uninstall()

	engine := NewEngine(&api.NetworkConfig{BlockPrivateIPs: true})

	require.False(t, engine.IsHostAllowedPort("192.168.1.10", 80))
	require.False(t, engine.IsHostAllowedPort("priv.test", 80))
	require.True(t, engine.IsHostAllowedPort("203.0.113.5", 80))
	require.True(t, engine.IsHostAllowedPort("public.test", 80))
	assert.Empty(t, engine.allowPrivateNames,
		"no allow_private list means no NAME entry resolution at all")
}
