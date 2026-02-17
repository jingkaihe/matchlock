//go:build linux

// guest-init is the unified guest runtime binary.
// Invoked as /init it acts as PID1 and performs bootstrapping.
// Invoked as guest-agent or guest-fused (via argv[0]) it runs that mode.
package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/jingkaihe/matchlock/internal/errx"
	guestagent "github.com/jingkaihe/matchlock/internal/guestruntime/agent"
	guestfused "github.com/jingkaihe/matchlock/internal/guestruntime/fused"
	"golang.org/x/sys/unix"
)

const (
	procCmdlinePath   = "/proc/cmdline"
	procMountsPath    = "/proc/mounts"
	etcHostnamePath   = "/etc/hostname"
	etcHostsPath      = "/etc/hosts"
	etcResolvConfPath = "/etc/resolv.conf"

	guestFusedPath = "/opt/matchlock/guest-fused"
	guestAgentPath = "/opt/matchlock/guest-agent"

	defaultWorkspace = "/workspace"
	defaultPATH      = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

	networkInterface  = "eth0"
	defaultNetworkMTU = 1500
	workspaceWaitStep = 100 * time.Millisecond
	workspaceWaitMax  = 30 * time.Second
	fuseSuperMagic    = 0x65735546
)

type diskMount struct {
	Device string
	Path   string
}

type bootConfig struct {
	DNSServers []string
	Hostname   string
	Workspace  string
	MTU        int
	Disks      []diskMount
}

func main() {
	switch runtimeRole() {
	case "guest-agent":
		guestagent.Run()
		return
	case "guest-fused":
		guestfused.Run()
		return
	default:
		runInit()
	}
}

func runtimeRole() string {
	name := filepath.Base(os.Args[0])
	switch name {
	case "guest-agent", "guest-fused":
		return name
	default:
		return "init"
	}
}

func runInit() {
	prepareBaseFilesystems()
	cfg, err := parseBootConfig(procCmdlinePath)
	if err != nil {
		fatal(err)
	}

	_ = os.Setenv("PATH", defaultPATH)
	configureCgroupDelegation()

	if err := configureHostname(cfg.Hostname); err != nil {
		fatal(err)
	}

	if err := writeResolvConf(etcResolvConfPath, cfg.DNSServers); err != nil {
		fatal(err)
	}

	bringUpNetwork(networkInterface, cfg.MTU)
	mountExtraDisks(cfg.Disks)

	if err := startGuestFused(guestFusedPath); err != nil {
		fatal(err)
	}

	if err := waitForWorkspaceMount(procMountsPath, cfg.Workspace, workspaceWaitMax); err != nil {
		fatal(err)
	}

	if err := unix.Exec(guestAgentPath, []string{guestAgentPath}, os.Environ()); err != nil {
		fatal(errx.With(ErrExecGuestAgent, ": %w", err))
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "FATAL: %v\n", err)
	os.Exit(1)
}

func warnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "WARNING: "+format+"\n", args...)
}

func parseBootConfig(cmdlinePath string) (*bootConfig, error) {
	data, err := os.ReadFile(cmdlinePath)
	if err != nil {
		return nil, errx.Wrap(ErrReadCmdline, err)
	}

	cfg := &bootConfig{
		Workspace: defaultWorkspace,
		MTU:       defaultNetworkMTU,
	}
	for _, field := range strings.Fields(string(data)) {
		switch {
		case strings.HasPrefix(field, "matchlock.dns="):
			v := strings.TrimPrefix(field, "matchlock.dns=")
			for _, ns := range strings.Split(v, ",") {
				ns = strings.TrimSpace(ns)
				if ns != "" {
					cfg.DNSServers = append(cfg.DNSServers, ns)
				}
			}

		case strings.HasPrefix(field, "hostname="):
			v := strings.TrimPrefix(field, "hostname=")
			if v != "" {
				cfg.Hostname = v
			}

		case strings.HasPrefix(field, "matchlock.workspace="):
			v := strings.TrimPrefix(field, "matchlock.workspace=")
			if v != "" {
				cfg.Workspace = v
			}

		case strings.HasPrefix(field, "matchlock.mtu="):
			v := strings.TrimPrefix(field, "matchlock.mtu=")
			mtu, convErr := strconv.Atoi(v)
			if convErr != nil || mtu <= 0 || mtu > 65535 {
				return nil, errx.With(ErrInvalidMTU, ": %q", v)
			}
			cfg.MTU = mtu

		case strings.HasPrefix(field, "matchlock.disk."):
			spec := strings.TrimPrefix(field, "matchlock.disk.")
			i := strings.IndexByte(spec, '=')
			if i <= 0 || i == len(spec)-1 {
				continue
			}
			cfg.Disks = append(cfg.Disks, diskMount{Device: spec[:i], Path: spec[i+1:]})
		}
	}

	if len(cfg.DNSServers) == 0 {
		return nil, ErrMissingDNS
	}

	return cfg, nil
}

