//go:build acceptance

package acceptance

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/sdk"
)

func matchlockConfig(t *testing.T) sdk.Config {
	t.Helper()
	cfg := sdk.DefaultConfig()
	if os.Getenv("MATCHLOCK_BIN") == "" {
		cfg.BinaryPath = "matchlock"
	}
	return cfg
}

func launchAlpine(t *testing.T) *sdk.Client {
	t.Helper()
	client, err := sdk.NewClient(matchlockConfig(t))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	sandbox := sdk.New("alpine:latest")
	_, err = client.Launch(sandbox)
	if err != nil {
		client.Close()
		t.Fatalf("Launch: %v", err)
	}

	t.Cleanup(func() {
		client.Close()
		client.Remove()
	})
	return client
}

func TestExecSimpleCommand(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.Exec("echo hello")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", result.ExitCode)
	}
	if got := strings.TrimSpace(result.Stdout); got != "hello" {
		t.Errorf("stdout = %q, want %q", got, "hello")
	}
}

func TestExecNonZeroExit(t *testing.T) {
	t.Skip("known bug: guest agent does not propagate non-zero exit codes")

	client := launchAlpine(t)

	result, err := client.Exec("false")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode == 0 {
		t.Errorf("exit code = 0, want non-zero")
	}
}

func TestExecFailedCommandStderr(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.Exec("cat /nonexistent_file_abc123")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(result.Stderr, "No such file or directory") {
		t.Errorf("stderr = %q, want to contain 'No such file or directory'", result.Stderr)
	}
}

func TestExecStderr(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.Exec("sh -c 'echo err >&2'")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(result.Stderr, "err") {
		t.Errorf("stderr = %q, want to contain %q", result.Stderr, "err")
	}
}

func TestExecMultipleCommands(t *testing.T) {
	client := launchAlpine(t)

	for i, cmd := range []string{"echo one", "echo two", "echo three"} {
		result, err := client.Exec(cmd)
		if err != nil {
			t.Fatalf("Exec[%d]: %v", i, err)
		}
		if result.ExitCode != 0 {
			t.Errorf("Exec[%d] exit code = %d, want 0", i, result.ExitCode)
		}
	}
}

func TestExecStream(t *testing.T) {
	client := launchAlpine(t)

	var stdout, stderr bytes.Buffer
	result, err := client.ExecStream("echo streamed", &stdout, &stderr)
	if err != nil {
		t.Fatalf("ExecStream: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", result.ExitCode)
	}
	if got := strings.TrimSpace(stdout.String()); got != "streamed" {
		t.Errorf("stdout = %q, want %q", got, "streamed")
	}
}

func TestWriteAndReadFile(t *testing.T) {
	client := launchAlpine(t)

	content := []byte("hello from acceptance test")
	if err := client.WriteFile("/workspace/test.txt", content); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := client.ReadFile("/workspace/test.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("ReadFile = %q, want %q", got, content)
	}
}

func TestWriteFileAndExec(t *testing.T) {
	client := launchAlpine(t)

	script := []byte("#!/bin/sh\necho script-output\n")
	if err := client.WriteFileMode("/workspace/run.sh", script, 0755); err != nil {
		t.Fatalf("WriteFileMode: %v", err)
	}

	result, err := client.Exec("sh /workspace/run.sh")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := strings.TrimSpace(result.Stdout); got != "script-output" {
		t.Errorf("stdout = %q, want %q", got, "script-output")
	}
}

func TestListFiles(t *testing.T) {
	client := launchAlpine(t)

	if err := client.WriteFile("/workspace/a.txt", []byte("a")); err != nil {
		t.Fatalf("WriteFile a: %v", err)
	}
	if err := client.WriteFile("/workspace/b.txt", []byte("bb")); err != nil {
		t.Fatalf("WriteFile b: %v", err)
	}

	files, err := client.ListFiles("/workspace")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}

	names := make(map[string]bool)
	for _, f := range files {
		names[f.Name] = true
	}
	if !names["a.txt"] || !names["b.txt"] {
		t.Errorf("ListFiles = %v, want a.txt and b.txt present", names)
	}
}

func TestExecWithDir(t *testing.T) {
	t.Skip("known bug: working_dir is not applied by guest agent")

	client := launchAlpine(t)

	_, err := client.Exec("mkdir -p /tmp/testdir && echo hi > /tmp/testdir/hello.txt")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	result, err := client.ExecWithDir("cat hello.txt", "/tmp/testdir")
	if err != nil {
		t.Fatalf("ExecWithDir: %v", err)
	}
	if got := strings.TrimSpace(result.Stdout); got != "hi" {
		t.Errorf("stdout = %q, want %q", got, "hi")
	}
}

func TestGuestEnvironment(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.Exec("cat /etc/os-release")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(result.Stdout, "Alpine") {
		t.Errorf("expected Alpine in os-release, got: %s", result.Stdout)
	}
}

func TestLargeOutput(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.Exec("seq 1 1000")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", result.ExitCode)
	}
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	if len(lines) != 1000 {
		t.Errorf("got %d lines, want 1000", len(lines))
	}
}

func TestLargeFileRoundtrip(t *testing.T) {
	client := launchAlpine(t)

	data := bytes.Repeat([]byte("abcdefghij"), 10000) // 100KB
	if err := client.WriteFile("/workspace/large.bin", data); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := client.ReadFile("/workspace/large.bin")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("ReadFile size = %d, want %d", len(got), len(data))
	}
}
