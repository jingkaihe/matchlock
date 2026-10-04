//go:build linux

package firecracker

import (
	"os"
	"os/exec"
	"path/filepath"
)

// PackagedDir holds the Firecracker and jailer binaries shipped by the
// matchlock Linux packages.
const PackagedDir = "/usr/libexec/matchlock"

const (
	envFirecracker = "MATCHLOCK_FIRECRACKER"
	envJailer      = "MATCHLOCK_JAILER"
)

func ResolveFirecrackerPath() string {
	return resolveBinary("firecracker", envFirecracker)
}

func ResolveJailerPath() string {
	return resolveBinary("jailer", envJailer)
}

func resolveBinary(name, envVar string) string {
	if override := os.Getenv(envVar); override != "" {
		return override
	}

	packaged := filepath.Join(PackagedDir, name)
	if _, err := os.Stat(packaged); err == nil {
		return packaged
	}

	if path, err := exec.LookPath(name); err == nil {
		return path
	}

	return name
}
