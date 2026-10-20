//go:build acceptance

package acceptance

import (
	"context"
	"fmt"
	"io"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/sdk"
	"github.com/stretchr/testify/require"
)

// TestSDKIPv6InterceptionAllowedRefusedAndNoLeak is the real-VM acceptance test
// for the IPv6 interception path (US-008). It runs one intercepted sandbox and
// answers four questions against it:
//
//	A. ALLOWED: does an allow_private-exempt IPv6 destination reach the host
//	   fixture through the proxy (payload echoed back)?
//	B. REFUSED: are unlisted private IPv6 literals (the TAP's own fc00::/7
//	   gateway on an unexempted port, a bare fc00::/7 literal and the host's
//	   200::/7 Yggdrasil address) refused without ever dialing the fixture?
//	C. NO LEAK: is a guest IPv6 packet that the ip6 table does not redirect
//	   (UDP to a non-53 port toward a non-gateway address) dropped instead of
//	   delivered around the proxy?
//	D. PUBLIC: a separate test covers a public IPv6 destination.
//
// The exemption is port-scoped on the whole ULA space
// ("[fd00::/8]:<allowedPort>") because the per-VM gateway address (fd00:<octet>::1)
// is only known after the VM is created, while the destination port is known
// before it (the fixtures reserve their ports up front). That keeps every
// refusal assertion a REAL positive control: the very same fixture answers on
// the same port when it is exempted, so "no payload" means "the proxy refused",
// not "nothing was listening".
//
// Swap stays disabled (SwapMB unset) and both VMs are removed by id through the
// launch helper's cleanup.
func TestSDKIPv6InterceptionAllowedRefusedAndNoLeak(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("IPv6 interception (and therefore the dual-stack proxy) is a Linux-backend feature")
	}

	const (
		payloadAllowed = "matchlock-ipv6-fixture-A"
		payloadBlocked = "matchlock-ipv6-fixture-B"
		payloadOverlay = "matchlock-ipv6-fixture-YGG"
	)

	// Fixtures first: their ports are needed for the allow_private entry that is
	// fixed at sandbox creation.
	allowedPort, allowedHits := startIPv6EchoFixture(t)
	blockedPort, blockedHits := startIPv6EchoFixture(t)
	udpPort, udpHits := startUDPEchoFixture6(t)
	require.NotEqual(t, allowedPort, blockedPort, "fixtures must use distinct ports")

	// The Yggdrasil overlay address (200::/7) is a real host address: binding a
	// fixture there makes "the refused destination never reached the fixture" an
	// assertion about a destination the host could actually have answered on.
	// Without such an address the fixture stays on the wildcard and the 200::/7
	// probe targets a literal the host does not own.
	overlayAddr := hostOverlayIPv6(t)
	overlayBind := overlayAddr
	if overlayBind == "" {
		overlayBind = "::"
	}
	overlayPort, overlayHits := startIPv6EchoFixtureOn(t, overlayBind)

	allowEntry := "[fd00::/8]:" + strconv.Itoa(allowedPort)
	t.Logf("fixtures: allowed=%d blocked=%d udp=%d overlay=%s overlayBind=%s overlayPort=%d allow_private=%q",
		allowedPort, blockedPort, udpPort, overlayAddr, overlayBind, overlayPort, allowEntry)

	client := launchAlpineWithNetwork(t, sdk.New("alpine:latest").
		WithBlockPrivateIPs(true).
		WithAllowPrivate(allowEntry).
		WithNetworkInterception(). // block_private_ips is enforced only with the proxy active
		WithCPUs(0.5))
	t.Logf("sandbox %s launched", client.VMID())

	gateway6 := guestIPv6Gateway(t, client)
	t.Logf("guest IPv6 default gateway: %s", gateway6)
	requireIPv6Literal(t, gateway6, "guest IPv6 default gateway")

	// The gateway is a unique-local address, i.e. an fc00::/7 literal: the
	// refusal cases below are the fc00::/7 half of the acceptance criteria.
	require.True(t, ipv6InRange(gateway6, ipv6ULARange), "gateway %s must be inside fc00::/7", gateway6)

	// A freshly configured IPv6 address is TENTATIVE until duplicate address
	// detection finishes, and a tentative address cannot be used as a source, so
	// a guest connect issued during that window fails. Wait for the guest link to
	// become usable rather than probing a link that is still coming up.
	waitForGuestIPv6Usable(t, client)

	// The 200::/7 (Yggdrasil) destination: the host's own overlay address when it
	// has one, otherwise a literal the host does not own.
	overlayDest := overlayProbeDestination(overlayAddr)
	require.True(t, ipv6InRange(overlayDest, ipv6YggdrasilRange),
		"the 200::/7 probe destination %s must be inside 200::/7", overlayDest)
	if overlayAddr == "" {
		t.Logf("case B3: no 200::/7 address on the host; probing the literal %s only", overlayDest)
	}

	t.Logf("guest IPv6 inventory:\n%s", execGuest(t, client,
		"sh -c 'ip -6 addr show dev eth0; ip -6 route show default; busybox nc --help 2>&1 | head -3'"))

	// Host-side controls: prove the refusal fixtures are alive and would answer a
	// connection if one were made, so "the fixture was never dialed" below is
	// about the proxy's refusal rather than about a dead listener.
	dialEchoControl(t, gateway6, blockedPort, "control-blocked")
	if overlayAddr != "" {
		dialEchoControl(t, overlayAddr, overlayPort, "control-overlay")
	} else {
		t.Logf("skipping the overlay fixture control dial: the host owns no 200::/7 address to route to")
	}
	blockedBaseline := blockedHits.Load()
	overlayBaseline := overlayHits.Load()
	t.Logf("refusal fixture baselines after host-side controls: blocked=%d overlay=%d",
		blockedBaseline, overlayBaseline)

	// --- Case A: the exempted fixture must be reachable through the proxy. ---
	allowedBaseline := allowedHits.Load()
	outA, rcA := runIPv6TCPProbe(t, client, gateway6, allowedPort, payloadAllowed)
	t.Logf("case A allowed probe to [%s]:%d rc=%d fixtureHits=%d out=%q",
		gateway6, allowedPort, rcA, allowedHits.Load(), outA)
	require.Contains(t, outA, payloadAllowed,
		"allow_private-exempt IPv6 destination must echo the payload back through the proxy")
	require.Greater(t, allowedHits.Load(), allowedBaseline,
		"the exempted fixture must have been dialed by the proxy")

	// --- Case B1: the same fc00::/7 gateway literal, unexempted port. ---
	outB1, rcB1 := runIPv6TCPProbe(t, client, gateway6, blockedPort, payloadBlocked)
	t.Logf("case B1 refused probe to [%s]:%d rc=%d fixtureHits=%d out=%q",
		gateway6, blockedPort, rcB1, blockedHits.Load(), outB1)
	require.NotContains(t, outB1, payloadBlocked,
		"an unlisted private IPv6 destination must return no payload")
	require.Equal(t, blockedBaseline, blockedHits.Load(),
		"a refused IPv6 destination must never reach the fixture")

	// --- Case B2: a bare fc00::/7 literal the host owns no address for. ---
	const bareULALiteral = "fc00:1::1"
	require.True(t, ipv6InRange(bareULALiteral, ipv6ULARange), "%s must be inside fc00::/7", bareULALiteral)
	outB2, rcB2 := runIPv6TCPProbe(t, client, bareULALiteral, blockedPort, payloadBlocked)
	t.Logf("case B2 refused probe to [%s]:%d rc=%d fixtureHits=%d out=%q",
		bareULALiteral, blockedPort, rcB2, blockedHits.Load(), outB2)
	require.NotContains(t, outB2, payloadBlocked,
		"a bare unlisted fc00::/7 literal must return no payload")
	require.Equal(t, blockedBaseline, blockedHits.Load(),
		"a bare unlisted fc00::/7 literal must never reach the fixture")

	// --- Case B3: the host's 200::/7 (Yggdrasil) address, unlisted. ---
	if overlayAddr != "" {
		require.True(t, ipv6InRange(overlayAddr, ipv6YggdrasilRange),
			"host overlay address %s must be inside 200::/7", overlayAddr)
	}
	outB3, rcB3 := runIPv6TCPProbe(t, client, overlayDest, overlayPort, payloadOverlay)
	t.Logf("case B3 refused probe to [%s]:%d rc=%d fixtureHits=%d out=%q",
		overlayDest, overlayPort, rcB3, overlayHits.Load(), outB3)
	require.NotContains(t, outB3, payloadOverlay,
		"an unlisted 200::/7 (Yggdrasil) literal must return no payload")
	require.Equal(t, overlayBaseline, overlayHits.Load(),
		"a refused 200::/7 destination must never reach the fixture")

	// --- Case C: a non-redirected IPv6 path must not leak around the proxy. ---
	// The ip6 table redirects TCP (all ports) and DNS (UDP/TCP 53); every other
	// packet is dropped unless it is addressed to the gateway itself. The same
	// wildcard-bound UDP fixture gives a differential: the gateway address is
	// accepted, a different host IPv6 address is dropped.
	nonGateway := overlayDest
	outC, rcC := runIPv6UDPProbe(t, client, nonGateway, udpPort, payloadOverlay)
	t.Logf("case C non-redirected UDP probe to [%s]:%d rc=%d fixtureHits=%d out=%q",
		nonGateway, udpPort, rcC, udpHits.Load(), outC)
	require.NotContains(t, outC, payloadOverlay,
		"a non-redirected IPv6 packet must not deliver a payload (no leak around the proxy)")
	require.NotContains(t, outC, "udp-echo", "the UDP fixture must not have answered")
	require.Zero(t, udpHits.Load(),
		"the UDP fixture must not have been reached by the non-redirected packet")

	// Differential control: the same fixture DOES answer when the destination is
	// the gateway address, so the assertion above is about the drop, not about a
	// broken UDP probe.
	outCtrl, rcCtrl := runIPv6UDPProbe(t, client, gateway6, udpPort, payloadOverlay)
	t.Logf("case C control UDP probe to [%s]:%d rc=%d fixtureHits=%d out=%q",
		gateway6, udpPort, rcCtrl, udpHits.Load(), outCtrl)
	require.Contains(t, outCtrl, "udp-echo",
		"UDP to the gateway address is accepted, which proves the drop above is specific")
	require.Greater(t, udpHits.Load(), int64(0), "the UDP fixture must have answered the control probe")
	t.Logf("case C rc: non-redirected=%d gateway-control=%d", rcC, rcCtrl)
}

