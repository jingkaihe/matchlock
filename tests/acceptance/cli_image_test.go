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

	// Pull, then remove twice - second remove should fail
	runCLIWithTimeout(t, 5*time.Minute, "pull", img)

	_, _, exitCode := runCLI(t, "image", "rm", img)
	require.Equal(t, 0, exitCode)

	_, _, exitCode = runCLI(t, "image", "rm", img)
	assert.NotEqual(t, 0, exitCode, "second image rm should fail for already-removed image")
}

func TestCLIImageGC(t *testing.T) {
	stdout, _, exitCode := runCLI(t, "image", "gc")
	require.Equal(t, 0, exitCode)
	assert.Contains(t, stdout, "Removed")
}
