package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const initScript = `#!/bin/sh
# Matchlock minimal init - runs as PID 1
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

# Remount root read-write (may be mounted ro by initramfs)
mount -o remount,rw / 2>/dev/null || true

mount -t proc proc /proc 2>/dev/null || true
mount -t sysfs sys /sys 2>/dev/null || true
mount -t devtmpfs dev /dev 2>/dev/null || true

mkdir -p /dev/pts /dev/shm /dev/mqueue
mount -t devpts devpts /dev/pts 2>/dev/null || true
mount -t tmpfs tmpfs /dev/shm 2>/dev/null || true
mount -t mqueue mqueue /dev/mqueue 2>/dev/null || true
mount -t tmpfs tmpfs /run 2>/dev/null || true
mount -t tmpfs tmpfs /tmp 2>/dev/null || true

# Mount BPF filesystem (required for runc cgroup device management)
mkdir -p /sys/fs/bpf
mount -t bpf bpf /sys/fs/bpf 2>/dev/null || true

# Mount cgroup2 unified hierarchy and enable controller delegation
mkdir -p /sys/fs/cgroup
mount -t cgroup2 cgroup2 /sys/fs/cgroup 2>/dev/null || true
# Enable subtree delegation for BuildKit/runc. Cgroup v2 requires that no
# processes live in a cgroup before its subtree_control can be written.
# Move our PID to a child cgroup ("init"), then enable controllers on root.
if [ -f /sys/fs/cgroup/cgroup.subtree_control ] && [ -f /sys/fs/cgroup/cgroup.controllers ]; then
    mkdir -p /sys/fs/cgroup/init
    echo $$ > /sys/fs/cgroup/init/cgroup.procs 2>/dev/null || true
    for c in $(cat /sys/fs/cgroup/cgroup.controllers); do
        echo "+$c" > /sys/fs/cgroup/cgroup.subtree_control 2>/dev/null || true
    done
fi

hostname matchlock

# Configure DNS - remove any existing file/symlink and write fresh
rm -f /etc/resolv.conf
printf "nameserver 8.8.8.8\nnameserver 8.8.4.4\n" > /etc/resolv.conf

# Network setup - bring up interface and get IP via DHCP
ip link set eth0 up 2>/dev/null || ifconfig eth0 up 2>/dev/null

# Try DHCP if kernel didn't configure IP (NAT mode)
if ! ip addr show eth0 2>/dev/null | grep -q "inet "; then
    # Alpine/busybox udhcpc
    if command -v udhcpc >/dev/null 2>&1; then
        udhcpc -i eth0 -n -q 2>/dev/null &
    # Debian/Ubuntu dhclient
    elif command -v dhclient >/dev/null 2>&1; then
        dhclient eth0 2>/dev/null &
    fi
    sleep 2
fi

# Start FUSE daemon for VFS
/opt/matchlock/guest-fused &

# Get workspace path from kernel cmdline
WORKSPACE=$(cat /proc/cmdline | tr ' ' '\n' | grep 'matchlock.workspace=' | cut -d= -f2)
[ -z "$WORKSPACE" ] && WORKSPACE="/workspace"

# Wait for VFS mount before starting agent
for i in $(seq 1 50); do
    if mount | grep -q "$WORKSPACE"; then
        break
    fi
    sleep 0.1
done

# CA cert is injected directly into rootfs at /etc/ssl/certs/matchlock-ca.crt
# No VFS-based injection needed

exec /opt/matchlock/guest-agent
`

