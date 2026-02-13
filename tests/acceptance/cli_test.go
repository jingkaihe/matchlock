//go:build acceptance

package acceptance

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func matchlockBin(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("MATCHLOCK_BIN"); bin != "" {
		return bin
	}
	return "matchlock"
}

func runCLI(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	bin := matchlockBin(t)
	cmd := exec.Command(bin, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			require.NoError(t, err, "failed to run %s %v", bin, args)
		}
	}
	return stdout.String(), stderr.String(), exitCode
}

// runCLIWithTimeout runs the CLI with a timeout and returns stdout, stderr, exit code.
func runCLIWithTimeout(t *testing.T, timeout time.Duration, args ...string) (string, string, int) {
	t.Helper()
	bin := matchlockBin(t)
	cmd := exec.Command(bin, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	require.NoError(t, cmd.Start(), "failed to start %s %v", bin, args)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		exitCode := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			}
		}
		return stdout.String(), stderr.String(), exitCode
	case <-time.After(timeout):
		cmd.Process.Kill()
		require.Fail(t, "command timed out", "%s %v", bin, args)
		return "", "", -1
	}
}

// ---------------------------------------------------------------------------
// version
// ---------------------------------------------------------------------------

func TestCLIVersion(t *testing.T) {
	stdout, _, exitCode := runCLI(t, "version")
	require.Equal(t, 0, exitCode)
	assert.True(t, strings.HasPrefix(stdout, "matchlock "), "stdout = %q, want prefix 'matchlock '", stdout)
	assert.Contains(t, stdout, "commit:")
}

func TestCLIVersionFlag(t *testing.T) {
	stdout, _, exitCode := runCLI(t, "--version")
	require.Equal(t, 0, exitCode)
	assert.Contains(t, stdout, "matchlock")
}

// ---------------------------------------------------------------------------
// build
// ---------------------------------------------------------------------------

func TestCLIPull(t *testing.T) {
	stdout, _, exitCode := runCLIWithTimeout(t, 5*time.Minute, "pull", "alpine:latest")
	require.Equalf(t, 0, exitCode, "stdout: %s", stdout)
	assert.Contains(t, stdout, "Digest:")
}

func TestCLIPullMissingImage(t *testing.T) {
	_, _, exitCode := runCLI(t, "pull")
	assert.NotEqual(t, 0, exitCode, "expected non-zero exit code for missing image arg")
}

func TestCLIBuildMissingContext(t *testing.T) {
	_, _, exitCode := runCLI(t, "build")
	assert.NotEqual(t, 0, exitCode, "expected non-zero exit code for missing context arg")
}

// ---------------------------------------------------------------------------
// run (with --rm, the default)
// ---------------------------------------------------------------------------

func TestCLIRunEchoHello(t *testing.T) {
	stdout, _, exitCode := runCLIWithTimeout(t, 2*time.Minute, "run", "--image", "alpine:latest", "echo", "hello")
	require.Equal(t, 0, exitCode)
	assert.Contains(t, stdout, "hello")
}

func TestCLIRunCatOsRelease(t *testing.T) {
	stdout, _, exitCode := runCLIWithTimeout(t, 2*time.Minute, "run", "--image", "alpine:latest", "cat", "/etc/os-release")
	require.Equal(t, 0, exitCode)
	assert.Contains(t, stdout, "Alpine")
}

func TestCLIRunMissingImage(t *testing.T) {
	_, _, exitCode := runCLI(t, "run", "echo", "hello")
	assert.NotEqual(t, 0, exitCode, "expected non-zero exit code when --image is missing")
}

func TestCLIRunNoCommand(t *testing.T) {
	// Alpine has CMD ["/bin/sh"], so running without user-provided args uses
	// the image default command and should succeed (exit 0).
	_, _, exitCode := runCLI(t, "run", "--image", "alpine:latest")
	assert.Equal(t, 0, exitCode, "image CMD /bin/sh should be used")
}

func TestCLIRunMultiWordCommand(t *testing.T) {
	// "--" separates matchlock flags from the guest command so cobra
	// doesn't interpret -c as a flag.
	stdout, _, exitCode := runCLIWithTimeout(t, 2*time.Minute, "run", "--image", "alpine:latest", "--", "sh", "-c", "echo foo bar")
	require.Equal(t, 0, exitCode)
	assert.Contains(t, stdout, "foo bar")
}

