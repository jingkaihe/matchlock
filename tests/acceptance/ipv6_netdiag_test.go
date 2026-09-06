//go:build acceptance

package acceptance

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/sdk"
	"github.com/stretchr/testify/require"
)

// TestSDKIPv6NetDiag launches ONE sandbox and, while it is ALIVE, captures the
// host and guest IPv4/IPv6 networking state plus a single tcp4 control probe. Its
// purpose is to explain the repeatable tcp4 timeout seen on subnet 192.168.100.0/24
// (which succeeded on .102): a stale TAP/route or reverse-path issue on .100 would
// show here because we inspect BEFORE teardown removes the TAP. All evidence is
// logged; assertions are intentionally permissive (this is a diagnostic).
//
// CONTRACT CHANGE: the sandbox this diag creates is now intercepted on BOTH
// families, so the host inventory below also captures the per-TAP ip/ip6 nftables
// tables (a dual-stack sandbox must install matchlock_<tap> AND matchlock6_<tap>)
// and the qm-* TAP's IPv6 addressing. Nothing here asserts the old
// "IPv6 is fully dropped" state - the guest IPv6 line was already logged only -
// so this diagnostic describes the new redirected-not-dropped contract.
func TestSDKIPv6NetDiag(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("MATCHLOCK_BACKEND") != "qemu" {
		t.Skip("diag is linux/QEMU-specific")
	}
	probeBytes := buildIP6Probe(t)

	client := launchWithBuilderTimeout(t, sdk.New("alpine:latest").
		AllowHost("httpbin.org").AllowPrivateIPs().WithCPUs(0.5), 150*time.Second)
	vmID := client.VMID()
	tap := tapForVMID(vmID)
	require.NotEmpty(t, tap)

	probePath := "/workspace/ip6probe"
	require.NoError(t, client.WriteFileMode(context.Background(), probePath, probeBytes, 0o755))

	gw, err := tapIPv4(tap)
	require.NoError(t, err, "read TAP IPv4 gateway")
	_, err = client.AllowListAdd(context.Background(), gw)
	require.NoError(t, err)
	t.Logf("gateway=%s tap=%s", gw, tap)

	// ---- HOST: addresses/routes/rules/listeners while sandbox is alive ----
	hostCmds := []string{
		"ip -4 -o addr show",
		"ip -4 rule show",
		"ip -4 route show table all",
		"ip -4 route get 192.168.100.1 2>&1",
		"ip -4 route get 192.168.100.2 2>&1",
		"ss -lntp 2>&1 | head -40",
		"cat /proc/sys/net/ipv4/conf/all/rp_filter 2>&1",
		fmt.Sprintf("cat /proc/sys/net/ipv4/conf/%s/rp_filter 2>&1", tap),
		fmt.Sprintf("cat /proc/sys/net/ipv4/conf/%s/route_localnet 2>&1", tap),
		"nft list tables 2>&1 | grep -E 'matchlock6?_' || true",
		"ip -6 -o addr show 2>&1 | grep -E 'qm-|fc-' || true",
		"ip -6 route show table all 2>&1 | head -20",
	}
	for _, c := range hostCmds {
		out, err := exec.Command("sh", "-c", c).CombinedOutput()
		t.Logf("HOST $ %s\n%s", c, out)
		_ = err
	}
	// List all qm-* TAPs (to spot stale ones shadowing .100).
	if out, err := exec.Command("sh", "-c", "ip -o link show | grep -E 'qm-|fc-'").CombinedOutput(); err == nil {
		t.Logf("HOST TAPs:\n%s", out)
	}
	if out, err := exec.Command("sh", "-c", "ip -4 -o addr show | grep -E 'qm-|fc-'").CombinedOutput(); err == nil {
		t.Logf("HOST TAP addrs:\n%s", out)
	}

	// ---- GUEST: addresses/routes/arp while alive ----
	guestCmds := []string{
		"ip -4 addr show",
		"ip -4 route show",
		"cat /proc/net/arp",
		"ip -6 addr show | head -20",
	}
	for _, c := range guestCmds {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		res, err := client.Exec(ctx, c)
		cancel()
		if err != nil {
			t.Logf("GUEST $ %s -> ERR %v", c, err)
			continue
		}
		t.Logf("GUEST $ %s (rc=%d)\nstdout=%s\nstderr=%s", c, res.ExitCode, res.Stdout, res.Stderr)
	}

	// ---- Single tcp4 control probe (the failing case) while alive ----
	nonce := fmt.Sprintf("ctrlnonce-%d", time.Now().UnixNano())
	rc4, out4, host4 := runHostListenerAndGuestProbe(t, client, probePath, "tcp4", gw, tap, nonce)
	t.Logf("tcp4 control: rc=%d out=%q hostRecv=%q (gateway=%s tap=%s)", rc4, out4, host4, gw, tap)

	// ---- Host-local control: does the HOST reach its own gateway-bound listener? ----
	// Bind a listener on the gateway IP and try a host-loopback dial.
	ln, err := net.Listen("tcp4", net.JoinHostPort(gw, "0"))
	if err != nil {
		t.Logf("host bind on gateway IP failed: %v", err)
	} else {
		port := ln.Addr().(*net.TCPAddr).Port
		defer ln.Close()
		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			_ = c.(*net.TCPConn).CloseRead()
			_, _ = c.(*net.TCPConn).Read(make([]byte, 64))
		}()
		conn, err := net.DialTimeout("tcp4", net.JoinHostPort(gw, fmt.Sprint(port)), 3*time.Second)
		if err != nil {
			t.Logf("HOST->host-gateway-listener dial FAILED: %v", err)
		} else {
			t.Logf("HOST->host-gateway-listener dial OK (address %s)", conn.LocalAddr())
			_ = conn.Close()
		}
	}
}
