//go:build acceptance

package acceptance

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecSimpleCommand(t *testing.T) {
	t.Parallel()
	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "echo hello")
	require.NoError(t, err, "Exec")
	assert.Equal(t, 0, result.ExitCode)
	assert.Equal(t, "hello", strings.TrimSpace(result.Stdout))
}

func TestExecNonZeroExit(t *testing.T) {
	t.Parallel()
	t.Skip("known bug: guest agent does not propagate non-zero exit codes")

	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "false")
	require.NoError(t, err, "Exec")
	assert.NotEqual(t, 0, result.ExitCode, "exit code should be non-zero")
}

func TestExecFailedCommandStderr(t *testing.T) {
	t.Parallel()
	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "cat /nonexistent_file_abc123")
	require.NoError(t, err, "Exec")
	assert.Contains(t, result.Stderr, "No such file or directory")
}

func TestExecStderr(t *testing.T) {
	t.Parallel()
	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "sh -c 'echo err >&2'")
	require.NoError(t, err, "Exec")
	assert.Contains(t, result.Stderr, "err")
}

func TestExecMultipleCommands(t *testing.T) {
	t.Parallel()
	client := launchAlpine(t)

	for i, cmd := range []string{"echo one", "echo two", "echo three"} {
		result, err := client.Exec(context.Background(), cmd)
		require.NoErrorf(t, err, "Exec[%d]", i)
		assert.Equalf(t, 0, result.ExitCode, "Exec[%d] exit code", i)
	}
}

func TestExecStream(t *testing.T) {
	t.Parallel()
	client := launchAlpine(t)

	var stdout, stderr bytes.Buffer
	result, err := client.ExecStream(context.Background(), "echo streamed", &stdout, &stderr)
	require.NoError(t, err, "ExecStream")
	assert.Equal(t, 0, result.ExitCode)
	assert.Equal(t, "streamed", strings.TrimSpace(stdout.String()))
}

func TestLog(t *testing.T) {
	t.Parallel()
	client := launchWithBuilder(t, sdk.New("alpine:latest").
		WithNoNetwork().
		WithEntrypoint("sh", "-c", "echo first && sleep 2 && echo second"))

	require.Eventually(t, func() bool {
		logOutput, err := client.Log(context.Background())
		return err == nil && strings.Contains(logOutput, "first")
	}, 20*time.Second, 100*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- client.LogStream(ctx, &out)
	}()

	require.Eventually(t, func() bool {
		return strings.Contains(out.String(), "second")
	}, 20*time.Second, 100*time.Millisecond)

	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	assert.Contains(t, out.String(), "first")
	assert.Contains(t, out.String(), "second")
}

func TestExecWithDir(t *testing.T) {
	t.Parallel()
	client := launchAlpine(t)

	_, err := client.Exec(context.Background(), "mkdir -p /tmp/testdir && echo hi > /tmp/testdir/hello.txt")
	require.NoError(t, err, "setup")

	result, err := client.ExecWithDir(context.Background(), "cat hello.txt", "/tmp/testdir")
	require.NoError(t, err, "ExecWithDir")
	assert.Equal(t, "hi", strings.TrimSpace(result.Stdout))
}

func TestExecWithDirPwd(t *testing.T) {
	t.Parallel()
	client := launchAlpine(t)

	result, err := client.ExecWithDir(context.Background(), "pwd", "/tmp")
	require.NoError(t, err, "ExecWithDir")
	assert.Equal(t, "/tmp", strings.TrimSpace(result.Stdout))
}

func TestExecDefaultDirWithoutWorkspaceIsRoot(t *testing.T) {
	t.Parallel()
	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "pwd")
	require.NoError(t, err, "Exec")
	assert.Equal(t, "/", strings.TrimSpace(result.Stdout))
}

func TestExecWithDirRelativeCommand(t *testing.T) {
	t.Parallel()
	client := launchAlpine(t)

	_, err := client.Exec(context.Background(), "mkdir -p /opt/myapp && echo '#!/bin/sh\necho running-from-myapp' > /opt/myapp/run.sh && chmod +x /opt/myapp/run.sh")
	require.NoError(t, err, "setup")

	result, err := client.ExecWithDir(context.Background(), "sh run.sh", "/opt/myapp")
	require.NoError(t, err, "ExecWithDir")
	assert.Equal(t, "running-from-myapp", strings.TrimSpace(result.Stdout))
}

func TestExecStreamWithDir(t *testing.T) {
	t.Parallel()
	client := launchAlpine(t)

	var stdout, stderr bytes.Buffer
	result, err := client.ExecStreamWithDir(context.Background(), "pwd", "/var", &stdout, &stderr)
	require.NoError(t, err, "ExecStreamWithDir")
	assert.Equal(t, 0, result.ExitCode)
	assert.Equal(t, "/var", strings.TrimSpace(stdout.String()))
}

