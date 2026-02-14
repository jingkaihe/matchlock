//go:build darwin

package darwin

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"

	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/Code-Hex/vz/v3"
	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/vm"
	"github.com/jingkaihe/matchlock/pkg/vsock"
)

type DarwinMachine struct {
	id          string
	config      *vm.VMConfig
	vm          *vz.VirtualMachine
	socketPair  *SocketPair
	tempRootfs  string // Temp copy of rootfs, cleaned up on Stop
	started     bool
	mu          sync.Mutex
	vfsListener *vz.VirtioSocketListener
}

func (m *DarwinMachine) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.started {
		return nil
	}

	if err := m.vm.Start(); err != nil {
		return errx.Wrap(ErrVMStart, err)
	}

	m.started = true

	if err := m.waitForReady(ctx, 30*time.Second); err != nil {
		m.Stop(ctx)
		return errx.Wrap(ErrVMNotReady, err)
	}

	return nil
}

func (m *DarwinMachine) waitForReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		conn, err := m.dialVsock(VsockPortReady)
		if err == nil {
			conn.Close()
			return nil
		}

		time.Sleep(100 * time.Millisecond)
	}

	return ErrVMReady
}

func (m *DarwinMachine) dialVsock(port uint32) (net.Conn, error) {
	socketDevices := m.vm.SocketDevices()
	if len(socketDevices) == 0 {
		return nil, ErrNoVsockDevice
	}
	return socketDevices[0].Connect(port)
}

// DialVsock opens a host-initiated vsock stream to a guest service port.
func (m *DarwinMachine) DialVsock(port uint32) (net.Conn, error) {
	return m.dialVsock(port)
}

func (m *DarwinMachine) Stop(ctx context.Context) error {
	return m.stop(ctx, false)
}

func (m *DarwinMachine) stop(ctx context.Context, force bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.started {
		if m.tempRootfs != "" {
			os.Remove(m.tempRootfs)
			m.tempRootfs = ""
		}
		return nil
	}

	if !force && m.vm.CanRequestStop() {
		success, err := m.vm.RequestStop()
		if err == nil && success {
			stateChanged := m.vm.StateChangedNotify()
		waitLoop:
			for {
				if m.vm.State() == vz.VirtualMachineStateStopped {
					break
				}
				select {
				case <-stateChanged:
					if m.vm.State() == vz.VirtualMachineStateStopped {
						break waitLoop
					}
				case <-ctx.Done():
					break waitLoop
				}
			}

			if m.vm.State() == vz.VirtualMachineStateStopped {
				m.started = false
				m.cleanup()
				return nil
			}
		}
	}

	if m.vm.CanStop() {
		if err := m.vm.Stop(); err != nil {
			m.cleanup()
			return err
		}
	}

	m.started = false
	m.cleanup()
	return nil
}

func (m *DarwinMachine) cleanup() {
	if m.socketPair != nil {
		m.socketPair.Close()
	}
	if m.tempRootfs != "" {
		os.Remove(m.tempRootfs)
		m.tempRootfs = ""
	}
}

func (m *DarwinMachine) Wait(ctx context.Context) error {
	stateChanged := m.vm.StateChangedNotify()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case state := <-stateChanged:
			if state == vz.VirtualMachineStateStopped {
				return nil
			}
		}
	}
}

func (m *DarwinMachine) Exec(ctx context.Context, command string, opts *api.ExecOptions) (*api.ExecResult, error) {
	if opts != nil && opts.Stdin != nil {
		conn, err := m.dialVsock(VsockPortExec)
		if err != nil {
			return nil, errx.Wrap(ErrExecConnect, err)
		}
		return vsock.ExecPipe(ctx, conn, command, opts)
	}

	start := time.Now()

	conn, err := m.dialVsock(VsockPortExec)
	if err != nil {
		return nil, errx.Wrap(ErrExecConnect, err)
	}
	defer conn.Close()

	// Watch for context cancellation and close the connection to unblock reads.
	// Closing the connection causes the guest agent to see EOF and kill the child.
	stop := context.AfterFunc(ctx, func() {
		conn.Close()
	})
	defer stop()

	req := vsock.ExecRequest{
		Command: command,
	}
	if opts != nil {
		req.WorkingDir = opts.WorkingDir
		req.Env = opts.Env
		req.User = opts.User
	}

	reqData, err := json.Marshal(req)
	if err != nil {
		return nil, errx.Wrap(ErrExecEncode, err)
	}

	streaming := opts != nil && (opts.Stdout != nil || opts.Stderr != nil)

	header := make([]byte, 5)
	if streaming {
		header[0] = vsock.MsgTypeExecStream
	} else {
		header[0] = vsock.MsgTypeExec
	}
	header[1] = byte(len(reqData) >> 24)
	header[2] = byte(len(reqData) >> 16)
	header[3] = byte(len(reqData) >> 8)
	header[4] = byte(len(reqData))

	if _, err := conn.Write(header); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errx.Wrap(ErrExecWriteHeader, err)
	}
	if _, err := conn.Write(reqData); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errx.Wrap(ErrExecWriteReq, err)
	}

	var stdout, stderr bytes.Buffer
	for {
		if _, err := vsock.ReadFull(conn, header); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errx.Wrap(ErrExecReadHeader, err)
		}

		msgType := header[0]
		length := uint32(header[1])<<24 | uint32(header[2])<<16 | uint32(header[3])<<8 | uint32(header[4])

		data := make([]byte, length)
		if length > 0 {
			if _, err := vsock.ReadFull(conn, data); err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, errx.Wrap(ErrExecReadData, err)
			}
		}

		switch msgType {
		case vsock.MsgTypeStdout:
			if streaming && opts.Stdout != nil {
				opts.Stdout.Write(data)
			}
			stdout.Write(data)
		case vsock.MsgTypeStderr:
			if streaming && opts.Stderr != nil {
				opts.Stderr.Write(data)
			}
			stderr.Write(data)
		case vsock.MsgTypeExecResult:
			var resp vsock.ExecResponse
			if err := json.Unmarshal(data, &resp); err != nil {
				return nil, errx.Wrap(ErrExecDecode, err)
			}

			duration := time.Since(start)

			stdoutData := stdout.Bytes()
			stderrData := stderr.Bytes()
			if len(stdoutData) == 0 && len(resp.Stdout) > 0 {
				stdoutData = resp.Stdout
			}
			if len(stderrData) == 0 && len(resp.Stderr) > 0 {
				stderrData = resp.Stderr
			}

			result := &api.ExecResult{
				ExitCode:   resp.ExitCode,
				Stdout:     stdoutData,
				Stderr:     stderrData,
				Duration:   duration,
				DurationMS: duration.Milliseconds(),
			}

			if resp.Error != "" {
				return result, errx.With(ErrExecRemote, ": %s", resp.Error)
			}

			return result, nil
		}
	}
}

