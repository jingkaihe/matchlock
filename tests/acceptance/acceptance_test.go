//go:build acceptance

package acceptance

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/sdk"
)

func matchlockConfig(t *testing.T) sdk.Config {
	t.Helper()
	cfg := sdk.DefaultConfig()
	if os.Getenv("MATCHLOCK_BIN") == "" {
		cfg.BinaryPath = "matchlock"
	}
	return cfg
}

func launchAlpine(t *testing.T) *sdk.Client {
	t.Helper()
	client, err := sdk.NewClient(matchlockConfig(t))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	t.Cleanup(func() {
		client.Close(0)
		client.Remove()
	})

	sandbox := sdk.New("alpine:latest")
	_, err = client.Launch(sandbox)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	return client
}

func TestExecSimpleCommand(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "echo hello")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", result.ExitCode)
	}
	if got := strings.TrimSpace(result.Stdout); got != "hello" {
		t.Errorf("stdout = %q, want %q", got, "hello")
	}
}

func TestExecNonZeroExit(t *testing.T) {
	t.Skip("known bug: guest agent does not propagate non-zero exit codes")

	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "false")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode == 0 {
		t.Errorf("exit code = 0, want non-zero")
	}
}

func TestExecFailedCommandStderr(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "cat /nonexistent_file_abc123")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(result.Stderr, "No such file or directory") {
		t.Errorf("stderr = %q, want to contain 'No such file or directory'", result.Stderr)
	}
}

func TestExecStderr(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "sh -c 'echo err >&2'")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(result.Stderr, "err") {
		t.Errorf("stderr = %q, want to contain %q", result.Stderr, "err")
	}
}

func TestExecMultipleCommands(t *testing.T) {
	client := launchAlpine(t)

	for i, cmd := range []string{"echo one", "echo two", "echo three"} {
		result, err := client.Exec(context.Background(), cmd)
		if err != nil {
			t.Fatalf("Exec[%d]: %v", i, err)
		}
		if result.ExitCode != 0 {
			t.Errorf("Exec[%d] exit code = %d, want 0", i, result.ExitCode)
		}
	}
}

func TestExecStream(t *testing.T) {
	client := launchAlpine(t)

	var stdout, stderr bytes.Buffer
	result, err := client.ExecStream(context.Background(), "echo streamed", &stdout, &stderr)
	if err != nil {
		t.Fatalf("ExecStream: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", result.ExitCode)
	}
	if got := strings.TrimSpace(stdout.String()); got != "streamed" {
		t.Errorf("stdout = %q, want %q", got, "streamed")
	}
}

func TestWriteAndReadFile(t *testing.T) {
	client := launchAlpine(t)

	content := []byte("hello from acceptance test")
	if err := client.WriteFile(context.Background(), "/workspace/test.txt", content); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := client.ReadFile(context.Background(), "/workspace/test.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("ReadFile = %q, want %q", got, content)
	}
}

func TestWriteFileAndExec(t *testing.T) {
	client := launchAlpine(t)

	script := []byte("#!/bin/sh\necho script-output\n")
	if err := client.WriteFileMode(context.Background(), "/workspace/run.sh", script, 0755); err != nil {
		t.Fatalf("WriteFileMode: %v", err)
	}

	result, err := client.Exec(context.Background(), "sh /workspace/run.sh")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := strings.TrimSpace(result.Stdout); got != "script-output" {
		t.Errorf("stdout = %q, want %q", got, "script-output")
	}
}

func TestListFiles(t *testing.T) {
	client := launchAlpine(t)

	if err := client.WriteFile(context.Background(), "/workspace/a.txt", []byte("a")); err != nil {
		t.Fatalf("WriteFile a: %v", err)
	}
	if err := client.WriteFile(context.Background(), "/workspace/b.txt", []byte("bb")); err != nil {
		t.Fatalf("WriteFile b: %v", err)
	}

	files, err := client.ListFiles(context.Background(), "/workspace")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}

	names := make(map[string]bool)
	for _, f := range files {
		names[f.Name] = true
	}
	if !names["a.txt"] || !names["b.txt"] {
		t.Errorf("ListFiles = %v, want a.txt and b.txt present", names)
	}
}

func TestExecWithDir(t *testing.T) {
	client := launchAlpine(t)

	_, err := client.Exec(context.Background(), "mkdir -p /tmp/testdir && echo hi > /tmp/testdir/hello.txt")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	result, err := client.ExecWithDir(context.Background(), "cat hello.txt", "/tmp/testdir")
	if err != nil {
		t.Fatalf("ExecWithDir: %v", err)
	}
	if got := strings.TrimSpace(result.Stdout); got != "hi" {
		t.Errorf("stdout = %q, want %q", got, "hi")
	}
}

func TestExecWithDirPwd(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.ExecWithDir(context.Background(), "pwd", "/tmp")
	if err != nil {
		t.Fatalf("ExecWithDir: %v", err)
	}
	if got := strings.TrimSpace(result.Stdout); got != "/tmp" {
		t.Errorf("pwd = %q, want %q", got, "/tmp")
	}
}

