package linux

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/vm"
	"github.com/jingkaihe/matchlock/pkg/vsock"
)

const (
	// VsockPortExec is the port for command execution
	VsockPortExec = 5000
	// VsockPortVFS is the port for VFS protocol
	VsockPortVFS = 5001
	// VsockPortReady is the port for ready signal
	VsockPortReady = 5002
)

type LinuxBackend struct{}

func NewLinuxBackend() *LinuxBackend {
	return &LinuxBackend{}
}

func (b *LinuxBackend) Name() string {
	return "firecracker"
}

func (b *LinuxBackend) Create(ctx context.Context, config *vm.VMConfig) (vm.Machine, error) {
	tapName := fmt.Sprintf("fc-%s", config.ID[:8])
	tapFD, err := CreateTAP(tapName)
	if err != nil {
		return nil, fmt.Errorf("failed to create TAP device: %w", err)
	}

	if err := ConfigureInterface(tapName, "192.168.100.1/24"); err != nil {
		syscall.Close(tapFD)
		return nil, fmt.Errorf("failed to configure TAP interface: %w", err)
	}

	if err := SetMTU(tapName, 1500); err != nil {
		syscall.Close(tapFD)
		return nil, fmt.Errorf("failed to set MTU: %w", err)
	}

	m := &LinuxMachine{
		id:         config.ID,
		config:     config,
		tapName:    tapName,
		tapFD:      tapFD,
		macAddress: GenerateMAC(config.ID),
	}

	return m, nil
}

type LinuxMachine struct {
	id           string
	config       *vm.VMConfig
	tapName      string
	tapFD        int
	macAddress   string
	cmd          *exec.Cmd
	pid          int
	started      bool
	vsockConn    *vsock.Conn
	vsockMu      sync.Mutex
	vsockUDSPath string
}

func (m *LinuxMachine) Start(ctx context.Context) error {
	if m.started {
		return nil
	}

	fcConfig := m.generateFirecrackerConfig()

	configPath := filepath.Join(filepath.Dir(m.config.SocketPath), "config.json")
	if err := os.WriteFile(configPath, fcConfig, 0644); err != nil {
		return fmt.Errorf("failed to write firecracker config: %w", err)
	}

	m.cmd = exec.CommandContext(ctx, "firecracker",
		"--api-sock", m.config.SocketPath,
		"--config-file", configPath,
	)

	if m.config.LogPath != "" {
		logFile, err := os.Create(m.config.LogPath)
		if err != nil {
			return fmt.Errorf("failed to create log file: %w", err)
		}
		m.cmd.Stdout = logFile
		m.cmd.Stderr = logFile
	}

	if err := m.cmd.Start(); err != nil {
		return fmt.Errorf("failed to start firecracker: %w", err)
	}

	m.pid = m.cmd.Process.Pid
	m.started = true

	// Wait for VM to be ready
	if m.config.VsockCID > 0 {
		if err := m.waitForReady(ctx, 30*time.Second); err != nil {
			m.Stop(ctx)
			return fmt.Errorf("VM failed to become ready: %w", err)
		}
	} else {
		// Fallback: wait a bit for boot
		time.Sleep(500 * time.Millisecond)
	}

	return nil
}

func (m *LinuxMachine) waitForReady(ctx context.Context, timeout time.Duration) error {
	if m.config.VsockPath == "" {
		return nil
	}

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Try to connect to the ready port via UDS forwarded by Firecracker
		conn, err := m.dialVsock(VsockPortReady)
		if err == nil {
			conn.Close()
			return nil
		}

		time.Sleep(100 * time.Millisecond)
	}

	return fmt.Errorf("timeout waiting for VM ready signal")
}

// dialVsock connects to the guest via the Firecracker vsock UDS
func (m *LinuxMachine) dialVsock(port uint32) (net.Conn, error) {
	if m.config.VsockPath == "" {
		return nil, fmt.Errorf("vsock not configured")
	}

	// Firecracker exposes vsock via Unix socket: {uds_path}_{port}
	udsPath := fmt.Sprintf("%s_%d", m.config.VsockPath, port)

	conn, err := net.Dial("unix", udsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to vsock UDS %s: %w", udsPath, err)
	}

	return conn, nil
}

func (m *LinuxMachine) generateFirecrackerConfig() []byte {
	kernelArgs := m.config.KernelArgs
	if kernelArgs == "" {
		kernelArgs = "console=ttyS0 reboot=k panic=1 pci=off ip=192.168.100.2::192.168.100.1:255.255.255.0::eth0:off"
	}

	config := fmt.Sprintf(`{
  "boot-source": {
    "kernel_image_path": %q,
    "boot_args": %q
  },
  "drives": [
    {
      "drive_id": "rootfs",
      "path_on_host": %q,
      "is_root_device": true,
      "is_read_only": false
    }
  ],
  "machine-config": {
    "vcpu_count": %d,
    "mem_size_mib": %d
  },
  "network-interfaces": [
    {
      "iface_id": "eth0",
      "guest_mac": %q,
      "host_dev_name": %q
    }
  ]
}`,
		m.config.KernelPath,
		kernelArgs,
		m.config.RootfsPath,
		m.config.CPUs,
		m.config.MemoryMB,
		m.macAddress,
		m.tapName,
	)

	if m.config.VsockCID > 0 {
		config = fmt.Sprintf(`{
  "boot-source": {
    "kernel_image_path": %q,
    "boot_args": %q
  },
  "drives": [
    {
      "drive_id": "rootfs",
      "path_on_host": %q,
      "is_root_device": true,
      "is_read_only": false
    }
  ],
  "machine-config": {
    "vcpu_count": %d,
    "mem_size_mib": %d
  },
  "network-interfaces": [
    {
      "iface_id": "eth0",
      "guest_mac": %q,
      "host_dev_name": %q
    }
  ],
  "vsock": {
    "guest_cid": %d,
    "uds_path": %q
  }
}`,
			m.config.KernelPath,
			kernelArgs,
			m.config.RootfsPath,
			m.config.CPUs,
			m.config.MemoryMB,
			m.macAddress,
			m.tapName,
			m.config.VsockCID,
			m.config.VsockPath,
		)
	}

	return []byte(config)
}

