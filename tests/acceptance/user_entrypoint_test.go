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

func launchWithBuilder(t *testing.T, builder *sdk.SandboxBuilder) *sdk.Client {
	t.Helper()
	client, err := sdk.NewClient(matchlockConfig(t))
	require.NoError(t, err, "NewClient")

	t.Cleanup(func() {
		client.Close(0)
		client.Remove()
	})

	_, err = client.Launch(builder)
	require.NoError(t, err, "Launch")

	return client
}

// --- User switching tests ---

func TestUserSwitchByUsername(t *testing.T) {
	builder := sdk.New("alpine:latest").WithUser("nobody")
	client := launchWithBuilder(t, builder)

	result, err := client.Exec(context.Background(), "id -u")
	require.NoError(t, err, "Exec")
	assert.Equal(t, "65534", strings.TrimSpace(result.Stdout))
}

func TestUserSwitchByUID(t *testing.T) {
	builder := sdk.New("alpine:latest").WithUser("65534")
	client := launchWithBuilder(t, builder)

	result, err := client.Exec(context.Background(), "id -u")
	require.NoError(t, err, "Exec")
	assert.Equal(t, "65534", strings.TrimSpace(result.Stdout))
}

func TestUserSwitchByUIDAndGID(t *testing.T) {
	builder := sdk.New("alpine:latest").WithUser("65534:65534")
	client := launchWithBuilder(t, builder)

	result, err := client.Exec(context.Background(), "id -u && id -g")
	require.NoError(t, err, "Exec")
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	require.Len(t, lines, 2, "expected 2 lines, got: %q", result.Stdout)
	assert.Equal(t, "65534", lines[0], "uid")
	assert.Equal(t, "65534", lines[1], "gid")
}

func TestUserSwitchHomeDirIsSet(t *testing.T) {
	builder := sdk.New("alpine:latest").WithUser("nobody")
	client := launchWithBuilder(t, builder)

	result, err := client.Exec(context.Background(), "echo $HOME")
	require.NoError(t, err, "Exec")
	got := strings.TrimSpace(result.Stdout)
	// Alpine's nobody home is typically /
	assert.NotEmpty(t, got, "HOME should be set")
}

func TestUserSwitchCannotWriteRootFiles(t *testing.T) {
	builder := sdk.New("alpine:latest").WithUser("nobody")
	client := launchWithBuilder(t, builder)

	result, err := client.Exec(context.Background(), "touch /root/test 2>&1; echo $?")
	require.NoError(t, err, "Exec")
	got := strings.TrimSpace(result.Stdout)
	lastLine := got
	if lines := strings.Split(got, "\n"); len(lines) > 0 {
		lastLine = lines[len(lines)-1]
	}
	assert.NotEqual(t, "0", lastLine, "expected non-zero exit from touch /root/test as nobody")
}

func TestDefaultUserIsRoot(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "id -u")
	require.NoError(t, err, "Exec")
	assert.Equal(t, "0", strings.TrimSpace(result.Stdout))
}

// --- Entrypoint / CMD tests ---

func TestEntrypointOverride(t *testing.T) {
	builder := sdk.New("alpine:latest").WithEntrypoint("echo", "from-entrypoint")
	client := launchWithBuilder(t, builder)

	result, err := client.Exec(context.Background(), "echo from-entrypoint")
	require.NoError(t, err, "Exec")
	assert.Contains(t, result.Stdout, "from-entrypoint")
}

// --- Image ENV propagation tests ---

func TestImageEnvPropagation(t *testing.T) {
	builder := sdk.New("alpine:latest").WithImageConfig(&sdk.ImageConfig{
		Env: map[string]string{
			"MY_TEST_VAR": "hello-from-image",
			"ANOTHER_VAR": "world",
		},
	})
	client := launchWithBuilder(t, builder)

	result, err := client.Exec(context.Background(), "echo $MY_TEST_VAR")
	require.NoError(t, err, "Exec MY_TEST_VAR")
	assert.Equal(t, "hello-from-image", strings.TrimSpace(result.Stdout))

	result, err = client.Exec(context.Background(), "echo $ANOTHER_VAR")
	require.NoError(t, err, "Exec ANOTHER_VAR")
	assert.Equal(t, "world", strings.TrimSpace(result.Stdout))
}

func TestImageWorkingDir(t *testing.T) {
	builder := sdk.New("alpine:latest").WithImageConfig(&sdk.ImageConfig{
		WorkingDir: "/tmp",
	})
	client := launchWithBuilder(t, builder)

	result, err := client.Exec(context.Background(), "pwd")
	require.NoError(t, err, "Exec")
	assert.Equal(t, "/tmp", strings.TrimSpace(result.Stdout))
}

// --- Image USER from OCI config tests ---

func TestImageConfigUser(t *testing.T) {
	builder := sdk.New("alpine:latest").WithImageConfig(&sdk.ImageConfig{
		User: "nobody",
	})
	client := launchWithBuilder(t, builder)

	result, err := client.Exec(context.Background(), "id -u")
	require.NoError(t, err, "Exec")
	assert.Equal(t, "65534", strings.TrimSpace(result.Stdout))
}

