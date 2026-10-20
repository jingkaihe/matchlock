//go:build acceptance

package acceptance

import (
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/sdk"
	"github.com/jingkaihe/matchlock/pkg/state"
	"github.com/stretchr/testify/require"
)

// TestSDKAllowPrivateNameEntryCoversResolvedLiteral is the live-VM acceptance
// test for FORK-NET-FIX-0923 defect 1: an allow_private NAME entry must lift the
// private-IP block for the LITERAL destination the passthrough proxy observes,
// not only for a destination that is the name itself.
//
// The coordinator's reproduction on the fork tip (host with a Yggdrasil route,
// image igorhvr/bedlam-ubuntu, `--allow-host "*"`):
//
//	--allow-private 192.168.107.74:8888                    -> 200 (literal, v4)
//	--allow-private "[219:c447:...:8ca9]:8888"             -> 200 (literal, v6)
//	--allow-private 200::/7                                -> 200 (CIDR)
//	--allow-private ai.iasylum.net:8888                    -> by name "Empty reply
//	   from server", by literal "Connection reset by peer"
//
// The passthrough path (every port except the HTTP-proxied 80/443) calls
// policy.IsHostAllowedPort with the pre-DNAT destination IP, so a NAME entry used
// to be unmatchable there; only the literal and CIDR entry forms worked. The fix
// resolves a NAME entry host-side (TTL-cached) so the entry covers the addresses
// it resolves to, which this test proves end to end in a real guest.
//
// Portability: the fixture name must resolve on BOTH sides without operator DNS.
// The guest side comes from network.add_hosts (AddHost -> /etc/hosts in the guest
// via the matchlock.add_host kernel arg); the host side needs no DNS at all
// because the same add_hosts mapping is authoritative for NAME-entry resolution
// in the policy engine (it is consulted before any lookup).
//
// The sandbox carries NO allowlist entry (no AllowHost): an empty allowlist keeps
// the allowlist check out of the way, exactly like the coordinator's
// `--allow-host "*"`, so what the assertions observe is the private-block
// decision. Swap stays disabled (SwapMB unset), and each sandbox is removed by id
// with `matchlock list` asserted to have no row for it afterwards.
func TestSDKAllowPrivateNameEntryCoversResolvedLiteral(t *testing.T) {
	if runtime.GOOS != "linux" {
		// The enforcement point under test is the Linux interception path
		// (nftables DNAT + passthrough proxy + SO_ORIGINAL_DST). The darwin
		// backend intercepts through the gVisor userspace stack, so the same
		// guard as the IPv4/IPv6 parity siblings applies.
		t.Skip("IPv6 interception (and therefore the dual-stack proxy) is a Linux-backend feature")
	}

	hostIP := discoverHostPrivateIPv4(t)
	if hostIP == "" {
		t.Skip("no non-loopback RFC1918 IPv4 assigned to the host")
	}

	// The fixture name: not resolvable by any public DNS, resolvable here only
	// through the test's own add_hosts mapping. The proxy sees the literal, which
	// is what the defect case probes.
	const fixtureName = "matchlock-priv-name.test"
	const (
		payloadByName    = "matchlock-name-entry-by-name"
		payloadByLiteral = "matchlock-name-entry-by-literal"
		payloadControl   = "matchlock-name-entry-refused"
		payloadHostDial  = "matchlock-name-entry-host-control"
	)

	// The fixture port is known BEFORE the sandbox is created, because the
	// allow_private entry is fixed at creation time.
	port, hits := startEchoFixture(t, hostIP)
	t.Logf("fixture: name=%s bind=%s:%d (RFC1918 host address)", fixtureName, hostIP, port)

	// --- Case (d): host-side control dial, so "never dialed" below means the
	// proxy refused rather than that nothing was listening. ---
	hostBaseline := hits.Load()
	dialEchoControl(t, hostIP, port, payloadHostDial)
	require.Equal(t, hostBaseline+1, hits.Load(),
		"the host-side control dial must reach the fixture")
	t.Logf("case (d) host-side control dial to %s:%d ok, fixture hits=%d",
		hostIP, port, hits.Load())

	// --- Sandbox 1: private block on, the fixture NAME exempted by NAME entry. ---
	allowEntry := fixtureName + ":" + strconv.Itoa(port)
	nameClient := launchAlpineWithNetwork(t, sdk.New("alpine:latest").
		WithBlockPrivateIPs(true).
		WithAllowPrivate(allowEntry).
		AddHost(fixtureName, hostIP). // guest /etc/hosts AND authoritative for the host-side name-entry resolution
		WithNetworkInterception().    // block_private_ips is enforced only with the proxy active
		WithCPUs(0.5))
	t.Logf("sandbox 1 %s launched with allow_private=%q add_host=%s->%s",
		nameClient.VMID(), allowEntry, fixtureName, hostIP)

	// --- Case (a): the guest resolves the name through its own /etc/hosts and
	// the NAME entry must lift the block for the resulting private literal. ---
	baseline := hits.Load()
	outByName := runEchoProbe(t, nameClient, fixtureName, port, payloadByName)
	t.Logf("case (a) probe BY NAME to %s:%d %s fixtureHits=%d out=%q",
		fixtureName, port, probeRC(outByName), hits.Load(), outByName)
	require.Contains(t, outByName, payloadByName,
		"an allow_private NAME entry must let the guest reach the fixture by name")
	require.Greater(t, hits.Load(), baseline,
		"the by-name probe must have been dialed through the proxy")

	// --- Case (b): the DEFECT case. The passthrough proxy only ever sees the
	// literal destination, which is exactly what this probe targets. ---
	baseline = hits.Load()
	outByLiteral := runEchoProbe(t, nameClient, hostIP, port, payloadByLiteral)
	t.Logf("case (b) probe BY LITERAL to %s:%d %s fixtureHits=%d out=%q",
		hostIP, port, probeRC(outByLiteral), hits.Load(), outByLiteral)
	require.Contains(t, outByLiteral, payloadByLiteral,
		"an allow_private NAME entry must cover the literal destination the passthrough proxy sees")
	require.Greater(t, hits.Load(), baseline,
		"the by-literal probe must have been dialed through the proxy")

	// --- Case (c): the same fixture and the same add_hosts mapping, but no
	// allow_private entry: the private block must stay in force. ---
	controlClient := launchAlpineWithNetwork(t, sdk.New("alpine:latest").
		WithBlockPrivateIPs(true).
		AddHost(fixtureName, hostIP).
		WithNetworkInterception().
		WithCPUs(0.5))
	t.Logf("sandbox 2 (control) %s launched with no allow_private entry", controlClient.VMID())

	baseline = hits.Load()
	refusedByLiteral := runEchoProbe(t, controlClient, hostIP, port, payloadControl)
	t.Logf("case (c) control probe BY LITERAL to %s:%d %s fixtureHits=%d out=%q",
		hostIP, port, probeRC(refusedByLiteral), hits.Load(), refusedByLiteral)
	require.NotContains(t, refusedByLiteral, payloadControl,
		"without an allow_private entry the same private fixture must be refused")
	require.Equal(t, baseline, hits.Load(),
		"the refused destination must never reach the fixture")

	// The name probe in the control sandbox resolves through the same add_hosts
	// mapping to the same private literal, so it must be refused as well: the
	// mapping alone does not lift the block.
	refusedByName := runEchoProbe(t, controlClient, fixtureName, port, payloadControl)
	t.Logf("case (c) control probe BY NAME to %s:%d %s fixtureHits=%d out=%q",
		fixtureName, port, probeRC(refusedByName), hits.Load(), refusedByName)
	require.NotContains(t, refusedByName, payloadControl,
		"the add_hosts mapping alone must not lift the private block for the name")
	require.Equal(t, baseline, hits.Load(),
		"the refused name destination must never reach the fixture")

	// Criterion: both sandboxes are removed by id and `matchlock list` shows no
	// row for either of them afterwards.
	removeSandboxAndAssertGone(t, nameClient)
	removeSandboxAndAssertGone(t, controlClient)
}

