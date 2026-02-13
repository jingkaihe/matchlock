//go:build acceptance

package acceptance

import (
	"bytes"
	"context"
	"strings"
	"testing"

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

func TestExecWithDirDefaultIsWorkspace(t *testing.T) {
	t.Parallel()
	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "pwd")
	require.NoError(t, err, "Exec")
	assert.Equal(t, "/workspace", strings.TrimSpace(result.Stdout))
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
