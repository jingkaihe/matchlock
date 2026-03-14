//go:build linux

package diagnose

import (
	"os/exec"
	"strings"

	"github.com/jingkaihe/matchlock/pkg/firecracker"
)

func firecrackerVersion() string {
	out, err := exec.Command(firecracker.ResolveFirecrackerPath(), "--version").Output()
	if err != nil {
		return ""
	}
	parts := strings.Fields(string(out))
	if len(parts) >= 2 {
		return parts[1]
	}
	return strings.TrimSpace(string(out))
}
