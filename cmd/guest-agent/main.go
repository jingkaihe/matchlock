// Guest agent runs inside the Firecracker VM and handles:
// 1. Command execution requests from the host
// 2. Ready signal to indicate VM is ready
// 3. VFS client connection to host for FUSE
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

const (
	AF_VSOCK     = 40
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

type ExecResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   []byte `json:"stdout"`
	Stderr   []byte `json:"stderr"`
	Error    string `json:"error"`
}

func main() {
	fmt.Println("Guest agent starting...")

	// Start ready listener first
	go serveReady()

	// Start exec service
	serveExec()
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
	defer syscall.Close(fd)

	// Read message header (type + length)
	header := make([]byte, 5)
	if _, err := readFull(fd, header); err != nil {
		return
	}

	msgType := header[0]
	if msgType != MsgTypeExec {
		return
	}

	length := uint32(header[1])<<24 | uint32(header[2])<<16 | uint32(header[3])<<8 | uint32(header[4])

	// Read request data
	data := make([]byte, length)
	if _, err := readFull(fd, data); err != nil {
		return
	}

	var req ExecRequest
	if err := json.Unmarshal(data, &req); err != nil {
		sendExecResponse(fd, &ExecResponse{Error: err.Error()})
		return
	}

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

// Implement net.Conn interface for compatibility
type vsockConn struct {
	fd int
}

func (c *vsockConn) Read(b []byte) (int, error) {
	return syscall.Read(c.fd, b)
}

func (c *vsockConn) Write(b []byte) (int, error) {
	return syscall.Write(c.fd, b)
}

func (c *vsockConn) Close() error {
	return syscall.Close(c.fd)
}

func (c *vsockConn) LocalAddr() net.Addr  { return nil }
func (c *vsockConn) RemoteAddr() net.Addr { return nil }

func (c *vsockConn) SetDeadline(t interface{}) error      { return nil }
func (c *vsockConn) SetReadDeadline(t interface{}) error  { return nil }
func (c *vsockConn) SetWriteDeadline(t interface{}) error { return nil }
