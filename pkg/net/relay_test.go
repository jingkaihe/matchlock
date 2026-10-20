package net

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRelayHalfClose_ClientHalfCloseDeliversResponse is the platform-independent
// regression for the alternate-port TCP differential. It targets the shared
// relayHalfClose helper directly so the Linux transparent proxy and the darwin
// gVisor passthrough are covered by one implementation and one test.
//
// A guest client that sends a request and then half-closes (FIN) — exactly what
// `echo PAYLOAD | nc -w N HOST PORT` does — must still receive the upstream's
// response. The relay must NOT set a deadline or cancel the pending direction
// when the first direction sees EOF; it must propagate the half-close
// (CloseWrite) and keep relaying the other direction until both are done.
func TestRelayHalfClose_ClientHalfCloseDeliversResponse(t *testing.T) {
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

	// Front listener stands in for the guest-facing accept loop. The guest side
	// is a real *net.TCPConn so CloseWrite propagates an actual FIN.
	front, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer front.Close()

	relayDone := make(chan struct{})
	go func() {
		guestConn, err := front.Accept()
		if err != nil {
			return
		}
		realConn, err := net.Dial("tcp", upstream.Addr().String())
		if err != nil {
			guestConn.Close()
			return
		}
		relayHalfClose(guestConn, realConn)
		guestConn.Close()
		realConn.Close()
		close(relayDone)
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
	case <-relayDone:
	case <-time.After(5 * time.Second):
		require.Fail(t, "relayHalfClose should return after both directions complete")
	}
}
