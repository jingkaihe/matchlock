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
