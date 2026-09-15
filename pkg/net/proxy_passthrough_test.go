//go:build linux

package net

import (
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/policy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandlePassthrough_Allowed(t *testing.T) {
	upstream := startEchoServer(t)
	defer upstream.Close()

	tp := &TransparentProxy{
		policy: policy.NewEngine(&api.NetworkConfig{
			AllowedHosts: []string{"127.0.0.1"},
		}),
		events: make(chan api.Event, 10),
	}

	client, server := net.Pipe()
	defer client.Close()

	_, portStr, _ := net.SplitHostPort(upstream.Addr().String())

	go tp.handlePassthrough(server, "127.0.0.1", mustAtoi(portStr))

	msg := []byte("hello passthrough")
	client.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err := client.Write(msg)
	require.NoError(t, err)

	buf := make([]byte, len(msg))
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = io.ReadFull(client, buf)
	require.NoError(t, err)

	assert.Equal(t, string(msg), string(buf))
}

func TestHandlePassthrough_Blocked(t *testing.T) {
	tp := &TransparentProxy{
		policy: policy.NewEngine(&api.NetworkConfig{
			AllowedHosts: []string{"allowed.example.com"},
		}),
		events: make(chan api.Event, 10),
	}

	client, server := net.Pipe()
	defer client.Close()

	done := make(chan struct{})
	go func() {
		tp.handlePassthrough(server, "93.184.216.34", 8080)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.Fail(t, "handlePassthrough should have returned quickly for blocked host")
	}

	select {
	case ev := <-tp.events:
		assert.True(t, ev.Network.Blocked, "expected blocked event")
		assert.Equal(t, "93.184.216.34:8080", ev.Network.Host)
	default:
		assert.Fail(t, "expected a blocked event to be emitted")
	}
}

func TestHandlePassthrough_EmptyAllowlist(t *testing.T) {
	upstream := startEchoServer(t)
	defer upstream.Close()

	tp := &TransparentProxy{
		policy: policy.NewEngine(&api.NetworkConfig{}),
		events: make(chan api.Event, 10),
	}

	client, server := net.Pipe()
	defer client.Close()

	_, portStr, _ := net.SplitHostPort(upstream.Addr().String())

	go tp.handlePassthrough(server, "127.0.0.1", mustAtoi(portStr))

	msg := []byte("open policy")
	client.SetWriteDeadline(time.Now().Add(2 * time.Second))
	client.Write(msg)

	buf := make([]byte, len(msg))
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := io.ReadFull(client, buf)
	require.NoError(t, err)

	assert.Equal(t, string(msg), string(buf))
}

func TestHandlePassthrough_UpstreamRefused(t *testing.T) {
	tp := &TransparentProxy{
		policy: policy.NewEngine(&api.NetworkConfig{
			AllowedHosts: []string{"127.0.0.1"},
		}),
		events: make(chan api.Event, 10),
	}

	// Use a port with nothing listening — connection refused
	client, server := net.Pipe()
	defer client.Close()

	done := make(chan struct{})
	go func() {
		tp.handlePassthrough(server, "127.0.0.1", 1)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.Fail(t, "handlePassthrough should return when upstream is unreachable")
	}
}

func TestHandlePassthrough_HalfClose(t *testing.T) {
	// Start a server that writes a response then closes its write side
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Read one message, echo it, then close
		buf := make([]byte, 64)
		n, _ := conn.Read(buf)
		conn.Write(buf[:n])
		conn.Close()
	}()

	tp := &TransparentProxy{
		policy: policy.NewEngine(&api.NetworkConfig{
			AllowedHosts: []string{"127.0.0.1"},
		}),
		events: make(chan api.Event, 10),
	}

	client, server := net.Pipe()
	defer client.Close()

	_, portStr, _ := net.SplitHostPort(ln.Addr().String())

	done := make(chan struct{})
	go func() {
		tp.handlePassthrough(server, "127.0.0.1", mustAtoi(portStr))
		close(done)
	}()

	msg := []byte("ping")
	client.SetWriteDeadline(time.Now().Add(2 * time.Second))
	client.Write(msg)

	buf := make([]byte, len(msg))
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	io.ReadFull(client, buf)

	assert.Equal(t, string(msg), string(buf))

	// handlePassthrough should exit cleanly after upstream closes
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.Fail(t, "handlePassthrough should have exited after upstream closed")
	}
}

