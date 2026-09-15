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
