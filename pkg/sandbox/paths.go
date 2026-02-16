package sandbox

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/jingkaihe/matchlock/pkg/kernel"
)

// DefaultKernelPath returns the path to the kernel image, downloading if needed.
// It checks in order: MATCHLOCK_KERNEL env, legacy paths, then downloads from OCI.
func DefaultKernelPath() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	path, err := kernel.ResolveKernelPath(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to resolve kernel path: %v\n", err)
		home, _ := os.UserHomeDir()
		arch := kernel.CurrentArch()
		return filepath.Join(home, ".cache/matchlock", arch.KernelFilename())
	}
	return path
}

// DefaultKernelPathWithVersion returns the path to a specific kernel version.
func DefaultKernelPathWithVersion(version string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	mgr := kernel.NewManager()
	arch := kernel.CurrentArch()
	return mgr.EnsureKernel(ctx, arch, version)
}

// DefaultInitramfsPath returns the default path to the initramfs image (optional, mainly for macOS).
func DefaultInitramfsPath() string {
	home, _ := os.UserHomeDir()
	sudoHome := ""
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" && os.Getuid() == 0 {
		sudoHome = filepath.Join("/home", sudoUser)
	}

	paths := []string{
		os.Getenv("MATCHLOCK_INITRAMFS"),
		filepath.Join(home, ".cache/matchlock/initramfs"),
	}
	if sudoHome != "" {
		paths = append(paths, filepath.Join(sudoHome, ".cache/matchlock/initramfs"))
	}

	for _, p := range paths {
		if p != "" {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

// DefaultGuestAgentPath returns the default path to guest-agent binary.
func DefaultGuestAgentPath() string {
	return findGuestBinary("guest-agent", "MATCHLOCK_GUEST_AGENT")
}

// DefaultGuestFusedPath returns the default path to guest-fused binary.
func DefaultGuestFusedPath() string {
	return findGuestBinary("guest-fused", "MATCHLOCK_GUEST_FUSED")
}

// DefaultGuestInitPath returns the default path to guest-init binary.
func DefaultGuestInitPath() string {
	return findGuestBinary("guest-init", "MATCHLOCK_GUEST_INIT")
}

func findGuestBinary(name, envVar string) string {
	home, _ := os.UserHomeDir()
	sudoHome := ""
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" && os.Getuid() == 0 {
		sudoHome = filepath.Join("/home", sudoUser)
	}

	execPath, _ := os.Executable()
	execDir := filepath.Dir(execPath)

	paths := []string{
		os.Getenv(envVar),
		filepath.Join(execDir, name),
		filepath.Join(home, ".cache/matchlock", name),
	}
	if sudoHome != "" {
		paths = append(paths, filepath.Join(sudoHome, ".cache/matchlock", name))
	}

	for _, p := range paths {
		if p != "" {
			if _, err := os.Stat(p); err == nil {
				if isCorrectELFArch(p) {
					return p
				}
				fmt.Fprintf(os.Stderr, "Warning: %s has wrong architecture, skipping\n", p)
			}
		}
	}
	return filepath.Join(execDir, name)
}

// isCorrectELFArch checks if an ELF binary matches the expected guest architecture.
// Returns true for non-ELF files or if the architecture matches.
func isCorrectELFArch(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer f.Close()

	var magic [4]byte
	if _, err := f.Read(magic[:]); err != nil {
		return true
	}
	if magic != [4]byte{0x7f, 'E', 'L', 'F'} {
		return true
	}

	// Read e_machine at offset 18 (2 bytes, little-endian)
	if _, err := f.Seek(18, 0); err != nil {
		return true
	}
	var machine uint16
	if err := binary.Read(f, binary.LittleEndian, &machine); err != nil {
		return true
	}

	// EM_AARCH64=183, EM_X86_64=62
	expectedArch := KernelArch()
	switch expectedArch {
	case "arm64":
		return machine == 183
	case "x86_64":
		return machine == 62
	}
	return true
}

// KernelVersion returns the current kernel version.
func KernelVersion() string {
	return kernel.Version
}

// KernelArch returns the current kernel architecture.
func KernelArch() string {
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		return "arm64"
	}
	return "x86_64"
}
