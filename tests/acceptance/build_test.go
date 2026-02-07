//go:build acceptance

package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCLIDockerfileBuild(t *testing.T) {
	contextDir := t.TempDir()
	dockerfile := filepath.Join(contextDir, "Dockerfile")
	helloFile := filepath.Join(contextDir, "hello.txt")

	if err := os.WriteFile(helloFile, []byte("hello from matchlock build"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dockerfile, []byte(`FROM busybox:latest
COPY hello.txt /hello.txt
`), 0644); err != nil {
		t.Fatal(err)
	}

	tag := "matchlock-test-build:latest"

	t.Cleanup(func() {
		runCLI(t, "image", "rm", tag)
	})

	// Build the image
	stdout, stderr, exitCode := runCLIWithTimeout(t, 10*time.Minute,
		"build",
		"-f", dockerfile,
		"-t", tag,
		contextDir,
	)
	if exitCode != 0 {
		t.Fatalf("build exit code = %d\nstdout: %s\nstderr: %s", exitCode, stdout, stderr)
	}
	if !strings.Contains(stdout, "Successfully built and tagged") {
		t.Errorf("build stdout = %q, want to contain 'Successfully built and tagged'", stdout)
	}

	// Verify the image appears in the local store
	imgStdout, _, imgExitCode := runCLI(t, "image", "ls")
	if imgExitCode != 0 {
		t.Fatalf("image ls exit code = %d", imgExitCode)
	}
	if !strings.Contains(imgStdout, tag) {
		t.Errorf("image ls output should contain %q, got: %s", tag, imgStdout)
	}

	// Run the built image and verify hello.txt is present
	runStdout, runStderr, runExitCode := runCLIWithTimeout(t, 2*time.Minute,
		"run", "--image", tag, "cat", "/hello.txt",
	)
	if runExitCode != 0 {
		t.Fatalf("run exit code = %d\nstdout: %s\nstderr: %s", runExitCode, runStdout, runStderr)
	}
	if got := strings.TrimSpace(runStdout); got != "hello from matchlock build" {
		t.Errorf("cat /hello.txt = %q, want %q", got, "hello from matchlock build")
	}
}
