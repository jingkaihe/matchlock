//go:build linux

package firecracker

import (
	"os"
	"os/exec"
	"path/filepath"
)

const (
	defaultLibexecDir = "/usr/libexec/matchlock"
	envFirecracker    = "MATCHLOCK_FIRECRACKER"
	envJailer         = "MATCHLOCK_JAILER"
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

	packaged := filepath.Join(defaultLibexecDir, name)
	if _, err := os.Stat(packaged); err == nil {
		return packaged
	}

	if path, err := exec.LookPath(name); err == nil {
		return path
	}

	return name
}
