package sandbox

import (
	"context"
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

// DefaultRootfsPath returns the default path to the rootfs image for the given variant.
func DefaultRootfsPath(image string) string {
	if image == "" {
		image = "standard"
	}

	home, _ := os.UserHomeDir()
	sudoHome := ""
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" && os.Getuid() == 0 {
		sudoHome = filepath.Join("/home", sudoUser)
	}

	filename := "rootfs-" + image + ".ext4"
	paths := []string{
		os.Getenv("MATCHLOCK_ROOTFS"),
		filepath.Join(home, ".cache/matchlock", filename),
	}
	if sudoHome != "" {
		paths = append(paths, filepath.Join(sudoHome, ".cache/matchlock", filename))
	}

	for _, p := range paths {
		if p != "" {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return filepath.Join(home, ".cache/matchlock", filename)
}

// DefaultGuestAgentPath returns the default path to guest-agent binary.
func DefaultGuestAgentPath() string {
	return findGuestBinary("guest-agent", "MATCHLOCK_GUEST_AGENT")
}

// DefaultGuestFusedPath returns the default path to guest-fused binary.
func DefaultGuestFusedPath() string {
	return findGuestBinary("guest-fused", "MATCHLOCK_GUEST_FUSED")
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
				return p
			}
		}
	}
	return filepath.Join(execDir, name)
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