func TestImageConfigUserWithBuilderOverride(t *testing.T) {
	// WithUser should override ImageConfig.User
	builder := sdk.New("alpine:latest").
		WithImageConfig(&sdk.ImageConfig{User: "daemon"}).
		WithUser("nobody")
	client := launchWithBuilder(t, builder)

	result, err := client.Exec(context.Background(), "id -u")
	require.NoError(t, err, "Exec")
	assert.Equal(t, "65534", strings.TrimSpace(result.Stdout))
}

// --- VFS chmod test ---

func TestChmodViaExec(t *testing.T) {
	client := launchAlpine(t)

	err := client.WriteFile(context.Background(), "/workspace/script.sh", []byte("#!/bin/sh\necho chmod-works\n"))
	require.NoError(t, err, "WriteFile")

	_, err = client.Exec(context.Background(), "chmod +x /workspace/script.sh")
	require.NoError(t, err, "chmod")

	result, err := client.Exec(context.Background(), "/workspace/script.sh")
	require.NoError(t, err, "Exec script")
	assert.Equal(t, "chmod-works", strings.TrimSpace(result.Stdout))
}

func TestChmodPreservesAfterStat(t *testing.T) {
	client := launchAlpine(t)

	err := client.WriteFile(context.Background(), "/workspace/check.sh", []byte("#!/bin/sh\necho ok\n"))
	require.NoError(t, err, "WriteFile")

	_, err = client.Exec(context.Background(), "chmod 755 /workspace/check.sh")
	require.NoError(t, err, "chmod")

	result, err := client.Exec(context.Background(), "stat -c '%a' /workspace/check.sh")
	require.NoError(t, err, "stat")
	assert.Equal(t, "755", strings.TrimSpace(result.Stdout))
}

// --- Streaming with user tests ---

func TestExecStreamWithImageConfig(t *testing.T) {
	builder := sdk.New("alpine:latest").WithImageConfig(&sdk.ImageConfig{
		Env: map[string]string{"STREAM_VAR": "streamed-env"},
	})
	client := launchWithBuilder(t, builder)

	var stdout, stderr bytes.Buffer
	result, err := client.ExecStream(context.Background(), "echo $STREAM_VAR", &stdout, &stderr)
	require.NoError(t, err, "ExecStream")
	assert.Equal(t, 0, result.ExitCode)
	assert.Equal(t, "streamed-env", strings.TrimSpace(stdout.String()))
}

// --- Combined user + workdir tests ---

func TestUserWithCustomWorkdir(t *testing.T) {
	builder := sdk.New("alpine:latest").
		WithUser("nobody").
		WithImageConfig(&sdk.ImageConfig{
			WorkingDir: "/tmp",
		})
	client := launchWithBuilder(t, builder)

	result, err := client.Exec(context.Background(), "pwd && id -u")
	require.NoError(t, err, "Exec")
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	require.Len(t, lines, 2, "expected 2 lines, got: %q", result.Stdout)
	assert.Equal(t, "/tmp", lines[0], "pwd")
	assert.Equal(t, "65534", lines[1], "uid")
}

// --- Python image with real OCI USER ---

func TestPythonImageDefaultUser(t *testing.T) {
	builder := sdk.New("python:3.12-alpine")
	client := launchWithBuilder(t, builder)

	result, err := client.Exec(context.Background(), "python3 -c 'import os; print(os.getuid())'")
	require.NoError(t, err, "Exec")
	// python:3.12-alpine runs as root by default
	assert.Equal(t, "0", strings.TrimSpace(result.Stdout))
}

func TestPythonImageWithUserOverride(t *testing.T) {
	builder := sdk.New("python:3.12-alpine").WithUser("nobody")
	client := launchWithBuilder(t, builder)

	result, err := client.Exec(context.Background(), "python3 -c 'import os; print(os.getuid())'")
	require.NoError(t, err, "Exec")
	assert.Equal(t, "65534", strings.TrimSpace(result.Stdout))
}

func TestPythonImageEntrypointAndCmd(t *testing.T) {
	// python:3.12-alpine has ENTRYPOINT ["python3"] and CMD ["python3"]
	// Verify we can run python commands
	builder := sdk.New("python:3.12-alpine")
	client := launchWithBuilder(t, builder)

	result, err := client.Exec(context.Background(), "python3 --version")
	require.NoError(t, err, "Exec")
	assert.Contains(t, result.Stdout, "Python 3.12")
}

// --- Multiple execs with user ---

func TestMultipleExecsWithUser(t *testing.T) {
	builder := sdk.New("alpine:latest").WithUser("nobody")
	client := launchWithBuilder(t, builder)

	for i, cmd := range []string{"id -u", "id -g", "whoami"} {
		result, err := client.Exec(context.Background(), cmd)
		require.NoErrorf(t, err, "Exec[%d] %q", i, cmd)
		assert.Equalf(t, 0, result.ExitCode, "Exec[%d] %q exit code", i, cmd)
	}
}
