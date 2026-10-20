//go:build acceptance

package acceptance

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/sdk"
	"github.com/stretchr/testify/require"
)

// TestSDKIPv6LinkLocalByPass determines whether a QEMU guest can reach the host
// over IPv6 link-local when the only interception is IPv4-family nftables. It is
// a QEMU-specific diagnostic: it derives a qm-* TAP and requires MATCHLOCK_BACKEND
// to be explicitly "qemu", so it skips (not fails) otherwise.
//
// Discriminating design (single sandbox):
//  1. POSITIVE CONTROL — host TCP4 listener on the TAP gateway IPv4; the gateway
//     is added to the allowlist so the guest reaches the host listener (proves the
//     probe, injection, guest network, and host-listener plumbing are sound).
//  2. QUESTION — host TCP6 listener on the TAP's fe80:: link-local; the same guest
//     dials it. There is no IPv6 DNAT/proxy, so a successful exchange is evidence
//     of an IPv6 host-access enforcement gap on this QEMU configuration.
//
// The IPv6 destination is intentionally NOT allowlisted (there is no IPv6 proxy
// to consult it), so a completed tcp6 exchange is a real finding, not an allowlist
// hit. We use bounded exec contexts and validate the host-side received nonce.
func TestSDKIPv6LinkLocalBypass(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("IPv6 interception (and therefore the dual-stack proxy) is a Linux-backend feature")
	}
	if os.Getenv("MATCHLOCK_BACKEND") != "qemu" {
		t.Skipf("IPv6 link-local TAP diagnostic is Linux/QEMU-specific: requires MATCHLOCK_BACKEND=qemu (got %q)", os.Getenv("MATCHLOCK_BACKEND"))
	}

	probeBytes := buildIP6Probe(t)

	// Use a generous launch timeout: QEMU TCG boots can exceed the 45s default
	// launchTimeout under load (a prior run reached the 45s deadline during a
	// launch under contention, so we allow 150s like the concurrent test path).
	builder := sdk.New("alpine:latest").
		AllowHost("httpbin.org").
		AllowPrivateIPs().
		WithCPUs(0.5)
	client := launchWithBuilderTimeout(t, builder, 150*time.Second)
	vmID := client.VMID()
	require.NotEmpty(t, vmID)
	tap := tapForVMID(vmID)
	require.NotEmpty(t, tap, "derive TAP name")

	probePath := "/workspace/ip6probe"
	require.NoError(t, client.WriteFileMode(context.Background(), probePath, probeBytes, 0o755), "inject probe")

	gwIPv4, err := tapIPv4(tap)
	require.NoError(t, err, "read TAP IPv4 gateway")
	hostLL6, err := tapLinkLocal6(tap)
	require.NoError(t, err, "read TAP fe80::")
	t.Logf("TAP=%s gwIPv4=%s fe80::=%s", tap, gwIPv4, hostLL6)

	// Allowlist the gateway IPv4 so the tcp4 control reaches the host listener
	// (the catch-all DNAT -> passthrough otherwise closes non-allowlisted IPs).
	_, err = client.AllowListAdd(context.Background(), gwIPv4)
	require.NoError(t, err, "allowlist gateway IPv4 for control")

	// Positive control: tcp4 to the gateway. This MUST succeed — it proves the
	// probe, injection, guest network, allowlist, and host-listener plumbing are
	// sound. If it fails, the tcp6 result below cannot be interpreted (no basis to
	// distinguish "IPv6 bypass" from "broken probe/network").
	cNonce := fmt.Sprintf("ctrlnonce-%d", time.Now().UnixNano())
	rc4, out4, host4 := runHostListenerAndGuestProbe(t, client, probePath, "tcp4", gwIPv4, tap, cNonce)
	t.Logf("tcp4 control: rc=%d out=%q hostRecv=%q", rc4, out4, host4)
	require.Equalf(t, 0, rc4, "tcp4 control must succeed (proves plumbing). out=%s", out4)
	require.Contains(t, out4, "PROBE_RECV", "tcp4 control must receive ACK; out=%s", out4)
	require.Equal(t, cNonce, host4, "host listener must receive the tcp4 request nonce")

	// Question: tcp6 to the TAP fe80:: link-local. The IPv4-only interception has
	// no IPv6 DNAT/proxy, so if this completes the guest reached the host over IPv6.
	v6Nonce := fmt.Sprintf("v6nonce-%d", time.Now().UnixNano())
	rc6, out6, host6 := runHostListenerAndGuestProbe(t, client, probePath, "tcp6", hostLL6, tap, v6Nonce)
	t.Logf("tcp6 probe: rc=%d out=%q hostRecv=%q", rc6, out6, host6)
	t.Logf("DECISION tcp6_reached=%v (host_saw_nonce=%v)", rc6 == 0, host6 == v6Nonce)
	require.NotEmpty(t, out6, "tcp6 probe must produce output")
}

