package sandbox

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jingkaihe/matchlock/internal/errx"
)

const defaultBootstrapRootfsMB = 64

// swapPageSize is the guest page size assumed by the Linux version-1 swap
// header. x86_64 is always 4K; arm64 is pinned to 4K via CONFIG_ARM64_4K_PAGES.
const swapPageSize = 4096

// swapMagic is the 10-byte magic terminating the first page of a swap device.
const swapMagic = "SWAPSPACE2"

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

// createSwapImage writes a Linux version-1 swap header into a fresh sparse
// image of sizeMB megabytes. It is pure Go so swap provisioning works on macOS
// hosts without mkswap. The image is attached to the guest as a raw block
// device; only page 0 carries the header and the remaining pages stay sparse
// until the guest writes to them.
func createSwapImage(path string, sizeMB int64) error {
	if sizeMB <= 0 {
		return errx.With(ErrCreateRootfs, ": invalid swap size %dMB", sizeMB)
	}
	sizeBytes := sizeMB * 1024 * 1024
	if sizeBytes%swapPageSize != 0 {
		// sizeMB is whole megabytes, so this is defensive: it guarantees the
		// header never claims a last_page that does not match the real device.
		return errx.With(ErrCreateRootfs, ": swap size %dMB is not %d-byte page-aligned", sizeMB, swapPageSize)
	}
	return writeSwapImage(path, sizeBytes)
}

// writeSwapImage writes the version-1 swap header into a sparse image of
// exactly sizeBytes. sizeBytes must be a positive multiple of swapPageSize.
func writeSwapImage(path string, sizeBytes int64) error {
	if sizeBytes <= 0 {
		return errx.With(ErrCreateRootfs, ": invalid swap image size %d bytes", sizeBytes)
	}
	if sizeBytes%swapPageSize != 0 {
		return errx.With(ErrCreateRootfs, ": swap image size %d bytes is not %d-byte page-aligned", sizeBytes, swapPageSize)
	}

	f, err := os.Create(path)
	if err != nil {
		return errx.With(ErrCreateRootfs, ": create %s: %w", path, err)
	}
	if err := f.Truncate(sizeBytes); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return errx.With(ErrCreateRootfs, ": truncate %s: %w", path, err)
	}

	// Page 0 layout (little-endian): 1024 bootbits zero, version, last_page,
	// nr_badpages, then the SWAPSPACE2 magic ending the page.
	header := make([]byte, swapPageSize)
	binary.LittleEndian.PutUint32(header[1024:1028], 1)
	binary.LittleEndian.PutUint32(header[1028:1032], uint32(sizeBytes/swapPageSize-1))
	binary.LittleEndian.PutUint32(header[1032:1036], 0)
	copy(header[swapPageSize-len(swapMagic):], swapMagic)

	if _, err := f.WriteAt(header, 0); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return errx.With(ErrCreateRootfs, ": write swap header %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return errx.With(ErrCreateRootfs, ": sync %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return errx.With(ErrCreateRootfs, ": close %s: %w", path, err)
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
