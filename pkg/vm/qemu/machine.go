//go:build linux

package qemu

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/jingkaihe/matchlock/pkg/vm"
	"github.com/jingkaihe/matchlock/pkg/vsock"
	"golang.org/x/sys/unix"
)

const (
	vsockPortExec  = 5000
	vsockPortVFS   = 5001
	vsockPortReady = 5002
	startupTimeout = 180 * time.Second
	stopGrace      = 5 * time.Second
)

func runtimeGOARCH() string { return runtime.GOARCH }

// Machine is a QEMU TCG-backed VM.
type Machine struct {
	id      string
	config  *vm.VMConfig
	cid     uint32
	cidLock *os.File

	// cmd is the QEMU process. pid is the child PID.
	cmd *exec.Cmd
	pid int

	// serialLog is the QEMU -serial file: path (guest console).
	serialLog string

	// qemuStderr captures QEMU's own stderr. It is mutex-guarded because it is
	// read for diagnostics while QEMU may still be writing to it.
	qemuStderr *syncBuffer

	// done is closed exactly once when cmd.Wait returns. waitErr holds the raw
	// Wait error. mutex protects started/stopping/waitErr/done for the case
	// where Close and Stop race.
	done    chan struct{}
	waitErr error
	mutex   sync.Mutex

	// stopOnce ensures only the first Stop signals the process (idempotent
	// shutdown across concurrent Close/Stop). Per-machine, not package-level.
	stopOnce sync.Once
	started  bool
	stopping bool

	// net holds the host TAP setup when the guest has a NIC (nil for
	// --no-network). Its fd is passed to QEMU via ExtraFiles so QEMU needs no
	// CAP_NET_ADMIN; teardown happens in Close after the child exits.
	net *networkSetup

	// vfsPort is the uniquely-assigned host vsock port for this sandbox's VFS
	// (fused) listener, or 0 if the guest has no VFS/workspace. Written before
	// Start() and read only by bootArgs(), so no lock is needed. It is passed to
	// the guest via the matchlock.vfs_port= kernel argument so guest-fused dials
	// exactly this sandbox's port.
	vfsPort uint32

	// execPoolMu guards execIdle/execPoolDone. execIdle holds idle, healthy
	// exec-service vsock connections for reuse across sequential short execs.
	// QEMU dials one vsock connection per exec by default; a 1 Hz sampler plus
	// readiness polls then manufactures a socket lifecycle (and potential RST)
	// per request, which under memory pressure overlaps and resets the long
	// pressure exec. Reuse collapses that churn to a single idle connection.
	execPoolMu   sync.Mutex
	execIdle     []net.Conn
	execPoolDone bool

	// execDial dials an exec-service connection. It is a field so tests can
	// substitute a local transport; production leaves it nil and uses DialVsock.
	execDial func(ctx context.Context) (net.Conn, error)

	diskCleanup func() error
}

// execIdleMax bounds how many idle exec connections a Machine retains. Two
// concurrent execs (a long pressure workload plus a short sampler) need only
// one idle connection; the cap keeps a transient burst from pinning sockets.
const execIdleMax = 4

// ID returns the sandbox ID.
func (m *Machine) ID() string { return m.id }

