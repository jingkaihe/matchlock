//go:build acceptance

package acceptance

import (
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

// --- Dockerfile build ---

func TestCLIDockerfileBuild(t *testing.T) {
	contextDir := t.TempDir()
	dockerfile := filepath.Join(contextDir, "Dockerfile")
	helloFile := filepath.Join(contextDir, "hello.txt")

	err := os.WriteFile(helloFile, []byte("hello from matchlock build"), 0644)
	require.NoError(t, err)
	err = os.WriteFile(dockerfile, []byte(`FROM busybox:latest
COPY hello.txt /hello.txt
`), 0644)
	require.NoError(t, err)

	tag := "matchlock-test-build:latest"

	t.Cleanup(func() {
		runCLI(t, "image", "rm", tag)
	})

	stdout, stderr, exitCode := runCLIWithTimeout(t, 10*time.Minute,
		"build",
		"-f", dockerfile,
		"-t", tag,
		contextDir,
	)
	require.Equalf(t, 0, exitCode, "stdout: %s\nstderr: %s", stdout, stderr)
	assert.Contains(t, stdout, "Successfully built and tagged")

	imgStdout, _, imgExitCode := runCLI(t, "image", "ls")
	require.Equal(t, 0, imgExitCode)
	assert.Contains(t, imgStdout, tag)

	runStdout, runStderr, runExitCode := runCLIWithTimeout(t, 2*time.Minute,
		"run", "--image", tag, "cat", "/hello.txt",
	)
	require.Equalf(t, 0, runExitCode, "stdout: %s\nstderr: %s", runStdout, runStderr)
	assert.Equal(t, "hello from matchlock build", strings.TrimSpace(runStdout))
}

// --- Symlink preservation ---

func TestImageSymlinksPreserved(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "readlink /bin/sh")
	require.NoError(t, err, "Exec")
	got := strings.TrimSpace(result.Stdout)
	assert.Contains(t, got, "busybox")
}

func TestImageSymlinksInLib(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "ls -la / | grep '^l' | head -5")
	require.NoError(t, err, "Exec")
	assert.Equal(t, 0, result.ExitCode)
}

func TestPythonImageSymlinks(t *testing.T) {
	builder := sdk.New("python:3.12-alpine")
	client := launchWithBuilder(t, builder)

	result, err := client.Exec(context.Background(), "readlink /usr/local/bin/python3")
	require.NoError(t, err, "Exec")
	got := strings.TrimSpace(result.Stdout)
	assert.Contains(t, got, "python")
}

// --- File ownership (uid/gid) ---

func TestImageFileOwnershipRoot(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "stat -c '%u:%g' /etc/passwd")
	require.NoError(t, err, "Exec")
	assert.Equal(t, "0:0", strings.TrimSpace(result.Stdout))
}

func TestImageFileOwnershipNonRoot(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "stat -c '%u:%g' /etc/shadow")
	require.NoError(t, err, "Exec")
	got := strings.TrimSpace(result.Stdout)
	uid := strings.Split(got, ":")[0]
	assert.Equal(t, "0", uid)
}

func TestPythonImageOwnership(t *testing.T) {
	builder := sdk.New("python:3.12-alpine")
	client := launchWithBuilder(t, builder)

	result, err := client.Exec(context.Background(), "stat -c '%u:%g' /usr/local/bin/python3.12")
	require.NoError(t, err, "Exec")
	assert.Equal(t, "0:0", strings.TrimSpace(result.Stdout))
}

// --- File permissions ---

func TestImageFilePermissions(t *testing.T) {
	client := launchAlpine(t)

	tests := []struct {
		path string
		mode string
	}{
		{"/etc/passwd", "644"},
		{"/etc/shadow", "640"},
		{"/bin/busybox", "755"},
	}

	for _, tc := range tests {
		result, err := client.Exec(context.Background(), "stat -c '%a' "+tc.path)
		require.NoErrorf(t, err, "stat %s", tc.path)
		assert.Equalf(t, tc.mode, strings.TrimSpace(result.Stdout), "%s mode", tc.path)
	}
}

// --- Busybox symlinks ---

func TestBusyboxSymlinksWork(t *testing.T) {
	client := launchAlpine(t)

	// Alpine's /bin/ls, /bin/cat etc. are symlinks to busybox.
	// Verify the symlink chain resolves and commands execute correctly.
	for _, cmd := range []string{"ls /", "cat /etc/hostname", "id -u"} {
		result, err := client.Exec(context.Background(), cmd)
		require.NoErrorf(t, err, "Exec %q", cmd)
		assert.Equalf(t, 0, result.ExitCode, "%q exit code; stderr: %s", cmd, result.Stderr)
	}
}
