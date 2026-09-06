package policy

import (
	"testing"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIsPrivateIP_ChecksAllResolvedAddresses qualifies that isPrivateIP checks
// EVERY address returned for a hostname, not just the first. A mixed
// public/private answer set (public listed first) must still be treated as
// private; otherwise a private address can be camouflaged behind a leading
// public address.
func TestIsPrivateIP_ChecksAllResolvedAddresses(t *testing.T) {
	dns := newPolicySyntheticDNS(t, map[string][][]string{
		"mixed.test.": {
			{"203.0.113.99", "127.0.0.1"}, // public first, private second
		},
	})
	dns.install()
	defer dns.uninstall()

	assert.True(t, isPrivateIP("mixed.test"),
		"hostname resolving to any private address must be considered private")
}

// TestIsPrivateIP_PublicHostnameNotPrivate is the positive control: a hostname
// resolving only to public addresses is not private.
func TestIsPrivateIP_PublicHostnameNotPrivate(t *testing.T) {
	dns := newPolicySyntheticDNS(t, map[string][][]string{
		"public.test.": {
			{"203.0.113.99", "198.51.100.7"},
		},
	})
	dns.install()
	defer dns.uninstall()

	assert.False(t, isPrivateIP("public.test"))
}

// TestAllowedHostIPs_MixedAnswerDenied verifies AllowedHostIPs rejects a
// hostname whose answer set contains any private address.
func TestAllowedHostIPs_MixedAnswerDenied(t *testing.T) {
	dns := newPolicySyntheticDNS(t, map[string][][]string{
		"mixed.test.": {
			{"203.0.113.99", "127.0.0.1"},
		},
	})
	dns.install()
	defer dns.uninstall()

	engine := NewEngine(&api.NetworkConfig{BlockPrivateIPs: true})
	ips, ok := engine.AllowedHostIPs("mixed.test")
	assert.False(t, ok, "mixed public/private answer set must be denied")
	assert.Nil(t, ips)
}

// TestAllowedHostIPs_PublicReturnsVerifiedSet verifies a public hostname returns
// its verified address set so the caller can bind the dial to it.
func TestAllowedHostIPs_PublicReturnsVerifiedSet(t *testing.T) {
	dns := newPolicySyntheticDNS(t, map[string][][]string{
		"public.test.": {
			{"203.0.113.99"},
		},
	})
	dns.install()
	defer dns.uninstall()

	engine := NewEngine(&api.NetworkConfig{BlockPrivateIPs: true})
	ips, ok := engine.AllowedHostIPs("public.test")
	require.True(t, ok)
	require.Len(t, ips, 1)
	assert.Equal(t, "203.0.113.99", ips[0].String())
}

// TestAllowedHostIPs_ResolvesOnce verifies a single AllowedHostIPs call performs
// exactly one resolution (no second unverified resolution), which is the basis of
// the dial-target binding. On a rebinding name the returned set is the first
// (verified) stage, not a later private one.
func TestAllowedHostIPs_ResolvesOnce(t *testing.T) {
	dns := newPolicySyntheticDNS(t, map[string][][]string{
		"rebind.test.": {
			{"203.0.113.99"}, // first resolution (check)
			{"127.0.0.1"},    // a later resolution (would-be dial in a TOCTOU)
		},
	})
	dns.install()
	defer dns.uninstall()

	engine := NewEngine(&api.NetworkConfig{BlockPrivateIPs: true})

	ips, ok := engine.AllowedHostIPs("rebind.test")
	require.True(t, ok)
	require.Len(t, ips, 1)
	assert.Equal(t, "203.0.113.99", ips[0].String(), "must return the verified (public) address")
	assert.Equal(t, 1, dns.aCount("rebind.test."),
		"AllowedHostIPs must resolve the hostname exactly once")
}

// TestAllowedHostIPs_FullCycleWithResolverOverhead is a sanity check that a
// repeated call re-resolves and would then observe the private stage — which
// the dial binding must never do, because the handler dials the returned IP
// literal rather than re-resolving.
func TestAllowedHostIPs_DetectsPrivateOnSeparateCall(t *testing.T) {
	dns := newPolicySyntheticDNS(t, map[string][][]string{
		"rebind.test.": {
			{"203.0.113.99"},
			{"127.0.0.1"},
		},
	})
	dns.install()
	defer dns.uninstall()

	engine := NewEngine(&api.NetworkConfig{BlockPrivateIPs: true})

	ips, ok := engine.AllowedHostIPs("rebind.test")
	require.True(t, ok)
	assert.Equal(t, "203.0.113.99", ips[0].String())

	// Second call re-resolves and now sees the private stage → denied.
	ips2, ok := engine.AllowedHostIPs("rebind.test")
	assert.False(t, ok, "a later resolution that becomes private must be denied")
	assert.Nil(t, ips2)
}
