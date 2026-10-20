//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readProcState(t *testing.T, pid int) byte {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	require.NoError(t, err)
	end := strings.LastIndexByte(string(data), ')')
	require.Greater(t, end+2, 0)
	return data[end+2]
}

func waitForZombie(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if state := readProcState(t, pid); state == 'Z' {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d never became a zombie", pid)
}

// TestIsProcessRunningIgnoresZombies locks in the core defect: an unreaped
// (zombie) child still answers kill(pid, 0), but must never be reported as
// running.
func TestIsProcessRunningIgnoresZombies(t *testing.T) {
	child := exec.Command("/bin/sh", "-c", "exit 7")
	require.NoError(t, child.Start())

	waitForZombie(t, child.Process.Pid)

	// The old predicate (kill(pid, 0)) treats a zombie as alive.
	assert.NoError(t, syscall.Kill(child.Process.Pid, 0))
	assert.False(t, isProcessRunning(child.Process.Pid), "a zombie must not count as running")
	assert.True(t, processIsZombie(child.Process.Pid))

	// Reap it so the test does not leak a zombie; Wait returns the exit status.
	assert.Error(t, child.Wait())
}

func TestProcessIsZombieFalseForRunningProcess(t *testing.T) {
	assert.False(t, processIsZombie(os.Getpid()))
}

func TestProcessIsZombieTrueForVanishedProcess(t *testing.T) {
	child := exec.Command("/bin/sh", "-c", "exit 0")
	require.NoError(t, child.Run())
	assert.True(t, processIsZombie(child.Process.Pid))
}

// TestStartDetachedChildReapsEarlyExit proves the stub child is reaped exactly
// once (no lingering /proc entry / zombie) while the error still surfaces.
func TestStartDetachedChildReapsEarlyExit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	pidFile := home + "/child.pid"
	script := fmt.Sprintf("echo $$ > %q; echo 'reaped boom' >&2; exit 9", pidFile)

	err := startDetachedChild("/bin/sh", []string{"-c", script}, 30*time.Second)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDetachedRunExited)
	assert.Contains(t, err.Error(), "reaped boom")

	raw, readErr := os.ReadFile(pidFile)
	require.NoError(t, readErr)
	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, parseErr)
	require.Greater(t, pid, 0)

	_, statErr := os.Stat(fmt.Sprintf("/proc/%d", pid))
	assert.True(t, os.IsNotExist(statErr), "detached child pid=%d must be reaped, not left as a zombie", pid)
}
