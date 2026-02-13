package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/sdk"
)

var (
	errCreateClient   = errors.New("create client")
	errLaunchSandbox  = errors.New("launch sandbox")
	errExecPythonVer  = errors.New("exec python3 --version")
	errExecPipInstall = errors.New("exec pip install uv")
	errWriteFile      = errors.New("write_file")
	errExecStream     = errors.New("exec_stream")
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := sdk.DefaultConfig()
	if os.Getenv("MATCHLOCK_BIN") == "" {
		cfg.BinaryPath = "./bin/matchlock"
	}

	client, err := sdk.NewClient(cfg)
	if err != nil {
		return errx.Wrap(errCreateClient, err)
	}
	defer client.Remove()
	defer client.Close(0)

	sandbox := sdk.New("python:3.12-alpine").
		AllowHost(
			"dl-cdn.alpinelinux.org",
			"files.pythonhosted.org", "pypi.org",
			"astral.sh", "github.com", "objects.githubusercontent.com",
			"api.anthropic.com",
		).
		AddSecret("ANTHROPIC_API_KEY", os.Getenv("ANTHROPIC_API_KEY"), "api.anthropic.com")

	vmID, err := client.Launch(sandbox)
	if err != nil {
		return errx.Wrap(errLaunchSandbox, err)
	}
	slog.Info("sandbox ready", "vm", vmID)

	// Buffered exec — collects all output, returns when done
	result, err := client.Exec(context.Background(), "python3 --version")
	if err != nil {
		return errx.Wrap(errExecPythonVer, err)
	}
	fmt.Print(result.Stdout)

	// Install uv
	if _, err := client.Exec(context.Background(), "pip install --quiet uv"); err != nil {
		return errx.Wrap(errExecPipInstall, err)
	}

	// Write a Python script that uses the Anthropic SDK to stream plain text
	script := `# /// script
# requires-python = ">=3.12"
# dependencies = ["anthropic"]
# ///
import anthropic, os

client = anthropic.Anthropic(api_key=os.environ["ANTHROPIC_API_KEY"])
with client.messages.stream(
    model="claude-haiku-4-5-20251001",
    max_tokens=1000,
    messages=[{"role": "user", "content": "Explain TCP to me"}],
) as stream:
    for text in stream.text_stream:
        print(text, end="", flush=True)
print()
`
	if err := client.WriteFile(context.Background(), "/workspace/ask.py", []byte(script)); err != nil {
		return errx.Wrap(errWriteFile, err)
	}

	// Streaming exec — prints plain text as it arrives
	streamResult, err := client.ExecStream(context.Background(),
		"uv run /workspace/ask.py",
		os.Stdout, os.Stderr,
	)
	if err != nil {
		return errx.Wrap(errExecStream, err)
	}
	fmt.Println()
	slog.Info("done", "exit_code", streamResult.ExitCode, "duration_ms", streamResult.DurationMS)
	return nil
}