func TestCLIRunVolumeMountNestedGuestPath(t *testing.T) {
	hostDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(hostDir, "probe.txt"), []byte("mounted-nested-path"), 0644), "write probe file")

	stdout, stderr, exitCode := runCLIWithTimeout(
		t,
		2*time.Minute,
		"run",
		"--image", "alpine:latest",
		"-v", hostDir+":/workspace/not_exist_folder",
		"cat", "/workspace/not_exist_folder/probe.txt",
	)
	require.Equal(t, 0, exitCode, "stdout: %s\nstderr: %s", stdout, stderr)
	assert.Equal(t, "mounted-nested-path", strings.TrimSpace(stdout))
}

func TestCLIRunVolumeMountSingleFile(t *testing.T) {
	hostDir := t.TempDir()
	hostFile := filepath.Join(hostDir, "1file.txt")
	require.NoError(t, os.WriteFile(hostFile, []byte("single-file-mounted"), 0644), "write host file")

	stdout, stderr, exitCode := runCLIWithTimeout(
		t,
		2*time.Minute,
		"run",
		"--image", "alpine:latest",
		"-v", hostFile+":/workspace/1file.txt",
		"--", "sh", "-c", "ls /workspace && cat /workspace/1file.txt",
	)
	require.Equal(t, 0, exitCode, "stdout: %s\nstderr: %s", stdout, stderr)
	assert.Contains(t, stdout, "1file.txt")
	assert.Contains(t, stdout, "single-file-mounted")
}

func TestCLIRunVolumeMountRejectsGuestPathOutsideWorkspace(t *testing.T) {
	hostDir := t.TempDir()

	_, stderr, exitCode := runCLIWithTimeout(
		t,
		2*time.Minute,
		"run",
		"--image", "alpine:latest",
		"--workspace", "/workspace/project",
		"-v", hostDir+":/workspace",
		"--", "true",
	)
	require.NotEqual(t, 0, exitCode)
	require.Contains(t, stderr, "invalid volume mount")
	require.Contains(t, stderr, "must be within workspace")
}

// ---------------------------------------------------------------------------
// run --rm=false + exec + kill + rm (full lifecycle)
// ---------------------------------------------------------------------------