func prepareBaseFilesystems() {
	_ = unix.Mount("", "/", "", unix.MS_REMOUNT, "rw")

	_ = os.MkdirAll("/proc", 0555)
	_ = os.MkdirAll("/sys", 0555)
	_ = os.MkdirAll("/dev", 0755)
	_ = os.MkdirAll("/dev/pts", 0755)
	_ = os.MkdirAll("/dev/shm", 01777)
	_ = os.MkdirAll("/dev/mqueue", 0755)
	_ = os.MkdirAll("/run", 0755)
	_ = os.MkdirAll("/tmp", 01777)
	_ = os.MkdirAll("/sys/fs/bpf", 0755)
	_ = os.MkdirAll("/sys/fs/cgroup", 0755)

	mountIgnore("proc", "/proc", "proc", 0, "")
	mountIgnore("sys", "/sys", "sysfs", 0, "")
	mountIgnore("dev", "/dev", "devtmpfs", 0, "")
	mountIgnore("devpts", "/dev/pts", "devpts", 0, "")
	mountIgnore("tmpfs", "/dev/shm", "tmpfs", 0, "")
	mountIgnore("mqueue", "/dev/mqueue", "mqueue", 0, "")
	mountIgnore("tmpfs", "/run", "tmpfs", 0, "")
	mountIgnore("tmpfs", "/tmp", "tmpfs", 0, "")
	mountIgnore("bpf", "/sys/fs/bpf", "bpf", 0, "")
	mountIgnore("cgroup2", "/sys/fs/cgroup", "cgroup2", 0, "")
}

func configureCgroupDelegation() {
	subtree := "/sys/fs/cgroup/cgroup.subtree_control"
	controllers := "/sys/fs/cgroup/cgroup.controllers"
	if !pathExists(subtree) || !pathExists(controllers) {
		return
	}

	_ = os.MkdirAll("/sys/fs/cgroup/init", 0755)
	if err := os.WriteFile("/sys/fs/cgroup/init/cgroup.procs", []byte(fmt.Sprintf("%d\n", os.Getpid())), 0644); err != nil {
		warnf("move PID to /sys/fs/cgroup/init failed: %v", err)
	}

	data, err := os.ReadFile(controllers)
	if err != nil {
		warnf("read cgroup controllers failed: %v", err)
		return
	}
	for _, c := range strings.Fields(string(data)) {
		_ = os.WriteFile(subtree, []byte("+"+c), 0644)
	}
}

// configureHostname calls sethostname and writes /etc/hostname.
//
// Hostname is set before user-space via the `hostname=` kernel arg, but we set
// it here too to keep /etc/hostname in sync for tools that read the file.
func configureHostname(hostname string) error {
	if hostname == "" {
		hostname = "matchlock"
	}
	if err := unix.Sethostname([]byte(hostname)); err != nil {
		warnf("set hostname failed: %v", err)
	}
	if err := os.WriteFile(etcHostnamePath, []byte(hostname+"\n"), 0644); err != nil {
		return errx.With(ErrWriteHostname, " write %s: %w", etcHostnamePath, err)
	}
	if err := writeEtcHosts(etcHostsPath, hostname); err != nil {
		return errx.With(ErrWriteHosts, " write %s: %w", etcHostsPath, err)
	}

	return nil
}

func writeEtcHosts(path, hostname string) error {
	return os.WriteFile(path, []byte(renderEtcHosts(hostname)), 0644)
}

func renderEtcHosts(hostname string) string {
	var b strings.Builder
	b.WriteString("127.0.0.1 localhost localhost.localdomain ")
	b.WriteString(hostname)
	b.WriteByte('\n')
	b.WriteString("::1 localhost ip6-localhost ip6-loopback\n")
	b.WriteString("ff02::1 ip6-allnodes\n")
	b.WriteString("ff02::2 ip6-allrouters\n")
	return b.String()
}

func writeResolvConf(path string, servers []string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return errx.With(ErrWriteResolvConf, " remove %s: %w", path, err)
	}

	var b strings.Builder
	for _, ns := range servers {
		if ns == "" {
			continue
		}
		b.WriteString("nameserver ")
		b.WriteString(ns)
		b.WriteByte('\n')
	}

	if err := os.WriteFile(path, []byte(b.String()), 0644); err != nil {
		return errx.With(ErrWriteResolvConf, " write %s: %w", path, err)
	}
	return nil
}