// TestSDKIPv6PublicEgressThroughProxy covers the public-IPv6 half: a public IPv6
// destination must be reachable through the interception proxy. The destination
// is an IPv6 LITERAL on purpose: a literal cannot be reached over IPv4, so the
// observed HTTP response proves the IPv6 path end to end (no DNS/precedence
// ambiguity). The test skips with an explicit reason when the host has no IPv6
// connectivity at all.
func TestSDKIPv6PublicEgressThroughProxy(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("IPv6 interception (and therefore the dual-stack proxy) is a Linux-backend feature")
	}
	if !hostHasIPv6Connectivity() {
		t.Skip("host has no IPv6 connectivity")
	}

	const publicV6 = "2606:4700:4700::1111" // Cloudflare, answers HTTP on 80
	client := launchAlpineWithNetwork(t, sdk.New("alpine:latest").
		AllowHost(publicV6).
		WithBlockPrivateIPs(true).
		WithNetworkInterception().
		WithCPUs(0.5))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := fmt.Sprintf(
		"sh -c 'wget -S -q -O /dev/null \"http://[%s]/\" 2>&1; echo RC=$?'", publicV6)
	res, err := client.Exec(ctx, cmd)
	require.NoError(t, err, "exec public IPv6 probe")
	out := res.Stdout + res.Stderr
	t.Logf("public IPv6 probe to [%s]: rc=%d out=%q", publicV6, res.ExitCode, out)
	require.Contains(t, out, "HTTP/1.",
		"a public IPv6 destination must get an HTTP response through the proxy")
}

