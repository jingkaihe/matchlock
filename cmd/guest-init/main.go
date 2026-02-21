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

	overlayLowerMount = "/run/matchlock/lower"
	overlayUpperMount = "/run/matchlock/upperfs"
	overlayMergedRoot = "/run/matchlock/merged"
	overlayPutOldDir  = ".old-root"
)

type diskMount struct {
	Device string
	Path   string
}

type hostIPMapping struct {
	Host string
	IP   string
}

type bootConfig struct {
	DNSServers []string
	Hostname   string
	AddHosts   []hostIPMapping
	Workspace  string
	MTU        int
	Disks      []diskMount
	Overlay    overlayBootConfig
}

type overlayBootConfig struct {
	Enabled      bool
	LowerDevices []string
	UpperDevice  string
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
	prepareEarlyFilesystems()
	cfg, err := parseBootConfig(procCmdlinePath)
	if err != nil {
		fatal(err)
	}
	if cfg.Overlay.Enabled {
		if err := setupOverlayRoot(cfg.Overlay); err != nil {
			fatal(err)
		}
	}
	prepareBaseFilesystems()

	_ = os.Setenv("PATH", defaultPATH)
	configureCgroupDelegation()

	if err := configureHostname(cfg.Hostname, cfg.AddHosts); err != nil {
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

		case strings.HasPrefix(field, "matchlock.overlay="):
			v := strings.TrimPrefix(field, "matchlock.overlay=")
			cfg.Overlay.Enabled = v == "1" || strings.EqualFold(v, "true")

		case strings.HasPrefix(field, "matchlock.overlay.lower="):
			spec := strings.TrimPrefix(field, "matchlock.overlay.lower=")
			cfg.Overlay.LowerDevices = cfg.Overlay.LowerDevices[:0]
			for _, dev := range strings.Split(spec, ",") {
				dev = strings.TrimSpace(dev)
				if dev != "" {
					cfg.Overlay.LowerDevices = append(cfg.Overlay.LowerDevices, dev)
				}
			}
			cfg.Overlay.Enabled = true

		case strings.HasPrefix(field, "matchlock.overlay.upper="):
			cfg.Overlay.UpperDevice = strings.TrimPrefix(field, "matchlock.overlay.upper=")
			cfg.Overlay.Enabled = true

		case strings.HasPrefix(field, "matchlock.add_host."):
			spec := strings.TrimPrefix(field, "matchlock.add_host.")
			i := strings.IndexByte(spec, '=')
			if i <= 0 || i == len(spec)-1 {
				return nil, errx.With(ErrInvalidAddHost, ": %q", field)
			}
			mapping, parseErr := parseAddHostField(spec[i+1:])
			if parseErr != nil {
				return nil, parseErr
			}
			cfg.AddHosts = append(cfg.AddHosts, mapping)
		}
	}

	if len(cfg.DNSServers) == 0 {
		return nil, ErrMissingDNS
	}
	if cfg.Overlay.Enabled {
		if len(cfg.Overlay.LowerDevices) == 0 || cfg.Overlay.UpperDevice == "" {
			return nil, errx.With(ErrInvalidOverlayCfg, ": lowers=%v upper=%q", cfg.Overlay.LowerDevices, cfg.Overlay.UpperDevice)
		}
	}

	return cfg, nil
}

func prepareEarlyFilesystems() {
	_ = unix.Mount("", "/", "", unix.MS_REMOUNT, "rw")
	_ = os.MkdirAll("/proc", 0555)
	_ = os.MkdirAll("/dev", 0755)
	mountIgnore("proc", "/proc", "proc", 0, "")
	mountIgnore("dev", "/dev", "devtmpfs", 0, "")
}

func prepareBaseFilesystems() {
	_ = unix.Mount("", "/", "", unix.MS_REMOUNT, "rw")

	_ = os.MkdirAll("/proc", 0555)
	_ = os.MkdirAll("/sys", 0555)
	_ = os.MkdirAll("/dev", 0755)
	_ = os.MkdirAll("/run", 0755)
	_ = os.MkdirAll("/tmp", 01777)
	_ = os.MkdirAll("/sys/fs/bpf", 0755)
	_ = os.MkdirAll("/sys/fs/cgroup", 0755)

	mountIgnore("proc", "/proc", "proc", 0, "")
	mountIgnore("sys", "/sys", "sysfs", 0, "")
	mountIgnore("dev", "/dev", "devtmpfs", 0, "")

	// /dev is now a separate devtmpfs mount; create these paths on that mount.
	_ = os.MkdirAll("/dev/pts", 0755)
	_ = os.MkdirAll("/dev/shm", 01777)
	_ = os.MkdirAll("/dev/mqueue", 0755)

	mountIgnore("devpts", "/dev/pts", "devpts", 0, "")
	mountIgnore("tmpfs", "/dev/shm", "tmpfs", 0, "")
	mountIgnore("mqueue", "/dev/mqueue", "mqueue", 0, "")
	mountIgnore("tmpfs", "/run", "tmpfs", 0, "")
	mountIgnore("tmpfs", "/tmp", "tmpfs", 0, "")
	mountIgnore("bpf", "/sys/fs/bpf", "bpf", 0, "")
	mountIgnore("cgroup2", "/sys/fs/cgroup", "cgroup2", 0, "")
}

