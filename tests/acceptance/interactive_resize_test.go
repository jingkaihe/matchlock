//go:build acceptance

package acceptance

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/stretchr/testify/require"
)

const (
	// resizeMeasurePoll is how often the test re-issues `measure` while waiting
	// for the guest to observe a resize. Each poll is one small vsock write, so
	// keep it responsive.
	resizeMeasurePoll = 500 * time.Millisecond
	// resizeMeasureTimeout bounds how long the guest gets to observe a resize.
	// 30s proved too tight once under a loaded parallel acceptance run, so allow
	// 90s; the long bounded window only costs wall-clock on genuine failure.
	resizeMeasureTimeout = 90 * time.Second
)

// TestCLIRunInteractivePTYResize verifies a SIGWINCH-driven terminal resize is
// forwarded to the guest PTY, so an interactive full-screen app reflows.
//
// A guest-side handshake program requests its own measurement via `measure`, so
// we never mistake a command we echoed for an actual measurement. Flow:
//  1. pty.StartWithSize at 24x80
//  2. wait for INITIAL:24 80 and READY (guest boot)
//  3. pty.Setsize to 40x100 + SIGWINCH matchlock's process, then poll `measure`
//     until the guest reports MEASURE:40 100 (handling of the resize is async)
//  4. resize to 100x43, poll until MEASURE:100 43 (guards against accidental match)
//  5. send `quit`, await normal owner exit, close PTY, await copier
//
// Robustness notes (this test once failed under a loaded full acceptance run:
// the second resize, 100x43, was never observed while the first, 40x100, was):
//
//   - Frame desync (root cause fixed in pkg/vsock). The host wrote MsgTypeResize
//     and MsgTypeStdin from independent goroutines on the same vsock conn with
//     no serialization, so a competing header could split another frame and the
//     guest frame parser would read a bogus length and stop answering. That is
//     why a resize could go entirely unmeasured rather than merely late; the
//     per-session FrameWriter (one locked write per frame) removed the desync.
//   - Stale output. The wait used to search the cumulative output buffer, so a
//     MEASURE line produced before the current resize could satisfy the wait.
//     waitForMeasure now snapshots the buffer length before it issues `measure`
//     and only searches the suffix, so pre-resize output can never count.
//   - Bounded wait. Resize delivery is asynchronous (host -> vsock -> guest ->
//     SIGWINCH); under a loaded run it can take well over the old 30s cap.
//     resizeMeasureTimeout is 90s with a 500ms poll.
//
// This test intentionally does NOT call t.Parallel(). Go runs sequential tests
// to completion before any t.Parallel VM test is released for the same test
// binary, so keeping it sequential avoids competing with another VM for the
// acceptance vCPU/TAP budget. Do not add t.Parallel() back.
func TestCLIRunInteractivePTYResize(t *testing.T) {
	script := `stty -echo
printf 'INITIAL:'
stty size
printf 'READY\n'
while IFS= read -r action; do
    case "$action" in
        measure) printf 'MEASURE:'; stty size ;;
        quit) exit 0 ;;
    esac
done`

	args := withAcceptanceRunCPUs([]string{
		"run",
		"--image", "alpine:latest",
		"--no-network",
		"--rm",
		"-it",
		"--",
		"sh", "-c", script,
	})
	cmd := exec.Command(matchlockBin(t), args...)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	require.NoError(t, err, "failed to start interactive matchlock run")

	out := &lockedBuffer{}
	copyDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(out, ptmx)
		close(copyDone)
	}()

	// Exactly one goroutine calls cmd.Wait, saves the error, and closes exited.
	// Closing exited is a repeatable completion signal (unlike a buffered error
	// channel that cleanup would consume a second time and deadlock on).
	var (
		waitMu  sync.Mutex
		waitErr error
		exited  = make(chan struct{})
	)
	go func() {
		err := cmd.Wait()
		waitMu.Lock()
		waitErr = err
		waitMu.Unlock()
		close(exited)
	}()

	ownerPID := func() int {
		if cmd.Process != nil {
			return cmd.Process.Pid
		}
		return 0
	}

	cleanup := func() {
		// Only signal if the owner has not already finished.
		select {
		case <-exited:
			// already reaped; nothing to signal
		default:
			if pid := ownerPID(); pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGTERM)
			}
			select {
			case <-exited:
			case <-time.After(15 * time.Second):
				if pid := ownerPID(); pid > 0 {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
				select {
				case <-exited:
				case <-time.After(10 * time.Second):
				}
			}
		}
		_ = ptmx.Close()
		select {
		case <-copyDone:
		case <-time.After(10 * time.Second):
		}
	}
	defer cleanup()

	// 1. Boot handshake.
	require.True(t, waitForContains(out.String, "READY", 90*time.Second),
		"guest never became ready; output:\n%s", out.String())
	require.True(t, waitForContains(out.String, "INITIAL:24 80", 20*time.Second),
		"guest never reported initial 24x80; output:\n%s", out.String())
	t.Log("ready + initial 24x80 observed")

	// 2. Resize to 40x100 + SIGWINCH the matchlock session leader.
	require.NoError(t, pty.Setsize(ptmx, &pty.Winsize{Rows: 40, Cols: 100}))
	if pid := ownerPID(); pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGWINCH)
	}
	firstResizeAt := time.Now()
	require.True(t, waitForMeasure(out, ptmx, "MEASURE:40 100", resizeMeasureTimeout),
		"guest never reported 40x100 after waiting %s; output:\n%s", time.Since(firstResizeAt), out.String())
	t.Log("resize to 40x100 observed")

	// 3. Resize to 100x43 (different pair) to guard against accidental match.
	require.NoError(t, pty.Setsize(ptmx, &pty.Winsize{Rows: 100, Cols: 43}))
	if pid := ownerPID(); pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGWINCH)
	}
	secondResizeAt := time.Now()
	require.True(t, waitForMeasure(out, ptmx, "MEASURE:100 43", resizeMeasureTimeout),
		"guest never reported 100x43 after waiting %s; output:\n%s", time.Since(secondResizeAt), out.String())
	t.Log("resize to 100x43 observed")

	// 4. Graceful quit.
	_, _ = ptmx.Write([]byte("quit\n"))
	select {
	case <-exited:
		waitMu.Lock()
		defer waitMu.Unlock()
		require.NoError(t, waitErr, "owner returned an error on quit; output:\n%s", out.String())
		t.Log("owner exited cleanly")
	case <-time.After(20 * time.Second):
		t.Fatalf("owner did not exit after quit; output:\n%s", out.String())
	}
	_ = ptmx.Close()
	<-copyDone
}

// waitForMeasure repeatedly sends `measure` to the guest until a reply emitted
// after this call began contains the expected string, or the timeout elapses.
//
// Resize delivery is asynchronous, so a single measurement can legitimately
// observe the old size. The out.Len() snapshot taken before the first `measure`
// is what makes the wait strict: only output appended after the snapshot is
// eligible, so a MEASURE line produced before the current resize (for example by
// the boot handshake or an earlier resize's poll loop) can never satisfy it.
// This closes the stale-output false positive that let a resize go unverified.
func waitForMeasure(out *lockedBuffer, ptmx *os.File, want string, timeout time.Duration) bool {
	start := out.Len()
	wantLower := strings.ToLower(want)
	deadline := time.Now().Add(timeout)
	for {
		if strings.Contains(strings.ToLower(out.StringFrom(start)), wantLower) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		_, _ = ptmx.Write([]byte("measure\n"))
		time.Sleep(resizeMeasurePoll)
	}
}

func waitForContains(fn func() string, substr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(strings.ToLower(fn()), strings.ToLower(substr)) {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}
