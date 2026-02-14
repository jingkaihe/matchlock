//go:build linux

// Guest agent runs inside the Firecracker VM and handles:
// 1. Command execution requests from the host
// 2. Ready signal to indicate VM is ready
// 3. VFS client connection to host for FUSE
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/creack/pty"
	"github.com/jingkaihe/matchlock/internal/errx"
)

const (
	cancelGracePeriod = 5 * time.Second

	AF_VSOCK        = 40
	VMADDR_CID_HOST = 2

	VsockPortExec  = 5000
	VsockPortVFS   = 5001
	VsockPortReady = 5002

	MsgTypeExec        uint8 = 1
	MsgTypeExecResult  uint8 = 2
	MsgTypeStdout      uint8 = 3
	MsgTypeStderr      uint8 = 4
	MsgTypeSignal      uint8 = 5
	MsgTypeReady       uint8 = 6
	MsgTypeStdin       uint8 = 7
	MsgTypeResize      uint8 = 8
	MsgTypeExecTTY     uint8 = 9
	MsgTypeExit        uint8 = 10
	MsgTypeExecStream  uint8 = 11
	MsgTypeExecPipe    uint8 = 12
	MsgTypePortForward uint8 = 13
)

type sockaddrVM struct {
	Family    uint16
	Reserved1 uint16
	Port      uint32
	CID       uint32
	Zero      [4]byte
}

type ExecRequest struct {
	Command    string            `json:"command"`
	Args       []string          `json:"args"`
	WorkingDir string            `json:"working_dir"`
	Env        map[string]string `json:"env"`
	Stdin      []byte            `json:"stdin"`
	User       string            `json:"user,omitempty"`
}

type ExecTTYRequest struct {
	Command    string            `json:"command"`
	Args       []string          `json:"args"`
	WorkingDir string            `json:"working_dir"`
	Env        map[string]string `json:"env"`
	Rows       uint16            `json:"rows"`
	Cols       uint16            `json:"cols"`
	User       string            `json:"user,omitempty"`
}

type ExecResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   []byte `json:"stdout"`
	Stderr   []byte `json:"stderr"`
	Error    string `json:"error"`
}

type PortForwardRequest struct {
	Host string `json:"host,omitempty"`
	Port uint16 `json:"port"`
}

func main() {
	// If re-execed as sandbox launcher, apply seccomp + drop caps + exec real command
	if isSandboxLauncher() {
		runSandboxLauncher()
		return
	}

	fmt.Println("Guest agent starting...")

	// Mount /proc inside new PID namespace (children need it)
	ensureProcMounted()

	// Start ready listener first
	go serveReady()

	// Start exec service
	serveExec()
}

// ensureProcMounted ensures /proc is mounted. When child processes run in a new
// PID namespace, they need a fresh /proc mount to see their own PID space.
func ensureProcMounted() {
	os.MkdirAll("/proc", 0555)
	syscall.Mount("proc", "/proc", "proc", 0, "")
}

func serveReady() {
	listener, err := listenVsock(VsockPortReady)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to listen on ready port: %v\n", err)
		return
	}
	defer syscall.Close(listener)

	fmt.Println("Ready signal listener started on port", VsockPortReady)

	for {
		conn, err := acceptVsock(listener)
		if err != nil {
			continue
		}
		// Just accept and close - connection success means ready
		syscall.Close(conn)
	}
}

func serveExec() {
	listener, err := listenVsock(VsockPortExec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to listen on exec port: %v\n", err)
		os.Exit(1)
	}
	defer syscall.Close(listener)

	fmt.Println("Exec service started on port", VsockPortExec)

	for {
		conn, err := acceptVsock(listener)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Accept error: %v\n", err)
			continue
		}
		go handleExec(conn)
	}
}

// monitorVsockCancel monitors the vsock fd for EOF (host closed the connection
// due to cancellation) and gracefully terminates the child process using
// process-group kill: SIGTERM → cancelGracePeriod → SIGKILL.
//
// Returns a channel that the caller MUST close after cmd.Wait() returns to
// prevent signals being sent to a recycled PID.
func monitorVsockCancel(fd int, cmd *exec.Cmd) chan struct{} {
	waitDone := make(chan struct{})
	go func() {
		buf := make([]byte, 1)
		syscall.Read(fd, buf)
		select {
		case <-waitDone:
			return
		default:
		}
		pid := cmd.Process.Pid
		syscall.Kill(-pid, syscall.SIGTERM)
		timer := time.AfterFunc(cancelGracePeriod, func() {
			select {
			case <-waitDone:
				return
			default:
			}
			syscall.Kill(-pid, syscall.SIGKILL)
		})
		go func() {
			<-waitDone
			timer.Stop()
		}()
	}()
	return waitDone
}

