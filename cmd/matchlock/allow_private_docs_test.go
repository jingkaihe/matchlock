package main

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNetPolicyDocsDocumentPrivateRanges locks the operator-facing private
// range list in README.md. Even though the list is enforced in
// pkg/policy/engine.go, an operator configures exceptions from the docs, so the
// documented ranges (RFC 1918, loopback, link-local, CGNAT/Tailscale,
// Yggdrasil, the IPv6 private blocks and the IPv4-mapped handling) must stay in
// sync with the code.
func TestNetPolicyDocsDocumentPrivateRanges(t *testing.T) {
	readme, err := os.ReadFile("../../README.md")
	require.NoError(t, err, "read README.md relative to cmd/matchlock")
	lowerReadme := strings.ToLower(string(readme))

	readmePhrases := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"127.0.0.0/8",
		"169.254.0.0/16",
		"100.64.0.0/10",
		"::1/128",
		"fc00::/7",
		"fe80::/10",
		"200::/7",
		"::ffff:0:0/96",
		"::ffff:192.168.1.1",
		"::ffff:8.8.8.8",
	}
	for _, phrase := range readmePhrases {
		assert.Contains(t, lowerReadme, phrase, "README.md must document %q", phrase)
	}
}

// TestNetPolicyDocsDocumentAllowPrivate locks the allow_private documentation:
// README.md must show both the CLI flag and the Go SDK builder, and
// docs/network-interception.md must spell out the entry syntax, the
// DNS-rebinding guard, that allowed_hosts is unchanged, and that NoNetwork
// still wins.
func TestNetPolicyDocsDocumentAllowPrivate(t *testing.T) {
	readme, err := os.ReadFile("../../README.md")
	require.NoError(t, err, "read README.md relative to cmd/matchlock")
	lowerReadme := strings.ToLower(string(readme))

	readmePhrases := []string{
		"block_private_ips",
		"--allow-private",
		".withallowprivate(",
		"rebinding",
	}
	for _, phrase := range readmePhrases {
		assert.Contains(t, lowerReadme, phrase, "README.md must document %q", phrase)
	}

	docs, err := os.ReadFile("../../docs/network-interception.md")
	require.NoError(t, err, "read docs/network-interception.md relative to cmd/matchlock")
	lowerDocs := strings.ToLower(string(docs))

	docsPhrases := []string{
		"allow_private",
		"[v6]:port",
		"any port",
		"rebinding",
		"allowed_hosts",
		"no_network",
		".withallowprivate(",
	}
	for _, phrase := range docsPhrases {
		assert.Contains(t, lowerDocs, phrase, "docs/network-interception.md must document %q", phrase)
	}
}

// TestNetPolicyDocsDocumentAllowPrivateNameResolution locks the documented
// semantics of an allow_private NAME entry: it covers the address set the name
// resolves to (host-side resolution, 60 s TTL refresh), an unresolvable entry
// never matches, the port scope applies to every entry form, add_hosts is the
// authoritative static mapping, and the DNS-rebinding guard keeps applying to a
// name *destination*. The implementation is
// pkg/policy/allow_private_name.go + Engine.allowPrivateAddress; an operator
// configures an exception from these two documents, so a change to the rule
// must not ship without the matching sentence.
func TestNetPolicyDocsDocumentAllowPrivateNameResolution(t *testing.T) {
	readme, err := os.ReadFile("../../README.md")
	require.NoError(t, err, "read README.md relative to cmd/matchlock")
	lowerReadme := strings.ToLower(string(readme))

	readmePhrases := []string{
		"resolves to",
		"refreshed with a 60 s ttl",
		"does not resolve",
		"add_hosts",
		".addhost(",
		"rebinding",
		"verified address is the one that",
	}
	for _, phrase := range readmePhrases {
		assert.Contains(t, lowerReadme, phrase, "README.md must document %q", phrase)
	}

	docs, err := os.ReadFile("../../docs/network-interception.md")
	require.NoError(t, err, "read docs/network-interception.md relative to cmd/matchlock")
	lowerDocs := strings.ToLower(string(docs))

	docsPhrases := []string{
		"address set the name resolves to",
		"refreshed with a 60 s ttl",
		"does not resolve",
		"add_hosts",
		"port scope applies to every entry form",
		"gets dialed",
		"unresolvable",
	}
	for _, phrase := range docsPhrases {
		assert.Contains(t, lowerDocs, phrase, "docs/network-interception.md must document %q", phrase)
	}
}
