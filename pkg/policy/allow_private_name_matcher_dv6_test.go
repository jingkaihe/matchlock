package policy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jingkaihe/matchlock/pkg/api"
)

// MatchesAllowPrivateName is the allow_private carve-out source for the DNS
// answer-mode rewrite (a name exempted here is always relayed verbatim). These
// pins fix its identity notion: ASCII case-insensitivity, trailing-dot
// normalization, glob entries, port scope IGNORED (a DNS query carries no
// port), and address/CIDR entries never matching a name.
//
// The dv6 suffix keeps this file coexistable with the icebox-dns lane's pins
// (same semantics, deliberately distinct identifiers). NOTE for merge: the
// icebox-dns (FIX2) lane folds names with asciiLower (ASCII-only);
// normalizeDNSName on this branch still lowercases with strings.ToLower, and
// these pins deliberately exercise only ASCII inputs so both folds pass —
// non-ASCII behavior is deliberately not pinned here and should be unified at
// merge by adopting the ASCII-only fold.
func TestDV6MatchesAllowPrivateName(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		AllowPrivate: []string{
			"ai.iasylum.net",      // bare name
			"Scoped.Example:8888", // name with a port scope
			"*.internal.example",  // glob entry
			"192.168.107.74:8888", // IPv4 literal with port
			"200:1234::1",         // IPv6 literal (Yggdrasil-style)
			"[fd00::1]:443",       // bracketed IPv6 literal with port
			"100.64.0.0/10",       // CIDR
		},
	})

	t.Run("bare name matches, ignoring ASCII case and trailing root dot", func(t *testing.T) {
		assert.True(t, engine.MatchesAllowPrivateName("ai.iasylum.net"))
		assert.True(t, engine.MatchesAllowPrivateName("AI.IASYLUM.NET"))
		assert.True(t, engine.MatchesAllowPrivateName("ai.iasylum.net."))
		assert.False(t, engine.MatchesAllowPrivateName("other.example"))
	})

	t.Run("port scope is ignored for names", func(t *testing.T) {
		assert.True(t, engine.MatchesAllowPrivateName("scoped.example"),
			"a port-scoped name entry matches the NAME; a DNS query carries no port")
		assert.True(t, engine.MatchesAllowPrivateName("Scoped.Example."))
	})

	t.Run("glob entries match like the engine's other name comparisons", func(t *testing.T) {
		assert.True(t, engine.MatchesAllowPrivateName("a.internal.example"))
		assert.False(t, engine.MatchesAllowPrivateName("internal.example"))
		assert.False(t, engine.MatchesAllowPrivateName("a.other.example"))
	})

	t.Run("address and CIDR entries never match a name", func(t *testing.T) {
		for _, host := range []string{
			"192.168.107.74",
			"200:1234::1",
			"fd00::1",
			"100.64.0.1",
		} {
			assert.False(t, engine.MatchesAllowPrivateName(host),
				"%q is an address entry, not a name entry", host)
		}
	})

	t.Run("empty input and empty allowlist", func(t *testing.T) {
		assert.False(t, engine.MatchesAllowPrivateName(""))
		assert.False(t, engine.MatchesAllowPrivateName("   "))
		empty := NewEngine(&api.NetworkConfig{})
		assert.False(t, empty.MatchesAllowPrivateName("ai.iasylum.net"),
			"no allow_private entries: the carve-out matches nothing")
	})
}

// TestDV6MatchesAllowPrivateNameUsedByResolverConfig exercises the exact seam
// the darwin stack consumes: a policy engine handed to the DNS layer supplies
// the matcher, and the query path passes names WITHOUT a trailing dot (the
// resolver normalizes the wire name, then trims the root dot).
func TestDV6MatchesAllowPrivateNameResolverSeam(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{AllowPrivate: []string{"ai.iasylum.net"}})
	matcher := engine.MatchesAllowPrivateName
	require.NotNil(t, matcher)
	assert.True(t, matcher("ai.iasylum.net"), "the resolver-trimmed form must match")
	assert.False(t, matcher("ai.iasylum.net.evil.example"), "no suffix confusion")
}