// Start boots the guest. It must be called once.
func (m *Machine) Start(ctx context.Context) error {
	if m.started {
		return nil
	}
	if err := m.validate(); err != nil {
		return err
	}

	qemuName := qemuSystemName()
	if qemuName == "" {
		return errx.Wrap(ErrQEMUNotFound, fmt.Errorf("unsupported arch %s", runtimeGOARCH()))
	}
	if _, err := exec.LookPath(qemuName); err != nil {
		return errx.Wrap(ErrQEMUNotFound, err)
	}

	m.serialLog = m.snapshotPath()
	if err := os.MkdirAll(filepath.Dir(m.serialLog), 0o755); err != nil {
		return errx.Wrap(ErrStart, err)
	}

	ds, err := m.prepareDisks()
	if err != nil {
		return err
	}
	m.diskCleanup = ds.cleanup

	// Captures QEMU's own stderr for diagnostics on failure. The serial log is
	// separate and attached via -serial file:. The buffer is mutex-guarded so a
	// transport-reset diagnostic can read it safely while QEMU is still running.
	stderr := &syncBuffer{}
	m.qemuStderr = stderr
	logPath := m.config.LogPath

	args := []string{
		"-machine", machineType(),
		"-accel", "tcg",
		"-cpu", guestCPU(),
		"-m", fmt.Sprintf("%d", effectiveMem(m.config.MemoryMB)),
		"-smp", fmt.Sprintf("%d", effectiveVCPUs(m.config.CPUs)),
		"-nodefaults",
		"-display", "none",
		"-monitor", "none",
		"-no-reboot",
	}
	args = append(args, m.diskArgs(ds.disks)...)

	// Attach the guest NIC (virtio-net-pci) to the host TAP via the already-open
	// fd. The fd is passed through ExtraFiles; QEMU never reopens the TAP name
	// and therefore never needs CAP_NET_ADMIN itself.
	extraFiles := []*os.File{m.cidLock}
	if m.net != nil {
		args = append(args, m.net.networkArgs(len(extraFiles))...)
		extraFiles = append(extraFiles, m.net.tapFile)
	}

	args = append(args,
		"-device", fmt.Sprintf("vhost-vsock-pci,guest-cid=%d", m.cid),
		"-serial", "file:"+m.serialLog,
		"-kernel", m.config.KernelPath,
		"-append", m.bootArgs(),
	)
	if logPath != "" {
		args = append(args, "-D", logPath)
	}

	cmd := exec.Command(qemuName, args...)
	cmd.Env = os.Environ()
	// fd 3 = cid lock; fd 4 = tap (when present). Passed so QEMU inherits both
	// without needing CAP_NET_ADMIN and holds the CID/lock until it exits.
	cmd.ExtraFiles = extraFiles
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	cmd.SysProcAttr = &unix.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		_ = m.diskCleanup()
		return errx.Wrap(ErrStart, err)
	}
	m.cmd = cmd
	m.pid = cmd.Process.Pid
	m.started = true

	// Exactly one goroutine calls Wait; the raw error is memoized and done is
	// closed once.
	m.mutex.Lock()
	m.done = make(chan struct{})
	m.mutex.Unlock()
	go func() {
		err := cmd.Wait()
		m.mutex.Lock()
		m.waitErr = err
		m.mutex.Unlock()
		close(m.done)
	}()

	if err := m.waitReady(ctx); err != nil {
		_ = m.Stop(context.Background())
		if m.serialLog != "" {
			appendLogTail(stderr, m.serialLog)
		}
		return err
	}
	return nil
}

func (m *Machine) validate() error {
	if m.config.KernelPath == "" {
		return errx.With(ErrKernelNotFound, ": empty kernel path")
	}
	if _, err := os.Stat(m.config.KernelPath); err != nil {
		return errx.With(ErrKernelNotFound, ": %s: %w", m.config.KernelPath, err)
	}
	if m.config.RootfsPath == "" {
		return errx.With(ErrRootfsNotFound, ": empty rootfs path")
	}
	if _, err := os.Stat(m.config.RootfsPath); err != nil {
		return errx.With(ErrRootfsNotFound, ": %s: %w", m.config.RootfsPath, err)
	}
	return nil
}