func handleExec(fd int) {
	// Read message header (type + length)
	header := make([]byte, 5)
	if _, err := readFull(fd, header); err != nil {
		syscall.Close(fd)
		return
	}

	msgType := header[0]
	length := uint32(header[1])<<24 | uint32(header[2])<<16 | uint32(header[3])<<8 | uint32(header[4])

	// Read request data
	data := make([]byte, length)
	if _, err := readFull(fd, data); err != nil {
		syscall.Close(fd)
		return
	}

	switch msgType {
	case MsgTypeExec:
		handleExecBatch(fd, data)
		syscall.Close(fd)
	case MsgTypeExecStream:
		handleExecStreamBatch(fd, data)
		syscall.Close(fd)
	case MsgTypeExecPipe:
		handleExecPipe(fd, data)
	case MsgTypePortForward:
		handlePortForward(fd, data)
	case MsgTypeExecTTY:
		handleExecTTY(fd, data)
	default:
		syscall.Close(fd)
	}
}

func handlePortForward(fd int, data []byte) {
	var req PortForwardRequest
	if err := json.Unmarshal(data, &req); err != nil {
		sendMessage(fd, MsgTypeStderr, []byte(fmt.Sprintf("invalid port-forward request: %v", err)))
		syscall.Close(fd)
		return
	}

	host := req.Host
	if host == "" {
		host = "127.0.0.1"
	}
	if req.Port == 0 {
		sendMessage(fd, MsgTypeStderr, []byte("invalid remote port 0"))
		syscall.Close(fd)
		return
	}

	target, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(req.Port))))
	if err != nil {
		sendMessage(fd, MsgTypeStderr, []byte(fmt.Sprintf("connect %s:%d failed: %v", host, req.Port, err)))
		syscall.Close(fd)
		return
	}
	defer target.Close()

	sendMessage(fd, MsgTypeReady, nil)

	done := make(chan struct{}, 2)
	go func() {
		copyFDToConn(fd, target)
		if cw, ok := target.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		copyConnToFD(target, fd)
		done <- struct{}{}
	}()

	<-done
	<-done
	syscall.Close(fd)
}

func copyFDToConn(fd int, dst net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		n, err := syscall.Read(fd, buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return
			}
		}
		if err != nil || n == 0 {
			return
		}
	}
}

func copyConnToFD(src net.Conn, fd int) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, writeErr := syscall.Write(fd, buf[:n]); writeErr != nil {
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				return
			}
			return
		}
	}
}

func handleExecBatch(fd int, data []byte) {
	var req ExecRequest
	if err := json.Unmarshal(data, &req); err != nil {
		sendExecResponse(fd, &ExecResponse{Error: err.Error()})
		return
	}

	wipeBytes(data)

	var stdout, stderr bytes.Buffer
	cmd := exec.Command("sh", "-c", req.Command)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if req.WorkingDir != "" {
		cmd.Dir = req.WorkingDir
	}

	if len(req.Env) > 0 {
		env := os.Environ()
		for k, v := range req.Env {
			env = append(env, fmt.Sprintf("%s=%s", k, v))
		}
		cmd.Env = env
	}

	applyUserEnv(cmd, req.User)
	applySandboxSysProcAttrBatch(cmd)
	wrapCommandForSandbox(cmd)
	wipeMap(req.Env)

	if err := cmd.Start(); err != nil {
		sendExecResponse(fd, &ExecResponse{ExitCode: 1, Error: err.Error()})
		return
	}

	waitDone := monitorVsockCancel(fd, cmd)

	err := cmd.Wait()
	close(waitDone)

	resp := &ExecResponse{
		Stdout: stdout.Bytes(),
		Stderr: stderr.Bytes(),
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			resp.ExitCode = exitErr.ExitCode()
		} else {
			resp.Error = err.Error()
			resp.ExitCode = 1
		}
	}

	sendExecResponse(fd, resp)
}