// ipv6ULARange is fc00::/7, the unique-local range the policy blocks.
var ipv6ULARange = mustParseCIDR6("fc00::/7")

// ipv6YggdrasilRange is 200::/7, the Yggdrasil overlay range the policy blocks.
var ipv6YggdrasilRange = mustParseCIDR6("200::/7")

// yggdrasilLiteral stands in for a 200::/7 destination when the host owns no
// address in the range: the packet still leaves the guest toward the TAP, so the
// refusal path is exercised either way.
const yggdrasilLiteral = "200::1"

// overlayProbeDestination picks the 200::/7 destination to probe: the host's own
// overlay address (which the fixture is bound to) or the bare literal.
func overlayProbeDestination(overlayAddr string) string {
	if overlayAddr != "" {
		return overlayAddr
	}
	return yggdrasilLiteral
}

// waitForGuestIPv6Usable waits until the guest's unique-local IPv6 address left
// the TENTATIVE state, i.e. duplicate address detection finished. The kernel does
// not use a tentative address as a source address, so a connect issued earlier
// fails even though the address is configured. Failing here (with the raw address
// state) is the honest outcome when DAD never completes.
func waitForGuestIPv6Usable(t *testing.T, client *sdk.Client) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		res, err := client.Exec(ctx, "ip -6 addr show dev eth0")
		cancel()
		require.NoError(t, err, "poll the guest IPv6 address state")
		state := res.Stdout + res.Stderr
		for _, line := range strings.Split(state, "\n") {
			if strings.Contains(line, "inet6 fd00:") && !strings.Contains(line, "tentative") {
				t.Logf("guest IPv6 address usable (DAD finished): %s", strings.TrimSpace(state))
				return
			}
		}
		if time.Now().After(deadline) {
			require.FailNowf(t, "guest IPv6 address never left the tentative state",
				"still unusable after DAD window:\n%s", state)
		}
		t.Logf("guest IPv6 address still tentative, waiting: %s", strings.TrimSpace(state))
		time.Sleep(500 * time.Millisecond)
	}
}

