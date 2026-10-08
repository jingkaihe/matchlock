//go:build linux

// guest-init is the unified guest runtime binary.
// Invoked as /init it acts as PID1 and performs bootstrapping.
// Invoked as guest-agent or guest-fused (via argv[0]) it runs that mode.
package main

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

	// trustedGuestRuntimeRoot is the guest path where the sandbox injects the
	// guest-init/guest-agent/guest-fused binaries. An exact-destination mount
	// must never be created over this root or any of its subpaths.
	trustedGuestRuntimeRoot = "/opt/matchlock"

	defaultPATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

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

var cpuMaxPaths = []string{
	"/sys/fs/cgroup/init/cpu.max",
	"/sys/fs/cgroup/cpu.max",
}

// swapDevicePattern accepts one or more lowercase ASCII letters after the "vd"
// prefix. Current backends emit single-letter names (vda..vdz), but the pattern
// also tolerates a future multi-letter scheme (vdaa) so the guest never rejects
// a name a backend legitimately generates.
var swapDevicePattern = regexp.MustCompile(`^vd[a-z]+$`)

type diskMount struct {
	Device   string
	Path     string
	ReadOnly bool
	OwnerUID *uint32
	OwnerGID *uint32
}

type hostIPMapping struct {
	Host string
	IP   string
}

type bootConfig struct {
	DNSServers  []string
	Hostname    string
	AddHosts    []hostIPMapping
	Workspace   string
	ExactMounts []string
	CPUs        float64
	MTU         int
	NoNetwork   bool
	Disks       []diskMount
	Overlay     overlayBootConfig
	SwapDevice  string
	Privileged  bool
	// IPv6 is the guest side of the per-VM IPv6 link announced by the host, or
	// nil when the boot has no IPv6 link (see activeIPv6Link).
	IPv6 *ipv6Link
}

