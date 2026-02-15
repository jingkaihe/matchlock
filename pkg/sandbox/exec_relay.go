package sandbox

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"

	"github.com/jingkaihe/matchlock/internal/errx"
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
	relayMsgExecPipe        uint8 = 8
	relayMsgPortForward     uint8 = 9
)

type relayExecRequest struct {
	Command    string `json:"command"`
	WorkingDir string `json:"working_dir,omitempty"`
	User       string `json:"user,omitempty"`
}

type relayExecInteractiveRequest struct {
	Command    string `json:"command"`
	WorkingDir string `json:"working_dir,omitempty"`
	User       string `json:"user,omitempty"`
	Rows       uint16 `json:"rows"`
	Cols       uint16 `json:"cols"`
}

type relayPortForwardRequest struct {
	RemotePort int `json:"remote_port"`
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
		return errx.With(ErrRelayListen, " %s: %w", socketPath, err)
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
	case relayMsgExecPipe:
		r.handleExecPipe(conn, data)
	case relayMsgPortForward:
		r.handlePortForward(conn, data)
	}
}

func (r *ExecRelay) handleExec(conn net.Conn, data []byte) {
	var req relayExecRequest
	if err := json.Unmarshal(data, &req); err != nil {
		sendRelayResult(conn, &relayExecResult{ExitCode: 1, Error: err.Error()})
		return
	}

	opts := r.sb.PrepareExecEnv()
	if req.WorkingDir != "" {
		opts.WorkingDir = req.WorkingDir
	}
	if req.User != "" {
		opts.User = req.User
	}

	// Create a context that is cancelled when the relay connection closes,
	// so that if the `matchlock exec` client disconnects, the command is killed.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		buf := make([]byte, 1)
		conn.Read(buf) // blocks until EOF
		cancel()
	}()

	result, err := r.sb.Exec(ctx, req.Command, opts)
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

	opts := r.sb.PrepareExecEnv()
	if req.WorkingDir != "" {
		opts.WorkingDir = req.WorkingDir
	}
	if req.User != "" {
		opts.User = req.User
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

func (r *ExecRelay) handleExecPipe(conn net.Conn, data []byte) {
	var req relayExecRequest
	if err := json.Unmarshal(data, &req); err != nil {
		exitData := make([]byte, 4)
		binary.BigEndian.PutUint32(exitData, 1)
		sendRelayMsg(conn, relayMsgExit, exitData)
		return
	}

	opts := r.sb.PrepareExecEnv()
	if req.WorkingDir != "" {
		opts.WorkingDir = req.WorkingDir
	}
	if req.User != "" {
		opts.User = req.User
	}

	stdinReader, stdinWriter := io.Pipe()
	opts.Stdin = stdinReader
	opts.Stdout = &relayWriter{conn: conn, msgType: relayMsgStdout}
	opts.Stderr = &relayWriter{conn: conn, msgType: relayMsgStderr}

	// Cancel the context when the relay client disconnects (read error/EOF
	// from the stdin reader goroutine) so the VM-side process is killed.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		defer stdinWriter.Close()
		for {
			msgType, msgData, err := readRelayMsg(conn)
			if err != nil {
				cancel()
				return
			}
			if msgType == relayMsgStdin {
				if len(msgData) == 0 {
					return
				}
				stdinWriter.Write(msgData)
			}
		}
	}()

	result, err := r.sb.Exec(ctx, req.Command, opts)

	exitCode := 0
	if err != nil {
		exitCode = 1
	} else {
		exitCode = result.ExitCode
	}

	sendRelayExit(conn, exitCode)
}

