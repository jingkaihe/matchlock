//go:build linux

package linux

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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

	// Use configured subnet or default to 192.168.100.0/24
	subnetCIDR := config.SubnetCIDR
	if subnetCIDR == "" {
		subnetCIDR = "192.168.100.1/24"
	}

	// Initial TAP configuration (will be refreshed after Firecracker starts)
	if err := ConfigureInterface(tapName, subnetCIDR); err != nil {
		syscall.Close(tapFD)
		DeleteInterface(tapName)
		return nil, fmt.Errorf("failed to configure TAP interface: %w", err)
	}

	if err := SetMTU(tapName, 1500); err != nil {
		syscall.Close(tapFD)
		DeleteInterface(tapName)
		return nil, fmt.Errorf("failed to set MTU: %w", err)
	}

	// Close the FD - Firecracker will re-open the device by name
	syscall.Close(tapFD)

	m := &LinuxMachine{
		id:         config.ID,
		config:     config,
		tapName:    tapName,
		tapFD:      -1, // FD closed, Firecracker will open it
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

	// Give Firecracker a moment to open the TAP device, then configure it
	time.Sleep(100 * time.Millisecond)

	// Re-configure the TAP interface (Firecracker resets it when opening)
	// Use configured subnet or default
	subnetCIDR := m.config.SubnetCIDR
	if subnetCIDR == "" {
		subnetCIDR = "192.168.100.1/24"
	}
	ConfigureInterface(m.tapName, subnetCIDR)
	SetMTU(m.tapName, 1500)

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
	vsockFailCount := 0
	maxVsockFailures := 50 // After 5 seconds of vsock failures, use fallback

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

		vsockFailCount++

		// If vsock consistently fails, fall back to checking if VM process is running
		// and the base vsock socket exists (indicates Firecracker has started)
		if vsockFailCount >= maxVsockFailures {
			// Check if the base vsock socket exists and VM is running
			if _, err := os.Stat(m.config.VsockPath); err == nil {
				// VM started but vsock ready signal not working
				// Wait a bit more for services to start, then proceed
				time.Sleep(3 * time.Second)
				return nil
			}
		}

		time.Sleep(100 * time.Millisecond)
	}

	return fmt.Errorf("timeout waiting for VM ready signal")
}

// dialVsock connects to the guest via the Firecracker vsock UDS
// Firecracker vsock protocol for host-initiated connections:
// 1. Connect to base UDS socket
// 2. Send "CONNECT <port>\n"
// 3. Read "OK <assigned_port>\n" acknowledgement
func (m *LinuxMachine) dialVsock(port uint32) (net.Conn, error) {
	if m.config.VsockPath == "" {
		return nil, fmt.Errorf("vsock not configured")
	}

	conn, err := net.Dial("unix", m.config.VsockPath)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to vsock UDS %s: %w", m.config.VsockPath, err)
	}

	// Send CONNECT command
	connectCmd := fmt.Sprintf("CONNECT %d\n", port)
	if _, err := conn.Write([]byte(connectCmd)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to send CONNECT command: %w", err)
	}

	// Read OK response (format: "OK <port>\n")
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to read CONNECT response: %w", err)
	}

	response := string(buf[:n])
	if len(response) < 3 || response[:2] != "OK" {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT failed, got: %q", response)
	}

	return conn, nil
}

