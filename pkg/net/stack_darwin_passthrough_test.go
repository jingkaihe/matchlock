//go:build darwin

package net

import (
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/policy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNetworkStackHandlePassthrough_ClientHalfCloseDeliversResponse is the
// darwin regression for the alternate-port TCP differential. The gVisor
// passthrough used to share one context.WithCancel between its two copy
// directions, so the guest->upstream copy returning on the client's FIN
// cancelled the still-pending upstream->guest copy before the response was
// written; on the wire that looked like `RC=0` with no payload. It must now use
// the shared relayHalfClose, propagate each EOF as a CloseWrite and keep
// relaying the other direction.
//
// handlePassthrough only touches policy/events and net.Dial, so no gVisor stack
// (and therefore no kernel/entitlement) is required: the guest side is a real
// *net.TCPConn accepted from a loopback listener, standing in for the
// *gonet.TCPConn that handleTCPConnection would pass in production.
func TestNetworkStackHandlePassthrough_ClientHalfCloseDeliversResponse(t *testing.T) {
	// Upstream reads one request, waits briefly (modeling a server that computes
	// its response), then echoes it and closes. The delay makes an
	// abort-on-first-EOF bug deterministic: the abort would fire before the
	// response existed, so the response would be dropped.
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer upstream.Close()
	go func() {
		c, err := upstream.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 4096)
		n, err := c.Read(buf)
		if err != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
		_, _ = c.Write(buf[:n])
	}()

	_, upstreamPortStr, err := net.SplitHostPort(upstream.Addr().String())
	require.NoError(t, err)
	upstreamPort, err := strconv.Atoi(upstreamPortStr)
	require.NoError(t, err)

	// Front listener stands in for the guest-facing gVisor accept path; the
	// accepted conn is a real *net.TCPConn so CloseWrite propagates an actual FIN.
	front, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer front.Close()

	ns := &NetworkStack{
		policy: policy.NewEngine(&api.NetworkConfig{
			AllowedHosts: []string{"127.0.0.1"},
		}),
		events: make(chan api.Event, 10),
	}

	handled := make(chan struct{})
	go func() {
		defer close(handled)
		guestConn, err := front.Accept()
		if err != nil {
			return
		}
		ns.handlePassthrough(guestConn, "127.0.0.1", upstreamPort)
	}()

	client, err := net.Dial("tcp", front.Addr().String())
	require.NoError(t, err)
	defer client.Close()

	_, err = client.Write([]byte("matchlock-altport-payload"))
	require.NoError(t, err)
	require.NoError(t, client.(*net.TCPConn).CloseWrite(), "client half-close")

	require.NoError(t, client.SetReadDeadline(time.Now().Add(5*time.Second)))
	got, err := io.ReadAll(client)
	require.NoError(t, err)
	assert.Equal(t, "matchlock-altport-payload", string(got),
		"response sent after the client half-closed must still be relayed")

	select {
	case <-handled:
	case <-time.After(5 * time.Second):
		require.Fail(t, "handlePassthrough should return after both directions complete")
	}
}

// startLoopbackEcho listens on 127.0.0.1, accepts a single connection, reads one
// chunk and echoes it back before closing. It returns the chosen port.
func startLoopbackEcho(t *testing.T) int {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 4096)
		n, err := c.Read(buf)
		if err != nil {
			return
		}
		_, _ = c.Write(buf[:n])
	}()

	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)
	return port
}

// passthroughEchoRoundTrip stands in for the guest-facing gVisor accept path and
// relays one payload through handlePassthrough, returning what the client read.
func passthroughEchoRoundTrip(t *testing.T, ns *NetworkStack, dstIP string, dstPort int) string {
	t.Helper()

	front, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer front.Close()

	handled := make(chan struct{})
	go func() {
		defer close(handled)
		guestConn, err := front.Accept()
		if err != nil {
			return
		}
		ns.handlePassthrough(guestConn, dstIP, dstPort)
	}()

	client, err := net.Dial("tcp", front.Addr().String())
	require.NoError(t, err)
	defer client.Close()

	_, err = client.Write([]byte("matchlock-altport-payload"))
	require.NoError(t, err)
	require.NoError(t, client.(*net.TCPConn).CloseWrite(), "client half-close")

	require.NoError(t, client.SetReadDeadline(time.Now().Add(5*time.Second)))
	got, err := io.ReadAll(client)
	require.NoError(t, err)

	select {
	case <-handled:
	case <-time.After(5 * time.Second):
		require.Fail(t, "handlePassthrough should return after both directions complete")
	}

	return string(got)
}