func (r *ExecRelay) handlePortForward(conn net.Conn, data []byte) {
	var req relayPortForwardRequest
	if err := json.Unmarshal(data, &req); err != nil {
		_ = sendRelayMsg(conn, relayMsgStderr, []byte(err.Error()))
		sendRelayExit(conn, 1)
		return
	}

	guestConn, err := r.sb.openGuestPort(req.RemotePort)
	if err != nil {
		_ = sendRelayMsg(conn, relayMsgStderr, []byte(err.Error()))
		sendRelayExit(conn, 1)
		return
	}
	defer guestConn.Close()

	go func() {
		for {
			msgType, msgData, err := readRelayMsg(conn)
			if err != nil {
				_ = guestConn.Close()
				return
			}
			if msgType != relayMsgStdin {
				continue
			}
			if len(msgData) == 0 {
				// Keep the guest stream open for reading response data.
				// Firecracker's host-side UDS transport can treat CloseWrite
				// as a full close, which drops the reverse direction.
				return
			}
			if _, err := guestConn.Write(msgData); err != nil {
				return
			}
		}
	}()

	buf := make([]byte, 32*1024)
	for {
		n, readErr := guestConn.Read(buf)
		if n > 0 {
			if err := sendRelayMsg(conn, relayMsgStdout, buf[:n]); err != nil {
				return
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) && !errors.Is(readErr, net.ErrClosed) {
				_ = sendRelayMsg(conn, relayMsgStderr, []byte(readErr.Error()))
				sendRelayExit(conn, 1)
				return
			}
			sendRelayExit(conn, 0)
			return
		}
	}
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

func sendRelayExit(conn net.Conn, code int) {
	exitData := make([]byte, 4)
	binary.BigEndian.PutUint32(exitData, uint32(code))
	sendRelayMsg(conn, relayMsgExit, exitData)
}

// ExecViaRelay connects to an exec relay socket and runs a command.
// The context controls the lifetime — if cancelled, the connection is closed.
func ExecViaRelay(ctx context.Context, socketPath, command, workingDir, user string) (*api.ExecResult, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, errx.Wrap(ErrRelayConnect, err)
	}
	defer conn.Close()

	// Watch for context cancellation
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()

	req := relayExecRequest{Command: command, WorkingDir: workingDir, User: user}
	reqData, _ := json.Marshal(req)
	if err := sendRelayMsg(conn, relayMsgExec, reqData); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errx.Wrap(ErrRelaySend, err)
	}

	msgType, data, err := readRelayMsg(conn)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errx.Wrap(ErrRelayRead, err)
	}

	if msgType != relayMsgExecResult {
		return nil, errx.With(ErrRelayUnexpected, ": %d", msgType)
	}

	var result relayExecResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, errx.Wrap(ErrRelayDecode, err)
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
func ExecInteractiveViaRelay(ctx context.Context, socketPath, command, workingDir, user string, rows, cols uint16, stdin io.Reader, stdout io.Writer) (int, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return 1, errx.Wrap(ErrRelayConnect, err)
	}
	defer conn.Close()

	req := relayExecInteractiveRequest{
		Command:    command,
		WorkingDir: workingDir,
		User:       user,
		Rows:       rows,
		Cols:       cols,
	}
	reqData, _ := json.Marshal(req)
	if err := sendRelayMsg(conn, relayMsgExecInteractive, reqData); err != nil {
		return 1, errx.Wrap(ErrRelaySend, err)
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

// ExecPipeViaRelay connects to an exec relay socket and runs a command with
// bidirectional stdin/stdout/stderr piping (no PTY).
func ExecPipeViaRelay(ctx context.Context, socketPath, command, workingDir, user string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return 1, errx.Wrap(ErrRelayConnect, err)
	}
	defer conn.Close()

	req := relayExecRequest{Command: command, WorkingDir: workingDir, User: user}
	reqData, _ := json.Marshal(req)
	if err := sendRelayMsg(conn, relayMsgExecPipe, reqData); err != nil {
		return 1, errx.Wrap(ErrRelaySend, err)
	}

	done := make(chan int, 1)
	errCh := make(chan error, 1)

	go func() {
		for {
			msgType, data, err := readRelayMsg(conn)
			if err != nil {
				if ctx.Err() != nil {
					errCh <- ctx.Err()
				} else {
					errCh <- err
				}
				return
			}
			switch msgType {
			case relayMsgStdout:
				stdout.Write(data)
			case relayMsgStderr:
				stderr.Write(data)
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

	go func() {
		buf := make([]byte, 4096)
		for {
			n, readErr := stdin.Read(buf)
			if n > 0 {
				sendRelayMsg(conn, relayMsgStdin, buf[:n])
			}
			if readErr != nil {
				sendRelayMsg(conn, relayMsgStdin, nil)
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
