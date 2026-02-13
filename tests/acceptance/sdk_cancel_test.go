//go:build acceptance

package acceptance

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecCancelKillsProcess(t *testing.T) {
	t.Parallel()
	client := launchAlpine(t)

	// Start a long-running sleep, then cancel it after 1s.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	start := time.Now()
	_, err := client.Exec(ctx, "sleep 60")
	elapsed := time.Since(start)

	require.Error(t, err, "expected error from cancelled exec")
	require.True(t, errors.Is(err, context.DeadlineExceeded), "expected DeadlineExceeded, got: %v", err)
	assert.Less(t, elapsed, 5*time.Second, "cancel took too long")
}

func TestExecStreamCancelKillsProcess(t *testing.T) {
	t.Parallel()
	client := launchAlpine(t)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	var stdout strings.Builder
	var stderr strings.Builder
	start := time.Now()
	_, err := client.ExecStream(ctx, "sleep 60", &stdout, &stderr)
	elapsed := time.Since(start)

	require.Error(t, err, "expected error from cancelled exec_stream")
	require.True(t, errors.Is(err, context.DeadlineExceeded), "expected DeadlineExceeded, got: %v", err)
	assert.Less(t, elapsed, 5*time.Second, "cancel took too long")
}

func TestExecCancelProcessActuallyDies(t *testing.T) {
	t.Parallel()
	client := launchAlpine(t)

	// Write a script that creates a marker file, sleeps, then removes it.
	// If cancellation kills the process, the marker should remain.
	script := `sh -c 'touch /tmp/cancel-marker && sleep 60 && rm /tmp/cancel-marker'`

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	client.Exec(ctx, script)

	// Wait for the process group SIGTERM to propagate and kill the sleep child.
	time.Sleep(2 * time.Second)

	// The marker file should still exist (sleep was killed before rm ran).
	result, err := client.Exec(context.Background(), "test -f /tmp/cancel-marker && echo exists")
	require.NoError(t, err, "check marker")
	assert.Equal(t, "exists", strings.TrimSpace(result.Stdout), "marker file missing - cancelled process was not killed")

	// The sleep process should no longer be running.
	// Use pgrep with -x for exact match on "sleep" to avoid matching the
	// sh -c wrapper that contains "sleep" in its command line.
	result, err = client.Exec(context.Background(), "pgrep -x sleep || echo gone")
	require.NoError(t, err, "check process")
	assert.Equal(t, "gone", strings.TrimSpace(result.Stdout), "sleep process still running after cancel: %s", result.Stdout)
}

func TestExecCancelGracefulShutdown(t *testing.T) {
	t.Parallel()
	client := launchAlpine(t)

	// Prove the guest agent sends SIGTERM before SIGKILL by observing timing.
	// A process that handles SIGTERM exits immediately; one that only dies to
	// SIGKILL takes >=5s (the cancelGracePeriod). We cancel after 1s and check
	// how long the process takes to die.
	//
	// "true; sleep 60" prevents busybox exec optimization so sh stays as PID 1
	// and sleep is a child process. The process-group SIGTERM kills sleep (not
	// PID 1, so no signal protection). If only SIGKILL were sent, sleep would
	// survive until the 5s grace period.

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	client.Exec(ctx, "true; sleep 60")

	// Check quickly - if SIGTERM worked, sleep dies within ~1s of cancel.
	// If only SIGKILL, it would take 5+ seconds.
	time.Sleep(1 * time.Second)

	result, err := client.Exec(context.Background(), "pgrep -x sleep || echo gone")
	require.NoError(t, err, "check process")
	assert.Equal(t, "gone", strings.TrimSpace(result.Stdout), "sleep still running 1s after cancel - SIGTERM may not be reaching child processes: %s", result.Stdout)
}

func TestExecManualCancelViaContext(t *testing.T) {
	t.Parallel()
	client := launchAlpine(t)

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(500 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := client.Exec(ctx, "sleep 60")
	elapsed := time.Since(start)

	require.ErrorIs(t, err, context.Canceled)
	assert.Less(t, elapsed, 5*time.Second, "cancel took too long")
}
