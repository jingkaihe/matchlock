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
			t.Fatalf("failed to run %s %v: %v", bin, args, err)
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

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start %s %v: %v", bin, args, err)
	}

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
		t.Fatalf("command timed out: %s %v", bin, args)
		return "", "", -1
	}
}

// ---------------------------------------------------------------------------
// version
// ---------------------------------------------------------------------------

func TestCLIVersion(t *testing.T) {
	stdout, _, exitCode := runCLI(t, "version")
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0", exitCode)
	}
	if !strings.HasPrefix(stdout, "matchlock ") {
		t.Errorf("stdout = %q, want prefix 'matchlock '", stdout)
	}
	if !strings.Contains(stdout, "commit:") {
		t.Errorf("stdout = %q, want to contain 'commit:'", stdout)
	}
}

func TestCLIVersionFlag(t *testing.T) {
	stdout, _, exitCode := runCLI(t, "--version")
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0", exitCode)
	}
	if !strings.Contains(stdout, "matchlock") {
		t.Errorf("stdout = %q, want to contain 'matchlock'", stdout)
	}
}

// ---------------------------------------------------------------------------
// build
// ---------------------------------------------------------------------------

func TestCLIPull(t *testing.T) {
	stdout, _, exitCode := runCLIWithTimeout(t, 5*time.Minute, "pull", "alpine:latest")
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stdout: %s", exitCode, stdout)
	}
	if !strings.Contains(stdout, "Digest:") {
		t.Errorf("expected pull output with Digest, got: %s", stdout)
	}
}

func TestCLIPullMissingImage(t *testing.T) {
	_, _, exitCode := runCLI(t, "pull")
	if exitCode == 0 {
		t.Errorf("expected non-zero exit code for missing image arg")
	}
}

func TestCLIBuildMissingContext(t *testing.T) {
	_, _, exitCode := runCLI(t, "build")
	if exitCode == 0 {
		t.Errorf("expected non-zero exit code for missing context arg")
	}
}

// ---------------------------------------------------------------------------
// run (with --rm, the default)
// ---------------------------------------------------------------------------

func TestCLIRunEchoHello(t *testing.T) {
	stdout, _, exitCode := runCLIWithTimeout(t, 2*time.Minute, "run", "--image", "alpine:latest", "echo", "hello")
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0", exitCode)
	}
	if !strings.Contains(stdout, "hello") {
		t.Errorf("stdout = %q, want to contain 'hello'", stdout)
	}
}

func TestCLIRunCatOsRelease(t *testing.T) {
	stdout, _, exitCode := runCLIWithTimeout(t, 2*time.Minute, "run", "--image", "alpine:latest", "cat", "/etc/os-release")
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0", exitCode)
	}
	if !strings.Contains(stdout, "Alpine") {
		t.Errorf("expected Alpine in os-release output, got: %s", stdout)
	}
}

func TestCLIRunMissingImage(t *testing.T) {
	_, _, exitCode := runCLI(t, "run", "echo", "hello")
	if exitCode == 0 {
		t.Errorf("expected non-zero exit code when --image is missing")
	}
}

func TestCLIRunNoCommand(t *testing.T) {
	// Alpine has CMD ["/bin/sh"], so running without user-provided args uses
	// the image default command and should succeed (exit 0).
	_, _, exitCode := runCLI(t, "run", "--image", "alpine:latest")
	if exitCode != 0 {
		t.Errorf("exit code = %d, want 0 (image CMD /bin/sh should be used)", exitCode)
	}
}

func TestCLIRunMultiWordCommand(t *testing.T) {
	// "--" separates matchlock flags from the guest command so cobra
	// doesn't interpret -c as a flag.
	stdout, _, exitCode := runCLIWithTimeout(t, 2*time.Minute, "run", "--image", "alpine:latest", "--", "sh", "-c", "echo foo bar")
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0", exitCode)
	}
	if !strings.Contains(stdout, "foo bar") {
		t.Errorf("stdout = %q, want to contain 'foo bar'", stdout)
	}
}

func TestCLIRunVolumeMountNestedGuestPath(t *testing.T) {
	hostDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostDir, "probe.txt"), []byte("mounted-nested-path"), 0644); err != nil {
		t.Fatalf("write probe file: %v", err)
	}

	stdout, stderr, exitCode := runCLIWithTimeout(
		t,
		2*time.Minute,
		"run",
		"--image", "alpine:latest",
		"-v", hostDir+":/workspace/not_exist_folder",
		"cat", "/workspace/not_exist_folder/probe.txt",
	)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", exitCode, stdout, stderr)
	}
	if got := strings.TrimSpace(stdout); got != "mounted-nested-path" {
		t.Errorf("stdout = %q, want %q", got, "mounted-nested-path")
	}
}

