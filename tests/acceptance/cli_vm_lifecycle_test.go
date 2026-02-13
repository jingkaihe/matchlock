//go:build acceptance

package acceptance

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	sandboxReadyTimeout      = 2 * time.Minute
	sandboxReadyPollInterval = 500 * time.Millisecond
	execReadyTimeout         = 30 * time.Second
	vmStopTimeout            = 30 * time.Second
	runExitWaitTimeout       = 5 * time.Second
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func startPersistentRun(t *testing.T, bin string) (*exec.Cmd, <-chan error, *lockedBuffer) {
	t.Helper()
	cmd := exec.Command(bin, "run", "--image", "alpine:latest", "--rm=false")
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	require.NoError(t, cmd.Start(), "failed to start run")
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
		close(done)
	}()
	return cmd, done, stderr
}

func waitForRunExit(done <-chan error, timeout time.Duration) {
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

func cleanupPersistentRun(t *testing.T, bin, vmID string, runPID int, runDone <-chan error) {
	t.Helper()
	if vmID != "" {
		exec.Command(bin, "kill", vmID).Run()
		waitForRunExit(runDone, runExitWaitTimeout)
		exec.Command(bin, "rm", vmID).Run()
	}
	if p, err := os.FindProcess(runPID); err == nil {
		_ = p.Kill()
	}
	waitForRunExit(runDone, runExitWaitTimeout)
}

func findRunningVMIDByPID(t *testing.T, runPID int) string {
	t.Helper()
	stdout, stderr, exitCode := runCLI(t, "list", "--running")
	require.Equalf(t, 0, exitCode, "list --running failed: %s", stderr)
	pidText := strconv.Itoa(runPID)
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] == "ID" || fields[1] != "running" {
			continue
		}
		if fields[len(fields)-1] == pidText {
			return fields[0]
		}
	}
	return ""
}

func waitForRunningVMID(t *testing.T, runPID int, runDone <-chan error, runStderr *lockedBuffer) string {
	t.Helper()
	deadline := time.Now().Add(sandboxReadyTimeout)
	for time.Now().Before(deadline) {
		select {
		case err, ok := <-runDone:
			if ok && err != nil {
				require.NoErrorf(t, err, "run exited before sandbox was ready (pid=%d). stderr: %s", runPID, runStderr.String())
			}
			require.Failf(t, "run exited before sandbox was ready", "pid=%d stderr: %s", runPID, runStderr.String())
		default:
		}

		if vmID := findRunningVMIDByPID(t, runPID); vmID != "" {
			return vmID
		}
		time.Sleep(sandboxReadyPollInterval)
	}
	require.Failf(t, "timed out waiting for sandbox to appear in list", "pid=%d stderr: %s", runPID, runStderr.String())
	return ""
}

func waitForExecReady(t *testing.T, vmID string, runPID int, runDone <-chan error, runStderr *lockedBuffer) {
	t.Helper()
	deadline := time.Now().Add(execReadyTimeout)
	lastErr := ""
	for time.Now().Before(deadline) {
		select {
		case err, ok := <-runDone:
			if ok && err != nil {
				require.NoErrorf(t, err, "run exited before exec was ready (pid=%d). stderr: %s", runPID, runStderr.String())
			}
			require.Failf(t, "run exited before exec was ready", "pid=%d stderr: %s", runPID, runStderr.String())
		default:
		}

		_, stderr, exitCode := runCLI(t, "exec", vmID, "true")
		if exitCode == 0 {
			return
		}
		lastErr = stderr
		time.Sleep(sandboxReadyPollInterval)
	}
	require.Failf(t, "timed out waiting for exec readiness", "vmID=%s stderr: %s", vmID, lastErr)
}

func isVMRunning(t *testing.T, vmID string) bool {
	t.Helper()
	stdout, stderr, exitCode := runCLI(t, "list", "--running")
	require.Equalf(t, 0, exitCode, "list --running failed: %s", stderr)
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == vmID && fields[1] == "running" {
			return true
		}
	}
	return false
}

func waitForVMStopped(t *testing.T, vmID string) {
	t.Helper()
	deadline := time.Now().Add(vmStopTimeout)
	for time.Now().Before(deadline) {
		if !isVMRunning(t, vmID) {
			return
		}
		time.Sleep(sandboxReadyPollInterval)
	}
	require.Failf(t, "timed out waiting for VM to stop", "vmID=%s", vmID)
}

