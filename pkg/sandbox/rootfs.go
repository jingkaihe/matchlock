package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jingkaihe/matchlock/internal/errx"
)

const defaultBootstrapRootfsMB = 64

func createExt4Image(path string, sizeMB int64) error {
	if sizeMB <= 0 {
		return errx.With(ErrCreateRootfs, ": invalid size %dMB", sizeMB)
	}

	f, err := os.Create(path)
	if err != nil {
		return errx.With(ErrCreateRootfs, ": create %s: %w", path, err)
	}
	targetBytes := sizeMB * 1024 * 1024
	if err := f.Truncate(targetBytes); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return errx.With(ErrCreateRootfs, ": truncate %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return errx.With(ErrCreateRootfs, ": close %s: %w", path, err)
	}

	mke2fsPath, err := exec.LookPath("mke2fs")
	if err != nil {
		mke2fsPath, err = exec.LookPath("mkfs.ext4")
		if err != nil {
			_ = os.Remove(path)
			return errx.With(ErrCreateRootfs, ": mke2fs/mkfs.ext4 not found; install e2fsprogs")
		}
	}

	cmd := exec.Command(mke2fsPath, "-t", "ext4", "-F", "-q", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(path)
		return errx.With(ErrCreateRootfs, ": mke2fs %s: %w: %s", path, err, out)
	}
	return nil
}

func createBootstrapRootfs(path string) error {
	if err := createExt4Image(path, defaultBootstrapRootfsMB); err != nil {
		return errx.Wrap(ErrPrepareBootstrapRoot, err)
	}
	guestInitPath := DefaultGuestInitPath()
	if _, err := os.Stat(guestInitPath); err != nil {
		_ = os.Remove(path)
		return errx.With(ErrPrepareBootstrapRoot, " guest-init at %s: %w", guestInitPath, err)
	}
	if err := injectHostBinaryIntoRootfs(path, guestInitPath, "/init"); err != nil {
		_ = os.Remove(path)
		return errx.Wrap(ErrPrepareBootstrapRoot, err)
	}
	return nil
}

// prepareOverlayUpperRootfs initializes a writable overlay upper image with
// matchlock runtime binaries under /upper and an empty /work directory for
// overlayfs workdir usage.
func prepareOverlayUpperRootfs(rootfsPath string) error {
	guestInitPath := DefaultGuestInitPath()
	if _, err := os.Stat(guestInitPath); err != nil {
		return errx.With(ErrGuestInit, " at %s: %w", guestInitPath, err)
	}

	var commands []string
	for _, dir := range []string{
		"/upper",
		"/upper/opt",
		"/upper/opt/matchlock",
		"/upper/sbin",
		"/upper/usr",
		"/upper/usr/sbin",
		"/work",
	} {
		commands = append(commands, fmt.Sprintf("mkdir %s", dir))
	}

	type injection struct {
		hostPath  string
		guestPath string
	}
	injections := []injection{
		{guestInitPath, "/upper/opt/matchlock/guest-init"},
		{guestInitPath, "/upper/opt/matchlock/guest-agent"},
		{guestInitPath, "/upper/opt/matchlock/guest-fused"},
		{guestInitPath, "/upper/sbin/matchlock-init"},
		{guestInitPath, "/upper/usr/sbin/matchlock-init"},
		{guestInitPath, "/upper/init"},
	}

	for _, inj := range injections {
		commands = append(commands, fmt.Sprintf("rm %s", inj.guestPath))
		commands = append(commands, fmt.Sprintf("write %s %s", inj.hostPath, inj.guestPath))
		commands = append(commands, fmt.Sprintf("set_inode_field %s mode 0100755", inj.guestPath))
	}

	cmd := exec.Command("debugfs", "-w", rootfsPath)
	cmd.Stdin = strings.NewReader(strings.Join(commands, "\n"))
	if output, err := cmd.CombinedOutput(); err != nil {
		return errx.With(ErrDebugfs, " prepare overlay upper: %w: %s", err, output)
	}
	return nil
}

func injectHostBinaryIntoRootfs(rootfsPath, hostPath, guestPath string) error {
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
	commands = append(commands, fmt.Sprintf("write %s %s", hostPath, guestPath))
	commands = append(commands, fmt.Sprintf("set_inode_field %s mode 0100755", guestPath))

	cmd := exec.Command("debugfs", "-w", rootfsPath)
	cmd.Stdin = strings.NewReader(strings.Join(commands, "\n"))
	if output, err := cmd.CombinedOutput(); err != nil {
		return errx.With(ErrDebugfs, " inject host binary: %w: %s", err, output)
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