// prepareRootfs injects matchlock components into an ext4 rootfs image using debugfs.
// This includes the guest-agent binary, guest-fused binary, and init scripts.
// DNS config is written at runtime by the init script to handle symlinked resolv.conf.
// It also optionally resizes the rootfs if diskSizeMB > 0.
func prepareRootfs(rootfsPath string, diskSizeMB int64) error {
	guestAgentPath := DefaultGuestAgentPath()
	guestFusedPath := DefaultGuestFusedPath()

	if _, err := os.Stat(guestAgentPath); err != nil {
		return fmt.Errorf("guest-agent not found at %s: %w", guestAgentPath, err)
	}
	if _, err := os.Stat(guestFusedPath); err != nil {
		return fmt.Errorf("guest-fused not found at %s: %w", guestFusedPath, err)
	}

	// Write init script to temp file for debugfs injection
	initTmp, err := os.CreateTemp("", "matchlock-init-*")
	if err != nil {
		return fmt.Errorf("create init temp: %w", err)
	}
	defer os.Remove(initTmp.Name())
	if _, err := initTmp.WriteString(initScript); err != nil {
		initTmp.Close()
		return fmt.Errorf("write init temp: %w", err)
	}
	initTmp.Close()

	// Build debugfs commands to inject all components.
	// debugfs cannot traverse symlinks, so we write to both /sbin/ and /usr/sbin/
	// to handle distros where /sbin is real (Alpine) or a symlink (Ubuntu).
	// rm before write because debugfs write silently fails on existing files.
	var commands []string

	// Create directories that may not exist (mkdir on existing dirs/symlinks is harmless)
	for _, dir := range []string{
		"/opt",
		"/opt/matchlock",
		"/sbin",
		"/usr/sbin",
		"/run",
		"/proc",
		"/sys",
		"/dev",
		"/workspace",
	} {
		commands = append(commands, fmt.Sprintf("mkdir %s", dir))
	}

	type injection struct {
		hostPath  string
		guestPath string
	}

	injections := []injection{
		{guestAgentPath, "/opt/matchlock/guest-agent"},
		{guestFusedPath, "/opt/matchlock/guest-fused"},
		// Write init to both real and usr-merged paths for cross-distro compat
		{initTmp.Name(), "/sbin/matchlock-init"},
		{initTmp.Name(), "/usr/sbin/matchlock-init"},
		{initTmp.Name(), "/init"},
		{initTmp.Name(), "/sbin/init"},
		{initTmp.Name(), "/usr/sbin/init"},
	}

	for _, inj := range injections {
		commands = append(commands, fmt.Sprintf("rm %s", inj.guestPath))
		commands = append(commands, fmt.Sprintf("write %s %s", inj.hostPath, inj.guestPath))
		commands = append(commands, fmt.Sprintf("set_inode_field %s mode 0100755", inj.guestPath))
	}

	cmdStr := strings.Join(commands, "\n")
	cmd := exec.Command("debugfs", "-w", rootfsPath)
	cmd.Stdin = strings.NewReader(cmdStr)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("debugfs inject components: %w: %s", err, output)
	}

	// Resize if requested
	if diskSizeMB > 0 {
		if err := resizeRootfs(rootfsPath, diskSizeMB); err != nil {
			return fmt.Errorf("resize rootfs: %w", err)
		}
	}

	return nil
}

// resizeRootfs expands an ext4 image to the given size in MB.
// Uses truncate to expand the sparse file and resize2fs to grow the filesystem.
// If the image is already larger than sizeMB, this is a no-op.
func resizeRootfs(rootfsPath string, sizeMB int64) error {
	if sizeMB <= 0 {
		return nil
	}

	fi, err := os.Stat(rootfsPath)
	if err != nil {
		return fmt.Errorf("stat rootfs: %w", err)
	}

	targetBytes := sizeMB * 1024 * 1024
	if fi.Size() >= targetBytes {
		return nil
	}

	if err := os.Truncate(rootfsPath, targetBytes); err != nil {
		return fmt.Errorf("truncate rootfs: %w", err)
	}

	e2fsckPath, _ := exec.LookPath("e2fsck")
	if e2fsckPath != "" {
		cmd := exec.Command(e2fsckPath, "-fy", rootfsPath)
		cmd.Stdin = nil
		cmd.CombinedOutput()
	}

	resize2fsPath, err := exec.LookPath("resize2fs")
	if err != nil {
		return fmt.Errorf("resize2fs not found; install e2fsprogs")
	}

	cmd := exec.Command(resize2fsPath, "-f", rootfsPath)
	cmd.Stdin = nil
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("resize2fs: %w: %s", err, out)
	}

	return nil
}

// injectFileIntoRootfs writes a file into an ext4 image using debugfs.
// This allows injecting files (like CA certs) without mounting the filesystem.
// Requires debugfs to be installed (part of e2fsprogs).
func injectFileIntoRootfs(rootfsPath, guestPath string, content []byte) error {
	tmpFile, err := os.CreateTemp("", "inject-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.Write(content); err != nil {
		tmpFile.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	tmpFile.Close()

	var commands []string
	dir := filepath.Dir(guestPath)
	if dir != "/" && dir != "." {
		var dirs []string
		for d := dir; d != "/" && d != "."; d = filepath.Dir(d) {
			dirs = append([]string{d}, dirs...)
		}
		for _, d := range dirs {
			commands = append(commands, fmt.Sprintf("mkdir %s", d))
		}
	}
	commands = append(commands, fmt.Sprintf("rm %s", guestPath))
	commands = append(commands, fmt.Sprintf("write %s %s", tmpPath, guestPath))

	cmdStr := strings.Join(commands, "\n")
	cmd := exec.Command("debugfs", "-w", rootfsPath)
	cmd.Stdin = strings.NewReader(cmdStr)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("debugfs: %w: %s", err, output)
	}

	return nil
}