// bootArgs reconstructs the kernel command line, mirroring the Linux backend
// (including overlay disk device names) but targeting QEMU's ARM virt console.
func (m *Machine) bootArgs() string {
	hostname := m.config.Hostname
	if hostname == "" {
		hostname = m.config.ID
	}

	var sb strings.Builder
	sb.WriteString("console=" + consoleDevice() + " root=/dev/vda rw rootwait init=/init reboot=k panic=1")

	workspaceArg := ""
	if m.config.Workspace != "" {
		workspaceArg = " matchlock.workspace=" + m.config.Workspace
	}
	exactArg := ""
	if len(m.config.ExactMounts) > 0 {
		exactArg = " matchlock.exact.mounts=" + strings.Join(m.config.ExactMounts, ",")
	}
	sb.WriteString(fmt.Sprintf(" hostname=%s%s%s", hostname, workspaceArg, exactArg))
	sb.WriteString(" matchlock.dns=" + vm.KernelDNSParam(m.config.DNSServers))

	if m.config.NoNetwork {
		sb.WriteString(" ip=off matchlock.no_network=1")
	} else {
		// Static guest IP + gateway from the sandbox-allocated subnet so
		// guest-init's bringUpNetwork sees an IPv4 address and brings eth0 up.
		sb.WriteString(m.networkBootArgs())
	}

	if m.config.MTU > 0 {
		sb.WriteString(fmt.Sprintf(" matchlock.mtu=%d", m.config.MTU))
	}
	if m.config.Privileged {
		sb.WriteString(" matchlock.privileged=1")
	}
	sb.WriteString(fmt.Sprintf(" matchlock.cpus=%g", m.config.CPUs))

	// Per-sandbox VFS vsock port: guest-init reads matchlock.vfs_port and passes
	// it to guest-fused, which dials hostCID:port to reach this sandbox's VFS
	// server. Absent (port 0) means no VFS, so guest-fused is not launched.
	if m.vfsPort != 0 {
		sb.WriteString(fmt.Sprintf(" matchlock.vfs_port=%d", m.vfsPort))
	}

	// Overlay: vda is rootfs; lowers are vdb, vdc, ...; upper is the next letter.
	piece := "b"
	if m.config.OverlayEnabled {
		lowerDevs := make([]string, 0, len(m.config.OverlayLowerPaths))
		lowerFS := make([]string, 0, len(m.config.OverlayLowerPaths))
		for i := range m.config.OverlayLowerPaths {
			lowerDevs = append(lowerDevs, "vd"+piece)
			fsType := "erofs"
			if i < len(m.config.OverlayLowerFSTypes) && m.config.OverlayLowerFSTypes[i] != "" {
				fsType = m.config.OverlayLowerFSTypes[i]
			}
			lowerFS = append(lowerFS, fsType)
			piece = nextLetter(piece)
		}
		upper := "vd" + piece
		piece = nextLetter(piece)
		sb.WriteString(fmt.Sprintf(" matchlock.overlay=1 matchlock.overlay.lower=%s matchlock.overlay.lowerfs=%s matchlock.overlay.upper=%s",
			strings.Join(lowerDevs, ","), strings.Join(lowerFS, ","), upper))
	}
	for _, d := range m.config.ExtraDisks {
		dev := "vd" + piece
		piece = nextLetter(piece)
		if d.Swap {
			// Swap is never mounted; guest-init swapon's the device directly.
			sb.WriteString(fmt.Sprintf(" matchlock.swap=%s", dev))
			continue
		}
		sb.WriteString(fmt.Sprintf(" matchlock.disk.%s=%s", dev, diskKernelArg(d)))
	}
	for i, mapping := range m.config.AddHosts {
		sb.WriteString(fmt.Sprintf(" matchlock.add_host.%d=%s,%s", i, mapping.Host, mapping.IP))
	}

	return sb.String()
}

func diskKernelArg(d vm.DiskConfig) string {
	parts := []string{d.GuestMount}
	if d.ReadOnly {
		parts = append(parts, "ro")
	}
	// The guest applies uid=/gid= to the mount root's owner (chownDiskMountRoot).
	// Firecracker passes these; QEMU must too, or the disk mount stays root-owned.
	if d.OwnerUID != nil {
		parts = append(parts, fmt.Sprintf("uid=%d", *d.OwnerUID))
	}
	if d.OwnerGID != nil {
		parts = append(parts, fmt.Sprintf("gid=%d", *d.OwnerGID))
	}
	return strings.Join(parts, ",")
}

func nextLetter(s string) string {
	if s == "" || len(s) != 1 {
		return "z"
	}
	return string(s[0] + 1)
}

// waitReady polls AF_VSOCK :5002 until the guest agent accepts, or times out.
func (m *Machine) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(startupTimeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case <-m.done:
			return errx.With(ErrNotReady, ": qemu exited: %v", m.waitDoneErr())
		default:
		}
		// Bounded, cancellable dial.
		dialCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		conn, err := DialVsock(dialCtx, m.cid, vsockPortReady)
		cancel()
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return errx.With(ErrNotReady, ": timed out after %s", startupTimeout)
}

// Done returns a channel closed when the QEMU process exits.
func (m *Machine) Done() <-chan struct{} { return m.done }

