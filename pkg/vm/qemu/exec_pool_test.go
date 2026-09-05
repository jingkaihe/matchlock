//go:build linux

package qemu

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestExecConnPoolReuseAndStaleDetection verifies the QEMU exec-connection pool
// reuses a healthy connection across sequential execs, discards a connection the
// guest closed while idle, and never pools a connection whose exchange did not
// complete. Reuse is what collapses the per-request vsock socket churn that
// amplifies transport resets under swap pressure.
func TestExecConnPoolReuseAndStaleDetection(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Hold the connection open and idle, like the guest exec agent
			// between sequential requests.
			go func() { _, _ = io.Copy(io.Discard, conn) }()
		}
	}()

	dials := 0
	m := &Machine{}
	m.execDial = func(ctx context.Context) (net.Conn, error) {
		dials++
		var d net.Dialer
		return d.DialContext(ctx, "tcp", ln.Addr().String())
	}

	ctx := context.Background()

	// First borrow dials; release makes it idle.
	c1, err := m.borrowExecConn(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, dials, "first borrow must dial")
	m.releaseExecConn(c1, true)

	// A healthy idle connection is reused without another dial.
	c2, err := m.borrowExecConn(ctx)
	require.NoError(t, err)
	require.True(t, c1 == c2, "healthy pooled connection must be reused")
	require.Equal(t, 1, dials, "reuse must not redial")

	// Simulate the guest resetting it while idle: the next borrow must detect
	// the dead socket, drop it, and dial a fresh one instead of failing the exec.
	m.releaseExecConn(c2, true)
	require.NoError(t, c2.Close())
	c3, err := m.borrowExecConn(ctx)
	require.NoError(t, err)
	require.False(t, c3 == c2, "stale connection must not be handed out")
	require.Equal(t, 2, dials, "stale connection must be replaced by a fresh dial")
	m.releaseExecConn(c3, true)

	// A connection whose exchange did not complete is closed, never pooled.
	c4, err := m.borrowExecConn(ctx)
	require.NoError(t, err)
	require.True(t, c3 == c4, "c3 should still be the idle pooled conn")
	m.releaseExecConn(c4, false)
	c5, err := m.borrowExecConn(ctx)
	require.NoError(t, err)
	require.False(t, c5 == c4, "unhealthy connection must not be pooled")
	require.Equal(t, 3, dials, "discarding an unhealthy connection must redial")
	m.releaseExecConn(c5, true)

	// closeExecPool closes idle connections and makes later borrows dial fresh.
	m.closeExecPool()
	c6, err := m.borrowExecConn(ctx)
	require.NoError(t, err)
	require.False(t, c6 == c5)
	require.Equal(t, 4, dials, "borrow after closeExecPool must dial a fresh connection")
	m.releaseExecConn(c6, true)
}

// TestExecConnUsable verifies the liveness predicate: an idle connection times
// out and is usable; a closed peer or pending byte is not.
func TestExecConnUsable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			accepted <- conn
		}
	}()

	var d net.Dialer
	client, err := d.DialContext(context.Background(), "tcp", ln.Addr().String())
	require.NoError(t, err)
	defer client.Close()
	server := <-accepted
	defer server.Close()

	require.True(t, execConnUsable(client), "idle connection with no pending data must be usable")

	// A readable byte means the stream is not at a clean request boundary.
	_, err = server.Write([]byte{0x01})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return !execConnUsable(client) },
		time.Second, 10*time.Millisecond,
		"a connection with a pending byte must not be reused")
}
