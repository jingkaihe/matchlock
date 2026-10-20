package policy

import (
	"testing"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/stretchr/testify/assert"
)

// These tests pin the exported allow_private NAME matcher the DNS forwarder's
// ipv4-preferring rewrite consumes as its fail-closed carve-out: normalized
// names, glob entries, ignored port scopes and the exclusion of address
// entries.
func TestMatchesAllowPrivateName(t *testing.T) {
	t.Run("no entries matches nothing", func(t *testing.T) {
		e := NewEngine(&api.NetworkConfig{})
		assert.False(t, e.MatchesAllowPrivateName("anything.example"))
	})

	t.Run("exact name is normalized", func(t *testing.T) {
		e := NewEngine(&api.NetworkConfig{AllowPrivate: []string{"Private.Example"}})
		assert.True(t, e.MatchesAllowPrivateName("private.example"))
		assert.True(t, e.MatchesAllowPrivateName("PRIVATE.EXAMPLE."), "a trailing root dot must normalize away")
		assert.False(t, e.MatchesAllowPrivateName("other.example"))
		assert.False(t, e.MatchesAllowPrivateName("sub.private.example"))
	})

	t.Run("wildcard entries", func(t *testing.T) {
		e := NewEngine(&api.NetworkConfig{AllowPrivate: []string{"*.example.com", "exact.example.org"}})
		assert.True(t, e.MatchesAllowPrivateName("api.example.com"))
		assert.True(t, e.MatchesAllowPrivateName("a.b.example.com"))
		assert.False(t, e.MatchesAllowPrivateName("badexample.com"), "a suffix match must respect the dot boundary")
		assert.False(t, e.MatchesAllowPrivateName("example.com"), "the bare domain is not matched by a subdomain glob")
		assert.True(t, e.MatchesAllowPrivateName("exact.example.org"))
	})

	t.Run("port scope is ignored", func(t *testing.T) {
		e := NewEngine(&api.NetworkConfig{AllowPrivate: []string{"private.example:443", "[bracket.example]:8443"}})
		assert.True(t, e.MatchesAllowPrivateName("private.example"),
			"a DNS query carries no port, so the entry's port scope must not gate the name match")
		assert.True(t, e.MatchesAllowPrivateName("bracket.example"))
		assert.False(t, e.MatchesAllowPrivateName("private.example.org"))
	})

	t.Run("ascii-only case folding (RFC 4343)", func(t *testing.T) {
		e := NewEngine(&api.NetworkConfig{AllowPrivate: []string{"k.example"}})
		assert.True(t, e.MatchesAllowPrivateName("K.EXAMPLE."), "ASCII case folding must keep matching")
		assert.False(t, e.MatchesAllowPrivateName("\u212A.example"),
			"a byte-distinct label (U+212A) must not fold onto the ASCII entry: the matcher shares the rewrite funnel's ASCII-only identity")
	})

	t.Run("ip literals and cidrs never match names", func(t *testing.T) {
		e := NewEngine(&api.NetworkConfig{AllowPrivate: []string{"192.168.1.1", "10.0.0.0/8", "fd00::1"}})
		assert.False(t, e.MatchesAllowPrivateName("192.168.1.1"))
		assert.False(t, e.MatchesAllowPrivateName("10.0.0.1"))
		assert.False(t, e.MatchesAllowPrivateName("fd00::1"))
	})

	t.Run("global wildcard and empty host", func(t *testing.T) {
		all := NewEngine(&api.NetworkConfig{AllowPrivate: []string{"*"}})
		assert.True(t, all.MatchesAllowPrivateName("any.example"), "a bare * entry matches every name")

		e := NewEngine(&api.NetworkConfig{AllowPrivate: []string{"private.example"}})
		assert.False(t, e.MatchesAllowPrivateName(""))
		assert.False(t, e.MatchesAllowPrivateName("   "))
	})
}