// waitDone blocks until the QEMU process exits (or ctx cancels) and returns the
// raw Wait error.
func (m *Machine) waitDone(ctx context.Context) (error, error) {
	select {
	case <-m.done:
		return m.waitDoneErr(), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *Machine) waitDoneErr() error {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return m.waitErr
}

// PID returns the QEMU process PID.
func (m *Machine) PID() int { return m.pid }

// VsockPath returns the guest CID as a string for diagnostics.
func (m *Machine) VsockPath() string { return fmt.Sprintf("cid:%d", m.cid) }

// VsockCID returns the guest CID.
func (m *Machine) VsockCID() uint32 { return m.cid }

// VFSListener binds a host-side AF_VSOCK listener on a kernel-assigned distinct
// port and returns a net.Listener that only serves connections FROM this
// machine's own guest CID (per-sandbox isolation). The bound port is recorded on
// the machine so bootArgs() can pass it to the guest (guest-fused dials
// hostCID:port). It must be called before Start().
func (m *Machine) VFSListener() (net.Listener, error) {
	base, port, err := ListenVsock(context.Background(), VMADDR_CID_ANY, VMADDR_PORT_ANY)
	if err != nil {
		return nil, errx.Wrap(ErrVsock, err)
	}
	m.vfsPort = port
	return &peerFilteredListener{Listener: base, expectedCID: m.cid}, nil
}

// VFSPort returns the VFS vsock port assigned to this machine (0 if none).
// bootArgs() reads it to emit matchlock.vfs_port.
func (m *Machine) VFSPort() uint32 { return m.vfsPort }

// DialVsock opens a host-initiated AF_VSOCK stream to a guest service port.
func (m *Machine) DialVsock(port uint32) (net.Conn, error) {
	return DialVsock(context.Background(), m.cid, port)
}

// VsockFD is not applicable (AF_VSOCK has no single host fd).
func (m *Machine) VsockFD() (int, error) { return -1, nil }

// NetworkFD returns the host TAP descriptor when a guest NIC was created, or
// -1 for --no-network. It is safe to call after Close (returns -1) because the
// TAP fd is closed during teardown. Backends without support return an error.
func (m *Machine) NetworkFD() (int, error) {
	if m.net == nil || m.net.tapFile == nil {
		return -1, nil
	}
	return int(m.net.tapFile.Fd()), nil
}

// RootfsPath returns the bootstrap root disk path.
func (m *Machine) RootfsPath() string { return m.config.RootfsPath }

// TapName returns the host TAP interface name ("" for --no-network). This is
// read by the sandbox to provision nftables NAT and proxy rules.
func (m *Machine) TapName() string {
	if m.net == nil {
		return ""
	}
	return m.net.tapName
}

func (m *Machine) WriteFile(ctx context.Context, path string, content []byte, mode uint32) error {
	conn, err := m.DialVsock(vsockPortExec)
	if err != nil {
		return errx.Wrap(ErrVsock, err)
	}
	return vsock.WriteFileVsock(conn, path, content, mode)
}

func (m *Machine) ReadFile(ctx context.Context, path string) ([]byte, error) {
	conn, err := m.DialVsock(vsockPortExec)
	if err != nil {
		return nil, errx.Wrap(ErrVsock, err)
	}
	return vsock.ReadFileVsock(conn, path)
}

func (m *Machine) ListFiles(ctx context.Context, path string) ([]api.FileInfo, error) {
	conn, err := m.DialVsock(vsockPortExec)
	if err != nil {
		return nil, errx.Wrap(ErrVsock, err)
	}
	return vsock.ListFilesVsock(conn, path)
}

func (m *Machine) Exec(ctx context.Context, command string, opts *api.ExecOptions) (*api.ExecResult, error) {
	if !m.started {
		return nil, errx.Wrap(ErrExec, fmt.Errorf("machine not started"))
	}
	if opts != nil && opts.Stdin != nil {
		conn, err := m.DialVsock(vsockPortExec)
		if err != nil {
			return nil, errx.Wrap(ErrVsock, err)
		}
		return vsock.ExecPipe(ctx, conn, command, opts)
	}
	return m.execStream(ctx, command, opts)
}

// dialExecConn opens a fresh exec-service vsock connection. Tests substitute
// execDial to exercise the pool over a local transport.
func (m *Machine) dialExecConn(ctx context.Context) (net.Conn, error) {
	if m.execDial != nil {
		return m.execDial(ctx)
	}
	return DialVsock(ctx, m.cid, vsockPortExec)
}

// borrowExecConn returns a pooled exec connection, dialing a fresh one only when
// the pool is empty or the pooled connection is no longer usable. A stale
// connection is dropped rather than handed to a caller: re-dialing has no side
// effects, whereas re-dispatching a cancelled exec would replay them.
func (m *Machine) borrowExecConn(ctx context.Context) (net.Conn, error) {
	for {
		m.execPoolMu.Lock()
		if m.execPoolDone || len(m.execIdle) == 0 {
			m.execPoolMu.Unlock()
			break
		}
		last := len(m.execIdle) - 1
		conn := m.execIdle[last]
		m.execIdle = m.execIdle[:last]
		m.execPoolMu.Unlock()

		if execConnUsable(conn) {
			return conn, nil
		}
		_ = conn.Close()
	}
	return m.dialExecConn(ctx)
}

// releaseExecConn returns a healthy connection to the bounded idle pool, or
// closes it. It must be called with healthy=true only after a request/response
// exchange fully completed, so a partial frame can never leak into a reused
// connection's stream.
func (m *Machine) releaseExecConn(conn net.Conn, healthy bool) {
	if conn == nil {
		return
	}
	if healthy {
		m.execPoolMu.Lock()
		if !m.execPoolDone && len(m.execIdle) < execIdleMax {
			m.execIdle = append(m.execIdle, conn)
			m.execPoolMu.Unlock()
			return
		}
		m.execPoolMu.Unlock()
	}
	_ = conn.Close()
}

// closeExecPool closes every idle pooled connection. It is idempotent and is
// called during Machine teardown so pooled fds do not outlive the VM.
func (m *Machine) closeExecPool() {
	m.execPoolMu.Lock()
	m.execPoolDone = true
	idle := m.execIdle
	m.execIdle = nil
	m.execPoolMu.Unlock()
	for _, conn := range idle {
		_ = conn.Close()
	}
}

// execConnUsable reports whether a pooled connection carries no pending data and
// has not been closed by the guest. An exec stream is request/response, so a
// connection is only reusable at a clean request boundary: a non-blocking poll
// must report no readable byte and no hangup. Polling the raw fd (rather than a
// deadline read) leaves the runtime poller's deadline state untouched.
func execConnUsable(conn net.Conn) bool {
	sc, ok := conn.(syscall.Conn)
	if !ok {
		return false
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return false
	}
	healthy := false
	_ = raw.Read(func(fd uintptr) bool {
		pfd := []unix.PollFd{{
			Fd:     int32(fd),
			Events: unix.POLLIN | unix.POLLRDHUP | unix.POLLHUP | unix.POLLERR,
		}}
		n, pollErr := unix.Poll(pfd, 0)
		healthy = pollErr == nil && (n == 0 || pfd[0].Revents == 0)
		return true
	})
	return healthy
}

func (m *Machine) execStream(ctx context.Context, command string, opts *api.ExecOptions) (*api.ExecResult, error) {
	start := time.Now()
	conn, err := m.borrowExecConn(ctx)
	if err != nil {
		return nil, errx.Wrap(ErrVsock, err)
	}
	// The connection is returned to the pool only when the exchange completed
	// cleanly. A context cancellation closes the connection out from under us;
	// stop() reports whether it prevented that, so a cancelled connection is
	// never pooled.
	healthy := false
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer func() {
		if !stop() {
			healthy = false
		}
		m.releaseExecConn(conn, healthy)
	}()

	req := vsock.ExecRequest{Command: command}
	if opts != nil {
		req.WorkingDir = opts.WorkingDir
		req.Env = opts.Env
		req.User = opts.User
	}
	reqData, err := json.Marshal(req)
	if err != nil {
		return nil, errx.Wrap(ErrExec, err)
	}

	streaming := opts != nil && (opts.Stdout != nil || opts.Stderr != nil)
	header := make([]byte, 5)
	if streaming {
		header[0] = vsock.MsgTypeExecStream
	} else {
		header[0] = vsock.MsgTypeExec
	}
	binary.BigEndian.PutUint32(header[1:], uint32(len(reqData)))

	if _, err := conn.Write(header); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errx.Wrap(ErrExec, err)
	}
	if _, err := conn.Write(reqData); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errx.Wrap(ErrExec, err)
	}

	var stdout, stderr strings.Builder
	for {
		if _, err := vsock.ReadFull(conn, header); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if isVsockReset(err) {
				return nil, m.recordVsockReset(err)
			}
			return nil, errx.Wrap(ErrExec, err)
		}
		msgType := header[0]
		length := binary.BigEndian.Uint32(header[1:])
		data := make([]byte, length)
		if length > 0 {
			if _, err := vsock.ReadFull(conn, data); err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				if isVsockReset(err) {
					return nil, m.recordVsockReset(err)
				}
				return nil, errx.Wrap(ErrExec, err)
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
				return nil, errx.Wrap(ErrExec, err)
			}
			duration := time.Since(start)
			so := []byte(stdout.String())
			se := []byte(stderr.String())
			if len(so) == 0 && len(resp.Stdout) > 0 {
				so = resp.Stdout
			}
			if len(se) == 0 && len(resp.Stderr) > 0 {
				se = resp.Stderr
			}
			result := &api.ExecResult{
				ExitCode:   resp.ExitCode,
				Stdout:     so,
				Stderr:     se,
				Duration:   duration,
				DurationMS: duration.Milliseconds(),
			}
			// The protocol exchange completed: the connection is reusable even
			// when the workload exited non-zero.
			healthy = true
			if resp.Error != "" {
				return result, errx.With(ErrExec, ": %s", resp.Error)
			}
			return result, nil
		}
	}
}