// TestNetworkStackHandlePassthrough_GatewayDialMapsToLoopback is the darwin
// regression for MATCHLOCK-DARWIN-ALTPORT-2. The guest's default route points at
// the gVisor netstack's virtual gateway (gatewayIP), which is not bound on the
// host, so the old code dialed a dead address: net.Dial failed, the error was
// swallowed, and the guest saw a completed handshake followed by EOF (RC=0, no
// payload). With the gatewayIP configured, a passthrough to that address must be
// mapped to host loopback and relay the echoed payload.
func TestNetworkStackHandlePassthrough_GatewayDialMapsToLoopback(t *testing.T) {
	upstreamPort := startLoopbackEcho(t)

	ns := &NetworkStack{
		// Empty AllowedHosts = allow all.
		policy:    policy.NewEngine(&api.NetworkConfig{}),
		events:    make(chan api.Event, 10),
		gatewayIP: "192.168.100.1",
	}

	got := passthroughEchoRoundTrip(t, ns, "192.168.100.1", upstreamPort)
	assert.Equal(t, "matchlock-altport-payload", got,
		"a passthrough to the virtual gateway must reach the host loopback listener")
}

// TestNetworkStackHandlePassthrough_NonGatewayDialsLiterally verifies the
// mapping is narrow: a destination other than the configured gateway keeps
// dialing its literal address (the Linux passthrough behavior).
func TestNetworkStackHandlePassthrough_NonGatewayDialsLiterally(t *testing.T) {
	upstreamPort := startLoopbackEcho(t)

	ns := &NetworkStack{
		policy:    policy.NewEngine(&api.NetworkConfig{}),
		events:    make(chan api.Event, 10),
		gatewayIP: "192.168.100.1",
	}

	got := passthroughEchoRoundTrip(t, ns, "127.0.0.1", upstreamPort)
	assert.Equal(t, "matchlock-altport-payload", got,
		"a non-gateway destination must still be dialed literally")
}

// TestNetworkStackHandlePassthrough_GatewayNotAllowlistedStillBlocked locks the
// policy order: the allowlist is evaluated against the ORIGINAL guest-visible
// destination, before any gateway->loopback mapping. Here the mapped target
// (127.0.0.1) is allowlisted but the gateway is not, so the connection must be
// blocked and the host loopback listener must never be dialed.
func TestNetworkStackHandlePassthrough_GatewayNotAllowlistedStillBlocked(t *testing.T) {
	accepted := make(chan struct{}, 1)
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer upstream.Close()
	go func() {
		c, err := upstream.Accept()
		if err != nil {
			return
		}
		accepted <- struct{}{}
		_ = c.Close()
	}()
	_, upstreamPortStr, err := net.SplitHostPort(upstream.Addr().String())
	require.NoError(t, err)
	upstreamPort, err := strconv.Atoi(upstreamPortStr)
	require.NoError(t, err)

	front, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer front.Close()

	ns := &NetworkStack{
		policy: policy.NewEngine(&api.NetworkConfig{
			// Only the mapped target is allowlisted, not the guest-visible gateway.
			AllowedHosts: []string{"127.0.0.1"},
		}),
		events:    make(chan api.Event, 10),
		gatewayIP: "192.168.100.1",
	}

	handled := make(chan struct{})
	go func() {
		defer close(handled)
		guestConn, err := front.Accept()
		if err != nil {
			return
		}
		ns.handlePassthrough(guestConn, "192.168.100.1", upstreamPort)
	}()

	client, err := net.Dial("tcp", front.Addr().String())
	require.NoError(t, err)
	defer client.Close()

	select {
	case <-handled:
	case <-time.After(2 * time.Second):
		require.Fail(t, "handlePassthrough should return quickly for a blocked host")
	}

	select {
	case ev := <-ns.events:
		require.NotNil(t, ev.Network)
		assert.True(t, ev.Network.Blocked, "expected a blocked event")
		assert.Equal(t, net.JoinHostPort("192.168.100.1", strconv.Itoa(upstreamPort)), ev.Network.Host,
			"blocked event must name the original guest-visible destination")
	default:
		assert.Fail(t, "expected a blocked event to be emitted")
	}

	select {
	case <-accepted:
		assert.Fail(t, "mapped loopback target must not be dialed when the original destination is blocked")
	default:
	}
}

// TestNetworkStackHandlePassthrough_BlockedEmitsEvent verifies the policy gate is
// preserved by the refactor: a dest IP outside the allowlist must not be dialed
// and must emit exactly one blocked event with the host:port that was requested.
func TestNetworkStackHandlePassthrough_BlockedEmitsEvent(t *testing.T) {
	front, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer front.Close()

	ns := &NetworkStack{
		policy: policy.NewEngine(&api.NetworkConfig{
			AllowedHosts: []string{"allowed.example.com"},
		}),
		events: make(chan api.Event, 10),
	}

	handled := make(chan struct{})
	go func() {
		defer close(handled)
		guestConn, err := front.Accept()
		if err != nil {
			return
		}
		ns.handlePassthrough(guestConn, "93.184.216.34", 8080)
	}()

	client, err := net.Dial("tcp", front.Addr().String())
	require.NoError(t, err)
	defer client.Close()

	select {
	case <-handled:
	case <-time.After(2 * time.Second):
		require.Fail(t, "handlePassthrough should return quickly for a blocked host")
	}

	select {
	case ev := <-ns.events:
		require.NotNil(t, ev.Network)
		assert.True(t, ev.Network.Blocked, "expected a blocked event")
		assert.Equal(t, "93.184.216.34:8080", ev.Network.Host)
	default:
		assert.Fail(t, "expected a blocked event to be emitted")
	}
}
