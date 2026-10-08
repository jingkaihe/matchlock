//go:build acceptance

package acceptance

import (
	"net"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/sdk"
	"github.com/stretchr/testify/require"
)

// TestSDKIPv4InterceptionParityWithDualStackProxy is the US-009 IPv4 regression
// gate: an intercepted sandbox is now dual-stack, so this test proves that the
// IPv4 enforcement in the SAME sandbox is byte-for-byte the behaviour it had
// before the IPv6 path existed.
//
// It answers four questions in one live dual-stack sandbox:
//
//	A. IPv4 ALLOWED: an allow_private-exempt private IPv4 host:port still reaches
//	   the host fixture through the passthrough proxy, and the fixture is dialed.
//	   This is the case a v6 listener or an altered original-destination lookup
//	   would break: the proxy binds the same HTTP/HTTPS/passthrough ports on the
//	   IPv6 gateway, and resolves the pre-DNAT IPv4 destination with
//	   SO_ORIGINAL_DST.
//	B. IPv4 REFUSED: an unlisted private IPv4 destination (the per-VM TAP
//	   gateway on an unexempted port) is still refused and never reaches the
//	   fixture, with a host-side control dial proving that fixture answers.
//	C. ONE LINK, TWO FAMILIES: the guest's IPv4 and IPv6 default gateways are
//	   both configured and both live on the SAME host TAP, i.e. the v6 link did
//	   not replace or shadow the IPv4 addressing.
//	D. BOTH RULE TABLES: the host ruleset carries the IPv4 interception table
//	   matchlock_<tap> AND the IPv6 one matchlock6_<tap> for that TAP, so the new
//	   ip6 table is additive and the IPv4 DNAT rules are still installed.
//
// Swap stays disabled (SwapMB unset) and the sandbox is removed by id through
// the launch helper's cleanup.
func TestSDKIPv4InterceptionParityWithDualStackProxy(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("IPv6 interception (and therefore the dual-stack proxy) is a Linux-backend feature")
	}

	hostIP := discoverHostPrivateIPv4(t)
	if hostIP == "" {
		t.Skip("no non-loopback RFC1918 IPv4 assigned to the host")
	}
	t.Logf("host private IPv4 fixture address: %s", hostIP)

	const payloadAllowed = "matchlock-ipv4-parity-fixture-A"
	const payloadBlocked = "matchlock-ipv4-parity-fixture-B"

	portA, hitsA := startEchoFixture(t, hostIP)
	portB, hitsB := startEchoFixture(t, "0.0.0.0")
	require.NotEqual(t, portA, portB, "the two fixtures must use distinct ports")

	client := launchAlpineWithNetwork(t, sdk.New("alpine:latest").
		WithBlockPrivateIPs(true).
		WithAllowPrivate(hostIP+":"+strconv.Itoa(portA)).
		WithNetworkInterception(). // block_private_ips is enforced only with the proxy active
		WithCPUs(0.5))
	t.Logf("sandbox %s launched", client.VMID())

	// --- Case C: both default gateways exist, and they share one TAP. ---
	gatewayIPv4 := guestDefaultGateway(t, client)
	gatewayIPv6 := guestIPv6Gateway(t, client)
	t.Logf("guest default gateways: IPv4=%s IPv6=%s", gatewayIPv4, gatewayIPv6)

	require.NotNil(t, net.ParseIP(gatewayIPv4).To4(), "guest IPv4 gateway %q must be an IPv4 literal", gatewayIPv4)
	requireIPv6Literal(t, gatewayIPv6, "guest IPv6 default gateway")

	tap6 := hostTAPForAddr(t, gatewayIPv6)
	tap4 := hostTAPForAddr(t, gatewayIPv4)
	t.Logf("host TAP holding the guest IPv6 gateway: %s; holding the IPv4 gateway: %s", tap6, tap4)
	require.Equal(t, tap4, tap6,
		"the per-VM IPv4 and IPv6 gateways must live on the same host TAP (v6 must not shadow v4)")

	// --- Case D: both families' interception tables exist for that TAP. ---
	tables := nftTableList(t)
	t.Logf("host nftables tables (matchlock*): %s",
		strings.Join(matchlockTableLines(tables), " | "))
	require.Contains(t, tables, "matchlock_"+tap4,
		"the IPv4 interception table must still be installed for TAP %s", tap4)
	require.Contains(t, tables, "matchlock6_"+tap4,
		"the IPv6 interception table must be installed alongside it for TAP %s", tap4)

	// --- Case A: IPv4 through the proxy is unchanged. ---
	allowedOut := runEchoProbe(t, client, hostIP, portA, payloadAllowed)
	t.Logf("case A IPv4 allowed probe to %s:%d fixtureHits=%d out=%q", hostIP, portA, hitsA.Load(), allowedOut)
	require.Contains(t, allowedOut, payloadAllowed,
		"allow_private host:port must still let the guest reach fixture A over IPv4")
	require.Greater(t, hitsA.Load(), int64(0), "fixture A must have been dialed over IPv4")

	// --- Case B: IPv4 refusal is unchanged, with a positive control. ---
	// The wildcard-bound fixture B answers on the TAP gateway address too, which
	// is what makes the host-side control dial below a real proof that
	// "no payload" means "the proxy refused" rather than "nothing was listening".
	dialEchoControl(t, gatewayIPv4, portB, payloadBlocked)
	blockedBaseline := hitsB.Load()

	blockedOut := runEchoProbe(t, client, gatewayIPv4, portB, payloadBlocked)
	t.Logf("case B IPv4 refused probe to %s:%d fixtureHits=%d out=%q", gatewayIPv4, portB, hitsB.Load(), blockedOut)
	require.NotContains(t, blockedOut, payloadBlocked,
		"an unlisted private IPv4 gateway:port must still be refused")
	require.Equal(t, blockedBaseline, hitsB.Load(),
		"a refused IPv4 destination must never reach the fixture")
}