func (m *Machine) ExecInteractive(ctx context.Context, command string, opts *api.ExecOptions, rows, cols uint16, stdin io.Reader, stdout io.Writer, resizeCh <-chan [2]uint16) (int, error) {
	if !m.started {
		return 1, errx.Wrap(ErrExec, fmt.Errorf("machine not started"))
	}
	conn, err := m.DialVsock(vsockPortExec)
	if err != nil {
		return 1, errx.Wrap(ErrVsock, err)
	}
	defer conn.Close()

	req := vsock.ExecTTYRequest{Command: command, Rows: rows, Cols: cols}
	if opts != nil {
		req.WorkingDir = opts.WorkingDir
		req.Env = opts.Env
		req.User = opts.User
	}
	reqData, err := json.Marshal(req)
	if err != nil {
		return 1, errx.Wrap(ErrExec, err)
	}
	// Send the ExecTTY request frame as one locked wire write (same path as
	// stdin/resize/signal) so it can never interleave with a concurrent frame.
	execTTY := func() error {
		if vc, ok := conn.(*vsockConn); ok {
			_, err := vc.writeFrame(vsock.MsgTypeExecTTY, reqData)
			return err
		}
		header := make([]byte, 5)
		header[0] = vsock.MsgTypeExecTTY
		binary.BigEndian.PutUint32(header[1:], uint32(len(reqData)))
		// Unsynchronized fallback (conn is not a *vsockConn). A frame is still
		// built then written, but this path should not occur for QEMU sessions.
		if _, err := conn.Write(header); err != nil {
			return err
		}
		if _, err := conn.Write(reqData); err != nil {
			return err
		}
		return nil
	}
	if err := execTTY(); err != nil {
		return 1, errx.Wrap(ErrExec, err)
	}

	done := make(chan int, 1)
	errCh := make(chan error, 1)
	go func() {
		readHeader := make([]byte, 5)
		for {
			if _, err := vsock.ReadFull(conn, readHeader); err != nil {
				errCh <- err
				return
			}
			msgType := readHeader[0]
			length := binary.BigEndian.Uint32(readHeader[1:])
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

	// Send framed messages over the connection as a session-local atomic unit
	// (stdin, resize, and signal goroutines all race on this conn). writeFrame
	// emits header+payload as one locked wire write, so frames never interleave.
	send := func(msgType uint8, data []byte) error {
		if vc, ok := conn.(*vsockConn); ok {
			_, err := vc.writeFrame(msgType, data)
			return err
		}
		return vsock.SendMessage(conn, msgType, data)
	}

	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				_ = send(vsock.MsgTypeStdin, buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	// Forward terminal resize events to the guest PTY (MsgTypeResize), which the
	// guest agent applies via pty.Setsize. Mirrors the Firecracker backend so
	// interactive full-screen apps reflow on resize.
	if resizeCh != nil {
		go func() {
			for size := range resizeCh {
				data := make([]byte, 4)
				binary.BigEndian.PutUint16(data[0:2], size[0]) // rows
				binary.BigEndian.PutUint16(data[2:4], size[1]) // cols
				_ = send(vsock.MsgTypeResize, data)
			}
		}()
	}

	select {
	case exitCode := <-done:
		return exitCode, nil
	case err := <-errCh:
		return 1, err
	case <-ctx.Done():
		_ = send(vsock.MsgTypeSignal, []byte{byte(unix.SIGTERM)})
		return 1, ctx.Err()
	}
}

// Stop terminates the QEMU process, waiting up to stopGrace for it to exit. A
// SIGTERM/SIGKILL exit is expected here (we initiated it) and is NOT reported
// as an error: Stop is about releasing the VM, and the guest command's own exit
// code is captured separately by the Exec path. Only a failure to release (a
// genuine timeout after SIGKILL) is an error.
//
// It is robust to the caller passing an already-expired context: the graceful
// wait uses the fixed stopGrace, and the caller's context only requests an
// immediate kill on cancellation.
func (m *Machine) Stop(ctx context.Context) error {
	// Fast path: if QEMU already exited before we signalled it, accept the exit
	// (it ran and stopped) and report nil unless there was a real crash. Do this
	// under the mutex so the "already done" check and any concurrent Close do
	// not both signal a running child.
	m.mutex.Lock()
	alreadyDone := m.done == nil || channelClosed(m.done)
	pid := m.pid
	m.mutex.Unlock()

	if alreadyDone {
		return m.stopOutcome()
	}
	if pid <= 0 {
		return nil
	}

	// Idempotent: only the first Stop signals the process.
	m.stopOnce.Do(func() {
		m.mutex.Lock()
		m.stopping = true
		m.mutex.Unlock()
		signalChildProcess(pid)
	})

	select {
	case <-m.done:
		return m.stopOutcome()
	case <-time.After(stopGrace):
		killChildProcess(pid)
		select {
		case <-m.done:
			return m.stopOutcome()
		case <-time.After(2 * time.Second):
			return errx.Wrap(ErrStop, fmt.Errorf("timed out"))
		}
	case <-ctx.Done():
		killChildProcess(pid)
		select {
		case <-m.done:
			return m.stopOutcome()
		case <-time.After(2 * time.Second):
			return ctx.Err()
		}
	}
}

func channelClosed(ch <-chan struct{}) bool {
	if ch == nil {
		return true
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// stopOutcome interprets the recorded wait error. A signal-kill exit is the
// expected result of an intentional Stop and is treated as success; only an
// unexpected non-signal error is surfaced.
func (m *Machine) stopOutcome() error {
	err := m.waitDoneErr()
	if err == nil {
		return nil
	}
	// A process killed by our own SIGTERM/SIGKILL appears as "signal: killed".
	// Treat any signal-termination as an intentional stop, but only when the
	// signal was ours (stopping is set); a spontaneous crash is an error.
	if m.isStopping() && isSignalErr(err) {
		return nil
	}
	return err
}

func isSignalErr(err error) bool {
	// exec.ExitError from Wait on a signal-killed child reports "signal: killed"
	// (or the specific signal). Match any signal word.
	return strings.Contains(err.Error(), "signal:")
}

func (m *Machine) isStopping() bool {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return m.stopping
}

// Wait blocks until the QEMU process exits.
func (m *Machine) Wait(ctx context.Context) error {
	_, err := m.waitDone(ctx)
	return err
}

// Close stops the VM and releases resources. It is safe to call more than once.
func (m *Machine) Close(ctx context.Context) error {
	var errs []error
	if m.started {
		if err := m.Stop(ctx); err != nil {
			errs = append(errs, errx.Wrap(ErrClose, err))
		}
	}
	// Pooled exec connections point at the now-stopped guest; close them so no
	// idle vsock fd outlives the VM.
	m.closeExecPool()
	if m.diskCleanup != nil {
		if err := m.diskCleanup(); err != nil {
			errs = append(errs, errx.Wrap(ErrClose, err))
		}
	}
	if m.net != nil {
		if err := m.net.teardownNetwork(); err != nil {
			errs = append(errs, errx.Wrap(ErrClose, err))
		}
	}
	if m.cidLock != nil {
		// releaseCIDLock itself waits for the child (which holds a dup via
		// ExtraFiles) to exit before dropping the lock.
		if err := m.releaseCIDLock(); err != nil {
			errs = append(errs, errx.Wrap(ErrClose, err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// releaseCIDLock closes the CID lock file. The child holds a dup via ExtraFiles
// until it exits, so closing our own fd after the child has exited releases the
// last reference. If the child is still running we wait briefly.
func (m *Machine) releaseCIDLock() error {
	if m.cidLock == nil {
		return nil
	}
	if m.started {
		select {
		case <-m.done:
		case <-time.After(2 * time.Second):
		}
	}
	// Do not explicitly LOCK_UN: lock lifetime is tied to the fd. Once both our
	// fd and the child's dup are closed, the kernel releases the lock. Closing
	// our fd is sufficient and avoids a race where we unlock while the child
	// still holds it.
	return m.cidLock.Close()
}

// --- helpers referenced by backend.go / machine_test.go ---

type diskMount struct {
	HostPath string
	ReadOnly bool
}

type diskSet struct {
	disks   []diskMount
	cleanup func() error
}

func (m *Machine) snapshotPath() string {
	dir := filepath.Dir(m.config.LogPath)
	if dir == "" || dir == "." {
		dir = os.TempDir()
	}
	return filepath.Join(dir, fmt.Sprintf("%s-serial.log", m.config.ID))
}

func effectiveMem(mb int) int {
	if mb > 0 {
		return mb
	}
	return 512
}

func effectiveVCPUs(cpus float64) int {
	v := int(math.Ceil(cpus))
	if v < 1 {
		return 1
	}
	return v
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func flockExclusive(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func signalChildProcess(pid int) {
	if pid <= 0 {
		return
	}
	// The child is its own process-group leader (Setpgid), so signalling the
	// group (-pid) covers the QEMU process and any descendants it spawned
	// (e.g. the guest-init or a gvisor vmm if ever added).
	_ = unix.Kill(-pid, unix.SIGTERM)
	_ = killNoInt(pid, unix.SIGTERM)
}

func killChildProcess(pid int) {
	if pid <= 0 {
		return
	}
	_ = unix.Kill(-pid, unix.SIGKILL)
	_ = killNoInt(pid, unix.SIGKILL)
}

// killNoInt sends sig to a single pid, retrying on EINTR. It does not signal
// the whole group; callers combine it with an explicit group signal as needed.
func killNoInt(pid int, sig unix.Signal) error {
	for {
		err := unix.Kill(pid, sig)
		if err == unix.EINTR {
			continue
		}
		return err
	}
}

// syncBuffer is an io.Writer whose accumulated bytes can be read safely while
// another goroutine is still writing (unlike strings.Builder). It backs the
// QEMU stderr capture, which QEMU writes to from os/exec's copier goroutine and
// which the reset diagnostic reads while the VM is still alive.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// stringer is the read side appendLogTail and the reset diagnostic need.
type stringer interface{ String() string }

func appendLogTail(stderr stringer, serialPath string) {
	if stderr == nil {
		return
	}
	// Best effort: append QEMU stderr to the log for diagnostics.
	_ = os.WriteFile(serialPath+".qemu-stderr", []byte(stderr.String()), 0o644)
}

// recordVsockReset persists the QEMU stderr and the guest serial log tail when
// a host vsock read is reset, then returns an error wrapping ErrVsockReset. The
// serial log is the only place a guest panic or a PID-1 OOM kill becomes
// visible, so a reset on a post-boot VM is otherwise opaque.
func (m *Machine) recordVsockReset(err error) error {
	if m.serialLog != "" {
		_ = os.WriteFile(m.serialLog+".vsock-reset", []byte(m.diagnosticText()), 0o644)
	}
	return errx.With(ErrVsockReset, ": %v", err)
}

// diagnosticText renders the QEMU stderr plus a bounded tail of the guest serial
// log for a transport-failure report.
func (m *Machine) diagnosticText() string {
	var sb strings.Builder
	sb.WriteString("qemu stderr:\n")
	if m.qemuStderr != nil {
		sb.WriteString(m.qemuStderr.String())
	}
	sb.WriteString("\nguest serial log tail:\n")
	if m.serialLog != "" {
		if data, err := os.ReadFile(m.serialLog); err == nil {
			const maxTail = 16 * 1024
			if len(data) > maxTail {
				data = data[len(data)-maxTail:]
			}
			sb.Write(data)
		}
	}
	return sb.String()
}

// isVsockReset reports whether err is a connection reset from the guest
// virtio-vsock transport rather than a normal host-initiated close.
func isVsockReset(err error) bool {
	return errors.Is(err, unix.ECONNRESET) || errors.Is(err, unix.EPIPE)
}
