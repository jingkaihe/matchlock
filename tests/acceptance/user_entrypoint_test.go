//go:build acceptance

package acceptance

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/sdk"
)

func launchWithBuilder(t *testing.T, builder *sdk.SandboxBuilder) *sdk.Client {
	t.Helper()
	client, err := sdk.NewClient(matchlockConfig(t))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	t.Cleanup(func() {
		client.Close(0)
		client.Remove()
	})

	_, err = client.Launch(builder)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	return client
}

// --- User switching tests ---

func TestUserSwitchByUsername(t *testing.T) {
	builder := sdk.New("alpine:latest").WithUser("nobody")
	client := launchWithBuilder(t, builder)

	result, err := client.Exec(context.Background(), "id -u")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	got := strings.TrimSpace(result.Stdout)
	if got != "65534" {
		t.Errorf("uid = %q, want %q (nobody)", got, "65534")
	}
}

func TestUserSwitchByUID(t *testing.T) {
	builder := sdk.New("alpine:latest").WithUser("65534")
	client := launchWithBuilder(t, builder)

	result, err := client.Exec(context.Background(), "id -u")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	got := strings.TrimSpace(result.Stdout)
	if got != "65534" {
		t.Errorf("uid = %q, want %q", got, "65534")
	}
}

func TestUserSwitchByUIDAndGID(t *testing.T) {
	builder := sdk.New("alpine:latest").WithUser("65534:65534")
	client := launchWithBuilder(t, builder)

	result, err := client.Exec(context.Background(), "id -u && id -g")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected 2 lines, got %d: %q", len(lines), result.Stdout)
	}
	if lines[0] != "65534" {
		t.Errorf("uid = %q, want %q", lines[0], "65534")
	}
	if lines[1] != "65534" {
		t.Errorf("gid = %q, want %q", lines[1], "65534")
	}
}

func TestUserSwitchHomeDirIsSet(t *testing.T) {
	builder := sdk.New("alpine:latest").WithUser("nobody")
	client := launchWithBuilder(t, builder)

	result, err := client.Exec(context.Background(), "echo $HOME")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	got := strings.TrimSpace(result.Stdout)
	// Alpine's nobody home is typically /
	if got == "" {
		t.Errorf("HOME is empty, expected it to be set")
	}
}

func TestUserSwitchCannotWriteRootFiles(t *testing.T) {
	builder := sdk.New("alpine:latest").WithUser("nobody")
	client := launchWithBuilder(t, builder)

	result, err := client.Exec(context.Background(), "touch /root/test 2>&1; echo $?")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	got := strings.TrimSpace(result.Stdout)
	lastLine := got
	if lines := strings.Split(got, "\n"); len(lines) > 0 {
		lastLine = lines[len(lines)-1]
	}
	if lastLine == "0" {
		t.Errorf("expected non-zero exit from touch /root/test as nobody, got 0")
	}
}

func TestDefaultUserIsRoot(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "id -u")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	got := strings.TrimSpace(result.Stdout)
	if got != "0" {
		t.Errorf("uid = %q, want %q (root)", got, "0")
	}
}

// --- Entrypoint / CMD tests ---

func TestEntrypointOverride(t *testing.T) {
	builder := sdk.New("alpine:latest").WithEntrypoint("echo", "from-entrypoint")
	client := launchWithBuilder(t, builder)

	result, err := client.Exec(context.Background(), "echo from-entrypoint")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(result.Stdout, "from-entrypoint") {
		t.Errorf("stdout = %q, want to contain 'from-entrypoint'", result.Stdout)
	}
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
	if err != nil {
		t.Fatalf("Exec MY_TEST_VAR: %v", err)
	}
	if got := strings.TrimSpace(result.Stdout); got != "hello-from-image" {
		t.Errorf("MY_TEST_VAR = %q, want %q", got, "hello-from-image")
	}

	result, err = client.Exec(context.Background(), "echo $ANOTHER_VAR")
	if err != nil {
		t.Fatalf("Exec ANOTHER_VAR: %v", err)
	}
	if got := strings.TrimSpace(result.Stdout); got != "world" {
		t.Errorf("ANOTHER_VAR = %q, want %q", got, "world")
	}
}

func TestImageWorkingDir(t *testing.T) {
	builder := sdk.New("alpine:latest").WithImageConfig(&sdk.ImageConfig{
		WorkingDir: "/tmp",
	})
	client := launchWithBuilder(t, builder)

	result, err := client.Exec(context.Background(), "pwd")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := strings.TrimSpace(result.Stdout); got != "/tmp" {
		t.Errorf("pwd = %q, want %q", got, "/tmp")
	}
}

// --- Image USER from OCI config tests ---

func TestImageConfigUser(t *testing.T) {
	builder := sdk.New("alpine:latest").WithImageConfig(&sdk.ImageConfig{
		User: "nobody",
	})
	client := launchWithBuilder(t, builder)

	result, err := client.Exec(context.Background(), "id -u")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	got := strings.TrimSpace(result.Stdout)
	if got != "65534" {
		t.Errorf("uid = %q, want %q (nobody)", got, "65534")
	}
}

