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
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/creack/pty"
)

const (
	AF_VSOCK        = 40
	VMADDR_CID_HOST = 2

	VsockPortExec  = 5000
	VsockPortVFS   = 5001
	VsockPortReady = 5002

	MsgTypeExec       uint8 = 1
	MsgTypeExecResult uint8 = 2
	MsgTypeStdout     uint8 = 3
	MsgTypeStderr     uint8 = 4
	MsgTypeSignal     uint8 = 5
	MsgTypeReady      uint8 = 6
	MsgTypeStdin      uint8 = 7
	MsgTypeResize     uint8 = 8
	MsgTypeExecTTY    uint8 = 9
	MsgTypeExit       uint8 = 10
	MsgTypeExecStream uint8 = 11
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
}

type ExecTTYRequest struct {
	Command    string            `json:"command"`
	Args       []string          `json:"args"`
	WorkingDir string            `json:"working_dir"`
	Env        map[string]string `json:"env"`
	Rows       uint16            `json:"rows"`
	Cols       uint16            `json:"cols"`
}

type ExecResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   []byte `json:"stdout"`
	Stderr   []byte `json:"stderr"`
	Error    string `json:"error"`
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
	case MsgTypeExecTTY:
		handleExecTTY(fd, data)
	default:
		syscall.Close(fd)
	}
}

func handleExecBatch(fd int, data []byte) {
	var req ExecRequest
	if err := json.Unmarshal(data, &req); err != nil {
		sendExecResponse(fd, &ExecResponse{Error: err.Error()})
		return
	}

	// Wipe the raw request data from memory now that it's been deserialized
	wipeBytes(data)

	// Execute command
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

	// Apply sandbox isolation: PID namespace + seccomp + cap drop via re-exec
	applySandboxSysProcAttr(cmd)
	wrapCommandForSandbox(cmd)

	// Wipe the request's env map from memory before running
	wipeMap(req.Env)

	err := cmd.Run()

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

	applySandboxSysProcAttr(cmd)
	wrapCommandForSandbox(cmd)
	wipeMap(req.Env)

	if err := cmd.Start(); err != nil {
		sendExecResponse(fd, &ExecResponse{ExitCode: 1, Error: err.Error()})
		return
	}

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
		return -1, fmt.Errorf("socket: %w", err)
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
		return -1, fmt.Errorf("bind: %w", errno)
	}

	if err := syscall.Listen(fd, syscall.SOMAXCONN); err != nil {
		syscall.Close(fd)
		return -1, fmt.Errorf("listen: %w", err)
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
		return -1, fmt.Errorf("socket: %w", err)
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
		return -1, fmt.Errorf("connect: %w", errno)
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
			return total, fmt.Errorf("EOF")
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


