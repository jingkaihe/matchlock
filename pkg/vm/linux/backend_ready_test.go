//go:build linux

package linux

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jingkaihe/matchlock/pkg/vm"
)

// newTestMachine builds a LinuxMachine wired with temp paths so Start can run
// without touching real host state. Callers override the seams they need.
func newTestMachine(t *testing.T, id string) (*LinuxMachine, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "vmm.log")
	sockPath := filepath.Join(dir, "vsock.sock")
	return &LinuxMachine{
		id: id,
		config: &vm.VMConfig{
			ID:         id,
			NoNetwork:  true,
			SocketPath: sockPath,
			VsockPath:  sockPath,
			LogPath:    logPath,
			VsockCID:   3,
		},
	}, logPath
}

// TestStartSurfacesVMMExitLogAndStops drives the early-exit path through the
// injected command seam: the "VMM" rejects the config, writes the real reason
// to the log, and exits. Start must surface that text (not a generic ready
// timeout) and must still run the failure-path teardown.
func TestStartSurfacesVMMExitLogAndStops(t *testing.T) {
	m, logPath := newTestMachine(t, "vm-early-exit")

	stopCalls := 0
	m.stopFn = func(context.Context) error {
		stopCalls++
		return nil
	}
	m.newCommandFn = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		// Emulate Firecracker: print the config rejection to the log (its
		// stderr is redirected to m.config.LogPath by Start) and exit non-zero.
		return exec.CommandContext(ctx, "sh", "-c",
			"echo 'VM config error: The number of vCPUs must be between 1 and 32' >&2; exit 1")
	}

	err := m.Start(context.Background())
	require.Error(t, err, "VMM early exit must fail Start")
	assert.ErrorIs(t, err, ErrVMNotReady)
	assert.NotErrorIs(t, err, ErrVMReadyTimeout,
		"a dead VMM must not be reported as a ready-signal timeout")
	assert.Contains(t, err.Error(), "VM config error",
		"Start must surface the VMM log's config error")
	assert.Contains(t, err.Error(), "VMM exited before ready signal")
	assert.Equal(t, 1, stopCalls, "failure path must tear the machine down via Stop")

	// The reason really came from the VMM log, not a fallback message.
	logData, readErr := os.ReadFile(logPath)
	require.NoError(t, readErr)
	assert.Contains(t, string(logData), "VM config error")
}

// TestWaitForReadyEarlyExitUsesInjectedLogSeam exercises waitForReady directly
// with the injected exit/log seams, without spawning any process.
func TestWaitForReadyEarlyExitUsesInjectedLogSeam(t *testing.T) {
	m := &LinuxMachine{
		config: &vm.VMConfig{VsockPath: "/nonexistent/vsock.sock"},
		done:   make(chan struct{}),
		readVMMLogTailFn: func() string {
			return "VM config error: The number of vCPUs must be between 1 and 32"
		},
		dialVsockFn: func(uint32) (net.Conn, error) {
			return nil, errors.New("should not be dialed after exit")
		},
	}
	close(m.done)

	err := m.waitForReady(context.Background(), time.Second)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrVMNotReady)
	assert.NotErrorIs(t, err, ErrVMReadyTimeout)
	assert.Contains(t, err.Error(), "VM config error")
}

// TestWaitForReadyTimeoutForLiveVMM asserts a VMM that stays alive but never
// signals ready still reports ErrVMReadyTimeout.
func TestWaitForReadyTimeoutForLiveVMM(t *testing.T) {
	m := &LinuxMachine{
		config: &vm.VMConfig{VsockPath: "/nonexistent/vsock.sock"},
		done:   make(chan struct{}), // open: process never exits
		dialVsockFn: func(uint32) (net.Conn, error) {
			return nil, errors.New("connection refused")
		},
	}

	err := m.waitForReady(context.Background(), 300*time.Millisecond)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrVMReadyTimeout)
	assert.ErrorIs(t, err, ErrVMNotReady)
	assert.Contains(t, err.Error(), "timeout waiting for VM ready signal")
}

// TestWaitForReadySuccess asserts the successful ready path returns nil as soon
// as the ready port accepts.
func TestWaitForReadySuccess(t *testing.T) {
	m := &LinuxMachine{
		config: &vm.VMConfig{VsockPath: "/nonexistent/vsock.sock"},
		done:   make(chan struct{}), // still running
		dialVsockFn: func(uint32) (net.Conn, error) {
			c1, c2 := net.Pipe()
			_ = c2.Close()
			return c1, nil
		},
	}

	require.NoError(t, m.waitForReady(context.Background(), time.Second))
}

// TestStartSucceedsWhenReady asserts the success path returns nil and does not
// invoke the failure-only Stop seam.
func TestStartSucceedsWhenReady(t *testing.T) {
	m, _ := newTestMachine(t, "vm-ready")

	stopCalls := 0
	m.stopFn = func(context.Context) error {
		stopCalls++
		return nil
	}
	m.newCommandFn = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sleep", "30")
	}
	m.dialVsockFn = func(uint32) (net.Conn, error) {
		c1, c2 := net.Pipe()
		_ = c2.Close()
		return c1, nil
	}

	t.Cleanup(func() {
		// Stop is stubbed in this test, so reap the injected process directly.
		if m.cmd != nil && m.cmd.Process != nil {
			_ = m.cmd.Process.Kill()
			_ = m.Wait(context.Background())
		}
	})

	require.NoError(t, m.Start(context.Background()))
	assert.Equal(t, 0, stopCalls, "success path must not tear the machine down")
}

// TestReadLogTailReturnsEndOfFile verifies the real log reader keeps the tail
// (where the config error is written) and bounds how much it returns.
func TestReadLogTailReturnsEndOfFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vmm.log")
	const reason = "VM config error: The number of vCPUs must be between 1 and 32"
	content := "START-MARKER\n" + strings.Repeat("boring boot filler\n", 400) + reason
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	tail := readLogTail(path)
	assert.Contains(t, tail, reason)
	assert.LessOrEqual(t, len(tail), vmmLogTailBytes)
	assert.NotContains(t, tail, "START-MARKER", "only the tail of the log is returned")

	assert.Equal(t, "", readLogTail(""), "empty path yields no tail")
	assert.Equal(t, "", readLogTail(filepath.Join(dir, "missing.log")))
}