// handleExecStreamBatch streams stdout/stderr as MsgTypeStdout/MsgTypeStderr
// chunks in real-time, then sends MsgTypeExecResult with just the exit code.
func handleExecStreamBatch(fd int, data []byte) {
	var req ExecRequest
	if err := json.Unmarshal(data, &req); err != nil {
		sendExecResponse(fd, &ExecResponse{Error: err.Error()})
		return
	}

	wipeBytes(data)

	cmd := exec.Command("sh", "-c", req.Command)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		sendExecResponse(fd, &ExecResponse{Error: err.Error()})
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		sendExecResponse(fd, &ExecResponse{Error: err.Error()})
		return
	}

	if req.WorkingDir != "" {
		cmd.Dir = req.WorkingDir
	}

	if len(req.Env) > 0 {
		env := os.Environ()
		for k, v := range req.Env {
			env = append(env, fmt.Sprintf("%s=%s", k, v))
		}
		cmd.Env = env
	}

	applyUserEnv(cmd, req.User)

	applySandboxSysProcAttrBatch(cmd)
	wrapCommandForSandbox(cmd)
	wipeMap(req.Env)

	if err := cmd.Start(); err != nil {
		sendExecResponse(fd, &ExecResponse{ExitCode: 1, Error: err.Error()})
		return
	}

	waitDone := monitorVsockCancel(fd, cmd)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := stdoutPipe.Read(buf)
			if n > 0 {
				sendMessage(fd, MsgTypeStdout, buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := stderrPipe.Read(buf)
			if n > 0 {
				sendMessage(fd, MsgTypeStderr, buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	wg.Wait()
	cmdErr := cmd.Wait()
	close(waitDone)

	resp := &ExecResponse{}
	if cmdErr != nil {
		if exitErr, ok := cmdErr.(*exec.ExitError); ok {
			resp.ExitCode = exitErr.ExitCode()
		} else {
			resp.Error = cmdErr.Error()
			resp.ExitCode = 1
		}
	}

	sendExecResponse(fd, resp)
}

// handleExecPipe is like handleExecStreamBatch but also accepts MsgTypeStdin
// frames from the host and pipes them into cmd.Stdin. This enables bidirectional
// stdio communication without a PTY — suitable for JSON-RPC and similar protocols.
func handleExecPipe(fd int, data []byte) {
	var req ExecRequest
	if err := json.Unmarshal(data, &req); err != nil {
		sendMessage(fd, MsgTypeStderr, []byte(fmt.Sprintf("failed to decode exec request: %v\n", err)))
		sendExitCode(fd, 1)
		syscall.Close(fd)
		return
	}

	wipeBytes(data)

	cmd := exec.Command("sh", "-c", req.Command)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		sendMessage(fd, MsgTypeStderr, []byte(fmt.Sprintf("failed to create stdin pipe: %v\n", err)))
		sendExitCode(fd, 1)
		syscall.Close(fd)
		return
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		sendMessage(fd, MsgTypeStderr, []byte(fmt.Sprintf("failed to create stdout pipe: %v\n", err)))
		sendExitCode(fd, 1)
		syscall.Close(fd)
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		sendMessage(fd, MsgTypeStderr, []byte(fmt.Sprintf("failed to create stderr pipe: %v\n", err)))
		sendExitCode(fd, 1)
		syscall.Close(fd)
		return
	}

	if req.WorkingDir != "" {
		cmd.Dir = req.WorkingDir
	}

	if len(req.Env) > 0 {
		env := os.Environ()
		for k, v := range req.Env {
			env = append(env, fmt.Sprintf("%s=%s", k, v))
		}
		cmd.Env = env
	}

	applyUserEnv(cmd, req.User)
	applySandboxSysProcAttrBatch(cmd)
	wrapCommandForSandbox(cmd)
	wipeMap(req.Env)

	if err := cmd.Start(); err != nil {
		sendMessage(fd, MsgTypeStderr, []byte(fmt.Sprintf("failed to start command: %v\n", err)))
		sendExitCode(fd, 1)
		syscall.Close(fd)
		return
	}

	// waitDone is closed after cmd.Wait() returns. Goroutines must not send
	// signals to cmd.Process after this because the PID may be recycled.
	waitDone := make(chan struct{})

	// Read MsgTypeStdin from host and write to cmd stdin.
	// On EOF or error from the vsock (host closed connection due to context
	// cancellation), close the stdin pipe and SIGTERM→SIGKILL the child process.
	// This replaces monitorVsockCancel for pipe mode since the stdin goroutine
	// already owns the read side of the vsock fd.
	go func() {
		for {
			msgType, msgData, err := readMessage(fd)
			if err != nil {
				stdinPipe.Close()
				// Host closed connection — gracefully terminate the child
				select {
				case <-waitDone:
					return
				default:
				}
				pid := cmd.Process.Pid
				syscall.Kill(-pid, syscall.SIGTERM)
				timer := time.AfterFunc(cancelGracePeriod, func() {
					select {
					case <-waitDone:
						return
					default:
					}
					syscall.Kill(-pid, syscall.SIGKILL)
				})
				go func() {
					<-waitDone
					timer.Stop()
				}()
				return
			}
			switch msgType {
			case MsgTypeStdin:
				if len(msgData) == 0 {
					stdinPipe.Close()
					return
				}
				stdinPipe.Write(msgData)
			case MsgTypeSignal:
				if len(msgData) >= 1 && cmd.Process != nil {
					syscall.Kill(-cmd.Process.Pid, syscall.Signal(msgData[0]))
				}
			}
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := stdoutPipe.Read(buf)
			if n > 0 {
				sendMessage(fd, MsgTypeStdout, buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := stderrPipe.Read(buf)
			if n > 0 {
				sendMessage(fd, MsgTypeStderr, buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	wg.Wait()
	cmd.Wait()
	close(waitDone)

	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	sendExitCode(fd, exitCode)

	// Small delay to ensure exit code message is flushed before closing fd
	time.Sleep(100 * time.Millisecond)
	syscall.Close(fd)
}

func handleExecTTY(fd int, data []byte) {
	var req ExecTTYRequest
	if err := json.Unmarshal(data, &req); err != nil {
		sendExitCode(fd, 1)
		syscall.Close(fd)
		return
	}

	// Wipe the raw request data from memory
	wipeBytes(data)

	cmd := exec.Command("sh", "-c", req.Command)

	if req.WorkingDir != "" {
		cmd.Dir = req.WorkingDir
	}

	if len(req.Env) > 0 {
		env := os.Environ()
		for k, v := range req.Env {
			env = append(env, fmt.Sprintf("%s=%s", k, v))
		}
		cmd.Env = env
	}

	applyUserEnv(cmd, req.User)

	// Apply sandbox isolation: PID namespace + seccomp + cap drop via re-exec
	applySandboxSysProcAttr(cmd)
	wrapCommandForSandbox(cmd)

	// Wipe the request's env map from memory before running
	wipeMap(req.Env)

	// Start command with PTY
	ptmx, err := pty.Start(cmd)
	if err != nil {
		sendExitCode(fd, 1)
		syscall.Close(fd)
		return
	}
	defer ptmx.Close()

	// Set initial window size
	if req.Rows > 0 && req.Cols > 0 {
		pty.Setsize(ptmx, &pty.Winsize{Rows: req.Rows, Cols: req.Cols})
	}

	// Handle SIGCHLD to know when process exits
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGCHLD)
	defer signal.Stop(sigCh)

	done := make(chan struct{})

	// Copy PTY output to vsock (stdout)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				sendMessage(fd, MsgTypeStdout, buf[:n])
			}
			if err != nil {
				break
			}
		}
	}()

	// Read messages from vsock (stdin, resize, signal)
	go func() {
		for {
			msgType, msgData, err := readMessage(fd)
			if err != nil {
				break
			}

			switch msgType {
			case MsgTypeStdin:
				ptmx.Write(msgData)
			case MsgTypeResize:
				if len(msgData) >= 4 {
					rows := binary.BigEndian.Uint16(msgData[0:2])
					cols := binary.BigEndian.Uint16(msgData[2:4])
					pty.Setsize(ptmx, &pty.Winsize{Rows: rows, Cols: cols})
				}
			case MsgTypeSignal:
				if len(msgData) >= 1 {
					sig := syscall.Signal(msgData[0])
					cmd.Process.Signal(sig)
				}
			}
		}
	}()

	// Wait for process to exit
	go func() {
		cmd.Wait()
		close(done)
	}()

	<-done

	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	sendExitCode(fd, exitCode)

	// Small delay to ensure exit code is transmitted before closing
	time.Sleep(100 * time.Millisecond)
	syscall.Close(fd)
}

func applyUserEnv(cmd *exec.Cmd, user string) {
	if user == "" {
		return
	}
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, "MATCHLOCK_USER="+user)
}

func sendMessage(fd int, msgType uint8, data []byte) {
	header := make([]byte, 5)
	header[0] = msgType
	binary.BigEndian.PutUint32(header[1:], uint32(len(data)))
	syscall.Write(fd, header)
	if len(data) > 0 {
		syscall.Write(fd, data)
	}
}

func sendExitCode(fd int, code int) {
	data := make([]byte, 4)
	binary.BigEndian.PutUint32(data, uint32(code))
	sendMessage(fd, MsgTypeExit, data)
}

func readMessage(fd int) (uint8, []byte, error) {
	header := make([]byte, 5)
	if _, err := readFull(fd, header); err != nil {
		return 0, nil, err
	}

	msgType := header[0]
	length := binary.BigEndian.Uint32(header[1:])

	if length == 0 {
		return msgType, nil, nil
	}

	data := make([]byte, length)
	if _, err := readFull(fd, data); err != nil {
		return 0, nil, err
	}

	return msgType, data, nil
}

func sendExecResponse(fd int, resp *ExecResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		return
	}

	header := make([]byte, 5)
	header[0] = MsgTypeExecResult
	header[1] = byte(len(data) >> 24)
	header[2] = byte(len(data) >> 16)
	header[3] = byte(len(data) >> 8)
	header[4] = byte(len(data))

	syscall.Write(fd, header)
	syscall.Write(fd, data)
}

func listenVsock(port uint32) (int, error) {
	fd, err := syscall.Socket(AF_VSOCK, syscall.SOCK_STREAM, 0)
	if err != nil {
		return -1, errx.Wrap(ErrSocket, err)
	}

	addr := sockaddrVM{
		Family: AF_VSOCK,
		CID:    0xFFFFFFFF, // VMADDR_CID_ANY
		Port:   port,
	}

	_, _, errno := syscall.Syscall(
		syscall.SYS_BIND,
		uintptr(fd),
		uintptr(unsafe.Pointer(&addr)),
		unsafe.Sizeof(addr),
	)
	if errno != 0 {
		syscall.Close(fd)
		return -1, errx.Wrap(ErrBind, errno)
	}

	if err := syscall.Listen(fd, syscall.SOMAXCONN); err != nil {
		syscall.Close(fd)
		return -1, errx.Wrap(ErrListen, err)
	}

	return fd, nil
}

func acceptVsock(listenFd int) (int, error) {
	var addr sockaddrVM
	addrLen := uint32(unsafe.Sizeof(addr))

	nfd, _, errno := syscall.Syscall(
		syscall.SYS_ACCEPT,
		uintptr(listenFd),
		uintptr(unsafe.Pointer(&addr)),
		uintptr(unsafe.Pointer(&addrLen)),
	)
	if errno != 0 {
		return -1, errno
	}

	return int(nfd), nil
}

func dialVsock(cid, port uint32) (int, error) {
	fd, err := syscall.Socket(AF_VSOCK, syscall.SOCK_STREAM, 0)
	if err != nil {
		return -1, errx.Wrap(ErrSocket, err)
	}

	addr := sockaddrVM{
		Family: AF_VSOCK,
		CID:    cid,
		Port:   port,
	}

	_, _, errno := syscall.Syscall(
		syscall.SYS_CONNECT,
		uintptr(fd),
		uintptr(unsafe.Pointer(&addr)),
		unsafe.Sizeof(addr),
	)
	if errno != 0 {
		syscall.Close(fd)
		return -1, errx.Wrap(ErrConnect, errno)
	}

	return fd, nil
}

func readFull(fd int, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := syscall.Read(fd, buf[total:])
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, ErrEOF
		}
		total += n
	}
	return total, nil
}

// VFS client for FUSE daemon (placeholder - would need full FUSE implementation)
type VFSClient struct {
	fd int
}

func NewVFSClient() (*VFSClient, error) {
	fd, err := dialVsock(VMADDR_CID_HOST, VsockPortVFS)
	if err != nil {
		return nil, err
	}
	return &VFSClient{fd: fd}, nil
}

func (c *VFSClient) Close() error {
	return syscall.Close(c.fd)
}