// dialEchoControl connects to a fixture from the TEST host (not from the guest)
// and asserts the echo comes back. It proves the fixture is alive and reachable,
// which is what makes a later "the fixture was never dialed" assertion meaningful.
func dialEchoControl(t *testing.T, host string, port int, payload string) {
	t.Helper()

	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 5*time.Second)
	require.NoError(t, err, "host-side control dial to [%s]:%d", host, port)
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	_, err = conn.Write([]byte(payload + "\n"))
	require.NoError(t, err, "host-side control dial write to [%s]:%d", host, port)
	got, err := io.ReadAll(io.LimitReader(conn, int64(len(payload)+1)))
	require.NoError(t, err, "host-side control dial read from [%s]:%d", host, port)
	require.Equal(t, payload+"\n", string(got),
		"fixture on [%s]:%d must echo the host-side control payload", host, port)
}

func mustParseCIDR6(cidr string) *net.IPNet {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(fmt.Sprintf("parse %s: %v", cidr, err))
	}
	return network
}

func ipv6InRange(addr string, network *net.IPNet) bool {
	ip := net.ParseIP(addr)
	return ip != nil && ip.To4() == nil && network.Contains(ip)
}

func requireIPv6Literal(t *testing.T, addr, what string) {
	t.Helper()
	ip := net.ParseIP(addr)
	require.NotNil(t, ip, "%s must be an IP literal, got %q", what, addr)
	require.Nil(t, ip.To4(), "%s must be an IPv6 literal, got %q", what, addr)
}

// startIPv6EchoFixture starts a dual-stack (wildcard) TCP echo server and returns
// its port plus a counter of accepted connections. A wildcard bind answers on
// every local IPv6 address, including the per-VM TAP gateway the guest targets.
func startIPv6EchoFixture(t *testing.T) (int, *atomic.Int64) {
	t.Helper()
	return startIPv6EchoFixtureOn(t, "::")
}

// startIPv6EchoFixtureOn is startIPv6EchoFixture bound to one address.
func startIPv6EchoFixtureOn(t *testing.T, bindIP string) (int, *atomic.Int64) {
	t.Helper()

	ln, err := net.Listen("tcp", net.JoinHostPort(bindIP, "0"))
	require.NoError(t, err, "start IPv6 echo fixture on %s", bindIP)
	t.Cleanup(func() { _ = ln.Close() })

	port := ln.Addr().(*net.TCPAddr).Port
	var hits atomic.Int64
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			hits.Add(1)
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(15 * time.Second))
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()

	return port, &hits
}