func TestImageConfigUserWithBuilderOverride(t *testing.T) {
	// WithUser should override ImageConfig.User
	builder := sdk.New("alpine:latest").
		WithImageConfig(&sdk.ImageConfig{User: "daemon"}).
		WithUser("nobody")
	client := launchWithBuilder(t, builder)

	result, err := client.Exec(context.Background(), "id -u")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	got := strings.TrimSpace(result.Stdout)
	if got != "65534" {
		t.Errorf("uid = %q, want %q (nobody override)", got, "65534")
	}
}

// --- VFS chmod test ---

func TestChmodViaExec(t *testing.T) {
	client := launchAlpine(t)

	if err := client.WriteFile(context.Background(), "/workspace/script.sh", []byte("#!/bin/sh\necho chmod-works\n")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := client.Exec(context.Background(), "chmod +x /workspace/script.sh")
	if err != nil {
		t.Fatalf("chmod: %v", err)
	}

	result, err := client.Exec(context.Background(), "/workspace/script.sh")
	if err != nil {
		t.Fatalf("Exec script: %v", err)
	}
	if got := strings.TrimSpace(result.Stdout); got != "chmod-works" {
		t.Errorf("stdout = %q, want %q", got, "chmod-works")
	}
}

func TestChmodPreservesAfterStat(t *testing.T) {
	client := launchAlpine(t)

	if err := client.WriteFile(context.Background(), "/workspace/check.sh", []byte("#!/bin/sh\necho ok\n")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := client.Exec(context.Background(), "chmod 755 /workspace/check.sh")
	if err != nil {
		t.Fatalf("chmod: %v", err)
	}

	result, err := client.Exec(context.Background(), "stat -c '%a' /workspace/check.sh")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	got := strings.TrimSpace(result.Stdout)
	if got != "755" {
		t.Errorf("mode = %q, want %q", got, "755")
	}
}

// --- Streaming with user tests ---

func TestExecStreamWithImageConfig(t *testing.T) {
	builder := sdk.New("alpine:latest").WithImageConfig(&sdk.ImageConfig{
		Env: map[string]string{"STREAM_VAR": "streamed-env"},
	})
	client := launchWithBuilder(t, builder)

	var stdout, stderr bytes.Buffer
	result, err := client.ExecStream(context.Background(), "echo $STREAM_VAR", &stdout, &stderr)
	if err != nil {
		t.Fatalf("ExecStream: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", result.ExitCode)
	}
	if got := strings.TrimSpace(stdout.String()); got != "streamed-env" {
		t.Errorf("stdout = %q, want %q", got, "streamed-env")
	}
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
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected 2 lines, got %d: %q", len(lines), result.Stdout)
	}
	if lines[0] != "/tmp" {
		t.Errorf("pwd = %q, want %q", lines[0], "/tmp")
	}
	if lines[1] != "65534" {
		t.Errorf("uid = %q, want %q", lines[1], "65534")
	}
}

// --- Python image with real OCI USER ---

func TestPythonImageDefaultUser(t *testing.T) {
	builder := sdk.New("python:3.12-alpine")
	client := launchWithBuilder(t, builder)

	result, err := client.Exec(context.Background(), "python3 -c 'import os; print(os.getuid())'")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	got := strings.TrimSpace(result.Stdout)
	// python:3.12-alpine runs as root by default
	if got != "0" {
		t.Errorf("uid = %q, want %q", got, "0")
	}
}

func TestPythonImageWithUserOverride(t *testing.T) {
	builder := sdk.New("python:3.12-alpine").WithUser("nobody")
	client := launchWithBuilder(t, builder)

	result, err := client.Exec(context.Background(), "python3 -c 'import os; print(os.getuid())'")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	got := strings.TrimSpace(result.Stdout)
	if got != "65534" {
		t.Errorf("uid = %q, want %q (nobody)", got, "65534")
	}
}

func TestPythonImageEntrypointAndCmd(t *testing.T) {
	// python:3.12-alpine has ENTRYPOINT ["python3"] and CMD ["python3"]
	// Verify we can run python commands
	builder := sdk.New("python:3.12-alpine")
	client := launchWithBuilder(t, builder)

	result, err := client.Exec(context.Background(), "python3 --version")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(result.Stdout, "Python 3.12") {
		t.Errorf("stdout = %q, want to contain 'Python 3.12'", result.Stdout)
	}
}

// --- Multiple execs with user ---

func TestMultipleExecsWithUser(t *testing.T) {
	builder := sdk.New("alpine:latest").WithUser("nobody")
	client := launchWithBuilder(t, builder)

	for i, cmd := range []string{"id -u", "id -g", "whoami"} {
		result, err := client.Exec(context.Background(), cmd)
		if err != nil {
			t.Fatalf("Exec[%d] %q: %v", i, cmd, err)
		}
		if result.ExitCode != 0 {
			t.Errorf("Exec[%d] %q exit code = %d, want 0", i, cmd, result.ExitCode)
		}
	}
}
