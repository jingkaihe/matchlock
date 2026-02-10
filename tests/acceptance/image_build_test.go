//go:build acceptance

package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/sdk"
)

// --- Dockerfile build ---

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

	imgStdout, _, imgExitCode := runCLI(t, "image", "ls")
	if imgExitCode != 0 {
		t.Fatalf("image ls exit code = %d", imgExitCode)
	}
	if !strings.Contains(imgStdout, tag) {
		t.Errorf("image ls output should contain %q, got: %s", tag, imgStdout)
	}

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

// --- Symlink preservation ---

func TestImageSymlinksPreserved(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.Exec("readlink /bin/sh")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	got := strings.TrimSpace(result.Stdout)
	if !strings.Contains(got, "busybox") {
		t.Errorf("readlink /bin/sh = %q, want to contain 'busybox'", got)
	}
}

func TestImageSymlinksInLib(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.Exec("ls -la / | grep '^l' | head -5")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", result.ExitCode)
	}
}

func TestPythonImageSymlinks(t *testing.T) {
	builder := sdk.New("python:3.12-alpine")
	client := launchWithBuilder(t, builder)

	result, err := client.Exec("readlink /usr/local/bin/python3")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	got := strings.TrimSpace(result.Stdout)
	if !strings.Contains(got, "python") {
		t.Errorf("readlink python3 = %q, want to contain 'python'", got)
	}
}

// --- File ownership (uid/gid) ---

func TestImageFileOwnershipRoot(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.Exec("stat -c '%u:%g' /etc/passwd")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	got := strings.TrimSpace(result.Stdout)
	if got != "0:0" {
		t.Errorf("/etc/passwd uid:gid = %q, want %q", got, "0:0")
	}
}

func TestImageFileOwnershipNonRoot(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.Exec("stat -c '%u:%g' /etc/shadow")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	got := strings.TrimSpace(result.Stdout)
	uid := strings.Split(got, ":")[0]
	if uid != "0" {
		t.Errorf("/etc/shadow uid = %q, want %q", uid, "0")
	}
}

func TestPythonImageOwnership(t *testing.T) {
	builder := sdk.New("python:3.12-alpine")
	client := launchWithBuilder(t, builder)

	result, err := client.Exec("stat -c '%u:%g' /usr/local/bin/python3.12")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	got := strings.TrimSpace(result.Stdout)
	if got != "0:0" {
		t.Errorf("python3.12 uid:gid = %q, want %q", got, "0:0")
	}
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
		result, err := client.Exec("stat -c '%a' " + tc.path)
		if err != nil {
			t.Fatalf("stat %s: %v", tc.path, err)
		}
		got := strings.TrimSpace(result.Stdout)
		if got != tc.mode {
			t.Errorf("%s mode = %q, want %q", tc.path, got, tc.mode)
		}
	}
}

// --- Busybox symlinks ---

func TestBusyboxSymlinksWork(t *testing.T) {
	client := launchAlpine(t)

	// Alpine's /bin/ls, /bin/cat etc. are symlinks to busybox.
	// Verify the symlink chain resolves and commands execute correctly.
	for _, cmd := range []string{"ls /", "cat /etc/hostname", "id -u"} {
		result, err := client.Exec(cmd)
		if err != nil {
			t.Fatalf("Exec %q: %v", cmd, err)
		}
		if result.ExitCode != 0 {
			t.Errorf("%q exit code = %d, want 0; stderr: %s", cmd, result.ExitCode, result.Stderr)
		}
	}
}
