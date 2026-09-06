package policy

import (
	"testing"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/stretchr/testify/require"
)

// TestEngineIsHostAllowedNoBypass verifies the allow-list cannot be bypassed by
// reaching a non-allowlisted destination via its raw IP address. This is the
// deterministic unit-level counterpart of the QEMU guest no-bypass acceptance
// test. With httpbin.org (hostname) allowlisted:
//
//   - The allowlisted hostname is allowed.
//   - A non-allowlisted HOSTNAME is denied.
//   - A non-allowlisted destination addressed by its RAW IP is denied (this is
//     the direct-IP bypass the catch-all DNAT -> proxy path is meant to prevent).
//   - A non-allowlisted host on a specific port is still denied (the engine
//     strips the port, but a non-allowlisted name never matches).
//   - An allowlisted hostname on an ALTERNATE port is still allowed by the engine
//     (port-stripping), but the proxy's passthrough path checks the bare dstIP,
//     so a real alternate-port TCP connection to an allowlisted hostname is only
//     reachable with the raw IP also allowlisted — a conservative, safe default.
func TestEngineIsHostAllowedNoBypass(t *testing.T) {
	eng := NewEngine(&api.NetworkConfig{
		AllowedHosts: []string{"httpbin.org"},
	})

	require.True(t, eng.IsHostAllowed("httpbin.org"), "allowlisted hostname must be allowed")
	require.True(t, eng.IsHostAllowed("httpbin.org:8080"), "allowed host with port must be allowed")

	require.False(t, eng.IsHostAllowed("example.com"), "non-allowlisted hostname must be denied")
	require.False(t, eng.IsHostAllowed("example.com:8080"), "non-allowlisted host with port must be denied")

	// Raw-IP destination (e.g. the IPv4 for an unallowlisted host) must be denied:
	// the proxy's passthrough passes the destination IP to IsHostAllowed, and an
	// IP is never a match for a hostname-only allowlist.
	require.False(t, eng.IsHostAllowed("185.199.108.153"), "raw-IP destination must be denied")
	require.False(t, eng.IsHostAllowed("185.199.108.153:80"), "raw-IP with port must be denied")
	require.False(t, eng.IsHostAllowed("1.1.1.1"), "any raw IP must be denied for a hostname allowlist")

	// Alternate-port passthrough semantics: an allowlisted hostname on a
	// non-80/443 port is still engine-allowed after port-stripping, but the
	// passthrough proxy authenticates by bare dstIP, so this is NOT a bypass of
	// the allowlist (the IP is not allowlisted). Document the conservative
	// behavior to lock out any regression that would flip it to a bypass.
	require.False(t, eng.IsHostAllowed("185.199.108.153:8080"), "raw-IP with alternate port must be denied")
}

// TestEngineIsHostAllowedEmptyAllowlist verifies that with NO allowlist, all hosts
// are permitted (the default permit-all mode) so the above no-bypass assertions
// are specifically about the allowlist being present.
func TestEngineIsHostAllowedEmptyAllowlist(t *testing.T) {
	eng := NewEngine(&api.NetworkConfig{})
	require.True(t, eng.IsHostAllowed("anything.example"))
	require.True(t, eng.IsHostAllowed("185.199.108.153"))
}