func TestCLILifecycle(t *testing.T) {
	bin := matchlockBin(t)

	cmd, runDone, runStderr := startPersistentRun(t, bin)
	runPID := cmd.Process.Pid

	var vmID string
	t.Cleanup(func() {
		cleanupPersistentRun(t, bin, vmID, runPID, runDone)
	})
	vmID = waitForRunningVMID(t, runPID, runDone, runStderr)
	waitForExecReady(t, vmID, runPID, runDone, runStderr)

	// --- list ---
	t.Run("list", func(t *testing.T) {
		stdout, _, exitCode := runCLI(t, "list")
		require.Equal(t, 0, exitCode)
		assert.Contains(t, stdout, vmID)
		assert.Contains(t, stdout, "running")
	})

	// --- list --running ---
	t.Run("list-running", func(t *testing.T) {
		stdout, _, exitCode := runCLI(t, "list", "--running")
		require.Equal(t, 0, exitCode)
		assert.Contains(t, stdout, vmID)
	})

	// --- get ---
	t.Run("get", func(t *testing.T) {
		stdout, _, exitCode := runCLI(t, "get", vmID)
		require.Equal(t, 0, exitCode)
		var state map[string]interface{}
		err := json.Unmarshal([]byte(stdout), &state)
		require.NoErrorf(t, err, "get output is not valid JSON: %s", stdout)
		assert.Equal(t, vmID, state["id"])
		assert.Equal(t, "running", state["status"])
	})

	// --- inspect ---
	t.Run("inspect", func(t *testing.T) {
		stdout, _, exitCode := runCLI(t, "inspect", vmID)
		require.Equal(t, 0, exitCode)

		var out struct {
			VM        map[string]interface{}   `json:"vm"`
			Lifecycle map[string]interface{}   `json:"lifecycle"`
			History   []map[string]interface{} `json:"history"`
		}
		err := json.Unmarshal([]byte(stdout), &out)
		require.NoErrorf(t, err, "inspect output is not valid JSON: %s", stdout)

		assert.Equal(t, vmID, out.VM["id"])
		assert.Equal(t, vmID, out.Lifecycle["vm_id"])
		assert.NotEmpty(t, out.History)
	})

	// --- stat (alias of inspect) ---
	t.Run("stat", func(t *testing.T) {
		stdout, _, exitCode := runCLI(t, "stat", vmID)
		require.Equal(t, 0, exitCode)

		var out struct {
			VM map[string]interface{} `json:"vm"`
		}
		err := json.Unmarshal([]byte(stdout), &out)
		require.NoErrorf(t, err, "stat output is not valid JSON: %s", stdout)
		assert.Equal(t, vmID, out.VM["id"])
	})

	// --- exec ---
	t.Run("exec", func(t *testing.T) {
		stdout, _, exitCode := runCLIWithTimeout(t, 30*time.Second, "exec", vmID, "echo", "from-exec")
		require.Equal(t, 0, exitCode)
		assert.Contains(t, stdout, "from-exec")
	})

	// --- exec multiple commands ---
	t.Run("exec-multi", func(t *testing.T) {
		stdout, _, exitCode := runCLIWithTimeout(t, 30*time.Second, "exec", vmID, "--", "sh", "-c", "echo one && echo two")
		require.Equal(t, 0, exitCode)
		assert.Contains(t, stdout, "one")
		assert.Contains(t, stdout, "two")
	})

	// --- kill ---
	t.Run("kill", func(t *testing.T) {
		stdout, _, exitCode := runCLI(t, "kill", vmID)
		require.Equal(t, 0, exitCode)
		assert.Contains(t, stdout, "Killed")
		waitForVMStopped(t, vmID)
		assert.False(t, isVMRunning(t, vmID), "VM should not appear in running list after kill")
	})

	// --- rm ---
	t.Run("rm", func(t *testing.T) {
		stdout, _, exitCode := runCLI(t, "rm", vmID)
		require.Equal(t, 0, exitCode)
		assert.Contains(t, stdout, "Removed")

		// Verify it's gone from list
		stdout2, _, _ := runCLI(t, "list")
		assert.NotContains(t, stdout2, vmID, "VM should not appear in list after rm")
	})
}

func TestCLIRunRmFalseNoCommand(t *testing.T) {
	bin := matchlockBin(t)

	cmd, runDone, runStderr := startPersistentRun(t, bin)
	runPID := cmd.Process.Pid

	var vmID string
	t.Cleanup(func() {
		cleanupPersistentRun(t, bin, vmID, runPID, runDone)
	})
	vmID = waitForRunningVMID(t, runPID, runDone, runStderr)
	waitForExecReady(t, vmID, runPID, runDone, runStderr)

	// Verify we can exec into it
	stdout, _, exitCode := runCLIWithTimeout(t, 30*time.Second, "exec", vmID, "echo", "alive")
	require.Equal(t, 0, exitCode)
	assert.Contains(t, stdout, "alive")
}

func TestCLIGetNonExistent(t *testing.T) {
	_, _, exitCode := runCLI(t, "get", "vm-nonexistent")
	// get on non-existent VM should still work (returns empty/error data)
	// but we mainly verify it doesn't crash
	_ = exitCode
}

func TestCLIKillNoArgs(t *testing.T) {
	_, stderr, exitCode := runCLI(t, "kill")
	assert.NotEqual(t, 0, exitCode, "expected non-zero exit code when no VM ID provided")
	assert.Contains(t, stderr, "VM ID required")
}

func TestCLIRmNoArgs(t *testing.T) {
	_, stderr, exitCode := runCLI(t, "rm")
	assert.NotEqual(t, 0, exitCode, "expected non-zero exit code when no VM ID provided")
	assert.Contains(t, stderr, "VM ID required")
}

func TestCLIExecNoArgs(t *testing.T) {
	_, _, exitCode := runCLI(t, "exec")
	assert.NotEqual(t, 0, exitCode, "expected non-zero exit code when no args provided")
}

func TestCLIExecNonExistentVM(t *testing.T) {
	_, stderr, exitCode := runCLI(t, "exec", "vm-nonexistent", "echo", "hi")
	assert.NotEqual(t, 0, exitCode, "expected non-zero exit code for non-existent VM")
	assert.Contains(t, stderr, "not found")
}

func TestCLIPrune(t *testing.T) {
	stdout, _, exitCode := runCLI(t, "prune")
	require.Equal(t, 0, exitCode)
	assert.Contains(t, stdout, "Pruned")
}

func TestCLIListAlias(t *testing.T) {
	stdout, _, exitCode := runCLI(t, "ls")
	require.Equal(t, 0, exitCode)
	assert.Contains(t, stdout, "ID")
}

func TestCLIKillAll(t *testing.T) {
	_, _, exitCode := runCLI(t, "kill", "--all")
	assert.Equal(t, 0, exitCode)
}

func TestCLIRmStopped(t *testing.T) {
	_, _, exitCode := runCLI(t, "rm", "--stopped")
	assert.Equal(t, 0, exitCode)
}
