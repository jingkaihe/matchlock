package sandbox

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"sync"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/vm"
)

const (
	relayMsgExec            uint8 = 1
	relayMsgExecResult      uint8 = 2
	relayMsgExecInteractive uint8 = 3
	relayMsgStdout          uint8 = 4
	relayMsgStderr          uint8 = 5
	relayMsgStdin           uint8 = 6
	relayMsgExit            uint8 = 7
)

type relayExecRequest struct {
	Command string `json:"command"`
}

type relayExecInteractiveRequest struct {
	Command string `json:"command"`
	Rows    uint16 `json:"rows"`
	Cols    uint16 `json:"cols"`
}

type relayExecResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   []byte `json:"stdout,omitempty"`
	Stderr   []byte `json:"stderr,omitempty"`
	Error    string `json:"error,omitempty"`
}

// ExecRelay serves exec requests from external processes via a Unix socket.
// This allows `matchlock exec` to run commands in a VM owned by another process.
type ExecRelay struct {
	sb       *Sandbox
	listener net.Listener
	mu       sync.Mutex
	stopped  bool
}

func NewExecRelay(sb *Sandbox) *ExecRelay {
	return &ExecRelay{sb: sb}
}

func (r *ExecRelay) Start(socketPath string) error {
	os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", socketPath, err)
	}
	r.listener = listener

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				r.mu.Lock()
				stopped := r.stopped
				r.mu.Unlock()
				if stopped {
					return
				}
				continue
			}
			go r.handleConn(conn)
		}
	}()

	return nil
}

func (r *ExecRelay) Stop() {
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
	if r.listener != nil {
		r.listener.Close()
	}
}

func (r *ExecRelay) handleConn(conn net.Conn) {
	defer conn.Close()

	msgType, data, err := readRelayMsg(conn)
	if err != nil {
		return
	}

	switch msgType {
	case relayMsgExec:
		r.handleExec(conn, data)
	case relayMsgExecInteractive:
		r.handleExecInteractive(conn, data)
	}
}

func (r *ExecRelay) handleExec(conn net.Conn, data []byte) {
	var req relayExecRequest
	if err := json.Unmarshal(data, &req); err != nil {
		sendRelayResult(conn, &relayExecResult{ExitCode: 1, Error: err.Error()})
		return
	}

	result, err := r.sb.Exec(context.Background(), req.Command, nil)
	if err != nil {
		sendRelayResult(conn, &relayExecResult{ExitCode: 1, Error: err.Error()})
		return
	}

	sendRelayResult(conn, &relayExecResult{
		ExitCode: result.ExitCode,
		Stdout:   result.Stdout,
		Stderr:   result.Stderr,
	})
}

func (r *ExecRelay) handleExecInteractive(conn net.Conn, data []byte) {
	var req relayExecInteractiveRequest
	if err := json.Unmarshal(data, &req); err != nil {
		sendRelayMsg(conn, relayMsgExit, []byte{0, 0, 0, 1})
		return
	}

	interactiveMachine, ok := r.sb.Machine().(vm.InteractiveMachine)
	if !ok {
		sendRelayMsg(conn, relayMsgExit, []byte{0, 0, 0, 1})
		return
	}

	opts := &api.ExecOptions{Env: make(map[string]string)}
	if r.sb.CAPool() != nil {
		certPath := "/etc/ssl/certs/matchlock-ca.crt"
		opts.Env["SSL_CERT_FILE"] = certPath
		opts.Env["REQUESTS_CA_BUNDLE"] = certPath
		opts.Env["CURL_CA_BUNDLE"] = certPath
		opts.Env["NODE_EXTRA_CA_CERTS"] = certPath
	}
	if r.sb.Policy() != nil {
		for name, placeholder := range r.sb.Policy().GetPlaceholders() {
			opts.Env[name] = placeholder
		}
	}

	stdinReader, stdinWriter := io.Pipe()
	stdoutWriter := &relayWriter{conn: conn, msgType: relayMsgStdout}

	// Read stdin from relay client and write to pipe
	go func() {
		defer stdinWriter.Close()
		for {
			msgType, data, err := readRelayMsg(conn)
			if err != nil {
				return
			}
			if msgType == relayMsgStdin {
				stdinWriter.Write(data)
			}
		}
	}()

	resizeCh := make(chan [2]uint16, 1)

	exitCode, err := interactiveMachine.ExecInteractive(
		context.Background(), req.Command, opts,
		req.Rows, req.Cols,
		stdinReader, stdoutWriter, resizeCh,
	)
	if err != nil {
		exitCode = 1
	}

	exitData := make([]byte, 4)
	binary.BigEndian.PutUint32(exitData, uint32(exitCode))
	sendRelayMsg(conn, relayMsgExit, exitData)
}

