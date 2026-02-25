package sdk

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"

	"github.com/jingkaihe/matchlock/internal/errx"
)

type ExecResult struct {
	// ExitCode is the command's exit code
	ExitCode int
	// Stdout is the standard output
	Stdout string
	// Stderr is the standard error
	Stderr string
	// DurationMS is the execution time in milliseconds
	DurationMS int64
}

// Exec executes a command in the sandbox and returns the buffered result.
// The context controls the lifetime of the request — if cancelled, a cancel
// RPC is sent to abort the in-flight execution.
func (c *Client) Exec(ctx context.Context, command string) (*ExecResult, error) {
	return c.ExecWithDir(ctx, command, "")
}

// ExecWithDir executes a command in the sandbox with a working directory.
func (c *Client) ExecWithDir(ctx context.Context, command, workingDir string) (*ExecResult, error) {
	params := map[string]string{
		"command": command,
	}
	if workingDir != "" {
		params["working_dir"] = workingDir
	}

	result, err := c.sendRequestCtx(ctx, "exec", params, nil)
	if err != nil {
		return nil, err
	}

	var execResult struct {
		ExitCode   int    `json:"exit_code"`
		Stdout     string `json:"stdout"`
		Stderr     string `json:"stderr"`
		DurationMS int64  `json:"duration_ms"`
	}
	if err := json.Unmarshal(result, &execResult); err != nil {
		return nil, errx.Wrap(ErrParseExecResult, err)
	}

	stdout, _ := base64.StdEncoding.DecodeString(execResult.Stdout)
	stderr, _ := base64.StdEncoding.DecodeString(execResult.Stderr)

	return &ExecResult{
		ExitCode:   execResult.ExitCode,
		Stdout:     string(stdout),
		Stderr:     string(stderr),
		DurationMS: execResult.DurationMS,
	}, nil
}

// ExecStreamResult holds the final result of a streaming exec (no stdout/stderr
// since those were delivered via the callback).
type ExecStreamResult struct {
	ExitCode   int
	DurationMS int64
}

// ExecStream executes a command and streams stdout/stderr to the provided writers
// in real-time. If stdout or stderr is nil, that stream is discarded.
// The final ExecStreamResult contains only the exit code and duration.
func (c *Client) ExecStream(ctx context.Context, command string, stdout, stderr io.Writer) (*ExecStreamResult, error) {
	return c.ExecStreamWithDir(ctx, command, "", stdout, stderr)
}

// ExecStreamWithDir executes a command with a working directory and streams
// stdout/stderr to the provided writers in real-time.
func (c *Client) ExecStreamWithDir(ctx context.Context, command, workingDir string, stdout, stderr io.Writer) (*ExecStreamResult, error) {
	params := map[string]string{
		"command": command,
	}
	if workingDir != "" {
		params["working_dir"] = workingDir
	}

	onNotification := func(method string, params json.RawMessage) {
		var chunk struct {
			Data string `json:"data"`
		}
		if err := json.Unmarshal(params, &chunk); err != nil {
			return
		}
		decoded, err := base64.StdEncoding.DecodeString(chunk.Data)
		if err != nil {
			return
		}
		switch method {
		case "exec_stream.stdout":
			if stdout != nil {
				stdout.Write(decoded)
			}
		case "exec_stream.stderr":
			if stderr != nil {
				stderr.Write(decoded)
			}
		}
	}

	result, err := c.sendRequestCtx(ctx, "exec_stream", params, onNotification)
	if err != nil {
		return nil, err
	}

	var streamResult struct {
		ExitCode   int   `json:"exit_code"`
		DurationMS int64 `json:"duration_ms"`
	}
	if err := json.Unmarshal(result, &streamResult); err != nil {
		return nil, errx.Wrap(ErrParseExecStreamResult, err)
	}

	return &ExecStreamResult{
		ExitCode:   streamResult.ExitCode,
		DurationMS: streamResult.DurationMS,
	}, nil
}

// WriteFile writes content to a file in the sandbox.
