package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jingkaihe/matchlock/pkg/api"
)

// These tests pin the DNS resolution priority flag surface of `matchlock run`
// (the network.dns_priority wire field). They carry no build tag and no cgo
// dependency, so they run in the ordinary unit suite on every platform.
//
// Function/file names carry a dv6 suffix deliberately: the icebox-dns lane
// (icebox/dns-rewrite) pins the same behavior in cmd_run_test.go, and a unique
// name here keeps both files coexistable if the merge keeps both.

// TestDV6BuildRunNetworkConfigDNSResolutionPriority pins the flag resolution:
// neither flag yields the explicit ipv4 default; --prioritize-ipv4-dns-
// resolution yields ipv4; --prioritize-ipv6-dns-resolution yields ipv6; and
// when BOTH are supplied ipv6 wins (the non-default flag wins, mirroring the
// --matchlock-no-swap/--matchlock-swap precedent), so passing both is never an
// error and always resolves to ipv6.
func TestDV6BuildRunNetworkConfigDNSResolutionPriority(t *testing.T) {
	build := func(prioritizeIPv4, prioritizeIPv6 bool) *api.NetworkConfig {
		return buildRunNetworkConfig(nil, nil, nil, false, false, nil, nil, "", 1500, prioritizeIPv4, prioritizeIPv6)
	}

	assert.Equal(t, api.DNSPriorityIPv4, build(false, false).DNSPriority,
		"no flag must carry the explicit ipv4 default")
	assert.Equal(t, api.DNSPriorityIPv4, build(true, false).DNSPriority,
		"--prioritize-ipv4-dns-resolution must select ipv4")
	assert.Equal(t, api.DNSPriorityIPv6, build(false, true).DNSPriority,
		"--prioritize-ipv6-dns-resolution must select ipv6")
	assert.Equal(t, api.DNSPriorityIPv6, build(true, true).DNSPriority,
		"both flags must resolve to ipv6 (the non-default flag wins)")
}

// TestDV6RunDNSResolutionPriorityFlagsRegistered pins that both flags are
// registered on the run command and visible in its usage output.
func TestDV6RunDNSResolutionPriorityFlagsRegistered(t *testing.T) {
	for _, name := range []string{"prioritize-ipv4-dns-resolution", "prioritize-ipv6-dns-resolution"} {
		require.NotNil(t, runCmd.Flags().Lookup(name), "--%s must be registered on the run command", name)
	}
	usage := runCmd.UsageString()
	assert.Contains(t, usage, "--prioritize-ipv4-dns-resolution")
	assert.Contains(t, usage, "--prioritize-ipv6-dns-resolution")
}