// startUDPEchoFixture6 starts a wildcard UDP echo server that answers every
// datagram with "udp-echo:" + the payload. An accepted datagram is what proves
// the packet reached the host; a dropped one is what proves it did not.
func startUDPEchoFixture6(t *testing.T) (int, *atomic.Int64) {
	t.Helper()

	conn, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6unspecified, Port: 0})
	require.NoError(t, err, "start UDP echo fixture")
	t.Cleanup(func() { _ = conn.Close() })

	var hits atomic.Int64
	go func() {
		buf := make([]byte, 2048)
		for {
			n, peer, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			hits.Add(1)
			if _, err := conn.WriteToUDP(append([]byte("udp-echo:"), buf[:n]...), peer); err != nil {
				return
			}
		}
	}()

	return conn.LocalAddr().(*net.UDPAddr).Port, &hits
}

// hostOverlayIPv6 returns a host address inside 200::/7 (the Yggdrasil overlay),
// or "" when the host has none.
func hostOverlayIPv6(t *testing.T) string {
	t.Helper()

	ifaces, err := net.Interfaces()
	require.NoError(t, err, "list host interfaces")
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ip := interfaceAddrIP(addr)
			if ip == nil || ip.To4() != nil || !ipv6YggdrasilRange.Contains(ip) {
				continue
			}
			t.Logf("host overlay (200::/7) address on %s: %s", iface.Name, ip)
			return ip.String()
		}
	}
	return ""
}

// hostHasIPv6Connectivity reports whether this host can actually reach a public
// IPv6 destination (a global address alone would not prove there is a route out).
// The probes use literals, so they need no resolver.
func hostHasIPv6Connectivity() bool {
	targets := [][2]string{
		{"2606:4700:4700::1111", "443"}, // Cloudflare
		{"2001:4860:4860::8888", "53"},  // Google DNS
	}
	for _, target := range targets {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(target[0], target[1]), 3*time.Second)
		if err != nil {
			continue
		}
		_ = conn.Close()
		return true
	}
	return false
}

// guestIPv6Gateway reads the guest's IPv6 default gateway from the live guest.
// The raw `ip -6 route show default` output is parsed in Go so no shell/awk
// quoting is involved.
func guestIPv6Gateway(t *testing.T, client *sdk.Client) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := client.Exec(ctx, "ip -6 route show default")
	require.NoError(t, err, "discover guest IPv6 default gateway")
	raw := strings.TrimSpace(res.Stdout + " " + res.Stderr)
	require.Equal(t, 0, res.ExitCode, "ip -6 route show default must succeed (out=%q)", raw)

	fields := strings.Fields(raw)
	for i, field := range fields {
		if field == "via" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	require.FailNowf(t, "no IPv6 default route in guest", "output=%q", raw)
	return ""
}

// execGuest runs a command in the guest and returns stdout+stderr; used for
// diagnostics, so a non-zero exit is logged rather than fatal.
func execGuest(t *testing.T, client *sdk.Client, cmd string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := client.Exec(ctx, cmd)
	if err != nil {
		return "exec error: " + err.Error()
	}
	return fmt.Sprintf("exit=%d stdout=%s stderr=%s", res.ExitCode, res.Stdout, res.Stderr)
}

// runIPv6TCPProbe sends payload to host:port over IPv6 with busybox nc and
// returns the combined output plus the command's exit code.
func runIPv6TCPProbe(t *testing.T, client *sdk.Client, host string, port int, payload string) (string, int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := fmt.Sprintf(
		"sh -c 'echo %s | nc -w 5 %s %d >/tmp/ipv6_probe.txt 2>&1; echo RC=$?; cat /tmp/ipv6_probe.txt'",
		payload, host, port,
	)
	res, err := client.Exec(ctx, cmd)
	require.NoError(t, err, "exec IPv6 probe to [%s]:%d", host, port)
	return res.Stdout + res.Stderr, res.ExitCode
}

// runIPv6UDPProbe sends payload to host:port over IPv6 UDP with busybox nc and
// returns the combined output plus the command's exit code.
func runIPv6UDPProbe(t *testing.T, client *sdk.Client, host string, port int, payload string) (string, int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := fmt.Sprintf(
		"sh -c 'echo %s | nc -u -w 3 %s %d >/tmp/ipv6_udp_probe.txt 2>&1; echo RC=$?; cat /tmp/ipv6_udp_probe.txt'",
		payload, host, port,
	)
	res, err := client.Exec(ctx, cmd)
	require.NoError(t, err, "exec IPv6 UDP probe to [%s]:%d", host, port)
	return res.Stdout + res.Stderr, res.ExitCode
}