// relayWriter forwards writes to the relay connection as messages.
type relayWriter struct {
	conn    net.Conn
	msgType uint8
}

func (w *relayWriter) Write(p []byte) (int, error) {
	if err := sendRelayMsg(w.conn, w.msgType, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func readRelayMsg(conn net.Conn) (uint8, []byte, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(conn, header); err != nil {
		return 0, nil, err
	}

	msgType := header[0]
	length := binary.BigEndian.Uint32(header[1:])

	if length == 0 {
		return msgType, nil, nil
	}

	data := make([]byte, length)
	if _, err := io.ReadFull(conn, data); err != nil {
		return 0, nil, err
	}

	return msgType, data, nil
}

func sendRelayMsg(conn net.Conn, msgType uint8, data []byte) error {
	header := make([]byte, 5)
	header[0] = msgType
	binary.BigEndian.PutUint32(header[1:], uint32(len(data)))
	if _, err := conn.Write(header); err != nil {
		return err
	}
	if len(data) > 0 {
		if _, err := conn.Write(data); err != nil {
			return err
		}
	}
	return nil
}

func sendRelayResult(conn net.Conn, result *relayExecResult) {
	data, _ := json.Marshal(result)
	sendRelayMsg(conn, relayMsgExecResult, data)
}

// ExecViaRelay connects to an exec relay socket and runs a command.
func ExecViaRelay(ctx context.Context, socketPath, command string) (*api.ExecResult, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("connect to exec relay: %w", err)
	}
	defer conn.Close()

	req := relayExecRequest{Command: command}
	reqData, _ := json.Marshal(req)
	if err := sendRelayMsg(conn, relayMsgExec, reqData); err != nil {
		return nil, fmt.Errorf("send exec request: %w", err)
	}

	msgType, data, err := readRelayMsg(conn)
	if err != nil {
		return nil, fmt.Errorf("read exec result: %w", err)
	}

	if msgType != relayMsgExecResult {
		return nil, fmt.Errorf("unexpected message type: %d", msgType)
	}

	var result relayExecResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("decode exec result: %w", err)
	}

	if result.Error != "" {
		return &api.ExecResult{
			ExitCode: result.ExitCode,
			Stdout:   result.Stdout,
			Stderr:   result.Stderr,
		}, fmt.Errorf("%s", result.Error)
	}

	return &api.ExecResult{
		ExitCode: result.ExitCode,
		Stdout:   result.Stdout,
		Stderr:   result.Stderr,
	}, nil
}

// ExecInteractiveViaRelay connects to an exec relay socket and runs an interactive command.
func ExecInteractiveViaRelay(ctx context.Context, socketPath, command string, rows, cols uint16, stdin io.Reader, stdout io.Writer) (int, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return 1, fmt.Errorf("connect to exec relay: %w", err)
	}
	defer conn.Close()

	req := relayExecInteractiveRequest{
		Command: command,
		Rows:    rows,
		Cols:    cols,
	}
	reqData, _ := json.Marshal(req)
	if err := sendRelayMsg(conn, relayMsgExecInteractive, reqData); err != nil {
		return 1, fmt.Errorf("send interactive exec request: %w", err)
	}

	done := make(chan int, 1)
	errCh := make(chan error, 1)

	// Read stdout/exit from relay
	go func() {
		for {
			msgType, data, err := readRelayMsg(conn)
			if err != nil {
				errCh <- err
				return
			}
			switch msgType {
			case relayMsgStdout:
				stdout.Write(data)
			case relayMsgExit:
				if len(data) >= 4 {
					done <- int(binary.BigEndian.Uint32(data))
				} else {
					done <- 0
				}
				return
			}
		}
	}()

	// Send stdin to relay
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				sendRelayMsg(conn, relayMsgStdin, buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	select {
	case exitCode := <-done:
		return exitCode, nil
	case err := <-errCh:
		return 1, err
	case <-ctx.Done():
		return 1, ctx.Err()
	}
}