func TestCLIRunVolumeMountSingleFile(t *testing.T) {
	hostDir := t.TempDir()
	hostFile := filepath.Join(hostDir, "1file.txt")
	if err := os.WriteFile(hostFile, []byte("single-file-mounted"), 0644); err != nil {
		t.Fatalf("write host file: %v", err)
	}

	stdout, stderr, exitCode := runCLIWithTimeout(
		t,
		2*time.Minute,
		"run",
		"--image", "alpine:latest",
		"-v", hostFile+":/workspace/1file.txt",
		"--", "sh", "-c", "ls /workspace && cat /workspace/1file.txt",
	)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout: %s\nstderr: %s", exitCode, stdout, stderr)
	}
	if !strings.Contains(stdout, "1file.txt") {
		t.Errorf("stdout = %q, want to contain %q", stdout, "1file.txt")
	}
	if !strings.Contains(stdout, "single-file-mounted") {
		t.Errorf("stdout = %q, want to contain %q", stdout, "single-file-mounted")
	}
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
	if exitCode == 0 {
		t.Fatalf("exit code = %d, want non-zero", exitCode)
	}
	if !strings.Contains(stderr, "invalid volume mount") {
		t.Fatalf("stderr = %q, want to contain %q", stderr, "invalid volume mount")
	}
	if !strings.Contains(stderr, "must be within workspace") {
		t.Fatalf("stderr = %q, want to contain %q", stderr, "must be within workspace")
	}
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
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start run: %v", err)
	}
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
	if vmID == "" {
		cmd.Process.Kill()
		t.Fatalf("timed out waiting for sandbox to appear in list. stderr: %s", runStderr.String())
	}

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
		if exitCode != 0 {
			t.Fatalf("list exit code = %d", exitCode)
		}
		if !strings.Contains(stdout, vmID) {
			t.Errorf("list output should contain %s, got: %s", vmID, stdout)
		}
		if !strings.Contains(stdout, "running") {
			t.Errorf("list output should show 'running' status, got: %s", stdout)
		}
	})

	// --- list --running ---
	t.Run("list-running", func(t *testing.T) {
		stdout, _, exitCode := runCLI(t, "list", "--running")
		if exitCode != 0 {
			t.Fatalf("list --running exit code = %d", exitCode)
		}
		if !strings.Contains(stdout, vmID) {
			t.Errorf("list --running should contain %s, got: %s", vmID, stdout)
		}
	})

	// --- get ---
	t.Run("get", func(t *testing.T) {
		stdout, _, exitCode := runCLI(t, "get", vmID)
		if exitCode != 0 {
			t.Fatalf("get exit code = %d", exitCode)
		}
		var state map[string]interface{}
		if err := json.Unmarshal([]byte(stdout), &state); err != nil {
			t.Fatalf("get output is not valid JSON: %v\noutput: %s", err, stdout)
		}
		if state["id"] != vmID {
			t.Errorf("get id = %v, want %s", state["id"], vmID)
		}
		if state["status"] != "running" {
			t.Errorf("get status = %v, want running", state["status"])
		}
	})

	// --- exec ---
	t.Run("exec", func(t *testing.T) {
		stdout, _, exitCode := runCLIWithTimeout(t, 30*time.Second, "exec", vmID, "echo", "from-exec")
		if exitCode != 0 {
			t.Fatalf("exec exit code = %d", exitCode)
		}
		if !strings.Contains(stdout, "from-exec") {
			t.Errorf("exec stdout = %q, want to contain 'from-exec'", stdout)
		}
	})

	// --- exec multiple commands ---
	t.Run("exec-multi", func(t *testing.T) {
		stdout, _, exitCode := runCLIWithTimeout(t, 30*time.Second, "exec", vmID, "--", "sh", "-c", "echo one && echo two")
		if exitCode != 0 {
			t.Fatalf("exec exit code = %d", exitCode)
		}
		if !strings.Contains(stdout, "one") || !strings.Contains(stdout, "two") {
			t.Errorf("exec stdout = %q, want 'one' and 'two'", stdout)
		}
	})

	// --- kill ---
	t.Run("kill", func(t *testing.T) {
		stdout, _, exitCode := runCLI(t, "kill", vmID)
		if exitCode != 0 {
			t.Fatalf("kill exit code = %d", exitCode)
		}
		if !strings.Contains(stdout, "Killed") {
			t.Errorf("kill output = %q, want to contain 'Killed'", stdout)
		}

		// Wait for the process to die and status to update
		time.Sleep(3 * time.Second)

		// Verify it's no longer running
		stdout2, _, _ := runCLI(t, "list", "--running")
		if strings.Contains(stdout2, vmID) {
			t.Errorf("VM %s should not appear in running list after kill", vmID)
		}
	})

	// --- rm ---
	t.Run("rm", func(t *testing.T) {
		stdout, _, exitCode := runCLI(t, "rm", vmID)
		if exitCode != 0 {
			t.Fatalf("rm exit code = %d", exitCode)
		}
		if !strings.Contains(stdout, "Removed") {
			t.Errorf("rm output = %q, want to contain 'Removed'", stdout)
		}

		// Verify it's gone from list
		stdout2, _, _ := runCLI(t, "list")
		if strings.Contains(stdout2, vmID) {
			t.Errorf("VM %s should not appear in list after rm", vmID)
		}
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
	if exitCode == 0 {
		t.Errorf("expected non-zero exit code when no VM ID provided")
	}
	if !strings.Contains(stderr, "VM ID required") {
		t.Errorf("stderr = %q, want to contain 'VM ID required'", stderr)
	}
}

// ---------------------------------------------------------------------------
// rm (no args)
// ---------------------------------------------------------------------------

func TestCLIRmNoArgs(t *testing.T) {
	_, stderr, exitCode := runCLI(t, "rm")
	if exitCode == 0 {
		t.Errorf("expected non-zero exit code when no VM ID provided")
	}
	if !strings.Contains(stderr, "VM ID required") {
		t.Errorf("stderr = %q, want to contain 'VM ID required'", stderr)
	}
}

// ---------------------------------------------------------------------------
// exec (no args / missing VM)
// ---------------------------------------------------------------------------

func TestCLIExecNoArgs(t *testing.T) {
	_, _, exitCode := runCLI(t, "exec")
	if exitCode == 0 {
		t.Errorf("expected non-zero exit code when no args provided")
	}
}

func TestCLIExecNonExistentVM(t *testing.T) {
	_, stderr, exitCode := runCLI(t, "exec", "vm-nonexistent", "echo", "hi")
	if exitCode == 0 {
		t.Errorf("expected non-zero exit code for non-existent VM")
	}
	if !strings.Contains(stderr, "not found") {
		t.Errorf("stderr = %q, want to contain 'not found'", stderr)
	}
}

// ---------------------------------------------------------------------------
// prune (idempotent — just verify it runs)
// ---------------------------------------------------------------------------

func TestCLIPrune(t *testing.T) {
	stdout, _, exitCode := runCLI(t, "prune")
	if exitCode != 0 {
		t.Fatalf("prune exit code = %d", exitCode)
	}
	if !strings.Contains(stdout, "Pruned") {
		t.Errorf("prune output = %q, want to contain 'Pruned'", stdout)
	}
}

// ---------------------------------------------------------------------------
// run with --rm=false and no command (start only, then kill)
// ---------------------------------------------------------------------------

func TestCLIRunRmFalseNoCommand(t *testing.T) {
	bin := matchlockBin(t)

	cmd := exec.Command(bin, "run", "--image", "alpine:latest", "--rm=false")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start: %v", err)
	}
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

	if vmID == "" {
		t.Fatalf("timed out waiting for sandbox; stderr: %s", stderr.String())
	}

	// Verify we can exec into it
	stdout, _, exitCode := runCLIWithTimeout(t, 30*time.Second, "exec", vmID, "echo", "alive")
	if exitCode != 0 {
		t.Fatalf("exec exit code = %d", exitCode)
	}
	if !strings.Contains(stdout, "alive") {
		t.Errorf("exec stdout = %q, want 'alive'", stdout)
	}
}