func (m *LinuxMachine) generateFirecrackerConfig() []byte {
	kernelArgs := m.config.KernelArgs
	if kernelArgs == "" {
		// Use configured IPs or defaults
		guestIP := m.config.GuestIP
		if guestIP == "" {
			guestIP = "192.168.100.2"
		}
		gatewayIP := m.config.GatewayIP
		if gatewayIP == "" {
			gatewayIP = "192.168.100.1"
		}
		workspace := m.config.Workspace
		if workspace == "" {
			workspace = "/workspace"
		}
		// Use acpi=off to avoid memory region conflicts with virtio-mmio devices
		// Firecracker uses virtio-mmio transport, not virtio-pci
		// Format: ip=<client-ip>:<server-ip>:<gw-ip>:<netmask>:<hostname>:<device>:<autoconf>:<dns0>:<dns1>
		// matchlock.workspace is passed to guest-fused via /proc/cmdline
		kernelArgs = fmt.Sprintf("console=ttyS0 reboot=k panic=1 acpi=off ip=%s::%s:255.255.255.0::eth0:off:8.8.8.8:8.8.4.4 matchlock.workspace=%s", guestIP, gatewayIP, workspace)
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

	// Check if process already exited
	if m.cmd.ProcessState != nil && m.cmd.ProcessState.Exited() {
		return nil
	}

	if err := m.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		// Process already finished is not an error
		if err.Error() == "os: process already finished" {
			return nil
		}
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

// execVsock executes a command via vsock.
// When opts.Stdout/Stderr are set, uses streaming mode (MsgTypeExecStream) and
// forwards output chunks to the writers in real-time.
func (m *LinuxMachine) execVsock(ctx context.Context, command string, opts *api.ExecOptions) (*api.ExecResult, error) {
	start := time.Now()

	conn, err := m.dialVsock(VsockPortExec)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to exec service: %w", err)
	}
	defer conn.Close()

	req := vsock.ExecRequest{
		Command: command,
	}
	if opts != nil {
		req.WorkingDir = opts.WorkingDir
		req.Env = opts.Env
	}

	reqData, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to encode exec request: %w", err)
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
		return nil, fmt.Errorf("failed to write header: %w", err)
	}
	if _, err := conn.Write(reqData); err != nil {
		return nil, fmt.Errorf("failed to write request: %w", err)
	}

	var stdout, stderr bytes.Buffer
	for {
		if _, err := readFull(conn, header); err != nil {
			return nil, fmt.Errorf("failed to read response header: %w", err)
		}

		msgType := header[0]
		length := uint32(header[1])<<24 | uint32(header[2])<<16 | uint32(header[3])<<8 | uint32(header[4])

		data := make([]byte, length)
		if length > 0 {
			if _, err := readFull(conn, data); err != nil {
				return nil, fmt.Errorf("failed to read response data: %w", err)
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
				return nil, fmt.Errorf("failed to decode exec response: %w", err)
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

// ExecInteractive executes a command with PTY support for interactive sessions
func (m *LinuxMachine) ExecInteractive(ctx context.Context, command string, opts *api.ExecOptions, rows, cols uint16, stdin io.Reader, stdout io.Writer, resizeCh <-chan [2]uint16) (int, error) {
	if m.config.VsockCID == 0 || m.config.VsockPath == "" {
		return 1, fmt.Errorf("vsock not configured")
	}

	conn, err := m.dialVsock(VsockPortExec)
	if err != nil {
		return 1, fmt.Errorf("failed to connect to exec service: %w", err)
	}
	defer conn.Close()

	// Build TTY exec request
	req := vsock.ExecTTYRequest{
		Command: command,
		Rows:    rows,
		Cols:    cols,
	}
	if opts != nil {
		req.WorkingDir = opts.WorkingDir
		req.Env = opts.Env
	}

	reqData, err := json.Marshal(req)
	if err != nil {
		return 1, fmt.Errorf("failed to encode request: %w", err)
	}

	// Send TTY exec request
	header := make([]byte, 5)
	header[0] = vsock.MsgTypeExecTTY
	binary.BigEndian.PutUint32(header[1:], uint32(len(reqData)))

	if _, err := conn.Write(header); err != nil {
		return 1, fmt.Errorf("failed to write header: %w", err)
	}
	if _, err := conn.Write(reqData); err != nil {
		return 1, fmt.Errorf("failed to write request: %w", err)
	}

	done := make(chan int, 1)
	errCh := make(chan error, 1)

	// Read stdout from guest
	go func() {
		header := make([]byte, 5)
		for {
			if _, err := readFull(conn, header); err != nil {
				errCh <- err
				return
			}

			msgType := header[0]
			length := binary.BigEndian.Uint32(header[1:])

			data := make([]byte, length)
			if length > 0 {
				if _, err := readFull(conn, data); err != nil {
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

	// Send stdin to guest
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				sendVsockMessage(conn, vsock.MsgTypeStdin, buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	// Handle resize events
	go func() {
		for size := range resizeCh {
			data := make([]byte, 4)
			binary.BigEndian.PutUint16(data[0:2], size[0]) // rows
			binary.BigEndian.PutUint16(data[2:4], size[1]) // cols
			sendVsockMessage(conn, vsock.MsgTypeResize, data)
		}
	}()

	select {
	case exitCode := <-done:
		return exitCode, nil
	case err := <-errCh:
		return 1, err
	case <-ctx.Done():
		sendVsockMessage(conn, vsock.MsgTypeSignal, []byte{byte(syscall.SIGTERM)})
		return 1, ctx.Err()
	}
}

func sendVsockMessage(conn net.Conn, msgType uint8, data []byte) error {
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

// TapName returns the TAP interface name
func (m *LinuxMachine) TapName() string {
	return m.tapName
}

func (m *LinuxMachine) PID() int {
	return m.pid
}

func (m *LinuxMachine) RootfsPath() string {
	return m.config.RootfsPath
}

func (m *LinuxMachine) Close() error {
	var errs []error

	if m.cmd != nil && m.cmd.Process != nil {
		if err := m.Stop(context.Background()); err != nil {
			errs = append(errs, fmt.Errorf("stop: %w", err))
		}
		// Wait for process to fully exit
		m.cmd.Wait()
	}

	if m.tapFD > 0 {
		if err := syscall.Close(m.tapFD); err != nil {
			errs = append(errs, fmt.Errorf("close tap fd: %w", err))
		}
	}

	if m.tapName != "" {
		if err := DeleteInterface(m.tapName); err != nil {
			errs = append(errs, fmt.Errorf("delete interface %s: %w", m.tapName, err))
		}
	}

	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}
