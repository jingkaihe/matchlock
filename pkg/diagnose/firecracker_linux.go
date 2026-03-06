//go:build linux

package diagnose

import (
	"os/exec"
	"strings"
)

func firecrackerVersion() string {
	out, err := exec.Command("firecracker", "--version").Output()
	if err != nil {
		return ""
	}
	parts := strings.Fields(string(out))
	if len(parts) >= 2 {
		return parts[1]
	}
	return strings.TrimSpace(string(out))
}
