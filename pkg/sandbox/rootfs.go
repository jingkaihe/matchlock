package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jingkaihe/matchlock/internal/errx"
)

// prepareRootfs injects matchlock guest runtime components into an ext4 rootfs
// image using debugfs. A single guest-init host binary is installed to multiple
// in-guest entrypoints (/init, guest-agent, guest-fused) and dispatches by argv[0].
// It also optionally resizes the rootfs if diskSizeMB > 0.
func prepareRootfs(rootfsPath string, diskSizeMB int64) error {
	guestInitPath := DefaultGuestInitPath()

	if _, err := os.Stat(guestInitPath); err != nil {
		return errx.With(ErrGuestInit, " at %s: %w", guestInitPath, err)
	}

	// Resize BEFORE injecting components so that the filesystem has free space.
	// Images built from large Dockerfiles may have little
	// free blocks in the ext4 image created by createExt4.
	if diskSizeMB > 0 {
		if err := resizeRootfs(rootfsPath, diskSizeMB); err != nil {
			return errx.Wrap(ErrResizeRootfs, err)
		}
	}

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
		{guestInitPath, "/opt/matchlock/guest-init"},
		{guestInitPath, "/opt/matchlock/guest-agent"},
		{guestInitPath, "/opt/matchlock/guest-fused"},
		// Write init binary to both real and usr-merged paths for cross-distro compat.
		{guestInitPath, "/sbin/matchlock-init"},
		{guestInitPath, "/usr/sbin/matchlock-init"},
		// The kernel cmdline uses init=/init to boot guest-init directly.
		{guestInitPath, "/init"},
		// NOTE: We intentionally do NOT overwrite /sbin/init or /usr/sbin/init.
		// Images with ENTRYPOINT ["/sbin/init"] (e.g. systemd) would re-execute
		// the image's init, while matchlock boots through init=/init.
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
		return errx.With(ErrDebugfs, " inject components: %w: %s", err, output)
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
		return errx.Wrap(ErrStatRootfs, err)
	}

	targetBytes := sizeMB * 1024 * 1024
	if fi.Size() >= targetBytes {
		return nil
	}

	if err := os.Truncate(rootfsPath, targetBytes); err != nil {
		return errx.Wrap(ErrTruncate, err)
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
		return errx.With(ErrResize2fs, ": %w: %s", err, out)
	}

	return nil
}

// injectConfigFileIntoRootfs writes a config file with 0644 into an ext4 image using debugfs.
// This allows injecting files (like CA certs) without mounting the filesystem.
// Requires debugfs to be installed (part of e2fsprogs).
func injectConfigFileIntoRootfs(rootfsPath, guestPath string, content []byte) error {
	tmpFile, err := os.CreateTemp("", "inject-*")
	if err != nil {
		return errx.Wrap(ErrCreateTemp, err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.Write(content); err != nil {
		tmpFile.Close()
		return errx.Wrap(ErrWriteTemp, err)
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
	commands = append(commands, fmt.Sprintf("set_inode_field %s mode 0100644", guestPath))

	cmdStr := strings.Join(commands, "\n")
	cmd := exec.Command("debugfs", "-w", rootfsPath)
	cmd.Stdin = strings.NewReader(cmdStr)
	if output, err := cmd.CombinedOutput(); err != nil {
		return errx.With(ErrDebugfs, ": %w: %s", err, output)
	}

	return nil
}