// TestHandlePassthrough_DialsLiteralOriginalDst qualifies the passthrough DNS
// path: handlePassthrough must dial the literal SO_ORIGINAL_DST IP (never re-
// resolving it as a hostname), so a DNS change between the policy check and the
// dial cannot redirect the passthrough TCP connection to an unverified address.
func TestHandlePassthrough_DialsLiteralOriginalDst(t *testing.T) {
	dns := newSyntheticDNS(t, syntheticDNSConfig{
		// If the passthrough path ever treated the destination as a hostname and
		// re-resolved it, it would resolve here to a private address and be denied
		// (or worse, dialed) — the regression below must never trigger this.
		"203.0.113.99.": {{"127.0.0.1"}},
	})
	dns.install()
	defer dns.uninstall()

	upstream := startEchoServer(t)
	defer upstream.Close()
	_, portStr, _ := net.SplitHostPort(upstream.Addr().String())

	var dialed atomic.Value // string
	tp := &TransparentProxy{
		policy: policy.NewEngine(&api.NetworkConfig{BlockPrivateIPs: true}),
		events: make(chan api.Event, 10),
		dial: func(network, addr string) (net.Conn, error) {
			dialed.Store(addr)
			// Route to the owned echo server regardless of the requested address so
			// the passthrough completes and we can observe the target it dialed.
			return net.Dial("tcp", upstream.Addr().String())
		},
	}

	client, server := net.Pipe()
	defer client.Close()

	go tp.handlePassthrough(server, "203.0.113.99", mustAtoi(portStr))

	msg := []byte("hello passthrough")
	client.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err := client.Write(msg)
	require.NoError(t, err)

	buf := make([]byte, len(msg))
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = io.ReadFull(client, buf)
	require.NoError(t, err)
	assert.Equal(t, string(msg), string(buf), "passthrough must still forward traffic")

	gotDial, _ := dialed.Load().(string)
	assert.Equal(t, fmt.Sprintf("203.0.113.99:%s", portStr), gotDial,
		"passthrough must dial the literal SO_ORIGINAL_DST IP, not a resolved hostname")

	assert.Equal(t, 0, dns.aCount("203.0.113.99."),
		"the literal destination IP must never be treated as a hostname and resolved")
}

// TestHandlePassthrough_ClientHalfCloseDeliversResponse is the regression for
// the alternate-port differential: a guest client that sends a request and then
// half-closes (FIN) — exactly what `echo PAYLOAD | nc -w N HOST PORT` does — must
// still receive the upstream's response.
//
// The relay used to set a deadline on BOTH connections the instant the first
// io.Copy finished, so the guest->upstream copy returning on the client half-close
// aborted the still-pending upstream->guest copy before the response could be
// written. Observed on the wire as `RC=0` with no payload. It must instead
// propagate the half-close (CloseWrite) and keep relaying the other direction.
func TestHandlePassthrough_ClientHalfCloseDeliversResponse(t *testing.T) {
	// Upstream reads one request, waits briefly (modeling a server that computes
	// its response), then echoes it and closes. The delay makes the old
	// deadline-on-first-EOF bug deterministic: the deadline fired before the
	// response existed, so the response was dropped.
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

	// Front listener stands in for the guest-facing accept loop.
	front, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer front.Close()

	_, upstreamPort, _ := net.SplitHostPort(upstream.Addr().String())
	var dialed atomic.Value // string
	tp := &TransparentProxy{
		policy: policy.NewEngine(&api.NetworkConfig{
			AllowedHosts: []string{"127.0.0.1"},
		}),
		events: make(chan api.Event, 10),
		dial: func(network, addr string) (net.Conn, error) {
			dialed.Store(addr)
			return net.Dial(network, addr)
		},
	}

	go func() {
		c, err := front.Accept()
		if err != nil {
			return
		}
		tp.handlePassthrough(c, "127.0.0.1", mustAtoi(upstreamPort))
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

	gotDial, _ := dialed.Load().(string)
	assert.Equal(t, net.JoinHostPort("127.0.0.1", upstreamPort), gotDial,
		"passthrough must dial the literal original destination")
}

func startEchoServer(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(conn)
		}
	}()
	return ln
}

func mustAtoi(s string) int {
	var n int
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
