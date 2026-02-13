//go:build acceptance

package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func TestCLIRunEnvInline(t *testing.T) {
	stdout, stderr, exitCode := runCLIWithTimeout(
		t,
		2*time.Minute,
		"run",
		"--image", "alpine:latest",
		"-e", "FOO=bar",
		"--", "sh", "-c", `printf "%s" "$FOO"`,
	)
	require.Equal(t, 0, exitCode, "stdout: %s\nstderr: %s", stdout, stderr)
	assert.Equal(t, "bar", strings.TrimSpace(stdout))
}

func TestCLIRunEnvFromHost(t *testing.T) {
	t.Setenv("MATCHLOCK_HOST_ENV_TEST", "from-host")

	stdout, stderr, exitCode := runCLIWithTimeout(
		t,
		2*time.Minute,
		"run",
		"--image", "alpine:latest",
		"-e", "MATCHLOCK_HOST_ENV_TEST",
		"--", "sh", "-c", `printf "%s" "$MATCHLOCK_HOST_ENV_TEST"`,
	)
	require.Equal(t, 0, exitCode, "stdout: %s\nstderr: %s", stdout, stderr)
	assert.Equal(t, "from-host", strings.TrimSpace(stdout))
}

func TestCLIRunEnvFromHostMissingFails(t *testing.T) {
	const key = "MATCHLOCK_ENV_MISSING_ABC123"
	_ = os.Unsetenv(key)

	_, stderr, exitCode := runCLIWithTimeout(
		t,
		2*time.Minute,
		"run",
		"--image", "alpine:latest",
		"-e", key,
		"--", "true",
	)
	require.NotEqual(t, 0, exitCode)
	require.Contains(t, stderr, "invalid environment variable")
	require.Contains(t, stderr, key)
}

func TestCLIRunEnvFile(t *testing.T) {
	t.Setenv("MATCHLOCK_ENV_FILE_HOST", "from-host")

	dir := t.TempDir()
	envFile := filepath.Join(dir, "app.env")
	content := "# sample env file\nFILE_ONLY=from-file\nMATCHLOCK_ENV_FILE_HOST\n"
	require.NoError(t, os.WriteFile(envFile, []byte(content), 0644))

	stdout, stderr, exitCode := runCLIWithTimeout(
		t,
		2*time.Minute,
		"run",
		"--image", "alpine:latest",
		"--env-file", envFile,
		"--", "sh", "-c", `printf "%s|%s" "$FILE_ONLY" "$MATCHLOCK_ENV_FILE_HOST"`,
	)
	require.Equal(t, 0, exitCode, "stdout: %s\nstderr: %s", stdout, stderr)
	assert.Equal(t, "from-file|from-host", strings.TrimSpace(stdout))
}

func TestCLIRunEnvFlagOverridesEnvFile(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "override.env")
	require.NoError(t, os.WriteFile(envFile, []byte("SAME=file\n"), 0644))

	stdout, stderr, exitCode := runCLIWithTimeout(
		t,
		2*time.Minute,
		"run",
		"--image", "alpine:latest",
		"--env-file", envFile,
		"-e", "SAME=flag",
		"--", "sh", "-c", `printf "%s" "$SAME"`,
	)
	require.Equal(t, 0, exitCode, "stdout: %s\nstderr: %s", stdout, stderr)
	assert.Equal(t, "flag", strings.TrimSpace(stdout))
}