func TestCLILifecycle(t *testing.T) {
	bin := matchlockBin(t)

	// Start a sandbox with --rm=false (it stays alive)
	cmd := exec.Command(bin, "run", "--image", "alpine:latest", "--rm=false")
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

// ---------------------------------------------------------------------------
// get (non-existent VM)
// ---------------------------------------------------------------------------

func TestCLIGetNonExistent(t *testing.T) {
	_, _, exitCode := runCLI(t, "get", "vm-nonexistent")
	// get on non-existent VM should still work (returns empty/error data)
	// but we mainly verify it doesn't crash
	_ = exitCode
}

// ---------------------------------------------------------------------------
// kill (no args)
// ---------------------------------------------------------------------------

func TestCLIKillNoArgs(t *testing.T) {
	_, stderr, exitCode := runCLI(t, "kill")
	assert.NotEqual(t, 0, exitCode, "expected non-zero exit code when no VM ID provided")
	assert.Contains(t, stderr, "VM ID required")
}

// ---------------------------------------------------------------------------
// rm (no args)
// ---------------------------------------------------------------------------

func TestCLIRmNoArgs(t *testing.T) {
	_, stderr, exitCode := runCLI(t, "rm")
	assert.NotEqual(t, 0, exitCode, "expected non-zero exit code when no VM ID provided")
	assert.Contains(t, stderr, "VM ID required")
}

// ---------------------------------------------------------------------------
// exec (no args / missing VM)
// ---------------------------------------------------------------------------

func TestCLIExecNoArgs(t *testing.T) {
	_, _, exitCode := runCLI(t, "exec")
	assert.NotEqual(t, 0, exitCode, "expected non-zero exit code when no args provided")
}

func TestCLIExecNonExistentVM(t *testing.T) {
	_, stderr, exitCode := runCLI(t, "exec", "vm-nonexistent", "echo", "hi")
	assert.NotEqual(t, 0, exitCode, "expected non-zero exit code for non-existent VM")
	assert.Contains(t, stderr, "not found")
}

// ---------------------------------------------------------------------------
// prune (idempotent — just verify it runs)
// ---------------------------------------------------------------------------

func TestCLIPrune(t *testing.T) {
	stdout, _, exitCode := runCLI(t, "prune")
	require.Equal(t, 0, exitCode)
	assert.Contains(t, stdout, "Pruned")
}

// ---------------------------------------------------------------------------
// run with --rm=false and no command (start only, then kill)
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// list (with alias)
// ---------------------------------------------------------------------------

func TestCLIListAlias(t *testing.T) {
	stdout, _, exitCode := runCLI(t, "ls")
	require.Equal(t, 0, exitCode)
	assert.Contains(t, stdout, "ID")
}

// ---------------------------------------------------------------------------
// help
// ---------------------------------------------------------------------------

func TestCLIHelp(t *testing.T) {
	stdout, _, exitCode := runCLI(t, "--help")
	require.Equal(t, 0, exitCode)
	for _, sub := range []string{"run", "exec", "build", "pull", "list", "get", "kill", "rm", "prune", "rpc", "version"} {
		assert.Containsf(t, stdout, sub, "help output should mention %q subcommand", sub)
	}
}

func TestCLIRunHelp(t *testing.T) {
	stdout, _, exitCode := runCLI(t, "run", "--help")
	require.Equal(t, 0, exitCode)
	for _, flag := range []string{"--image", "--cpus", "--memory", "--timeout", "--disk-size", "--allow-host", "--secret", "--rm"} {
		assert.Containsf(t, stdout, flag, "run --help should mention %q flag", flag)
	}
}

// ---------------------------------------------------------------------------
// kill --all (should succeed even with nothing running)
// ---------------------------------------------------------------------------

func TestCLIKillAll(t *testing.T) {
	_, _, exitCode := runCLI(t, "kill", "--all")
	assert.Equal(t, 0, exitCode)
}

// ---------------------------------------------------------------------------
// rm --stopped (should succeed even with nothing stopped)
// ---------------------------------------------------------------------------

func TestCLIRmStopped(t *testing.T) {
	_, _, exitCode := runCLI(t, "rm", "--stopped")
	assert.Equal(t, 0, exitCode)
}

// ---------------------------------------------------------------------------
// image ls / image rm (registry-cached images)
// ---------------------------------------------------------------------------

func TestCLIImageLsShowsHeader(t *testing.T) {
	stdout, _, exitCode := runCLI(t, "image", "ls")
	require.Equal(t, 0, exitCode)
	assert.Contains(t, stdout, "TAG")
	assert.Contains(t, stdout, "SOURCE")
}

func TestCLIImageRmNonExistent(t *testing.T) {
	_, _, exitCode := runCLI(t, "image", "rm", "nonexistent:tag")
	assert.NotEqual(t, 0, exitCode, "expected non-zero exit code for non-existent image")
}

func TestCLIImageRmNoArgs(t *testing.T) {
	_, _, exitCode := runCLI(t, "image", "rm")
	assert.NotEqual(t, 0, exitCode, "expected non-zero exit code when no tag provided")
}

func TestCLIImagePullAndRm(t *testing.T) {
	const img = "alpine:latest"

	// Pull the image (ensures it's in the registry cache)
	_, _, exitCode := runCLIWithTimeout(t, 5*time.Minute, "pull", img)
	require.Equal(t, 0, exitCode)

	// Verify it appears in image ls
	stdout, _, exitCode := runCLI(t, "image", "ls")
	require.Equal(t, 0, exitCode)
	require.Containsf(t, stdout, img, "image ls should contain %q", img)

	// Remove it
	stdout, _, exitCode = runCLI(t, "image", "rm", img)
	require.Equal(t, 0, exitCode)
	assert.Contains(t, stdout, "Removed")

	// Verify it's gone from image ls
	stdout, _, exitCode = runCLI(t, "image", "ls")
	require.Equal(t, 0, exitCode)
	assert.NotContainsf(t, stdout, img, "image ls should not contain %q after rm", img)
}

func TestCLIImageRmIdempotent(t *testing.T) {
	const img = "alpine:latest"

	// Pull, then remove twice — second remove should fail
	runCLIWithTimeout(t, 5*time.Minute, "pull", img)

	_, _, exitCode := runCLI(t, "image", "rm", img)
	require.Equal(t, 0, exitCode)

	_, _, exitCode = runCLI(t, "image", "rm", img)
	assert.NotEqual(t, 0, exitCode, "second image rm should fail for already-removed image")
}
