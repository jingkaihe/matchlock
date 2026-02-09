package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/jingkaihe/matchlock/pkg/sdk"
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
		return fmt.Errorf("create client: %w", err)
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
		return fmt.Errorf("launch sandbox: %w", err)
	}
	slog.Info("sandbox ready", "vm", vmID)

	// Buffered exec — collects all output, returns when done
	result, err := client.Exec("python3 --version")
	if err != nil {
		return fmt.Errorf("exec python3 --version: %w", err)
	}
	fmt.Print(result.Stdout)

	// Install uv
	if _, err := client.Exec("pip install --quiet uv"); err != nil {
		return fmt.Errorf("exec pip install uv: %w", err)
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
	if err := client.WriteFile("/workspace/ask.py", []byte(script)); err != nil {
		return fmt.Errorf("write_file: %w", err)
	}

	// Streaming exec — prints plain text as it arrives
	streamResult, err := client.ExecStream(
		"uv run /workspace/ask.py",
		os.Stdout, os.Stderr,
	)
	if err != nil {
		return fmt.Errorf("exec_stream: %w", err)
	}
	fmt.Println()
	slog.Info("done", "exit_code", streamResult.ExitCode, "duration_ms", streamResult.DurationMS)
	return nil
}
