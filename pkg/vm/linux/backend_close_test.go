//go:build linux

package linux

import (
	"context"
	"net"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestCloseWaitsForVMMExitWithCanceledContext is the regression guard for the
// persistent-TAP leak. Close is called with an already-canceled context (the
// production --graceful-shutdown default of 0 makes closeContext return an
// expired context), so Stop returns via ctx.Done() and a ctx-sensitive wait
// would return immediately. Close must still block until the VMM is reaped
// before tearing down the network, otherwise DeleteInterface races the
// still-attached persistent TAP and fails with EBUSY, leaking fc-* interfaces.
func TestCloseWaitsForVMMExitWithCanceledContext(t *testing.T) {
	m, _ := newTestMachine(t, "vm-close-canceled-ctx")
	m.tapName = "" // no TAP here; the wait behavior is what is under test

	const life = 600 * time.Millisecond

	// Stub Stop so it does NOT kill the "VMM": this isolates Close's own wait
	// from Stop's signaling, proving Close waits for the process by itself.
	m.stopFn = func(context.Context) error { return nil }
	m.newCommandFn = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "sleep 0.6; exit 0")
	}
	// Make the ready wait succeed immediately.
	m.dialVsockFn = func(uint32) (net.Conn, error) {
		c, s := net.Pipe()
		_ = s.Close()
		return c, nil
	}

	require.NoError(t, m.Start(context.Background()))

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	require.NoError(t, m.Close(canceled))
	elapsed := time.Since(start)

	require.GreaterOrEqual(t, elapsed, life/2,
		"Close returned before the VMM was reaped; TAP teardown would race the dying VMM")
	select {
	case <-m.done:
	default:
		t.Fatal("Close returned without the wait goroutine observing the VMM exit")
	}
}

// TestWaitForVMMExit covers the helper directly: nil done (Start never ran) and
// a closed done both return true; an open done times out instead of hanging.
func TestWaitForVMMExit(t *testing.T) {
	require.True(t, (&LinuxMachine{}).waitForVMMExit(time.Second),
		"a machine that never started has nothing to wait for")

	open := &LinuxMachine{done: make(chan struct{})}
	require.False(t, open.waitForVMMExit(30*time.Millisecond),
		"an exiting VMM must hit the bounded timeout")

	closed := &LinuxMachine{done: make(chan struct{})}
	close(closed.done)
	require.True(t, closed.waitForVMMExit(time.Second),
		"a reaped VMM must be observed immediately")
}