func setupOverlayRoot(cfg overlayBootConfig) error {
	if len(cfg.LowerDevices) == 0 || cfg.UpperDevice == "" {
		return errx.With(ErrInvalidOverlayCfg, ": lowers=%v upper=%q", cfg.LowerDevices, cfg.UpperDevice)
	}
	upperDev := filepath.Join("/dev", cfg.UpperDevice)
	lowerDirBase := overlayLowerMount
	upperDir := overlayUpperMount
	overlayUpperDir := filepath.Join(upperDir, "upper")
	workDir := filepath.Join(upperDir, "work")
	merged := overlayMergedRoot

	for _, dir := range []string{lowerDirBase, upperDir, merged} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return errx.With(ErrOverlaySetup, " mkdir %s: %w", dir, err)
		}
	}

	lowerDirs := make([]string, 0, len(cfg.LowerDevices))
	for i, dev := range cfg.LowerDevices {
		lowerDev := filepath.Join("/dev", dev)
		lowerDir := filepath.Join(lowerDirBase, strconv.Itoa(i))
		if err := os.MkdirAll(lowerDir, 0755); err != nil {
			return errx.With(ErrOverlaySetup, " mkdir lower %s: %w", lowerDir, err)
		}
		if err := unix.Mount(lowerDev, lowerDir, "ext4", uintptr(unix.MS_RDONLY), ""); err != nil {
			return errx.With(ErrOverlaySetup, " mount lower %s -> %s: %w", lowerDev, lowerDir, err)
		}
		lowerDirs = append(lowerDirs, lowerDir)
	}
	if err := unix.Mount(upperDev, upperDir, "ext4", 0, ""); err != nil {
		return errx.With(ErrOverlaySetup, " mount upper %s -> %s: %w", upperDev, upperDir, err)
	}
	if err := os.MkdirAll(overlayUpperDir, 0755); err != nil {
		return errx.With(ErrOverlaySetup, " mkdir upperdir %s: %w", overlayUpperDir, err)
	}
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return errx.With(ErrOverlaySetup, " mkdir workdir %s: %w", workDir, err)
	}

	orderedLower := make([]string, 0, len(lowerDirs))
	for i := len(lowerDirs) - 1; i >= 0; i-- {
		orderedLower = append(orderedLower, lowerDirs[i])
	}
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", strings.Join(orderedLower, ":"), overlayUpperDir, workDir)
	if err := unix.Mount("overlay", merged, "overlay", 0, opts); err != nil {
		return errx.With(ErrOverlaySetup, " mount overlay root: %w", err)
	}

	putOld := filepath.Join(merged, overlayPutOldDir)
	if err := os.MkdirAll(putOld, 0755); err != nil {
		return errx.With(ErrOverlaySetup, " mkdir put_old: %w", err)
	}

	if err := unix.PivotRoot(merged, putOld); err != nil {
		return errx.With(ErrOverlaySetup, " pivot_root: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return errx.With(ErrOverlaySetup, " chdir /: %w", err)
	}
	return nil
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

func parseAddHostField(value string) (hostIPMapping, error) {
	parts := strings.SplitN(value, ",", 2)
	if len(parts) != 2 {
		return hostIPMapping{}, errx.With(ErrInvalidAddHost, ": %q (expected host,ip)", value)
	}

	host := strings.TrimSpace(parts[0])
	ip := strings.TrimSpace(parts[1])
	if host == "" || ip == "" {
		return hostIPMapping{}, errx.With(ErrInvalidAddHost, ": %q has empty host or ip", value)
	}
	if strings.ContainsAny(host, " \t\n\r") {
		return hostIPMapping{}, errx.With(ErrInvalidAddHost, ": %q contains whitespace", host)
	}
	if strings.Contains(host, ":") {
		return hostIPMapping{}, errx.With(ErrInvalidAddHost, ": %q contains ':'", host)
	}
	if net.ParseIP(ip) == nil {
		return hostIPMapping{}, errx.With(ErrInvalidAddHost, ": %q invalid ip", ip)
	}

	return hostIPMapping{Host: host, IP: ip}, nil
}

// configureHostname calls sethostname and writes /etc/hostname.
//
// Hostname is set before user-space via the `hostname=` kernel arg, but we set
// it here too to keep /etc/hostname in sync for tools that read the file.
func configureHostname(hostname string, addHosts []hostIPMapping) error {
	if hostname == "" {
		hostname = "matchlock"
	}
	if err := unix.Sethostname([]byte(hostname)); err != nil {
		warnf("set hostname failed: %v", err)
	}
	if err := os.WriteFile(etcHostnamePath, []byte(hostname+"\n"), 0644); err != nil {
		return errx.With(ErrWriteHostname, " write %s: %w", etcHostnamePath, err)
	}
	if err := writeEtcHosts(etcHostsPath, hostname, addHosts); err != nil {
		return errx.With(ErrWriteHosts, " write %s: %w", etcHostsPath, err)
	}

	return nil
}

func writeEtcHosts(path, hostname string, addHosts []hostIPMapping) error {
	return os.WriteFile(path, []byte(renderEtcHosts(hostname, addHosts)), 0644)
}

func renderEtcHosts(hostname string, addHosts []hostIPMapping) string {
	var b strings.Builder
	b.WriteString("127.0.0.1 localhost localhost.localdomain ")
	b.WriteString(hostname)
	b.WriteByte('\n')
	for _, mapping := range addHosts {
		if mapping.Host == "" || mapping.IP == "" {
			continue
		}
		b.WriteString(mapping.IP)
		b.WriteByte(' ')
		b.WriteString(mapping.Host)
		b.WriteByte('\n')
	}
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