func TestExecWithDirDefaultIsWorkspace(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "pwd")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := strings.TrimSpace(result.Stdout); got != "/workspace" {
		t.Errorf("default pwd = %q, want %q", got, "/workspace")
	}
}

func TestExecWithDirRelativeCommand(t *testing.T) {
	client := launchAlpine(t)

	_, err := client.Exec(context.Background(), "mkdir -p /opt/myapp && echo '#!/bin/sh\necho running-from-myapp' > /opt/myapp/run.sh && chmod +x /opt/myapp/run.sh")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	result, err := client.ExecWithDir(context.Background(), "sh run.sh", "/opt/myapp")
	if err != nil {
		t.Fatalf("ExecWithDir: %v", err)
	}
	if got := strings.TrimSpace(result.Stdout); got != "running-from-myapp" {
		t.Errorf("stdout = %q, want %q", got, "running-from-myapp")
	}
}

func TestExecStreamWithDir(t *testing.T) {
	client := launchAlpine(t)

	var stdout, stderr bytes.Buffer
	result, err := client.ExecStreamWithDir(context.Background(), "pwd", "/var", &stdout, &stderr)
	if err != nil {
		t.Fatalf("ExecStreamWithDir: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", result.ExitCode)
	}
	if got := strings.TrimSpace(stdout.String()); got != "/var" {
		t.Errorf("pwd = %q, want %q", got, "/var")
	}
}

func TestGuestEnvironment(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "cat /etc/os-release")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(result.Stdout, "Alpine") {
		t.Errorf("expected Alpine in os-release, got: %s", result.Stdout)
	}
}

func TestLargeOutput(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "seq 1 1000")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", result.ExitCode)
	}
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	if len(lines) != 1000 {
		t.Errorf("got %d lines, want 1000", len(lines))
	}
}

func TestLargeFileRoundtrip(t *testing.T) {
	client := launchAlpine(t)

	data := bytes.Repeat([]byte("abcdefghij"), 10000) // 100KB
	if err := client.WriteFile(context.Background(), "/workspace/large.bin", data); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := client.ReadFile(context.Background(), "/workspace/large.bin")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("ReadFile size = %d, want %d", len(got), len(data))
	}
}

func TestExecCancelKillsProcess(t *testing.T) {
	client := launchAlpine(t)

	// Start a long-running sleep, then cancel it after 1s.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	start := time.Now()
	_, err := client.Exec(ctx, "sleep 60")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from cancelled exec, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got: %v", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("cancel took %v, expected < 10s", elapsed)
	}
}

func TestExecStreamCancelKillsProcess(t *testing.T) {
	client := launchAlpine(t)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	var stdout, stderr bytes.Buffer
	start := time.Now()
	_, err := client.ExecStream(ctx, "sleep 60", &stdout, &stderr)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from cancelled exec_stream, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got: %v", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("cancel took %v, expected < 10s", elapsed)
	}
}

func TestExecCancelProcessActuallyDies(t *testing.T) {
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
	if err != nil {
		t.Fatalf("check marker: %v", err)
	}
	if got := strings.TrimSpace(result.Stdout); got != "exists" {
		t.Errorf("marker file missing — cancelled process was not killed")
	}

	// The sleep process should no longer be running.
	// Use pgrep with -x for exact match on "sleep" to avoid matching the
	// sh -c wrapper that contains "sleep" in its command line.
	result, err = client.Exec(context.Background(), "pgrep -x sleep || echo gone")
	if err != nil {
		t.Fatalf("check process: %v", err)
	}
	if got := strings.TrimSpace(result.Stdout); got != "gone" {
		t.Errorf("sleep process still running after cancel: %s", result.Stdout)
	}
}

func TestExecCancelGracefulShutdown(t *testing.T) {
	client := launchAlpine(t)

	// Prove the guest agent sends SIGTERM before SIGKILL by observing timing.
	// A process that handles SIGTERM exits immediately; one that only dies to
	// SIGKILL takes ≥5s (the cancelGracePeriod). We cancel after 1s and check
	// how long the process takes to die.
	//
	// "true; sleep 60" prevents busybox exec optimization so sh stays as PID 1
	// and sleep is a child process. The process-group SIGTERM kills sleep (not
	// PID 1, so no signal protection). If only SIGKILL were sent, sleep would
	// survive until the 5s grace period.

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	client.Exec(ctx, "true; sleep 60")

	// Check quickly — if SIGTERM worked, sleep dies within ~1s of cancel.
	// If only SIGKILL, it would take 5+ seconds.
	time.Sleep(1 * time.Second)

	result, err := client.Exec(context.Background(), "pgrep -x sleep || echo gone")
	if err != nil {
		t.Fatalf("check process: %v", err)
	}
	if got := strings.TrimSpace(result.Stdout); got != "gone" {
		t.Errorf("sleep still running 1s after cancel — SIGTERM may not be reaching child processes: %s", result.Stdout)
	}
}

func TestExecManualCancelViaContext(t *testing.T) {
	client := launchAlpine(t)

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(500 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := client.Exec(ctx, "sleep 60")
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("cancel took %v, expected < 10s", elapsed)
	}
}
