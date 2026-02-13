//go:build acceptance

package acceptance

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLILifecycle(t *testing.T) {
	bin := matchlockBin(t)

	// Start a sandbox with --rm=false (it stays alive)
	cmd := exec.Command(bin, "run", "--image", "alpine:latest", "--rm=false", "-e", "VISIBLE_ENV=from-run")
	var runStderr strings.Builder
	cmd.Stderr = &runStderr
	require.NoError(t, cmd.Start(), "failed to start run")
	runPID := cmd.Process.Pid

	// Wait for the sandbox to register and become visible in "list"
	var vmID string
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		stdout, _, _ := runCLI(t, "list", "--running")
		for _, line := range strings.Split(stdout, "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && strings.HasPrefix(fields[0], "vm-") && fields[1] == "running" {
				vmID = fields[0]
				break
			}
		}
		if vmID != "" {
			break
		}
	}
	require.NotEmptyf(t, vmID, "timed out waiting for sandbox to appear in list. stderr: %s", runStderr.String())

	t.Cleanup(func() {
		// Ensure cleanup even if test fails partway
		exec.Command(bin, "kill", vmID).Run()
		// Wait for the run process to exit after kill
		time.Sleep(2 * time.Second)
		exec.Command(bin, "rm", vmID).Run()
		// Kill the process if it's still alive
		if p, err := os.FindProcess(runPID); err == nil {
			p.Kill()
		}
	})

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

		cfg, ok := state["config"].(map[string]interface{})
		require.True(t, ok, "state.config should be an object")
		env, ok := cfg["env"].(map[string]interface{})
		require.True(t, ok, "state.config.env should be an object")
		assert.Equal(t, "from-run", env["VISIBLE_ENV"])
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

		cfg, ok := out.VM["config"].(map[string]interface{})
		require.True(t, ok, "vm.config should be an object")
		env, ok := cfg["env"].(map[string]interface{})
		require.True(t, ok, "vm.config.env should be an object")
		assert.Equal(t, "from-run", env["VISIBLE_ENV"])
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

		// Wait for the process to die and status to update
		time.Sleep(3 * time.Second)

		// Verify it's no longer running
		stdout2, _, _ := runCLI(t, "list", "--running")
		assert.NotContains(t, stdout2, vmID, "VM should not appear in running list after kill")
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

	cmd := exec.Command(bin, "run", "--image", "alpine:latest", "--rm=false")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Start(), "failed to start")
	runPID := cmd.Process.Pid

	// Wait for the sandbox to come up
	var vmID string
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		stdout, _, _ := runCLI(t, "list", "--running")
		for _, line := range strings.Split(stdout, "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && strings.HasPrefix(fields[0], "vm-") && fields[1] == "running" {
				vmID = fields[0]
				break
			}
		}
		if vmID != "" {
			break
		}
	}

	t.Cleanup(func() {
		if vmID != "" {
			exec.Command(bin, "kill", vmID).Run()
			time.Sleep(2 * time.Second)
			exec.Command(bin, "rm", vmID).Run()
		}
		if p, err := os.FindProcess(runPID); err == nil {
			p.Kill()
		}
	})

	require.NotEmptyf(t, vmID, "timed out waiting for sandbox; stderr: %s", stderr.String())

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
