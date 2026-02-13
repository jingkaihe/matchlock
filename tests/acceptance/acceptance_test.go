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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, err, "NewClient")

	t.Cleanup(func() {
		client.Close(0)
		client.Remove()
	})

	sandbox := sdk.New("alpine:latest")
	_, err = client.Launch(sandbox)
	require.NoError(t, err, "Launch")

	return client
}

func TestExecSimpleCommand(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "echo hello")
	require.NoError(t, err, "Exec")
	assert.Equal(t, 0, result.ExitCode)
	assert.Equal(t, "hello", strings.TrimSpace(result.Stdout))
}

func TestExecNonZeroExit(t *testing.T) {
	t.Skip("known bug: guest agent does not propagate non-zero exit codes")

	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "false")
	require.NoError(t, err, "Exec")
	assert.NotEqual(t, 0, result.ExitCode, "exit code should be non-zero")
}

func TestExecFailedCommandStderr(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "cat /nonexistent_file_abc123")
	require.NoError(t, err, "Exec")
	assert.Contains(t, result.Stderr, "No such file or directory")
}

func TestExecStderr(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "sh -c 'echo err >&2'")
	require.NoError(t, err, "Exec")
	assert.Contains(t, result.Stderr, "err")
}

func TestExecMultipleCommands(t *testing.T) {
	client := launchAlpine(t)

	for i, cmd := range []string{"echo one", "echo two", "echo three"} {
		result, err := client.Exec(context.Background(), cmd)
		require.NoErrorf(t, err, "Exec[%d]", i)
		assert.Equalf(t, 0, result.ExitCode, "Exec[%d] exit code", i)
	}
}

func TestExecStream(t *testing.T) {
	client := launchAlpine(t)

	var stdout, stderr bytes.Buffer
	result, err := client.ExecStream(context.Background(), "echo streamed", &stdout, &stderr)
	require.NoError(t, err, "ExecStream")
	assert.Equal(t, 0, result.ExitCode)
	assert.Equal(t, "streamed", strings.TrimSpace(stdout.String()))
}

func TestWriteAndReadFile(t *testing.T) {
	client := launchAlpine(t)

	content := []byte("hello from acceptance test")
	err := client.WriteFile(context.Background(), "/workspace/test.txt", content)
	require.NoError(t, err, "WriteFile")

	got, err := client.ReadFile(context.Background(), "/workspace/test.txt")
	require.NoError(t, err, "ReadFile")
	assert.Equal(t, content, got)
}

func TestWriteFileAndExec(t *testing.T) {
	client := launchAlpine(t)

	script := []byte("#!/bin/sh\necho script-output\n")
	err := client.WriteFileMode(context.Background(), "/workspace/run.sh", script, 0755)
	require.NoError(t, err, "WriteFileMode")

	result, err := client.Exec(context.Background(), "sh /workspace/run.sh")
	require.NoError(t, err, "Exec")
	assert.Equal(t, "script-output", strings.TrimSpace(result.Stdout))
}

func TestListFiles(t *testing.T) {
	client := launchAlpine(t)

	err := client.WriteFile(context.Background(), "/workspace/a.txt", []byte("a"))
	require.NoError(t, err, "WriteFile a")
	err = client.WriteFile(context.Background(), "/workspace/b.txt", []byte("bb"))
	require.NoError(t, err, "WriteFile b")

	files, err := client.ListFiles(context.Background(), "/workspace")
	require.NoError(t, err, "ListFiles")

	names := make(map[string]bool)
	for _, f := range files {
		names[f.Name] = true
	}
	assert.True(t, names["a.txt"] && names["b.txt"], "ListFiles = %v, want a.txt and b.txt present", names)
}

func TestExecWithDir(t *testing.T) {
	client := launchAlpine(t)

	_, err := client.Exec(context.Background(), "mkdir -p /tmp/testdir && echo hi > /tmp/testdir/hello.txt")
	require.NoError(t, err, "setup")

	result, err := client.ExecWithDir(context.Background(), "cat hello.txt", "/tmp/testdir")
	require.NoError(t, err, "ExecWithDir")
	assert.Equal(t, "hi", strings.TrimSpace(result.Stdout))
}