// runHostListenerAndGuestProbe binds a host TCP listener on addr (family from
// network), serves newline-delimited NONCE/ACK on it, runs the guest probe, and
// returns the probe exit code, its output, and the nonce the host listener
// actually received (empty if it never saw a request). IPv6 uses the TAP zone for
// the host bind and %eth0 for the guest dial.
func runHostListenerAndGuestProbe(t *testing.T, client *sdk.Client, probePath, network, addr, tap, nonce string) (int, string, string) {
	t.Helper()

	bindAddr := net.JoinHostPort(addr, "0")
	if network == "tcp6" && tap != "" {
		bindAddr = net.JoinHostPort(addr+"%"+tap, "0")
	}
	ln, err := net.Listen(network, bindAddr)
	require.NoError(t, err, "bind %s listener on %s", network, bindAddr)
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	gotReq := make(chan string, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(20 * time.Second))
				line, err := bufio.NewReader(c).ReadString('\n')
				if err != nil {
					return
				}
				line = strings.TrimSpace(line)
				if !strings.HasPrefix(line, "NONCE ") {
					return
				}
				reqNonce := strings.TrimPrefix(line, "NONCE ")
				select {
				case gotReq <- reqNonce:
				default:
				}
				_, _ = fmt.Fprintf(c, "ACK %s %s\n", reqNonce, "hostack")
			}(conn)
		}
	}()

	dialAddr := addr
	if network == "tcp6" {
		dialAddr = addr + "%eth0"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := client.Exec(ctx, fmt.Sprintf("%s %s %s %d %s", probePath, network, dialAddr, port, nonce))
	if err != nil {
		return 1, "ERR exec: " + err.Error(), ""
	}

	var hostRecv string
	select {
	case hostRecv = <-gotReq:
	case <-time.After(3 * time.Second):
	}
	return res.ExitCode, res.Stdout + res.Stderr, hostRecv
}

// buildIP6Probe builds the committed guest probe package (probe/ip6probe) directly
// with the test host's Go toolchain into a static Linux binary, so the test is
// self-contained and does not require a manually staged MATCHLOCK_IP6PROBE.
func buildIP6Probe(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "ip6probe")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, "./probe/ip6probe")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "go build probe: %s", out)
	b, err := os.ReadFile(bin)
	require.NoError(t, err)
	return b
}

// tapForVMID derives the qm- TAP name from a QEMU vmID, mirroring
// pkg/vm/qemu.tapNameForVMID (first 8 hex chars of the id after "vm-").
func tapForVMID(vmID string) string {
	strip := strings.TrimPrefix(vmID, "vm-")
	if len(strip) >= 8 {
		strip = strip[:8]
	}
	return "qm-" + strip
}

func tapIPv4(tap string) (string, error) {
	return hostAddrFor(tap, func(ip net.IP) bool { return ip.To4() != nil })
}

func tapLinkLocal6(tap string) (string, error) {
	return hostAddrFor(tap, func(ip net.IP) bool { return ip.To4() == nil && ip.IsLinkLocalUnicast() })
}

func hostAddrFor(iface string, match func(net.IP) bool) (string, error) {
	ni, err := net.InterfaceByName(iface)
	if err != nil {
		return "", err
	}
	addrs, err := ni.Addrs()
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if match(ipnet.IP) {
			return ipnet.IP.String(), nil
		}
	}
	return "", fmt.Errorf("no matching address on %s", iface)
}
