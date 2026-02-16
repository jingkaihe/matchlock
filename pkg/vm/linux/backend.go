//go:build linux

package linux

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jingkaihe/matchlock/internal/errx"
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
	tapName := tapNameForVMID(config.ID)
	tapFD, err := CreateTAP(tapName)
	if err != nil {
		return nil, errx.Wrap(ErrTAPCreate, err)
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
		return nil, errx.Wrap(ErrTAPConfigure, err)
	}

	if err := SetMTU(tapName, effectiveMTU(config.MTU)); err != nil {
		syscall.Close(tapFD)
		DeleteInterface(tapName)
		return nil, errx.Wrap(ErrTAPSetMTU, err)
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

func tapNameForVMID(vmID string) string {
	suffix := strings.TrimPrefix(vmID, "vm-")
	if len(suffix) >= 8 {
		suffix = suffix[:8]
	} else {
		h := fnv.New32a()
		_, _ = h.Write([]byte(vmID))
		hash := fmt.Sprintf("%08x", h.Sum32())
		suffix = (suffix + hash)[:8]
	}
	return "fc-" + suffix
}

type LinuxMachine struct {
	id         string
	config     *vm.VMConfig
	tapName    string
	tapFD      int
	macAddress string
	cmd        *exec.Cmd
	pid        int
	started    bool
}

func (m *LinuxMachine) Start(ctx context.Context) error {
	if m.started {
		return nil
	}

	fcConfig := m.generateFirecrackerConfig()

	configPath := filepath.Join(filepath.Dir(m.config.SocketPath), "config.json")
	if err := os.WriteFile(configPath, fcConfig, 0644); err != nil {
		return errx.Wrap(ErrWriteConfig, err)
	}

	m.cmd = exec.CommandContext(ctx, "firecracker",
		"--api-sock", m.config.SocketPath,
		"--config-file", configPath,
	)

	if m.config.LogPath != "" {
		logFile, err := os.Create(m.config.LogPath)
		if err != nil {
			return errx.Wrap(ErrCreateLogFile, err)
		}
		m.cmd.Stdout = logFile
		m.cmd.Stderr = logFile
	}

	if err := m.cmd.Start(); err != nil {
		return errx.Wrap(ErrStartFirecracker, err)
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
	SetMTU(m.tapName, effectiveMTU(m.config.MTU))

	// Wait for VM to be ready
	if m.config.VsockCID > 0 {
		if err := m.waitForReady(ctx, 30*time.Second); err != nil {
			m.Stop(ctx)
			return errx.Wrap(ErrVMNotReady, err)
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

	return ErrVMReadyTimeout
}

// dialVsock connects to the guest via the Firecracker vsock UDS
// Firecracker vsock protocol for host-initiated connections:
// 1. Connect to base UDS socket
// 2. Send "CONNECT <port>\n"
// 3. Read "OK <assigned_port>\n" acknowledgement
func (m *LinuxMachine) dialVsock(port uint32) (net.Conn, error) {
	if m.config.VsockPath == "" {
		return nil, ErrVsockNotConfigured
	}

	conn, err := net.Dial("unix", m.config.VsockPath)
	if err != nil {
		return nil, errx.With(ErrVsockConnect, " %s: %w", m.config.VsockPath, err)
	}

	// Send CONNECT command
	connectCmd := fmt.Sprintf("CONNECT %d\n", port)
	if _, err := conn.Write([]byte(connectCmd)); err != nil {
		conn.Close()
		return nil, errx.Wrap(ErrVsockSendConnect, err)
	}

	// Read OK response (format: "OK <port>\n")
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		conn.Close()
		return nil, errx.Wrap(ErrVsockReadResponse, err)
	}

	response := string(buf[:n])
	if len(response) < 3 || response[:2] != "OK" {
		conn.Close()
		return nil, errx.With(ErrVsockConnectFailed, ", got: %q", response)
	}

	return conn, nil
}

// DialVsock opens a host-initiated vsock stream to a guest service port.
func (m *LinuxMachine) DialVsock(port uint32) (net.Conn, error) {
	return m.dialVsock(port)
}

func (m *LinuxMachine) generateFirecrackerConfig() []byte {
	kernelArgs := m.config.KernelArgs
	if kernelArgs == "" {
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
		mtu := effectiveMTU(m.config.MTU)
		kernelArgs = fmt.Sprintf("console=ttyS0 reboot=k panic=1 acpi=off init=/init ip=%s::%s:255.255.255.0::eth0:off%s matchlock.workspace=%s matchlock.dns=%s",
			guestIP, gatewayIP, vm.KernelIPDNSSuffix(m.config.DNSServers), workspace, vm.KernelDNSParam(m.config.DNSServers))
		kernelArgs += fmt.Sprintf(" matchlock.mtu=%d", mtu)
		if m.config.Privileged {
			kernelArgs += " matchlock.privileged=1"
		}
		for i, disk := range m.config.ExtraDisks {
			dev := string(rune('b' + i)) // vdb, vdc, ...
			kernelArgs += fmt.Sprintf(" matchlock.disk.vd%s=%s", dev, disk.GuestMount)
		}
	}

	type fcDrive struct {
		DriveID      string `json:"drive_id"`
		PathOnHost   string `json:"path_on_host"`
		IsRootDevice bool   `json:"is_root_device"`
		IsReadOnly   bool   `json:"is_read_only"`
	}

	drives := []fcDrive{
		{DriveID: "rootfs", PathOnHost: m.config.RootfsPath, IsRootDevice: true, IsReadOnly: false},
	}
	for i, disk := range m.config.ExtraDisks {
		drives = append(drives, fcDrive{
			DriveID:      fmt.Sprintf("disk%d", i),
			PathOnHost:   disk.HostPath,
			IsRootDevice: false,
			IsReadOnly:   disk.ReadOnly,
		})
	}

	type fcConfig struct {
		BootSource struct {
			KernelImagePath string `json:"kernel_image_path"`
			BootArgs        string `json:"boot_args"`
		} `json:"boot-source"`
		Drives        []fcDrive `json:"drives"`
		MachineConfig struct {
			VCPUCount  int `json:"vcpu_count"`
			MemSizeMiB int `json:"mem_size_mib"`
		} `json:"machine-config"`
		NetworkInterfaces []struct {
			IfaceID     string `json:"iface_id"`
			GuestMAC    string `json:"guest_mac"`
			HostDevName string `json:"host_dev_name"`
		} `json:"network-interfaces"`
		Vsock *struct {
			GuestCID uint32 `json:"guest_cid"`
			UDSPath  string `json:"uds_path"`
		} `json:"vsock,omitempty"`
	}

	var cfg fcConfig
	cfg.BootSource.KernelImagePath = m.config.KernelPath
	cfg.BootSource.BootArgs = kernelArgs
	cfg.Drives = drives
	cfg.MachineConfig.VCPUCount = m.config.CPUs
	cfg.MachineConfig.MemSizeMiB = m.config.MemoryMB
	cfg.NetworkInterfaces = []struct {
		IfaceID     string `json:"iface_id"`
		GuestMAC    string `json:"guest_mac"`
		HostDevName string `json:"host_dev_name"`
	}{
		{IfaceID: "eth0", GuestMAC: m.macAddress, HostDevName: m.tapName},
	}

	if m.config.VsockCID > 0 {
		cfg.Vsock = &struct {
			GuestCID uint32 `json:"guest_cid"`
			UDSPath  string `json:"uds_path"`
		}{
			GuestCID: m.config.VsockCID,
			UDSPath:  m.config.VsockPath,
		}
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		panic(fmt.Sprintf("failed to marshal firecracker config: %v", err))
	}
	return data
}

func effectiveMTU(mtu int) int {
	if mtu > 0 {
		return mtu
	}
	return api.DefaultNetworkMTU
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
		return nil, ErrVsockNotConfigured
	}
	return m.execVsock(ctx, command, opts)
}

// execVsock executes a command via vsock.
// When opts.Stdout/Stderr are set, uses streaming mode (MsgTypeExecStream) and
// forwards output chunks to the writers in real-time.
// When opts.Stdin is set, uses pipe mode (MsgTypeExecPipe) which additionally
// forwards stdin to the guest process without allocating a PTY.
func (m *LinuxMachine) execVsock(ctx context.Context, command string, opts *api.ExecOptions) (*api.ExecResult, error) {
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
		return nil, errx.Wrap(ErrExecEncodeRequest, err)
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
		return nil, errx.Wrap(ErrExecWriteRequest, err)
	}

	var stdout, stderr bytes.Buffer
	for {
		if _, err := vsock.ReadFull(conn, header); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errx.Wrap(ErrExecReadRespHeader, err)
		}

		msgType := header[0]
		length := uint32(header[1])<<24 | uint32(header[2])<<16 | uint32(header[3])<<8 | uint32(header[4])

		data := make([]byte, length)
		if length > 0 {
			if _, err := vsock.ReadFull(conn, data); err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, errx.Wrap(ErrExecReadRespData, err)
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
				return nil, errx.Wrap(ErrExecDecodeResponse, err)
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

// ExecInteractive executes a command with PTY support for interactive sessions
func (m *LinuxMachine) ExecInteractive(ctx context.Context, command string, opts *api.ExecOptions, rows, cols uint16, stdin io.Reader, stdout io.Writer, resizeCh <-chan [2]uint16) (int, error) {
	if m.config.VsockCID == 0 || m.config.VsockPath == "" {
		return 1, ErrVsockNotConfigured
	}

	conn, err := m.dialVsock(VsockPortExec)
	if err != nil {
		return 1, errx.Wrap(ErrExecConnect, err)
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
		req.User = opts.User
	}

	reqData, err := json.Marshal(req)
	if err != nil {
		return 1, errx.Wrap(ErrExecEncodeRequest, err)
	}

	// Send TTY exec request
	header := make([]byte, 5)
	header[0] = vsock.MsgTypeExecTTY
	binary.BigEndian.PutUint32(header[1:], uint32(len(reqData)))

	if _, err := conn.Write(header); err != nil {
		return 1, errx.Wrap(ErrExecWriteHeader, err)
	}
	if _, err := conn.Write(reqData); err != nil {
		return 1, errx.Wrap(ErrExecWriteRequest, err)
	}

	done := make(chan int, 1)
	errCh := make(chan error, 1)

	// Read stdout from guest
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

	// Send stdin to guest
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

	// Handle resize events
	go func() {
		for size := range resizeCh {
			data := make([]byte, 4)
			binary.BigEndian.PutUint16(data[0:2], size[0]) // rows
			binary.BigEndian.PutUint16(data[2:4], size[1]) // cols
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

func (m *LinuxMachine) NetworkFD() (int, error) {
	return m.tapFD, nil
}

func (m *LinuxMachine) VsockFD() (int, error) {
	return -1, ErrVsockNoDirectFD
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

func (m *LinuxMachine) Close(ctx context.Context) error {
	var errs []error

	if m.cmd != nil && m.cmd.Process != nil {
		if err := m.Stop(ctx); err != nil {
			errs = append(errs, errx.Wrap(ErrStop, err))
		}
		// Wait for process to fully exit
		m.cmd.Wait()
	}

	if m.tapFD > 0 {
		if err := syscall.Close(m.tapFD); err != nil {
			errs = append(errs, errx.Wrap(ErrCloseTapFD, err))
		}
	}

	if m.tapName != "" {
		if err := DeleteInterface(m.tapName); err != nil {
			errs = append(errs, errx.Wrap(ErrTAPDelete, err))
		}
	}

	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}
