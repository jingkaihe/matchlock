package vsock

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"syscall"
	"unsafe"

	"github.com/jingkaihe/matchlock/internal/errx"
)

const (
	// AF_VSOCK is the address family for vsock
	AF_VSOCK = 40

	// VMADDR_CID_ANY accepts connections from any CID
	VMADDR_CID_ANY = 0xFFFFFFFF
	// VMADDR_CID_HYPERVISOR is the CID for the hypervisor (host from guest perspective)
	VMADDR_CID_HYPERVISOR = 0
	// VMADDR_CID_LOCAL is the CID for local communication
	VMADDR_CID_LOCAL = 1
	// VMADDR_CID_HOST is the CID for the host
	VMADDR_CID_HOST = 2

	// VMADDR_PORT_ANY accepts connections on any port
	VMADDR_PORT_ANY = 0xFFFFFFFF

	// IOCTL_VM_SOCKETS_GET_LOCAL_CID gets the local CID
	IOCTL_VM_SOCKETS_GET_LOCAL_CID = 0x7B9
)

const (
	// ServicePortExec is the guest-agent exec and stream service port.
	ServicePortExec = 5000
	// ServicePortVFS is the guest VFS service port.
	ServicePortVFS = 5001
	// ServicePortReady is the guest ready-check service port.
	ServicePortReady = 5002
)

// sockaddrVM is the sockaddr_vm structure for vsock
type sockaddrVM struct {
	Family    uint16
	Reserved1 uint16
	Port      uint32
	CID       uint32
	Zero      [4]byte
}

// Addr represents a vsock address
type Addr struct {
	CID  uint32
	Port uint32
}

func (a *Addr) Network() string { return "vsock" }
func (a *Addr) String() string  { return fmt.Sprintf("%d:%d", a.CID, a.Port) }

// Conn represents a vsock connection
type Conn struct {
	fd       int
	local    *Addr
	remote   *Addr
	file     *os.File
	isClosed bool
}

func (c *Conn) Read(b []byte) (int, error) {
	return syscall.Read(c.fd, b)
}

func (c *Conn) Write(b []byte) (int, error) {
	return syscall.Write(c.fd, b)
}

func (c *Conn) Close() error {
	if c.isClosed {
		return nil
	}
	c.isClosed = true
	return syscall.Close(c.fd)
}

func (c *Conn) LocalAddr() net.Addr  { return c.local }
func (c *Conn) RemoteAddr() net.Addr { return c.remote }

func (c *Conn) SetDeadline(t interface{}) error      { return nil }
func (c *Conn) SetReadDeadline(t interface{}) error  { return nil }
func (c *Conn) SetWriteDeadline(t interface{}) error { return nil }

func (c *Conn) File() *os.File {
	if c.file == nil {
		c.file = os.NewFile(uintptr(c.fd), fmt.Sprintf("vsock:%s", c.remote))
	}
	return c.file
}

// Listener represents a vsock listener
type Listener struct {
	fd   int
	addr *Addr
}

// Listen creates a vsock listener on the given port
func Listen(port uint32) (*Listener, error) {
	return ListenCID(VMADDR_CID_ANY, port)
}

// ListenCID creates a vsock listener on the given CID and port
func ListenCID(cid, port uint32) (*Listener, error) {
	fd, err := syscall.Socket(AF_VSOCK, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, errx.Wrap(ErrCreateSocket, err)
	}

	addr := sockaddrVM{
		Family: AF_VSOCK,
		CID:    cid,
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
		return nil, errx.Wrap(ErrBind, errno)
	}

	if err := syscall.Listen(fd, syscall.SOMAXCONN); err != nil {
		syscall.Close(fd)
		return nil, errx.Wrap(ErrListen, err)
	}

	return &Listener{
		fd:   fd,
		addr: &Addr{CID: cid, Port: port},
	}, nil
}

func (l *Listener) Accept() (*Conn, error) {
	var addr sockaddrVM
	addrLen := uint32(unsafe.Sizeof(addr))

	nfd, _, errno := syscall.Syscall(
		syscall.SYS_ACCEPT,
		uintptr(l.fd),
		uintptr(unsafe.Pointer(&addr)),
		uintptr(unsafe.Pointer(&addrLen)),
	)
	if errno != 0 {
		return nil, errx.Wrap(ErrAccept, errno)
	}

	return &Conn{
		fd:     int(nfd),
		local:  l.addr,
		remote: &Addr{CID: addr.CID, Port: addr.Port},
	}, nil
}