func (m *DarwinMachine) ExecInteractive(ctx context.Context, command string, opts *api.ExecOptions, rows, cols uint16, stdin io.Reader, stdout io.Writer, resizeCh <-chan [2]uint16) (int, error) {
	conn, err := m.dialVsock(VsockPortExec)
	if err != nil {
		return 1, errx.Wrap(ErrExecConnect, err)
	}
	defer conn.Close()

	req := vsock.ExecTTYRequest{
		Command: command,
		Rows:    rows,
		Cols:    cols,
	}
	if opts != nil {
		req.WorkingDir = opts.WorkingDir
		req.Env = opts.Env
		req.User = opts.User
	}

	reqData, err := json.Marshal(req)
	if err != nil {
		return 1, errx.Wrap(ErrExecEncodeReq, err)
	}

	header := make([]byte, 5)
	header[0] = vsock.MsgTypeExecTTY
	binary.BigEndian.PutUint32(header[1:], uint32(len(reqData)))

	if _, err := conn.Write(header); err != nil {
		return 1, errx.Wrap(ErrExecWriteHeader, err)
	}
	if _, err := conn.Write(reqData); err != nil {
		return 1, errx.Wrap(ErrExecWriteReq, err)
	}

	done := make(chan int, 1)
	errCh := make(chan error, 1)

	go func() {
		header := make([]byte, 5)
		for {
			if _, err := vsock.ReadFull(conn, header); err != nil {
				errCh <- err
				return
			}

			msgType := header[0]
			length := binary.BigEndian.Uint32(header[1:])

			data := make([]byte, length)
			if length > 0 {
				if _, err := vsock.ReadFull(conn, data); err != nil {
					errCh <- err
					return
				}
			}

			switch msgType {
			case vsock.MsgTypeStdout:
				stdout.Write(data)
			case vsock.MsgTypeExit:
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
			n, err := stdin.Read(buf)
			if n > 0 {
				vsock.SendMessage(conn, vsock.MsgTypeStdin, buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	go func() {
		for size := range resizeCh {
			data := make([]byte, 4)
			binary.BigEndian.PutUint16(data[0:2], size[0])
			binary.BigEndian.PutUint16(data[2:4], size[1])
			vsock.SendMessage(conn, vsock.MsgTypeResize, data)
		}
	}()

	select {
	case exitCode := <-done:
		return exitCode, nil
	case err := <-errCh:
		return 1, err
	case <-ctx.Done():
		vsock.SendMessage(conn, vsock.MsgTypeSignal, []byte{byte(syscall.SIGTERM)})
		return 1, ctx.Err()
	}
}

func (m *DarwinMachine) NetworkFD() (int, error) {
	return m.socketPair.HostFD(), nil
}

func (m *DarwinMachine) NetworkFile() *os.File {
	return m.socketPair.HostFile()
}

func (m *DarwinMachine) VsockFD() (int, error) {
	return -1, ErrVsockFD
}

func (m *DarwinMachine) SocketDevice() *vz.VirtioSocketDevice {
	socketDevices := m.vm.SocketDevices()
	if len(socketDevices) == 0 {
		return nil
	}
	return socketDevices[0]
}

func (m *DarwinMachine) PID() int {
	return 0
}

func (m *DarwinMachine) RootfsPath() string {
	return m.tempRootfs
}

func (m *DarwinMachine) Close(ctx context.Context) error {
	var errs []error

	m.mu.Lock()
	started := m.started
	m.mu.Unlock()

	if started {
		if err := m.stop(ctx, false); err != nil {
			errs = append(errs, errx.Wrap(ErrStop, err))
		}
	}

	if m.vfsListener != nil {
		if err := m.vfsListener.Close(); err != nil {
			errs = append(errs, errx.Wrap(ErrCloseVFSListener, err))
		}
	}

	if m.socketPair != nil {
		if err := m.socketPair.Close(); err != nil {
			errs = append(errs, errx.Wrap(ErrCloseSocketPair, err))
		}
	}

	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

func (m *DarwinMachine) SetupVFSListener() (*vz.VirtioSocketListener, error) {
	socketDevice := m.SocketDevice()
	if socketDevice == nil {
		return nil, ErrNoVsockDevice
	}

	listener, err := socketDevice.Listen(VsockPortVFS)
	if err != nil {
		return nil, err
	}
	m.vfsListener = listener
	return listener, nil
}

func (m *DarwinMachine) Config() *vm.VMConfig {
	return m.config
}
