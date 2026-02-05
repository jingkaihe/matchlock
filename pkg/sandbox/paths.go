package sandbox

import (
	"os"
	"path/filepath"
)

// DefaultKernelPath returns the default path to the kernel image.
func DefaultKernelPath() string {
	home, _ := os.UserHomeDir()
	sudoHome := ""
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" && os.Getuid() == 0 {
		sudoHome = filepath.Join("/home", sudoUser)
	}

	paths := []string{
		os.Getenv("MATCHLOCK_KERNEL"),
		filepath.Join(home, ".cache/matchlock/kernel"),
	}
	if sudoHome != "" {
		paths = append(paths, filepath.Join(sudoHome, ".cache/matchlock/kernel"))
	}

	for _, p := range paths {
		if p != "" {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return filepath.Join(home, ".cache/matchlock/kernel")
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
	// Return empty if not found - initramfs is optional
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

	// Get executable directory for relative paths
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
