//go:build linux

package linux

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/firecracker"
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

	// vmmLogTailBytes bounds how much of the VMM log is attached to an
	// early-exit error. The tail carries the real reason (e.g. Firecracker's
	// "VM config error") without echoing an entire boot log.
	vmmLogTailBytes = 2048

	// vmmExitWaitTimeout bounds how long Close waits for the VMM process to be
	// reaped before tearing down its TAP. Stop has already signaled (and, on a
	// canceled context, killed) the process, so this is only a safety net
	// against an unkillable VMM; it must be long enough that the kernel has
	// released the TAP descriptor, or DeleteInterface would see EBUSY.
	vmmExitWaitTimeout = 10 * time.Second
)

type LinuxBackend struct{}

func NewLinuxBackend() *LinuxBackend {
	return &LinuxBackend{}
}

func (b *LinuxBackend) Name() string {
	return "firecracker"
}

func (b *LinuxBackend) Create(ctx context.Context, config *vm.VMConfig) (vm.Machine, error) {
	vcpus, ok := api.VCPUCount(config.CPUs)
	if !ok {
		return nil, errx.With(ErrInvalidCPUCount, ": cpus must be a finite number > 0")
	}
	if vcpus > api.MaxFirecrackerVCPUs {
		return nil, errx.With(ErrInvalidCPUCount, ": cpus must be <= %d (Firecracker maximum)", api.MaxFirecrackerVCPUs)
	}
	hostCPUs := runtime.NumCPU()
	if vcpus > hostCPUs {
		return nil, errx.With(ErrInvalidCPUCount, ": cpus must be <= host cpus (%d)", hostCPUs)
	}

	tapName := ""
	tapFD := -1
	if !config.NoNetwork {
		tapName = tapNameForVMID(config.ID)
		var err error
		tapFD, err = CreateTAP(tapName)
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
		tapFD = -1
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

	// done is closed exactly once when cmd.Wait returns; waitErr holds the raw
	// wait error. Exactly one goroutine (started by Start) calls cmd.Wait, so
	// Stop, Wait, and waitForReady can observe the VMM exit without racing a
	// one-shot Wait. mutex guards these fields and the test seams below.
	done    chan struct{}
	waitErr error
	mutex   sync.Mutex

	// stopOnce ensures only the first Stop signals the process.
	stopOnce sync.Once

	// Test seams; nil in production (see backend_ready_test.go).
	newCommandFn     func(ctx context.Context, name string, args ...string) *exec.Cmd
	dialVsockFn      func(port uint32) (net.Conn, error)
	readVMMLogTailFn func() string
	stopFn           func(ctx context.Context) error
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

	name := firecracker.ResolveFirecrackerPath()
	args := []string{
		"--api-sock", m.config.SocketPath,
		"--config-file", configPath,
	}
	var cmd *exec.Cmd
	if m.newCommandFn != nil {
		cmd = m.newCommandFn(ctx, name, args...)
	} else {
		cmd = exec.CommandContext(ctx, name, args...)
	}
	m.cmd = cmd

	if m.config.LogPath != "" {
		logFile, err := os.OpenFile(m.config.LogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_APPEND, 0644)
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

	// Exactly one goroutine calls Wait; the raw error is memoized and done is
	// closed once so waitForReady/Stop/Wait share the single wait result.
	m.mutex.Lock()
	m.done = make(chan struct{})
	m.mutex.Unlock()
	go func() {
		err := m.cmd.Wait()
		m.mutex.Lock()
		m.waitErr = err
		m.mutex.Unlock()
		close(m.done)
	}()

	if m.tapName != "" {
		// Give Firecracker a moment to open the TAP device, then configure it.
		time.Sleep(100 * time.Millisecond)

		// Re-configure the TAP interface (Firecracker resets it when opening).
		subnetCIDR := m.config.SubnetCIDR
		if subnetCIDR == "" {
			subnetCIDR = "192.168.100.1/24"
		}
		_ = ConfigureInterface(m.tapName, subnetCIDR)
		_ = SetMTU(m.tapName, effectiveMTU(m.config.MTU))
	}

	// Wait for VM to be ready
	if m.config.VsockCID > 0 {
		if err := m.waitForReady(ctx, 30*time.Second); err != nil {
			m.Stop(ctx)
			return err
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
			return errx.Wrap(ErrVMNotReady, ctx.Err())
		default:
		}

		// A VMM that has already exited will never signal ready. Surface the
		// real reason from its log (e.g. a rejected --config-file) instead of
		// waiting out the full timeout and reporting a generic timeout.
		if m.vmmExited() {
			return m.vmmExitedError()
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

	return errx.Wrap(ErrVMNotReady, ErrVMReadyTimeout)
}

// vmmExited reports whether the VMM process has exited. It only observes the
// wait goroutine started by Start; it never calls Wait itself.
func (m *LinuxMachine) vmmExited() bool {
	m.mutex.Lock()
	done := m.done
	m.mutex.Unlock()
	if done == nil {
		return false
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// waitForVMMExit blocks until the single wait goroutine observes the VMM exit
// or timeout elapses, returning true when the process was reaped. It is
// deliberately context-independent: teardown must reap the VMM before deleting
// its persistent TAP even when the caller's context is already canceled.
func (m *LinuxMachine) waitForVMMExit(timeout time.Duration) bool {
	m.mutex.Lock()
	done := m.done
	m.mutex.Unlock()
	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// vmmExitedError builds the error returned when the VMM exits before the ready
// signal. It includes the tail of the VMM log, which carries the real config
// error (e.g. "VM config error") written by Firecracker for a rejected
// --config-file.
func (m *LinuxMachine) vmmExitedError() error {
	if tail := m.vmmLogTail(); tail != "" {
		return errx.With(ErrVMNotReady, ": VMM exited before ready signal: %s", tail)
	}
	return errx.With(ErrVMNotReady, ": VMM exited before ready signal")
}

// vmmLogTail returns the tail of the VMM log. Tests inject readVMMLogTailFn to
// exercise the early-exit path without a real VMM writing to LogPath.
func (m *LinuxMachine) vmmLogTail() string {
	if m.readVMMLogTailFn != nil {
		return m.readVMMLogTailFn()
	}
	return readLogTail(m.config.LogPath)
}

// readLogTail returns up to vmmLogTailBytes of the end of path, best effort.
func readLogTail(path string) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return ""
	}
	start := int64(0)
	if info.Size() > vmmLogTailBytes {
		start = info.Size() - vmmLogTailBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(f, vmmLogTailBytes))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// dialVsock connects to the guest via the Firecracker vsock UDS
// Firecracker vsock protocol for host-initiated connections:
// 1. Connect to base UDS socket
// 2. Send "CONNECT <port>\n"
// 3. Read "OK <assigned_port>\n" acknowledgement
func (m *LinuxMachine) dialVsock(port uint32) (net.Conn, error) {
	if m.dialVsockFn != nil {
		return m.dialVsockFn(port)
	}
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
	// One effective vCPU count drives both the VMM machine-config and the
	// guest's matchlock.cpus= boot arg, so the guest never sees more CPUs than
	// the VMM was given. Create rejects explicit overshoot, but cap here too so
	// the two values stay consistent even if a caller bypasses Create.
	vcpus := effectiveVCPUs(m.config.CPUs)
	kernelArgs := m.config.KernelArgs
	if kernelArgs == "" {
		workspace := m.config.Workspace
		hostname := m.config.Hostname
		if hostname == "" {
			hostname = m.config.ID
		}
		workspaceArg := ""
		if workspace != "" {
			workspaceArg = " matchlock.workspace=" + workspace
		}

		kernelArgs = fmt.Sprintf("console=ttyS0 reboot=k panic=1 acpi=off init=/init hostname=%s%s matchlock.dns=%s",
			hostname, workspaceArg, vm.KernelDNSParam(m.config.DNSServers))
		if m.config.NoNetwork {
			kernelArgs += " ip=off matchlock.no_network=1"
		} else {
			guestIP := m.config.GuestIP
			if guestIP == "" {
				guestIP = "192.168.100.2"
			}
			gatewayIP := m.config.GatewayIP
			if gatewayIP == "" {
				gatewayIP = "192.168.100.1"
			}
			mtu := effectiveMTU(m.config.MTU)
			kernelArgs += fmt.Sprintf(" ip=%s::%s:255.255.255.0::eth0:off%s matchlock.mtu=%d",
				guestIP, gatewayIP, vm.KernelIPDNSSuffix(m.config.DNSServers), mtu)
		}
		if m.config.Privileged {
			kernelArgs += " matchlock.privileged=1"
		}
		kernelArgs += fmt.Sprintf(" matchlock.cpus=%d", vcpus)
		devLetter := 'b' // vda is rootfs
		if m.config.OverlayEnabled {
			lowerDevs := make([]string, 0, len(m.config.OverlayLowerPaths))
			lowerFS := make([]string, 0, len(m.config.OverlayLowerPaths))
			for i := range m.config.OverlayLowerPaths {
				lowerDevs = append(lowerDevs, fmt.Sprintf("vd%c", devLetter))
				fsType := "erofs"
				if i < len(m.config.OverlayLowerFSTypes) && m.config.OverlayLowerFSTypes[i] != "" {
					fsType = m.config.OverlayLowerFSTypes[i]
				}
				lowerFS = append(lowerFS, fsType)
				devLetter++
			}
			upperDev := fmt.Sprintf("vd%c", devLetter)
			devLetter++
			kernelArgs += fmt.Sprintf(
				" matchlock.overlay=1 matchlock.overlay.lower=%s matchlock.overlay.lowerfs=%s matchlock.overlay.upper=%s",
				strings.Join(lowerDevs, ","),
				strings.Join(lowerFS, ","),
				upperDev,
			)
		}
		for _, disk := range m.config.ExtraDisks {
			dev := fmt.Sprintf("vd%c", devLetter)
			devLetter++
			diskMount := diskKernelArg(disk)
			kernelArgs += fmt.Sprintf(" matchlock.disk.%s=%s", dev, diskMount)
		}
		for i, mapping := range m.config.AddHosts {
			kernelArgs += fmt.Sprintf(" matchlock.add_host.%d=%s,%s", i, mapping.Host, mapping.IP)
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
	if m.config.OverlayEnabled {
		for i, lower := range m.config.OverlayLowerPaths {
			drives = append(drives, fcDrive{
				DriveID:      fmt.Sprintf("overlay-lower-%d", i),
				PathOnHost:   lower,
				IsRootDevice: false,
				IsReadOnly:   true,
			})
		}
		drives = append(drives, fcDrive{
			DriveID:      "overlay-upper",
			PathOnHost:   m.config.OverlayUpperPath,
			IsRootDevice: false,
			IsReadOnly:   false,
		})
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
	cfg.MachineConfig.VCPUCount = vcpus
	cfg.MachineConfig.MemSizeMiB = m.config.MemoryMB
	cfg.NetworkInterfaces = make([]struct {
		IfaceID     string `json:"iface_id"`
		GuestMAC    string `json:"guest_mac"`
		HostDevName string `json:"host_dev_name"`
	}, 0, 1)
	if !m.config.NoNetwork {
		cfg.NetworkInterfaces = append(cfg.NetworkInterfaces, struct {
			IfaceID     string `json:"iface_id"`
			GuestMAC    string `json:"guest_mac"`
			HostDevName string `json:"host_dev_name"`
		}{
			IfaceID:     "eth0",
			GuestMAC:    m.macAddress,
			HostDevName: m.tapName,
		})
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

func diskKernelArg(disk vm.DiskConfig) string {
	parts := []string{disk.GuestMount}
	if disk.ReadOnly {
		parts = append(parts, "ro")
	}
	if disk.OwnerUID != nil {
		parts = append(parts, fmt.Sprintf("uid=%d", *disk.OwnerUID))
	}
	if disk.OwnerGID != nil {
		parts = append(parts, fmt.Sprintf("gid=%d", *disk.OwnerGID))
	}
	return strings.Join(parts, ",")
}

func effectiveMTU(mtu int) int {
	if mtu > 0 {
		return mtu
	}
	return api.DefaultNetworkMTU
}

// effectiveVCPUs rounds cpus up to a whole vCPU count and caps it at the
// Firecracker maximum. The result is the single source of truth for both the
// VMM machine-config and the guest's matchlock.cpus= boot arg.
func effectiveVCPUs(cpus float64) int {
	v := int(math.Ceil(cpus))
	if v < 1 {
		return 1
	}
	if v > api.MaxFirecrackerVCPUs {
		return api.MaxFirecrackerVCPUs
	}
	return v
}

func (m *LinuxMachine) Stop(ctx context.Context) error {
	if m.stopFn != nil {
		return m.stopFn(ctx)
	}

	m.mutex.Lock()
	cmd := m.cmd
	done := m.done
	m.mutex.Unlock()

	if cmd == nil || cmd.Process == nil || done == nil {
		return nil
	}

	// Fast path: the wait goroutine already observed the exit.
	select {
	case <-done:
		return nil
	default:
	}

	// Idempotent: only the first Stop signals the process.
	m.stopOnce.Do(func() {
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			_ = cmd.Process.Kill()
		}
	})

	select {
	case <-done:
		return nil
	case <-time.After(5 * time.Second):
		return cmd.Process.Kill()
	case <-ctx.Done():
		return cmd.Process.Kill()
	}
}

func (m *LinuxMachine) Wait(ctx context.Context) error {
	m.mutex.Lock()
	done := m.done
	cmd := m.cmd
	m.mutex.Unlock()
	if cmd == nil || done == nil {
		return nil
	}

	select {
	case <-done:
		m.mutex.Lock()
		err := m.waitErr
		m.mutex.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *LinuxMachine) Exec(ctx context.Context, command string, opts *api.ExecOptions) (*api.ExecResult, error) {
	if m.config.VsockCID == 0 || m.config.VsockPath == "" {
		return nil, ErrVsockNotConfigured
	}
	return m.execVsock(ctx, command, opts)
}

func (m *LinuxMachine) WriteFile(ctx context.Context, path string, content []byte, mode uint32) error {
	conn, err := m.dialVsock(VsockPortExec)
	if err != nil {
		return errx.Wrap(ErrExecConnect, err)
	}
	return vsock.WriteFileVsock(conn, path, content, mode)
}

func (m *LinuxMachine) ReadFile(ctx context.Context, path string) ([]byte, error) {
	conn, err := m.dialVsock(VsockPortExec)
	if err != nil {
		return nil, errx.Wrap(ErrExecConnect, err)
	}
	return vsock.ReadFileVsock(conn, path)
}

func (m *LinuxMachine) ListFiles(ctx context.Context, path string) ([]api.FileInfo, error) {
	conn, err := m.dialVsock(VsockPortExec)
	if err != nil {
		return nil, errx.Wrap(ErrExecConnect, err)
	}
	return vsock.ListFilesVsock(conn, path)
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
		// Always wait for the single Wait goroutine to observe the VMM exit
		// before tearing down the TAP. Wait(ctx) is context-sensitive and Close
		// is routinely called with an already-canceled context (e.g. a zero
		// --graceful-shutdown window), so waiting on ctx would return while the
		// VMM is still dying. DeleteInterface then races the still-attached
		// persistent TAP and fails with EBUSY, leaking the interface. Do not
		// call cmd.Wait again: the Start goroutine owns that one-shot wait.
		if !m.waitForVMMExit(vmmExitWaitTimeout) {
			errs = append(errs, errx.With(ErrStop, ": timeout waiting for VMM to exit before TAP teardown"))
		}
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
