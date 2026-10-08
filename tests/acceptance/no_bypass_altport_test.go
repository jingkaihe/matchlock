//go:build acceptance

package acceptance

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/sdk"
	"github.com/stretchr/testify/require"
)

// TestSDKAltPortTCPPolicyDifferential proves the allow-list is enforced on
// alternate TCP ports with the SAME live guest across allowed->denied->allowed.
// The catch-all DNAT redirects every guest TCP connection (any port) to the
// proxy's passthrough listener; passthrough authenticates by bare dstIP via
// IsHostAllowed (a hostname-only allowlist never matches a bare IP), so a guest
// connection to an un-allowlisted IP on a non-80/443 port is closed by policy.
//
// Discriminating design (single sandbox, same reachable fixture, BlockPrivateIPs
// disabled so the private gateway IP is governed purely by the allowlist):
//  1. ALLOWED — AllowListAdd the bare gateway IP: the guest reaches the host echo
//     server on the alt port via passthrough (proves the fixture is reachable
//     through DNAT+passthrough).
//  2. DENIED — AllowListDelete that IP (hostname-only allowlist remains): the SAME
//     connection is now closed by policy (bare IP not matched). The delta between
//     reached/closed isolates policy from network reachability.
//  3. RE-ALLOWED — AllowListAdd the IP again: reachability is restored.
func TestSDKAltPortTCPPolicyDifferential(t *testing.T) {
	// Host-side TCP echo server bound to all interfaces (covers the TAP gateway).
	const payload = "matchlock-altport-payload"
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	require.NoError(t, err, "start host echo server")
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
				_, _ = io.Copy(conn, conn)
			}(c)
		}
	}()

	client := launchAlpineWithNetwork(t, sdk.New("alpine:latest").
		AllowHost("httpbin.org"). // hostname-only baseline allowlist
		AllowPrivateIPs().        // private-IP blocking OFF so the gateway IP is governed by the allowlist
		WithCPUs(0.5))

	// Discover the sandbox gateway from the live guest (not assume 192.168.100.1).
	res, err := client.Exec(context.Background(), "sh -c 'ip -4 route show default | awk \"{print \\$3}\"'")
	require.NoError(t, err, "discover gateway")
	gatewayIP := strings.TrimSpace(res.Stdout)
	require.NotEmpty(t, gatewayIP, "empty gateway from guest")
	require.NotNil(t, net.ParseIP(gatewayIP), "parsed gateway IP: %q", gatewayIP)

	gatewayTarget := fmt.Sprintf("%s %s", gatewayIP, strconv.Itoa(port))
	probe := fmt.Sprintf("sh -c 'echo %s | nc -w 5 %s >/tmp/o.txt 2>&1; echo RC=$?; cat /tmp/o.txt'", payload, gatewayTarget)
	runProbe := func() string {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res, err := client.Exec(ctx, probe)
		require.NoError(t, err, "exec probe")
		return res.Stdout + res.Stderr
	}

	// Step 1: allowlist the bare gateway IP -> guest reaches the host echo server.
	_, err = client.AllowListAdd(context.Background(), gatewayIP)
	require.NoError(t, err, "allow-list add IP")
	require.Contains(t, runProbe(), payload, "with the IP allowlisted, guest must reach the host echo server")

	// Step 2: delete the IP (hostname-only allowlist remains) -> same connection
	// must be closed by policy and must NOT deliver the payload.
	_, err = client.AllowListDelete(context.Background(), gatewayIP)
	require.NoError(t, err, "allow-list delete IP")
	require.NotContains(t, runProbe(), payload, "hostname-only allowlist must close alt-port TCP to an un-allowlisted bare IP")

	// Step 3: re-add the IP -> works again.
	_, err = client.AllowListAdd(context.Background(), gatewayIP)
	require.NoError(t, err, "allow-list re-add IP")
	require.Contains(t, runProbe(), payload, "re-allowing the IP must restore alt-port reachability")
}