func TestGuestEnvironment(t *testing.T) {
	t.Parallel()
	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "cat /etc/os-release")
	require.NoError(t, err, "Exec")
	assert.Contains(t, result.Stdout, "Alpine")
}

func TestGuestEnvironmentFromBuilderEnv(t *testing.T) {
	t.Parallel()
	client := launchWithBuilder(t, sdk.New("alpine:latest").WithEnv("PLAIN_ENV", "from-builder"))

	result, err := client.Exec(context.Background(), `sh -c 'printf "%s" "$PLAIN_ENV"'`)
	require.NoError(t, err, "Exec")
	assert.Equal(t, "from-builder", strings.TrimSpace(result.Stdout))
}

func TestLargeOutput(t *testing.T) {
	t.Parallel()
	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "seq 1 1000")
	require.NoError(t, err, "Exec")
	assert.Equal(t, 0, result.ExitCode)
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	assert.Len(t, lines, 1000)
}

func TestHostFSMountAtomicRenamePreservesReadability(t *testing.T) {
	hostRepo := t.TempDir()
	script := strings.Join([]string{
		"set -eu",
		"echo direct > direct.txt",
		"cat direct.txt",
		"echo atomic > tmp.xyz",
		"mv tmp.xyz renamed.txt",
		"cat renamed.txt",
	}, "; ")
	result := runHostFSMountCommand(
		t,
		hostRepo,
		script,
	)
	require.Equalf(t, 0, result.ExitCode, "stdout: %s\nstderr: %s", result.Stdout, result.Stderr)
	assert.Contains(t, result.Stdout, "direct")
	assert.Contains(t, result.Stdout, "atomic")

	body, err := osReadTrim(filepath.Join(hostRepo, "renamed.txt"))
	require.NoError(t, err)
	assert.Equal(t, "atomic", body)
}

func TestHostFSMountDirectoryRenamePreservesDescendantReads(t *testing.T) {
	hostRepo := t.TempDir()
	script := strings.Join([]string{
		"set -eu",
		"mkdir -p old/sub",
		"echo nested > old/sub/file.txt",
		"cat old/sub/file.txt",
		"mv old new",
		"cat new/sub/file.txt",
	}, "; ")
	result := runHostFSMountCommand(
		t,
		hostRepo,
		script,
	)
	require.Equalf(t, 0, result.ExitCode, "stdout: %s\nstderr: %s", result.Stdout, result.Stderr)
	assert.Equal(t, 2, strings.Count(result.Stdout, "nested"))
}

func TestHostFSMountGitHeadLockRenamePreservesReadability(t *testing.T) {
	hostRepo := t.TempDir()
	script := strings.Join([]string{
		"set -eu",
		"mkdir -p .git",
		"echo 'ref: refs/heads/main' > .git/HEAD.lock",
		"mv .git/HEAD.lock .git/HEAD",
		"cat .git/HEAD",
	}, "; ")
	result := runHostFSMountCommand(
		t,
		hostRepo,
		script,
	)
	require.Equalf(t, 0, result.ExitCode, "stdout: %s\nstderr: %s", result.Stdout, result.Stderr)
	assert.Contains(t, result.Stdout, "ref: refs/heads/main")
}

func TestHostFSMountGitCheckoutAndStatusSucceed(t *testing.T) {
	hostRepo := t.TempDir()
	client := launchWithBuilder(t, sdk.New("alpine/git:latest").
		WithWorkspace("/workspace").
		MountHostDir("/workspace/repo", hostRepo),
	)

	script := strings.Join([]string{
		"set -eu",
		"git config --global --add safe.directory /workspace/repo",
		"git init >/dev/null",
		"git config user.email test@test.com",
		"git config user.name Test",
		"git config --local core.logAllRefUpdates false",
		"echo '# Test' > README.md",
		"git add README.md",
		"git commit -m init >/dev/null",
		"git checkout -b test-branch >/dev/null",
		"git status --short >/dev/null",
		"cat .git/HEAD",
	}, "; ")

	result, err := client.ExecWithDir(
		context.Background(),
		script,
		"/workspace/repo",
	)
	require.NoError(t, err, "ExecWithDir")
	require.Equalf(t, 0, result.ExitCode, "stdout: %s\nstderr: %s", result.Stdout, result.Stderr)
	assert.Contains(t, result.Stdout, "ref: refs/heads/test-branch")
}

func runHostFSMountCommand(t *testing.T, hostRepo string, cmd string) *sdk.ExecResult {
	t.Helper()
	client := launchWithBuilder(t, sdk.New("alpine:latest").
		WithWorkspace("/workspace").
		MountHostDir("/workspace/repo", hostRepo),
	)
	result, err := client.ExecWithDir(
		context.Background(),
		cmd,
		"/workspace/repo",
	)
	require.NoError(t, err, "ExecWithDir")
	return result
}

func osReadTrim(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