// ---------------------------------------------------------------------------
// list (with alias)
// ---------------------------------------------------------------------------

func TestCLIListAlias(t *testing.T) {
	stdout, _, exitCode := runCLI(t, "ls")
	if exitCode != 0 {
		t.Fatalf("ls exit code = %d", exitCode)
	}
	if !strings.Contains(stdout, "ID") {
		t.Errorf("ls output should contain header, got: %s", stdout)
	}
}

// ---------------------------------------------------------------------------
// help
// ---------------------------------------------------------------------------

func TestCLIHelp(t *testing.T) {
	stdout, _, exitCode := runCLI(t, "--help")
	if exitCode != 0 {
		t.Fatalf("--help exit code = %d", exitCode)
	}
	for _, sub := range []string{"run", "exec", "build", "pull", "list", "get", "kill", "rm", "prune", "rpc", "version"} {
		if !strings.Contains(stdout, sub) {
			t.Errorf("help output should mention %q subcommand", sub)
		}
	}
}

func TestCLIRunHelp(t *testing.T) {
	stdout, _, exitCode := runCLI(t, "run", "--help")
	if exitCode != 0 {
		t.Fatalf("run --help exit code = %d", exitCode)
	}
	for _, flag := range []string{"--image", "--cpus", "--memory", "--timeout", "--disk-size", "--allow-host", "--secret", "--rm"} {
		if !strings.Contains(stdout, flag) {
			t.Errorf("run --help should mention %q flag", flag)
		}
	}
}