func TestExecWithDirPwd(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.ExecWithDir(context.Background(), "pwd", "/tmp")
	require.NoError(t, err, "ExecWithDir")
	assert.Equal(t, "/tmp", strings.TrimSpace(result.Stdout))
}

func TestExecWithDirDefaultIsWorkspace(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "pwd")
	require.NoError(t, err, "Exec")
	assert.Equal(t, "/workspace", strings.TrimSpace(result.Stdout))
}

func TestExecWithDirRelativeCommand(t *testing.T) {
	client := launchAlpine(t)

	_, err := client.Exec(context.Background(), "mkdir -p /opt/myapp && echo '#!/bin/sh\necho running-from-myapp' > /opt/myapp/run.sh && chmod +x /opt/myapp/run.sh")
	require.NoError(t, err, "setup")

	result, err := client.ExecWithDir(context.Background(), "sh run.sh", "/opt/myapp")
	require.NoError(t, err, "ExecWithDir")
	assert.Equal(t, "running-from-myapp", strings.TrimSpace(result.Stdout))
}

func TestExecStreamWithDir(t *testing.T) {
	client := launchAlpine(t)

	var stdout, stderr bytes.Buffer
	result, err := client.ExecStreamWithDir(context.Background(), "pwd", "/var", &stdout, &stderr)
	require.NoError(t, err, "ExecStreamWithDir")
	assert.Equal(t, 0, result.ExitCode)
	assert.Equal(t, "/var", strings.TrimSpace(stdout.String()))
}

func TestGuestEnvironment(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "cat /etc/os-release")
	require.NoError(t, err, "Exec")
	assert.Contains(t, result.Stdout, "Alpine")
}

func TestLargeOutput(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "seq 1 1000")
	require.NoError(t, err, "Exec")
	assert.Equal(t, 0, result.ExitCode)
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	assert.Len(t, lines, 1000)
}

func TestLargeFileRoundtrip(t *testing.T) {
	client := launchAlpine(t)

	data := bytes.Repeat([]byte("abcdefghij"), 10000) // 100KB
	err := client.WriteFile(context.Background(), "/workspace/large.bin", data)
	require.NoError(t, err, "WriteFile")

	got, err := client.ReadFile(context.Background(), "/workspace/large.bin")
	require.NoError(t, err, "ReadFile")
	assert.Equal(t, data, got)
}

func TestExecCancelKillsProcess(t *testing.T) {
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
	client := launchAlpine(t)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	var stdout, stderr bytes.Buffer
	start := time.Now()
	_, err := client.ExecStream(ctx, "sleep 60", &stdout, &stderr)
	elapsed := time.Since(start)

	require.Error(t, err, "expected error from cancelled exec_stream")
	require.True(t, errors.Is(err, context.DeadlineExceeded), "expected DeadlineExceeded, got: %v", err)
	assert.Less(t, elapsed, 5*time.Second, "cancel took too long")
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
	require.NoError(t, err, "check marker")
	assert.Equal(t, "exists", strings.TrimSpace(result.Stdout), "marker file missing — cancelled process was not killed")

	// The sleep process should no longer be running.
	// Use pgrep with -x for exact match on "sleep" to avoid matching the
	// sh -c wrapper that contains "sleep" in its command line.
	result, err = client.Exec(context.Background(), "pgrep -x sleep || echo gone")
	require.NoError(t, err, "check process")
	assert.Equal(t, "gone", strings.TrimSpace(result.Stdout), "sleep process still running after cancel: %s", result.Stdout)
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
	require.NoError(t, err, "check process")
	assert.Equal(t, "gone", strings.TrimSpace(result.Stdout), "sleep still running 1s after cancel — SIGTERM may not be reaching child processes: %s", result.Stdout)
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

	require.ErrorIs(t, err, context.Canceled)
	assert.Less(t, elapsed, 5*time.Second, "cancel took too long")
}
