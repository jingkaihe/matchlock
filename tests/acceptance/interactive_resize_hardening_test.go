//go:build acceptance

package acceptance

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// These tests exercise the offset/snapshot hardening of waitForMeasure without
// a VM. They run in the acceptance package because waitForMeasure and
// lockedBuffer live here, but they need no sandbox backend, so they are cheap
// and can be selected with `-run TestWaitForMeasure`.

// TestLockedBufferStringFrom pins the offset helper contract: StringFrom(0) is
// the whole buffer, StringFrom(Len()) is empty, and a mid-buffer offset yields
// exactly the appended suffix.
func TestLockedBufferStringFrom(t *testing.T) {
	b := &lockedBuffer{}
	_, _ = b.Write([]byte("MEASURE:40 100\nMEASURE:100 43\n"))

	require.Equal(t, 30, b.Len())
	require.Equal(t, "MEASURE:40 100\nMEASURE:100 43\n", b.StringFrom(0))
	require.Equal(t, "MEASURE:100 43\n", b.StringFrom(15))
	require.Equal(t, "", b.StringFrom(b.Len()))
	require.Equal(t, "", b.StringFrom(b.Len()+100))
}

// TestWaitForMeasureIgnoresStaleOutput is the regression for the stale-output
// false positive: a MEASURE line already in the cumulative buffer before the
// current resize was requested must not satisfy the wait. The old
// implementation searched the whole buffer and would have returned true
// immediately; the hardened one must keep polling until its bounded timeout.
func TestWaitForMeasureIgnoresStaleOutput(t *testing.T) {
	out := &lockedBuffer{}
	// Simulate a measurement left behind by an earlier resize or the boot
	// handshake. It matches the string the test is about to wait for.
	_, _ = out.Write([]byte("MEASURE:100 43\n"))

	pr, pw, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = pr.Close()
		_ = pw.Close()
	})

	measureRequests := make(chan struct{}, 8)
	go func() {
		buf := make([]byte, 256)
		for {
			n, readErr := pr.Read(buf)
			if n > 0 {
				select {
				case measureRequests <- struct{}{}:
				default:
				}
			}
			if readErr != nil {
				return
			}
		}
	}()

	const timeout = 1200 * time.Millisecond
	start := time.Now()
	ok := waitForMeasure(out, pw, "MEASURE:100 43", timeout)
	elapsed := time.Since(start)

	require.False(t, ok,
		"a MEASURE line emitted before the resize must not satisfy the wait")
	require.GreaterOrEqual(t, elapsed, timeout,
		"the wait must poll until the timeout instead of short-circuiting on stale output")
	select {
	case <-measureRequests:
	case <-time.After(2 * time.Second):
		require.Fail(t, "waitForMeasure never issued a `measure` request")
	}
}

// TestWaitForMeasureAcceptsFreshOutput proves the offset does not break the
// happy path: a MEASURE reply produced after waitForMeasure issues `measure` is
// found even when the buffer already holds an unrelated, stale measurement.
func TestWaitForMeasureAcceptsFreshOutput(t *testing.T) {
	out := &lockedBuffer{}
	_, _ = out.Write([]byte("MEASURE:40 100\n"))

	pr, pw, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = pr.Close()
		_ = pw.Close()
	})

	go func() {
		buf := make([]byte, 256)
		for {
			n, readErr := pr.Read(buf)
			if n > 0 {
				// The guest answers the current resize only after the
				// `measure` request is written, i.e. after the snapshot.
				_, _ = out.Write([]byte("MEASURE:100 43\n"))
				return
			}
			if readErr != nil {
				return
			}
		}
	}()

	require.True(t, waitForMeasure(out, pw, "MEASURE:100 43", 5*time.Second),
		"a MEASURE reply emitted after the request must satisfy the wait; output:\n%s", out.String())
}