type overlayBootConfig struct {
	Enabled      bool
	LowerDevices []string
	LowerFSTypes []string
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

	// Swap must be enabled while guest-init still runs as PID 1 with full
	// capabilities: the workload launcher later drops CAP_SYS_ADMIN from the
	// bounding set (there is no CAP_SWAP), so an unprivileged workload can
	// never swapon/swapoff. devtmpfs is mounted on /dev by
	// prepareBaseFilesystems, so the swap block device node already exists.
	if cfg.SwapDevice != "" {
		if err := enableSwap(cfg.SwapDevice); err != nil {
			fatal(err)
		}
		// enableSwap's root:root 0600 node blocks non-root workloads, and the
		// cgroup v2 device policy below is what actually binds a uid-0
		// workload (owner DAC bits and CAP_MKNOD would otherwise expose the
		// VM-wide store). Attach it after swapon so PID 1 can open the device
		// first; the kernel keeps the backing file open, so later denial does
		// not disturb active swap.
		if swapDevicePolicyRequired(cfg) {
			if err := installSwapDevicePolicy(cfg.SwapDevice, cgroup2RootPath); err != nil {
				fatal(err)
			}
		}
	}

	_ = os.Setenv("PATH", defaultPATH)
	configureCgroupDelegation()
	configureCPULimit(cfg.CPUs)

	if err := configureHostname(cfg.Hostname, cfg.AddHosts); err != nil {
		fatal(err)
	}

	if err := writeResolvConf(etcResolvConfPath, cfg.DNSServers); err != nil {
		fatal(err)
	}

	if !cfg.NoNetwork {
		bringUpNetwork(networkInterface, cfg.MTU)
		// The guest side of the IPv6 link is installed here, while guest-init
		// still runs as PID 1 and before the workload starts: the kernel's ip=
		// boot argument configures IPv4 only, so the address and the default
		// route come from netlink (see network_ipv6.go).
		//
		// A failure is reported, not fatal: without the guest address there is
		// simply no IPv6 traffic, which is exactly the fail-closed state the ip6
		// interception table enforces, and an otherwise usable IPv4 sandbox
		// should still boot. bringUpNetwork treats MTU/link failures the same
		// way.
		if link := cfg.activeIPv6Link(); link != nil {
			if err := configureGuestIPv6(networkInterface, *link); err != nil {
				warnf("configure guest ipv6 %s: %v", link, err)
			}
		}
	}
	if err := mountExtraDisks(cfg.Disks); err != nil {
		fatal(err)
	}

	if cfg.Workspace != "" {
		if err := startGuestFused(guestFusedPath); err != nil {
			fatal(err)
		}

		if err := waitForWorkspaceMount(procMountsPath, cfg.Workspace, workspaceWaitMax); err != nil {
			fatal(err)
		}
	}

	// Exact-destination mounts: attach a FUSE tree at each requested absolute
	// guest path (for example /opt/project or a linked worktree) so the same
	// absolute path is physically reachable in the guest. The host router serves
	// each exact destination; guest-init only creates missing empty parents and
	// starts a per-destination FUSE daemon before the unprivileged harness runs.
	for _, mountpoint := range cfg.ExactMounts {
		if filepath.Clean(mountpoint) == filepath.Clean(cfg.Workspace) {
			continue
		}
		if err := ensureExactMountDir(mountpoint); err != nil {
			fatal(err)
		}
		if err := startGuestFused(guestFusedPath, mountpoint); err != nil {
			fatal(err)
		}
		if err := waitForWorkspaceMount(procMountsPath, mountpoint, workspaceWaitMax); err != nil {
			fatal(errx.With(ErrWorkspaceMountWait, " exact mount %s: %w", mountpoint, err))
		}
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
		CPUs: 1,
		MTU:  defaultNetworkMTU,
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

		case strings.HasPrefix(field, "matchlock.exact.mounts="):
			v := strings.TrimPrefix(field, "matchlock.exact.mounts=")
			if v != "" {
				for _, p := range strings.Split(v, ",") {
					p = strings.TrimSpace(p)
					if p != "" {
						if !strings.HasPrefix(p, "/") || !validExactMountChar(p) {
							return nil, errx.With(ErrInvalidExactMount, ": %q", p)
						}
						cfg.ExactMounts = append(cfg.ExactMounts, p)
					}
				}
			}

		case strings.HasPrefix(field, ipv6FieldPrefix):
			v := strings.TrimPrefix(field, ipv6FieldPrefix)
			if v == "" {
				// An empty field means "no IPv6 link": an IPv4-only boot must
				// behave exactly as before, so nothing is configured for it.
				continue
			}
			link, parseErr := parseIPv6LinkField(v)
			if parseErr != nil {
				return nil, parseErr
			}
			cfg.IPv6 = &link

		case strings.HasPrefix(field, "matchlock.mtu="):
			v := strings.TrimPrefix(field, "matchlock.mtu=")
			mtu, convErr := strconv.Atoi(v)
			if convErr != nil || mtu <= 0 || mtu > 65535 {
				return nil, errx.With(ErrInvalidMTU, ": %q", v)
			}
			cfg.MTU = mtu

		case strings.HasPrefix(field, "matchlock.cpus="):
			v := strings.TrimPrefix(field, "matchlock.cpus=")
			cpus, convErr := strconv.ParseFloat(v, 64)
			if convErr != nil || math.IsNaN(cpus) || math.IsInf(cpus, 0) || cpus <= 0 {
				return nil, errx.With(ErrInvalidCPUs, ": %q", v)
			}
			cfg.CPUs = cpus

		case strings.HasPrefix(field, "matchlock.no_network="):
			v := strings.TrimPrefix(field, "matchlock.no_network=")
			cfg.NoNetwork = v == "1" || strings.EqualFold(v, "true")

		case strings.HasPrefix(field, "matchlock.privileged="):
			v := strings.TrimPrefix(field, "matchlock.privileged=")
			cfg.Privileged = v == "1" || strings.EqualFold(v, "true")

		case strings.HasPrefix(field, "matchlock.swap="):
			dev := strings.TrimSpace(strings.TrimPrefix(field, "matchlock.swap="))
			if !swapDevicePattern.MatchString(dev) {
				return nil, errx.With(ErrInvalidSwap, ": %q", dev)
			}
			cfg.SwapDevice = dev

		case strings.HasPrefix(field, "matchlock.disk."):
			spec := strings.TrimPrefix(field, "matchlock.disk.")
			i := strings.IndexByte(spec, '=')
			if i <= 0 || i == len(spec)-1 {
				continue
			}
			disk, parseErr := parseDiskMountSpec(spec[i+1:])
			if parseErr != nil {
				return nil, parseErr
			}
			disk.Device = spec[:i]
			cfg.Disks = append(cfg.Disks, disk)

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

		case strings.HasPrefix(field, "matchlock.overlay.lowerfs="):
			spec := strings.TrimPrefix(field, "matchlock.overlay.lowerfs=")
			cfg.Overlay.LowerFSTypes = cfg.Overlay.LowerFSTypes[:0]
			for _, fs := range strings.Split(spec, ",") {
				fs = strings.TrimSpace(strings.ToLower(fs))
				if fs != "" {
					cfg.Overlay.LowerFSTypes = append(cfg.Overlay.LowerFSTypes, fs)
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
		if len(cfg.Overlay.LowerFSTypes) > 0 && len(cfg.Overlay.LowerFSTypes) != len(cfg.Overlay.LowerDevices) {
			return nil, errx.With(ErrInvalidOverlayCfg, ": lowerfs=%v does not match lowers=%v", cfg.Overlay.LowerFSTypes, cfg.Overlay.LowerDevices)
		}
	}

	return cfg, nil
}

// swapDeviceMode is the mode applied to the swap block device node after
// swapon. Access to a block device is governed by ordinary DAC (device-node
// permissions), NOT by CAP_SYS_ADMIN, so dropping CAP_SYS_ADMIN from the
// workload bounding set does not by itself stop a non-root workload from
// reading the VM-wide swap backing store. Narrowing the node to root-only 0600
// closes that path for every non-root workload (the common --user case).
//
// This is defense-in-depth, not the boundary. A uid-0 workload is the node
// *owner*, so the 0600 owner bits (0400/0200) grant it access; CAP_DAC_OVERRIDE
// is irrelevant for an owner and dropping it would not help. A uid-0 workload
// also holds CAP_MKNOD and can recreate a node from the major:minor exposed by
// /sys/block/<dev>/dev. The actual root-binding control is the cgroup v2
// device-controller BPF policy in swap_bpf.go, attached to the cgroup2 root and
// evaluated before and independently of file ownership for both open and
// mknod. Do not remove the node: swapon(2) has already opened the backing
// device (removal does not stop swap), and a present 0600 node yields a clear
// EACCES for non-root instead of an ambiguous ENOENT.
const swapDeviceMode os.FileMode = 0o600

// enableSwap turns the guest swap device on. It runs from guest-init (PID 1)
// while full capabilities are still held; the launcher drops CAP_SYS_ADMIN from
// the workload's bounding set afterwards, so the workload cannot manage swap.
// The block device is validated first so a wrong or mis-ordered device yields a
// clear error instead of an opaque swapon errno. After swapon succeeds the node
// is restricted to root:root 0600 so non-root workloads cannot read it.
func enableSwap(dev string) error {
	path := filepath.Join("/dev", dev)
	if err := validateSwapDevice(path); err != nil {
		return err
	}
	pathPtr, err := unix.BytePtrFromString(path)
	if err != nil {
		return errx.With(ErrEnableSwap, " %s: %w", path, err)
	}
	if _, _, errno := unix.RawSyscall(unix.SYS_SWAPON, uintptr(unsafe.Pointer(pathPtr)), 0, 0); errno != 0 {
		return errx.With(ErrEnableSwap, " %s: %w", path, errno)
	}
	// Restrict after a successful swapon: the kernel already holds the device
	// open, so tightening the node cannot break the active swap area.
	if err := restrictSwapDevice(path); err != nil {
		return err
	}
	return nil
}

// restrictSwapDevice applies the swap-device access policy (root:root, 0600) to
// the given device node.
func restrictSwapDevice(path string) error {
	return restrictDeviceNode(path, 0, 0, swapDeviceMode)
}

// restrictDeviceNode chowns path to uid:gid and chmods it to mode. It is the
// testable seam: production passes the root owner (0:0) and swapDeviceMode,
// while a unit test can pass the current euid/egid so the chown is permitted
// without privileges. Errors wrap ErrEnableSwap because the only caller is the
// swap boot path.
func restrictDeviceNode(path string, uid, gid int, mode os.FileMode) error {
	if err := os.Chown(path, uid, gid); err != nil {
		return errx.With(ErrEnableSwap, " chown %s to %d:%d: %w", path, uid, gid, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		return errx.With(ErrEnableSwap, " chmod %s to %#o: %w", path, mode, err)
	}
	return nil
}

// validateSwapDevice requires path to exist and be a block device. os.ModeDevice
// matches both block and character devices, so the character bit is excluded
// explicitly; a regular file or missing node is rejected before swapon(2).
func validateSwapDevice(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return errx.With(ErrInvalidSwap, " %s: %w", path, err)
	}
	if !isBlockDevice(info) {
		return errx.With(ErrInvalidSwap, " %s: not a block device (%s)", path, info.Mode())
	}
	return nil
}

func isBlockDevice(info os.FileInfo) bool {
	return info.Mode()&os.ModeDevice != 0 && info.Mode()&os.ModeCharDevice == 0
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
		lowerFS := "erofs"
		if i < len(cfg.LowerFSTypes) && cfg.LowerFSTypes[i] != "" {
			switch cfg.LowerFSTypes[i] {
			case "erofs":
				lowerFS = cfg.LowerFSTypes[i]
			default:
				return errx.With(ErrInvalidOverlayCfg, " invalid lower fs type %q for %s", cfg.LowerFSTypes[i], lowerDev)
			}
		}
		lowerDir := filepath.Join(lowerDirBase, strconv.Itoa(i))
		if err := os.MkdirAll(lowerDir, 0755); err != nil {
			return errx.With(ErrOverlaySetup, " mkdir lower %s: %w", lowerDir, err)
		}
		if err := unix.Mount(lowerDev, lowerDir, lowerFS, uintptr(unix.MS_RDONLY), ""); err != nil {
			return errx.With(ErrOverlaySetup, " mount lower %s (%s) -> %s: %w", lowerDev, lowerFS, lowerDir, err)
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

func configureCPULimit(cpus float64) {
	if math.IsNaN(cpus) || math.IsInf(cpus, 0) || cpus <= 0 {
		return
	}
	quota := int(cpus * 100000.0)
	if quota < 1000 {
		quota = 1000
	}
	limit := []byte(fmt.Sprintf("%d 100000", quota))
	for _, p := range cpuMaxPaths {
		if err := os.WriteFile(p, limit, 0644); err == nil {
			return
		}
	}
	warnf("apply cpu quota failed: %s", strings.TrimSpace(string(limit)))
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

func parseDiskMountSpec(value string) (diskMount, error) {
	parts := strings.Split(value, ",")
	if len(parts) == 0 {
		return diskMount{}, errx.With(ErrInvalidDiskMount, ": %q", value)
	}

	path := strings.TrimSpace(parts[0])
	if path == "" {
		return diskMount{}, errx.With(ErrInvalidDiskMount, ": %q has empty mount path", value)
	}
	if !strings.HasPrefix(path, "/") {
		return diskMount{}, errx.With(ErrInvalidDiskMount, ": %q must be an absolute path", path)
	}

	disk := diskMount{Path: path}
	for _, opt := range parts[1:] {
		opt = strings.TrimSpace(opt)
		key, value, hasValue := strings.Cut(opt, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)

		switch key {
		case "":
			continue
		case "ro", "readonly":
			if hasValue {
				return diskMount{}, errx.With(ErrInvalidDiskMount, ": unknown option %q", opt)
			}
			disk.ReadOnly = true
		case "rw":
			if hasValue {
				return diskMount{}, errx.With(ErrInvalidDiskMount, ": unknown option %q", opt)
			}
			disk.ReadOnly = false
		case "uid":
			if !hasValue {
				return diskMount{}, errx.With(ErrInvalidDiskMount, ": unknown option %q", opt)
			}
			uid, err := parseDiskOwnerID("uid", value)
			if err != nil {
				return diskMount{}, err
			}
			disk.OwnerUID = &uid
		case "gid":
			if !hasValue {
				return diskMount{}, errx.With(ErrInvalidDiskMount, ": unknown option %q", opt)
			}
			gid, err := parseDiskOwnerID("gid", value)
			if err != nil {
				return diskMount{}, err
			}
			disk.OwnerGID = &gid
		default:
			return diskMount{}, errx.With(ErrInvalidDiskMount, ": unknown option %q", opt)
		}
	}
	if disk.ReadOnly && (disk.OwnerUID != nil || disk.OwnerGID != nil) {
		return diskMount{}, errx.With(ErrInvalidDiskMount, ": ownership options require writable disk")
	}

	return disk, nil
}

func parseDiskOwnerID(name string, value string) (uint32, error) {
	if value == "" {
		return 0, errx.With(ErrInvalidDiskMount, ": %s cannot be empty", name)
	}
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0, errx.With(ErrInvalidDiskMount, ": %s %q must be an unsigned 32-bit integer: %w", name, value, err)
	}
	return uint32(parsed), nil
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

func mountExtraDisks(disks []diskMount) error {
	for _, d := range disks {
		if d.Device == "" || d.Path == "" {
			continue
		}
		if err := os.MkdirAll(d.Path, 0755); err != nil {
			return errx.With(ErrMountExtraDisk, " create mount path %s: %w", d.Path, err)
		}
		flags := uintptr(0)
		if d.ReadOnly {
			flags = unix.MS_RDONLY
		}
		if err := unix.Mount(filepath.Join("/dev", d.Device), d.Path, "ext4", flags, ""); err != nil {
			return errx.With(ErrMountExtraDisk, " /dev/%s -> %s: %w", d.Device, d.Path, err)
		}
		if err := chownDiskMountRoot(d); err != nil {
			return err
		}
	}
	return nil
}

func chownDiskMountRoot(d diskMount) error {
	if d.OwnerUID == nil && d.OwnerGID == nil {
		return nil
	}
	uid := -1
	gid := -1
	if d.OwnerUID != nil {
		uid = int(*d.OwnerUID)
	}
	if d.OwnerGID != nil {
		gid = int(*d.OwnerGID)
	}
	if err := os.Chown(d.Path, uid, gid); err != nil {
		return errx.With(ErrMountExtraDisk, " chown %s: %w", d.Path, err)
	}
	return nil
}

func startGuestFused(path string, mountpoint ...string) error {
	cmd := exec.Command(path)
	if len(mountpoint) > 0 && mountpoint[0] != "" {
		cmd = exec.Command(path, mountpoint[0])
	}
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

// validExactMountChar reports whether p is a safe absolute guest path for an
// exact-destination mount. It mirrors the host API guest-mount path validator
// (only alphanumeric, '/', '_', '.', '-' and no '..', so a path cannot escape or
// smuggle a shell/argv separator).
func validExactMountChar(p string) bool {
	if !strings.HasPrefix(p, "/") {
		return false
	}
	if strings.Contains(p, "..") {
		return false
	}
	for _, r := range p {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '/' || r == '_' || r == '.' || r == '-' {
			continue
		}
		return false
	}
	return true
}

// ensureExactMountDir validates and creates (only the missing, empty) guest
// parent directories for an exact-destination FUSE mount. It refuses an exact
// mount through a symlinked component, over a non-empty final directory (which
// would silently shadow guest content), and over the trusted guest runtime root
// /opt/matchlock or any of its subpaths (where guest-init/agent/fused are
// injected). Only missing components are created as empty directories; existing
// guest OS directories (for example /opt or /home) are left untouched.
func ensureExactMountDir(path string) error {
	path = filepath.Clean(path)
	if !strings.HasPrefix(path, "/") {
		return errx.With(ErrExactMountPrep, " %q is not absolute", path)
	}
	// The trusted guest runtime must never be shadowed by a host mount. Mounting
	// over /opt/matchlock or a subpath would hide the injected guest-init/agent/
	// fused binaries (or a future runtime file) and corrupt the sandbox.
	if path == trustedGuestRuntimeRoot || strings.HasPrefix(path, trustedGuestRuntimeRoot+"/") {
		return errx.With(ErrExactMountPrep, " %q shadows the trusted guest runtime %s", path, trustedGuestRuntimeRoot)
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	current := ""
	for _, part := range parts {
		if part == "" {
			continue
		}
		current += "/" + part
		fi, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				if err := os.Mkdir(current, 0755); err != nil && !os.IsExist(err) {
					return errx.With(ErrExactMountPrep, " mkdir %s: %w", current, err)
				}
				continue
			}
			return errx.With(ErrExactMountPrep, " lstat %s: %w", current, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return errx.With(ErrExactMountPrep, " %s is a symlink; refusing an exact mount through a link", current)
		}
		if !fi.IsDir() {
			return errx.With(ErrExactMountPrep, " %s is not a directory", current)
		}
		// Only the final (leaf) directory matters for the "do not shadow guest
		// content" rule: a non-empty leaf would be hidden by the FUSE mount.
		if filepath.Clean(current) == path {
			empty, err := isDirEmpty(current)
			if err != nil {
				return errx.With(ErrExactMountPrep, " %s: %w", current, err)
			}
			if !empty {
				return errx.With(ErrExactMountPrep, " %s already exists and is not empty; refusing to shadow guest content", current)
			}
		}
	}
	return nil
}

func isDirEmpty(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	_, err = f.Readdirnames(1)
	if err == io.EOF {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, nil // at least one entry
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