func (l *Listener) Close() error {
	return syscall.Close(l.fd)
}

func (l *Listener) Addr() net.Addr {
	return l.addr
}

// Dial connects to a vsock address
func Dial(cid, port uint32) (*Conn, error) {
	fd, err := syscall.Socket(AF_VSOCK, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, errx.Wrap(ErrCreateSocket, err)
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
		return nil, errx.Wrap(ErrConnect, errno)
	}

	return &Conn{
		fd:     fd,
		local:  &Addr{CID: VMADDR_CID_LOCAL, Port: 0},
		remote: &Addr{CID: cid, Port: port},
	}, nil
}

// GetLocalCID returns the local CID of the machine
func GetLocalCID() (uint32, error) {
	f, err := os.Open("/dev/vsock")
	if err != nil {
		return 0, errx.Wrap(ErrOpenDevice, err)
	}
	defer f.Close()

	var cid uint32
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		f.Fd(),
		IOCTL_VM_SOCKETS_GET_LOCAL_CID,
		uintptr(unsafe.Pointer(&cid)),
	)
	if errno != 0 {
		return 0, errx.Wrap(ErrGetLocalCID, errno)
	}

	return cid, nil
}

// Protocol for command execution over vsock
const (
	MsgTypeExec        uint8 = 1
	MsgTypeExecResult  uint8 = 2
	MsgTypeStdout      uint8 = 3
	MsgTypeStderr      uint8 = 4
	MsgTypeSignal      uint8 = 5
	MsgTypeReady       uint8 = 6
	MsgTypeStdin       uint8 = 7  // TTY: stdin data from host
	MsgTypeResize      uint8 = 8  // TTY: window resize
	MsgTypeExecTTY     uint8 = 9  // TTY: exec with PTY
	MsgTypeExit        uint8 = 10 // TTY: process exited
	MsgTypeExecStream  uint8 = 11 // Streaming batch: stdout/stderr sent as chunks, then ExecResult
	MsgTypeExecPipe    uint8 = 12 // Pipe mode: like ExecStream but also accepts MsgTypeStdin, sends MsgTypeExit
	MsgTypePortForward uint8 = 13 // Request guest-agent to proxy raw TCP to an in-guest address
)

// ExecRequest is sent from host to guest to execute a command
type ExecRequest struct {
	Command    string            `json:"command"`
	Args       []string          `json:"args,omitempty"`
	WorkingDir string            `json:"working_dir,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Stdin      []byte            `json:"stdin,omitempty"`
	User       string            `json:"user,omitempty"` // "uid", "uid:gid", or username
}

// ExecTTYRequest is sent from host to guest for interactive execution
type ExecTTYRequest struct {
	Command    string            `json:"command"`
	Args       []string          `json:"args,omitempty"`
	WorkingDir string            `json:"working_dir,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Rows       uint16            `json:"rows"`
	Cols       uint16            `json:"cols"`
	User       string            `json:"user,omitempty"` // "uid", "uid:gid", or username
}

// PortForwardRequest asks the guest agent to dial a TCP destination in guest
// network namespace and then switch the vsock stream into raw proxy mode.
type PortForwardRequest struct {
	Host string `json:"host,omitempty"`
	Port uint16 `json:"port"`
}

// WindowSize represents terminal dimensions
type WindowSize struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

// ExecResponse is sent from guest to host with execution results
type ExecResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   []byte `json:"stdout,omitempty"`
	Stderr   []byte `json:"stderr,omitempty"`
	Error    string `json:"error,omitempty"`
}

// WriteMessage writes a length-prefixed message to the connection
func WriteMessage(conn *Conn, msgType uint8, data []byte) error {
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

// ReadMessage reads a length-prefixed message from the connection
func ReadMessage(conn *Conn) (uint8, []byte, error) {
	header := make([]byte, 5)
	if _, err := readFull(conn, header); err != nil {
		return 0, nil, err
	}

	msgType := header[0]
	length := binary.BigEndian.Uint32(header[1:])

	if length == 0 {
		return msgType, nil, nil
	}

	data := make([]byte, length)
	if _, err := readFull(conn, data); err != nil {
		return 0, nil, err
	}

	return msgType, data, nil
}

func readFull(conn *Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}
