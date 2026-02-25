package sdk

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"sync"

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

// ExecPipeResult holds the final result of a pipe-mode exec.
type ExecPipeResult struct {
	ExitCode   int
	DurationMS int64
}

// ExecInteractiveResult holds the final result of an interactive TTY exec.
type ExecInteractiveResult struct {
	ExitCode   int
	DurationMS int64
}

// ExecInteractiveOptions controls interactive TTY execution.
type ExecInteractiveOptions struct {
	WorkingDir string
	User       string
	Rows       uint16
	Cols       uint16
	Stdin      io.Reader
	Stdout     io.Writer
	Resize     <-chan [2]uint16
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

// ExecPipe executes a command in pipe mode with bidirectional stdin/stdout/stderr.
// This mode does not allocate a PTY.
func (c *Client) ExecPipe(ctx context.Context, command string, stdin io.Reader, stdout, stderr io.Writer) (*ExecPipeResult, error) {
	return c.ExecPipeWithDir(ctx, command, "", stdin, stdout, stderr)
}

// ExecPipeWithDir executes a command in pipe mode with a working directory.
func (c *Client) ExecPipeWithDir(ctx context.Context, command, workingDir string, stdin io.Reader, stdout, stderr io.Writer) (*ExecPipeResult, error) {
	params := map[string]string{
		"command": command,
	}
	if workingDir != "" {
		params["working_dir"] = workingDir
	}

	reqID := c.requestID.Add(1)
	readyCh := make(chan struct{})
	doneCh := make(chan struct{})
	var readyOnce sync.Once
	markReady := func() {
		readyOnce.Do(func() { close(readyCh) })
	}

	go c.pumpExecInput(ctx, doneCh, readyCh, reqID, stdin, "exec_pipe.stdin", "exec_pipe.stdin_eof")

	onNotification := func(method string, params json.RawMessage) {
		switch method {
		case "exec_pipe.ready":
			markReady()
		case "exec_pipe.stdout", "exec_pipe.stderr":
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
			case "exec_pipe.stdout":
				if stdout != nil {
					stdout.Write(decoded)
				}
			case "exec_pipe.stderr":
				if stderr != nil {
					stderr.Write(decoded)
				}
			}
		}
	}

	result, err := c.sendRequestWithIDCtx(ctx, reqID, "exec_pipe", params, onNotification)
	close(doneCh)
	markReady()
	if err != nil {
		return nil, err
	}

	var pipeResult struct {
		ExitCode   int   `json:"exit_code"`
		DurationMS int64 `json:"duration_ms"`
	}
	if err := json.Unmarshal(result, &pipeResult); err != nil {
		return nil, errx.Wrap(ErrParseExecPipeResult, err)
	}

	return &ExecPipeResult{
		ExitCode:   pipeResult.ExitCode,
		DurationMS: pipeResult.DurationMS,
	}, nil
}

// ExecInteractive executes a command in TTY mode (equivalent to CLI -it behavior).
func (c *Client) ExecInteractive(ctx context.Context, command string, opts *ExecInteractiveOptions) (*ExecInteractiveResult, error) {
	if opts == nil {
		opts = &ExecInteractiveOptions{}
	}

	rows := opts.Rows
	cols := opts.Cols
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}

	params := map[string]interface{}{
		"command": command,
		"rows":    rows,
		"cols":    cols,
	}
	if opts.WorkingDir != "" {
		params["working_dir"] = opts.WorkingDir
	}
	if opts.User != "" {
		params["user"] = opts.User
	}

	reqID := c.requestID.Add(1)
	readyCh := make(chan struct{})
	doneCh := make(chan struct{})
	var readyOnce sync.Once
	markReady := func() {
		readyOnce.Do(func() { close(readyCh) })
	}

	go c.pumpExecInput(ctx, doneCh, readyCh, reqID, opts.Stdin, "exec_tty.stdin", "exec_tty.stdin_eof")
	go c.pumpTTYResize(ctx, doneCh, readyCh, reqID, opts.Resize)

	onNotification := func(method string, params json.RawMessage) {
		switch method {
		case "exec_tty.ready":
			markReady()
		case "exec_tty.stdout":
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
			if opts.Stdout != nil {
				opts.Stdout.Write(decoded)
			}
		}
	}

	result, err := c.sendRequestWithIDCtx(ctx, reqID, "exec_tty", params, onNotification)
	close(doneCh)
	markReady()
	if err != nil {
		return nil, err
	}

	var ttyResult struct {
		ExitCode   int   `json:"exit_code"`
		DurationMS int64 `json:"duration_ms"`
	}
	if err := json.Unmarshal(result, &ttyResult); err != nil {
		return nil, errx.Wrap(ErrParseExecTTYResult, err)
	}

	return &ExecInteractiveResult{
		ExitCode:   ttyResult.ExitCode,
		DurationMS: ttyResult.DurationMS,
	}, nil
}

func (c *Client) pumpExecInput(ctx context.Context, doneCh, readyCh <-chan struct{}, reqID uint64, stdin io.Reader, chunkMethod, eofMethod string) {
	select {
	case <-readyCh:
	case <-doneCh:
		return
	case <-ctx.Done():
		return
	}

	if stdin == nil {
		_ = c.sendNotification(eofMethod, map[string]interface{}{"id": reqID})
		return
	}

	buf := make([]byte, 4096)
	for {
		select {
		case <-doneCh:
			return
		default:
		}

		n, err := stdin.Read(buf)
		if n > 0 {
			encoded := base64.StdEncoding.EncodeToString(buf[:n])
			if sendErr := c.sendNotification(chunkMethod, map[string]interface{}{
				"id":   reqID,
				"data": encoded,
			}); sendErr != nil {
				return
			}
		}
		if err != nil {
			_ = c.sendNotification(eofMethod, map[string]interface{}{"id": reqID})
			return
		}
	}
}

func (c *Client) pumpTTYResize(ctx context.Context, doneCh, readyCh <-chan struct{}, reqID uint64, resizeCh <-chan [2]uint16) {
	if resizeCh == nil {
		return
	}

	select {
	case <-readyCh:
	case <-doneCh:
		return
	case <-ctx.Done():
		return
	}

	for {
		select {
		case <-doneCh:
			return
		case <-ctx.Done():
			return
		case size, ok := <-resizeCh:
			if !ok {
				return
			}
			_ = c.sendNotification("exec_tty.resize", map[string]interface{}{
				"id":   reqID,
				"rows": size[0],
				"cols": size[1],
			})
		}
	}
}

// WriteFile writes content to a file in the sandbox.
