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

func TestHostFSMountOwnerOverrideAllowsNonRootAccess(t *testing.T) {
	t.Parallel()
	hostRepo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(hostRepo, "seed.txt"), []byte("seed"), 0600))
	require.NoError(t, os.Chmod(hostRepo, 0700))

	client := launchWithBuilder(t, sdk.New("alpine:latest").
		WithWorkspace("/workspace").
		WithUser("65534:65534").
		MountHostDirAs("/workspace/repo", hostRepo, 65534, 65534),
	)

	script := strings.Join([]string{
		"set -eu",
		"stat -c '%u:%g:%a' .",
		"stat -c '%u:%g:%a' seed.txt",
		"test -w .",
		"echo non-root-write > created.txt",
		"cat created.txt",
	}, "; ")
	result, err := client.ExecWithDir(context.Background(), script, "/workspace/repo")
	require.NoError(t, err, "ExecWithDir")
	require.Equalf(t, 0, result.ExitCode, "stdout: %s\nstderr: %s", result.Stdout, result.Stderr)

	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	require.Len(t, lines, 3, "stdout: %q", result.Stdout)
	assert.Equal(t, "65534:65534:700", lines[0])
	assert.Equal(t, "65534:65534:600", lines[1])
	assert.Equal(t, "non-root-write", lines[2])

	body, err := osReadTrim(filepath.Join(hostRepo, "created.txt"))
	require.NoError(t, err)
	assert.Equal(t, "non-root-write", body)
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

func TestHostFSMountSymlinkCreateAndFollow(t *testing.T) {
	hostRepo := t.TempDir()
	script := strings.Join([]string{
		"set -eu",
		"echo target-content > real.txt",
		"ln -s real.txt link",
		"readlink link",
		"cat link",
	}, "; ")
	result := runHostFSMountCommand(t, hostRepo, script)
	require.Equalf(t, 0, result.ExitCode, "stdout: %s\nstderr: %s", result.Stdout, result.Stderr)

	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	require.Len(t, lines, 2)
	assert.Equal(t, "real.txt", lines[0])
	assert.Equal(t, "target-content", lines[1])

	target, err := os.Readlink(filepath.Join(hostRepo, "link"))
	require.NoError(t, err, "symlink should be visible on host")
	assert.Equal(t, "real.txt", target)
}

func TestHostFSMountSymlinkVisibleInReaddir(t *testing.T) {
	hostRepo := t.TempDir()
	script := strings.Join([]string{
		"set -eu",
		"echo content > real.txt",
		"ln -s real.txt link",
		"ls -la",
	}, "; ")
	result := runHostFSMountCommand(t, hostRepo, script)
	require.Equalf(t, 0, result.ExitCode, "stdout: %s\nstderr: %s", result.Stdout, result.Stderr)
	assert.Contains(t, result.Stdout, "link -> real.txt")
}

// pnpm creates a content-addressable store with deep symlink trees of the form
//
//	node_modules/pkg -> ../.pnpm/pkg@1.0.0/node_modules
//
// This test exercises creating a symlink to a directory and following it
// across a traversal, which covers the full symlink round-trip.
func TestHostFSMountPnpmStyleSymlinks(t *testing.T) {
	hostRepo := t.TempDir()
	script := strings.Join([]string{
		"set -eu",
		"mkdir -p .pnpm/pkg@1.0.0/node_modules",
		"echo pkg-content > .pnpm/pkg@1.0.0/node_modules/index.js",
		"mkdir -p node_modules",
		"ln -s ../.pnpm/pkg@1.0.0/node_modules node_modules/pkg",
		"readlink node_modules/pkg",
		"cat node_modules/pkg/index.js",
	}, "; ")
	result := runHostFSMountCommand(t, hostRepo, script)
	require.Equalf(t, 0, result.ExitCode, "stdout: %s\nstderr: %s", result.Stdout, result.Stderr)

	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	require.Len(t, lines, 2)
	assert.Equal(t, "../.pnpm/pkg@1.0.0/node_modules", lines[0])
	assert.Equal(t, "pkg-content", lines[1])
}

func TestHostFSMountPreExistingSymlinkVisibleFromVM(t *testing.T) {
	hostRepo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(hostRepo, "real.txt"), []byte("host-content\n"), 0644))
	require.NoError(t, os.Symlink("real.txt", filepath.Join(hostRepo, "link")))

	script := strings.Join([]string{
		"set -eu",
		"readlink link",
		"cat link",
	}, "; ")
	result := runHostFSMountCommand(t, hostRepo, script)
	require.Equalf(t, 0, result.ExitCode, "stdout: %s\nstderr: %s", result.Stdout, result.Stderr)

	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	require.Len(t, lines, 2)
	assert.Equal(t, "real.txt", lines[0])
	assert.Equal(t, "host-content", lines[1])
}

// Guests must not be able to create symlinks whose target resolves outside the
// mount root. The host provider rejects with EPERM; busybox `ln` surfaces this
// as a non-zero exit and the on-disk path must not exist afterwards.
func TestHostFSMountRejectsSymlinkEscapingMount(t *testing.T) {
	hostRepo := t.TempDir()

	// Each `ln` is expected to fail; suppress non-zero exit per-attempt so the
	// guest script can exercise all three cases before the assertions run.
	script := strings.Join([]string{
		"ln -s /etc/passwd absolute-link 2>&1 || echo ABS_REJECTED",
		"ln -s ../escape relative-link 2>&1 || echo REL_REJECTED",
		"ln -s ../../../etc/passwd deep-link 2>&1 || echo DEEP_REJECTED",
	}, "; ")
	result := runHostFSMountCommand(t, hostRepo, script)
	require.Equalf(t, 0, result.ExitCode, "stdout: %s\nstderr: %s", result.Stdout, result.Stderr)

	assert.Contains(t, result.Stdout, "ABS_REJECTED")
	assert.Contains(t, result.Stdout, "REL_REJECTED")
	assert.Contains(t, result.Stdout, "DEEP_REJECTED")

	for _, name := range []string{"absolute-link", "relative-link", "deep-link"} {
		_, err := os.Lstat(filepath.Join(hostRepo, name))
		assert.Truef(t, os.IsNotExist(err), "%s must not exist on host after rejection", name)
	}
}