// ---------------------------------------------------------------------------
// kill --all (should succeed even with nothing running)
// ---------------------------------------------------------------------------

func TestCLIKillAll(t *testing.T) {
	_, _, exitCode := runCLI(t, "kill", "--all")
	if exitCode != 0 {
		t.Errorf("kill --all exit code = %d, want 0", exitCode)
	}
}

// ---------------------------------------------------------------------------
// rm --stopped (should succeed even with nothing stopped)
// ---------------------------------------------------------------------------

func TestCLIRmStopped(t *testing.T) {
	_, _, exitCode := runCLI(t, "rm", "--stopped")
	if exitCode != 0 {
		t.Errorf("rm --stopped exit code = %d, want 0", exitCode)
	}
}

// ---------------------------------------------------------------------------
// image ls / image rm (registry-cached images)
// ---------------------------------------------------------------------------

func TestCLIImageLsShowsHeader(t *testing.T) {
	stdout, _, exitCode := runCLI(t, "image", "ls")
	if exitCode != 0 {
		t.Fatalf("image ls exit code = %d", exitCode)
	}
	if !strings.Contains(stdout, "TAG") || !strings.Contains(stdout, "SOURCE") {
		t.Errorf("image ls should show header, got: %s", stdout)
	}
}

func TestCLIImageRmNonExistent(t *testing.T) {
	_, _, exitCode := runCLI(t, "image", "rm", "nonexistent:tag")
	if exitCode == 0 {
		t.Errorf("expected non-zero exit code for non-existent image")
	}
}

func TestCLIImageRmNoArgs(t *testing.T) {
	_, _, exitCode := runCLI(t, "image", "rm")
	if exitCode == 0 {
		t.Errorf("expected non-zero exit code when no tag provided")
	}
}

func TestCLIImagePullAndRm(t *testing.T) {
	const img = "alpine:latest"

	// Pull the image (ensures it's in the registry cache)
	_, _, exitCode := runCLIWithTimeout(t, 5*time.Minute, "pull", img)
	if exitCode != 0 {
		t.Fatalf("pull exit code = %d", exitCode)
	}

	// Verify it appears in image ls
	stdout, _, exitCode := runCLI(t, "image", "ls")
	if exitCode != 0 {
		t.Fatalf("image ls exit code = %d", exitCode)
	}
	if !strings.Contains(stdout, img) {
		t.Fatalf("image ls should contain %q, got:\n%s", img, stdout)
	}

	// Remove it
	stdout, _, exitCode = runCLI(t, "image", "rm", img)
	if exitCode != 0 {
		t.Fatalf("image rm exit code = %d, want 0", exitCode)
	}
	if !strings.Contains(stdout, "Removed") {
		t.Errorf("image rm output = %q, want to contain 'Removed'", stdout)
	}

	// Verify it's gone from image ls
	stdout, _, exitCode = runCLI(t, "image", "ls")
	if exitCode != 0 {
		t.Fatalf("image ls exit code = %d", exitCode)
	}
	if strings.Contains(stdout, img) {
		t.Errorf("image ls should not contain %q after rm, got:\n%s", img, stdout)
	}
}

func TestCLIImageRmIdempotent(t *testing.T) {
	const img = "alpine:latest"

	// Pull, then remove twice — second remove should fail
	runCLIWithTimeout(t, 5*time.Minute, "pull", img)

	_, _, exitCode := runCLI(t, "image", "rm", img)
	if exitCode != 0 {
		t.Fatalf("first image rm exit code = %d, want 0", exitCode)
	}

	_, _, exitCode = runCLI(t, "image", "rm", img)
	if exitCode == 0 {
		t.Errorf("second image rm should fail for already-removed image")
	}
}
