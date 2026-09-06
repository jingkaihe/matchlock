//go:build acceptance

package acceptance

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/sdk"
	"github.com/stretchr/testify/require"
)

// TestSDKAllowPrivateReachesListedPrivateEndpoint proves allow_private lifts the
// private-IP block for exactly the listed host:port while every unlisted private
// destination stays refused, using real VMs.
//
// Host fixtures are tiny TCP echo servers. Fixture A binds a non-loopback RFC1918
// IPv4 assigned to this host (skip if none). Fixture B binds 0.0.0.0 so the guest
// can also target it through the TAP gateway address, which is private but
// unlisted. Alt ports hit the TAP catch-all DNAT, so the connection is decided by
// the passthrough proxy's policy check — the behavior under test.
//
// Both sandboxes force interception with WithNetworkInterception: block_private_ips
// is only enforced while the proxy is active, mirroring the harness config
// {block_private_ips:true, intercept:true}. Swap stays disabled (SwapMB unset).
func TestSDKAllowPrivateReachesListedPrivateEndpoint(t *testing.T) {
	hostIP := discoverHostPrivateIPv4(t)
	if hostIP == "" {
		t.Skip("no non-loopback RFC1918 IPv4 assigned to the host")
	}
	t.Logf("host private IPv4 fixture address: %s", hostIP)

	const payloadA = "matchlock-allow-private-fixture-A"
	const payloadB = "matchlock-allow-private-fixture-B"

	portA, hitsA := startEchoFixture(t, hostIP)
	portB, hitsB := startEchoFixture(t, "0.0.0.0")
	require.NotEqual(t, portA, portB, "the two fixtures must use distinct ports")

	// Sandbox 1: private-IP block stays on, with fixture A's host:port exempted.
	listingClient := launchAlpineWithNetwork(t, sdk.New("alpine:latest").
		WithBlockPrivateIPs(true).
		WithAllowPrivate(hostIP+":"+strconv.Itoa(portA)).
		WithNetworkInterception(). // block_private_ips is enforced only with the proxy active
		WithCPUs(0.5))

	gatewayIP := guestDefaultGateway(t, listingClient)
	t.Logf("guest default gateway: %s", gatewayIP)

	// The listed fixture must be reachable through the passthrough proxy.
	allowedOut := runEchoProbe(t, listingClient, hostIP, portA, payloadA)
	require.Contains(t, allowedOut, payloadA,
		"allow_private host:port must let the guest reach fixture A")
	require.Greater(t, hitsA.Load(), int64(0), "fixture A must have been dialed")

	// A private destination that is NOT listed (the TAP gateway, fixture B's
	// port) must stay refused and must never reach the fixture.
	blockedOut := runEchoProbe(t, listingClient, gatewayIP, portB, payloadB)
	require.NotContains(t, blockedOut, payloadB,
		"an unlisted private gateway:port must be refused")
	require.Zero(t, hitsB.Load(), "a refused private destination must not reach fixture B")

	// Sandbox 2: same private block, no allow_private entry at all.
	unlistedClient := launchAlpineWithNetwork(t, sdk.New("alpine:latest").
		WithBlockPrivateIPs(true).
		WithNetworkInterception().
		WithCPUs(0.5))

	hitsABefore := hitsA.Load()
	refusedOut := runEchoProbe(t, unlistedClient, hostIP, portA, payloadA)
	require.NotContains(t, refusedOut, payloadA,
		"without an allow_private entry the same private fixture must be refused")
	require.Equal(t, hitsABefore, hitsA.Load(),
		"the unlisted sandbox must not reach fixture A")
}

// discoverHostPrivateIPv4 returns a non-loopback RFC1918 IPv4 assigned to an up
// host interface, preferring physical-looking interfaces over virtual bridges.
func discoverHostPrivateIPv4(t *testing.T) string {
	t.Helper()

	ifaces, err := net.Interfaces()
	require.NoError(t, err, "list host interfaces")

	var fallback string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ip := interfaceAddrIP(addr).To4()
			if ip == nil || !isRFC1918(ip) {
				continue
			}
			if isVirtualInterfaceName(iface.Name) {
				if fallback == "" {
					fallback = ip.String()
				}
				continue
			}
			return ip.String()
		}
	}
	return fallback
}

func interfaceAddrIP(addr net.Addr) net.IP {
	switch v := addr.(type) {
	case *net.IPNet:
		return v.IP
	case *net.IPAddr:
		return v.IP
	default:
		return nil
	}
}

func isRFC1918(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	switch {
	case ip4[0] == 10:
		return true
	case ip4[0] == 172 && ip4[1]&0xf0 == 16:
		return true
	case ip4[0] == 192 && ip4[1] == 168:
		return true
	default:
		return false
	}
}

func isVirtualInterfaceName(name string) bool {
	for _, prefix := range []string{"docker", "br-", "veth", "virbr", "tap", "tun", "fc-", "qm-"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// startEchoFixture starts a TCP echo server on bindIP:0 and returns the chosen
// port plus a counter of accepted connections. It closes on test cleanup.
func startEchoFixture(t *testing.T, bindIP string) (int, *atomic.Int64) {
	t.Helper()

	ln, err := net.Listen("tcp", net.JoinHostPort(bindIP, "0"))
	require.NoError(t, err, "start host echo fixture on %s", bindIP)
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

// guestDefaultGateway reads the IPv4 default gateway from the live guest.
func guestDefaultGateway(t *testing.T, client *sdk.Client) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := client.Exec(ctx, "sh -c 'ip -4 route show default | awk \"{print \\$3}\"'")
	require.NoError(t, err, "discover guest default gateway")
	gatewayIP := strings.TrimSpace(res.Stdout)
	require.NotEmpty(t, gatewayIP, "empty default gateway from guest")
	require.NotNil(t, net.ParseIP(gatewayIP), "guest default gateway must be an IP literal, got %q", gatewayIP)
	return gatewayIP
}

// runEchoProbe sends payload to host:port from inside the guest with busybox nc
// and returns the combined output. An allowed echo returns the payload; a
// policy-refused connection returns no payload.
func runEchoProbe(t *testing.T, client *sdk.Client, host string, port int, payload string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := fmt.Sprintf(
		"sh -c 'echo %s | nc -w 5 %s %d >/tmp/allow_private_probe.txt 2>&1; echo RC=$?; cat /tmp/allow_private_probe.txt'",
		payload, host, port,
	)
	res, err := client.Exec(ctx, cmd)
	require.NoError(t, err, "exec echo probe to %s:%d", host, port)
	return res.Stdout + res.Stderr
}