func bringUpNetwork(iface string, mtu int) {
	if mtu <= 0 {
		mtu = defaultNetworkMTU
	}
	if err := setInterfaceMTU(iface, mtu); err != nil {
		warnf("%v", err)
	}

	if err := setInterfaceUp(iface); err != nil {
		warnf("%v", err)
	}

	hasIP, err := interfaceHasIPv4(iface)
	if err != nil {
		warnf("check interface %s address: %v", iface, err)
	}
	if hasIP {
		return
	}

	// Fallback to guest DHCP clients when the kernel cmdline did not preconfigure IPv4.
	started := false
	if path, err := exec.LookPath("udhcpc"); err == nil {
		if err := startBackground(path, "-i", iface, "-n", "-q"); err != nil {
			warnf("start udhcpc failed: %v", err)
		} else {
			started = true
		}
	} else if path, err := exec.LookPath("dhclient"); err == nil {
		if err := startBackground(path, iface); err != nil {
			warnf("start dhclient failed: %v", err)
		} else {
			started = true
		}
	}

	if started {
		time.Sleep(2 * time.Second)
	}
}

func setInterfaceMTU(name string, mtu int) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return errx.With(ErrSetInterfaceMTU, " socket: %w", err)
	}
	defer unix.Close(fd)

	const ifNameLen = 16
	var ifr struct {
		name [ifNameLen]byte
		mtu  int32
		_    [20]byte
	}
	copy(ifr.name[:], name)
	ifr.mtu = int32(mtu)

	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.SIOCSIFMTU, uintptr(unsafe.Pointer(&ifr)))
	if errno != 0 {
		return errx.With(ErrSetInterfaceMTU, " %s=%d: %v", name, mtu, errno)
	}
	return nil
}

func setInterfaceUp(name string) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return errx.With(ErrBringUpInterface, " socket: %w", err)
	}
	defer unix.Close(fd)

	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return errx.With(ErrBringUpInterface, " ifreq %s: %w", name, err)
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return errx.With(ErrBringUpInterface, " get flags %s: %w", name, err)
	}

	flags := ifr.Uint16()
	if flags&uint16(unix.IFF_UP) != 0 {
		return nil
	}
	ifr.SetUint16(flags | uint16(unix.IFF_UP))
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
		return errx.With(ErrBringUpInterface, " set flags %s: %w", name, err)
	}
	return nil
}

func interfaceHasIPv4(name string) (bool, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return false, err
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return false, err
	}
	for _, addr := range addrs {
		switch v := addr.(type) {
		case *net.IPNet:
			if v.IP != nil && v.IP.To4() != nil {
				return true, nil
			}
		case *net.IPAddr:
			if v.IP != nil && v.IP.To4() != nil {
				return true, nil
			}
		}
	}
	return false, nil
}

func mountExtraDisks(disks []diskMount) {
	for _, d := range disks {
		if d.Device == "" || d.Path == "" {
			continue
		}
		if err := os.MkdirAll(d.Path, 0755); err != nil {
			warnf("create disk mount path %s failed: %v", d.Path, err)
			continue
		}
		if err := unix.Mount(filepath.Join("/dev", d.Device), d.Path, "ext4", 0, ""); err != nil {
			warnf("mount /dev/%s at %s failed: %v", d.Device, d.Path, err)
		}
	}
}

func startGuestFused(path string) error {
	cmd := exec.Command(path)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return errx.With(ErrStartGuestFused, " %s: %w", path, err)
	}
	go func() {
		_ = cmd.Wait()
	}()
	return nil
}

func waitForWorkspaceMount(mountsPath, workspace string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		mounted, err := workspaceMounted(mountsPath, workspace)
		if err != nil {
			warnf("workspace mount check failed: %v", err)
		} else if mounted {
			fuseReady, fuseErr := workspaceIsFUSE(workspace)
			if fuseErr != nil {
				warnf("workspace fs type check failed: %v", fuseErr)
			} else if fuseReady {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return errx.With(ErrWorkspaceMountWait, ": %s", workspace)
		}
		time.Sleep(workspaceWaitStep)
	}
}

func workspaceMounted(mountsPath, workspace string) (bool, error) {
	f, err := os.Open(mountsPath)
	if err != nil {
		return false, errx.Wrap(ErrWorkspaceMount, err)
	}
	defer f.Close()

	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 3 {
			continue
		}
		source, target, fstype := fields[0], fields[1], fields[2]
		if target == workspace && source == "matchlock" && strings.HasPrefix(fstype, "fuse.") {
			return true, nil
		}
	}
	if err := s.Err(); err != nil {
		return false, errx.Wrap(ErrWorkspaceMount, err)
	}
	return false, nil
}

func workspaceIsFUSE(workspace string) (bool, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(workspace, &st); err != nil {
		return false, errx.Wrap(ErrWorkspaceMount, err)
	}
	return uint64(st.Type) == fuseSuperMagic, nil
}

func mountIgnore(source, target, fstype string, flags uintptr, data string) {
	if err := unix.Mount(source, target, fstype, flags, data); err != nil {
		if err == unix.EBUSY || err == unix.EEXIST {
			return
		}
	}
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func startBackground(path string, args ...string) error {
	cmd := exec.Command(path, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() {
		_ = cmd.Wait()
	}()
	return nil
}
