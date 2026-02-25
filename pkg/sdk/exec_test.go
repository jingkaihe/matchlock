package sdk

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type inboundRPC struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      *uint64         `json:"id,omitempty"`
}

func newExecRPCClient(t *testing.T, handle func(inboundRPC, func(interface{}))) (*Client, func()) {
	t.Helper()

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(stdinR)
		for scanner.Scan() {
			var req inboundRPC
			if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
				continue
			}
			handle(req, func(msg interface{}) {
				data, err := json.Marshal(msg)
				if err != nil {
					return
				}
				_, _ = fmt.Fprintln(stdoutW, string(data))
			})
		}
		_ = stdoutW.Close()
	}()

	c := &Client{
		stdin:   stdinW,
		stdout:  bufio.NewReader(stdoutR),
		pending: make(map[uint64]*pendingRequest),
	}

	cleanup := func() {
		_ = stdinW.Close()
		_ = stdoutW.Close()
		<-done
	}
	return c, cleanup
}

func TestExecPipeWithDir(t *testing.T) {
	var gotCommand string
	var gotDir string
	var gotStdin string
	var reqID uint64

	client, cleanup := newExecRPCClient(t, func(req inboundRPC, write func(interface{})) {
		switch req.Method {
		case "exec_pipe":
			require.NotNil(t, req.ID)
			reqID = *req.ID
			var params struct {
				Command    string `json:"command"`
				WorkingDir string `json:"working_dir,omitempty"`
			}
			require.NoError(t, json.Unmarshal(req.Params, &params))
			gotCommand = params.Command
			gotDir = params.WorkingDir

			write(map[string]interface{}{
				"jsonrpc": "2.0",
				"method":  "exec_pipe.ready",
				"params": map[string]interface{}{
					"id": reqID,
				},
			})
		case "exec_pipe.stdin":
			var params struct {
				ID   uint64 `json:"id"`
				Data string `json:"data"`
			}
			require.NoError(t, json.Unmarshal(req.Params, &params))
			require.Equal(t, reqID, params.ID)
			decoded, err := base64.StdEncoding.DecodeString(params.Data)
			require.NoError(t, err)
			gotStdin += string(decoded)
		case "exec_pipe.stdin_eof":
			var params struct {
				ID uint64 `json:"id"`
			}
			require.NoError(t, json.Unmarshal(req.Params, &params))
			require.Equal(t, reqID, params.ID)

			write(map[string]interface{}{
				"jsonrpc": "2.0",
				"method":  "exec_pipe.stdout",
				"params": map[string]interface{}{
					"id":   reqID,
					"data": base64.StdEncoding.EncodeToString([]byte("out-data")),
				},
			})
			write(map[string]interface{}{
				"jsonrpc": "2.0",
				"method":  "exec_pipe.stderr",
				"params": map[string]interface{}{
					"id":   reqID,
					"data": base64.StdEncoding.EncodeToString([]byte("err-data")),
				},
			})
			write(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      reqID,
				"result": map[string]interface{}{
					"exit_code":   5,
					"duration_ms": 17,
				},
			})
		}
	})
	defer cleanup()

	stdin := strings.NewReader("hello-pipe")
	var stdout, stderr bytes.Buffer
	result, err := client.ExecPipeWithDir(context.Background(), "cat", "/tmp", stdin, &stdout, &stderr)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, 5, result.ExitCode)
	assert.Equal(t, int64(17), result.DurationMS)
	assert.Equal(t, "cat", gotCommand)
	assert.Equal(t, "/tmp", gotDir)
	assert.Equal(t, "hello-pipe", gotStdin)
	assert.Equal(t, "out-data", stdout.String())
	assert.Equal(t, "err-data", stderr.String())
}

func TestExecInteractive(t *testing.T) {
	var gotCommand string
	var gotRows uint16
	var gotCols uint16
	var gotStdin string
	var gotResize [2]uint16
	var reqID uint64
	gotEOF := false
	gotResizeEvent := false

	client, cleanup := newExecRPCClient(t, func(req inboundRPC, write func(interface{})) {
		switch req.Method {
		case "exec_tty":
			require.NotNil(t, req.ID)
			reqID = *req.ID
			var params struct {
				Command string `json:"command"`
				Rows    uint16 `json:"rows"`
				Cols    uint16 `json:"cols"`
			}
			require.NoError(t, json.Unmarshal(req.Params, &params))
			gotCommand = params.Command
			gotRows = params.Rows
			gotCols = params.Cols

			write(map[string]interface{}{
				"jsonrpc": "2.0",
				"method":  "exec_tty.ready",
				"params": map[string]interface{}{
					"id": reqID,
				},
			})
		case "exec_tty.stdin":
			var params struct {
				ID   uint64 `json:"id"`
				Data string `json:"data"`
			}
			require.NoError(t, json.Unmarshal(req.Params, &params))
			require.Equal(t, reqID, params.ID)
			decoded, err := base64.StdEncoding.DecodeString(params.Data)
			require.NoError(t, err)
			gotStdin += string(decoded)
		case "exec_tty.resize":
			var params struct {
				ID   uint64 `json:"id"`
				Rows uint16 `json:"rows"`
				Cols uint16 `json:"cols"`
			}
			require.NoError(t, json.Unmarshal(req.Params, &params))
			require.Equal(t, reqID, params.ID)
			gotResize = [2]uint16{params.Rows, params.Cols}
			gotResizeEvent = true
			if gotEOF {
				write(map[string]interface{}{
					"jsonrpc": "2.0",
					"method":  "exec_tty.stdout",
					"params": map[string]interface{}{
						"id":   reqID,
						"data": base64.StdEncoding.EncodeToString([]byte("tty-out")),
					},
				})
				write(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      reqID,
					"result": map[string]interface{}{
						"exit_code":   0,
						"duration_ms": 23,
					},
				})
			}
		case "exec_tty.stdin_eof":
			var params struct {
				ID uint64 `json:"id"`
			}
			require.NoError(t, json.Unmarshal(req.Params, &params))
			require.Equal(t, reqID, params.ID)
			gotEOF = true
			if gotResizeEvent {
				write(map[string]interface{}{
					"jsonrpc": "2.0",
					"method":  "exec_tty.stdout",
					"params": map[string]interface{}{
						"id":   reqID,
						"data": base64.StdEncoding.EncodeToString([]byte("tty-out")),
					},
				})
				write(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      reqID,
					"result": map[string]interface{}{
						"exit_code":   0,
						"duration_ms": 23,
					},
				})
			}
		}
	})
	defer cleanup()

	resize := make(chan [2]uint16, 1)
	resize <- [2]uint16{55, 132}
	close(resize)

	var stdout bytes.Buffer
	result, err := client.ExecInteractive(context.Background(), "sh", &ExecInteractiveOptions{
		Rows:   40,
		Cols:   100,
		Stdin:  strings.NewReader("tty-input"),
		Stdout: &stdout,
		Resize: resize,
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, 0, result.ExitCode)
	assert.Equal(t, int64(23), result.DurationMS)
	assert.Equal(t, "sh", gotCommand)
	assert.Equal(t, uint16(40), gotRows)
	assert.Equal(t, uint16(100), gotCols)
	assert.Equal(t, "tty-input", gotStdin)
	assert.Equal(t, [2]uint16{55, 132}, gotResize)
	assert.Equal(t, "tty-out", stdout.String())
}