func (m *LinuxMachine) Stop(ctx context.Context) error {
	if m.cmd == nil || m.cmd.Process == nil {
		return nil
	}

	if err := m.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return m.cmd.Process.Kill()
	}

	done := make(chan error, 1)
	go func() {
		_, err := m.cmd.Process.Wait()
		done <- err
	}()

	select {
	case <-done:
		return nil
	case <-time.After(5 * time.Second):
		return m.cmd.Process.Kill()
	case <-ctx.Done():
		return m.cmd.Process.Kill()
	}
}

func (m *LinuxMachine) Wait(ctx context.Context) error {
	if m.cmd == nil {
		return nil
	}
	return m.cmd.Wait()
}

func (m *LinuxMachine) Exec(ctx context.Context, command string, opts *api.ExecOptions) (*api.ExecResult, error) {
	if m.config.VsockCID == 0 || m.config.VsockPath == "" {
		return nil, fmt.Errorf("vsock not configured; VsockCID and VsockPath are required")
	}
	return m.execVsock(ctx, command, opts)
}

// execVsock executes a command via vsock
func (m *LinuxMachine) execVsock(ctx context.Context, command string, opts *api.ExecOptions) (*api.ExecResult, error) {
	start := time.Now()

	conn, err := m.dialVsock(VsockPortExec)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to exec service: %w", err)
	}
	defer conn.Close()

	// Build exec request
	req := vsock.ExecRequest{
		Command: command,
	}
	if opts != nil {
		req.WorkingDir = opts.WorkingDir
		req.Env = opts.Env
	}

	// Encode and send request
	reqData, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to encode exec request: %w", err)
	}

	// Write message type and length
	header := make([]byte, 5)
	header[0] = vsock.MsgTypeExec
	header[1] = byte(len(reqData) >> 24)
	header[2] = byte(len(reqData) >> 16)
	header[3] = byte(len(reqData) >> 8)
	header[4] = byte(len(reqData))

	if _, err := conn.Write(header); err != nil {
		return nil, fmt.Errorf("failed to write header: %w", err)
	}
	if _, err := conn.Write(reqData); err != nil {
		return nil, fmt.Errorf("failed to write request: %w", err)
	}

	// Read response
	var stdout, stderr bytes.Buffer
	for {
		// Read message header
		if _, err := readFull(conn, header); err != nil {
			return nil, fmt.Errorf("failed to read response header: %w", err)
		}

		msgType := header[0]
		length := uint32(header[1])<<24 | uint32(header[2])<<16 | uint32(header[3])<<8 | uint32(header[4])

		// Read message data
		data := make([]byte, length)
		if length > 0 {
			if _, err := readFull(conn, data); err != nil {
				return nil, fmt.Errorf("failed to read response data: %w", err)
			}
		}

		switch msgType {
		case vsock.MsgTypeStdout:
			stdout.Write(data)
		case vsock.MsgTypeStderr:
			stderr.Write(data)
		case vsock.MsgTypeExecResult:
			var resp vsock.ExecResponse
			if err := json.Unmarshal(data, &resp); err != nil {
				return nil, fmt.Errorf("failed to decode exec response: %w", err)
			}

			duration := time.Since(start)
			result := &api.ExecResult{
				ExitCode:   resp.ExitCode,
				Stdout:     stdout.Bytes(),
				Stderr:     stderr.Bytes(),
				Duration:   duration,
				DurationMS: duration.Milliseconds(),
			}

			if resp.Error != "" {
				return result, fmt.Errorf("exec error: %s", resp.Error)
			}

			return result, nil
		}
	}
}

func readFull(conn net.Conn, buf []byte) (int, error) {
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



func (m *LinuxMachine) NetworkFD() (int, error) {
	return m.tapFD, nil
}

func (m *LinuxMachine) VsockFD() (int, error) {
	return -1, fmt.Errorf("vsock not implemented for direct FD access; use VsockPath for UDS")
}

// VsockPath returns the vsock UDS path for connecting to guest services
func (m *LinuxMachine) VsockPath() string {
	return m.config.VsockPath
}

// VsockCID returns the guest CID
func (m *LinuxMachine) VsockCID() uint32 {
	return m.config.VsockCID
}

func (m *LinuxMachine) PID() int {
	return m.pid
}

func (m *LinuxMachine) Close() error {
	var errs []error

	if m.cmd != nil && m.cmd.Process != nil {
		if err := m.Stop(context.Background()); err != nil {
			errs = append(errs, err)
		}
	}

	if m.tapFD > 0 {
		if err := syscall.Close(m.tapFD); err != nil {
			errs = append(errs, err)
		}
	}

	if m.tapName != "" {
		DeleteInterface(m.tapName)
	}

	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}