// probeRC extracts the "RC=<code>" line the probe shell writes next to the
// payload, so a failure log carries the guest-side exit status.
func probeRC(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "RC=") {
			return strings.TrimSpace(line)
		}
	}
	return "RC=?"
}

// removeSandboxAndAssertGone stops a sandbox and removes its state BY ID, then
// proves the id is gone from the state store and from `matchlock list`.
//
// `matchlock rm` is attempted first; under a restricted harness it cannot
// reconcile the nftables ruleset ("netlink receive: operation not permitted") and
// leaves a stopped row behind, so state.Manager.Remove is the documented bypass
// (the same one tests/acceptance/swap_test.go uses).
func removeSandboxAndAssertGone(t *testing.T, client *sdk.Client) {
	t.Helper()

	id := client.VMID()
	require.NotEmpty(t, id, "the sandbox id must be known to remove it by id")

	require.NoError(t, client.Close(0), "close sandbox %s", id)
	if rmErr := client.Remove(); rmErr != nil {
		t.Logf("removal: `matchlock rm %s` failed (%v); falling back to state.Manager.Remove", id, rmErr)
	} else {
		t.Logf("removal: `matchlock rm %s` succeeded", id)
	}

	stateMgr := state.NewManager()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := stateMgr.Get(id); err != nil {
			break
		}
		if time.Now().After(deadline) {
			require.NoError(t, stateMgr.Remove(id), "remove the state of sandbox %s", id)
			break
		}
		_ = stateMgr.Remove(id)
		time.Sleep(250 * time.Millisecond)
	}

	_, getErr := stateMgr.Get(id)
	require.Error(t, getErr, "sandbox %s must have no state row after removal", id)

	out, stderr, code := runCLI(t, "list")
	t.Logf("removal: `matchlock list` exit=%d stdout=%q stderr=%q", code, out, stderr)
	require.Equal(t, 0, code, "matchlock list must succeed after removing %s", id)
	require.NotContains(t, out, id, "matchlock list must show no row for the removed sandbox %s", id)
	t.Logf("removal: sandbox %s is gone from the state store and from `matchlock list`", id)
}