// hostTAPForAddr returns the name of the host VM TAP (fc-*/qm-*) that holds addr.
// It fails rather than guessing when no TAP holds the address, so the parity
// assertions cannot silently run against the wrong interface.
func hostTAPForAddr(t *testing.T, addr string) string {
	t.Helper()

	want := net.ParseIP(addr)
	require.NotNil(t, want, "expected an IP literal, got %q", addr)

	ifaces, err := net.Interfaces()
	require.NoError(t, err, "list host interfaces")

	var holders []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ip := interfaceAddrIP(a); ip != nil && ip.Equal(want) {
				holders = append(holders, iface.Name)
				break
			}
		}
	}
	t.Logf("host interfaces holding %s: %v", addr, holders)

	for _, name := range holders {
		if strings.HasPrefix(name, "fc-") || strings.HasPrefix(name, "qm-") {
			return name
		}
	}
	require.FailNowf(t, "no VM TAP holds the address",
		"expected an fc-*/qm-* interface to hold %s, holders=%v", addr, holders)
	return ""
}

// nftTableList returns the host ruleset's table list. matchlock installs its
// interception tables with nft, so nft must be present wherever a sandbox runs.
func nftTableList(t *testing.T) string {
	t.Helper()

	bin, err := exec.LookPath("nft")
	require.NoError(t, err, "nft must be installed to inspect the host interception ruleset")
	out, err := exec.Command(bin, "list", "tables").CombinedOutput()
	require.NoError(t, err, "nft list tables: %s", out)
	return string(out)
}

// matchlockTableLines filters the nft table listing down to the interception
// tables, so the log line stays readable (other runs' TAP tables are expected).
func matchlockTableLines(tables string) []string {
	var lines []string
	for _, line := range strings.Split(tables, "\n") {
		if strings.Contains(line, "matchlock") {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	return lines
}
