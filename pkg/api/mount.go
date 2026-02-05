package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ParseVolumeMount parses a volume mount string in format "host:guest" or "host:guest:ro".
// Guest paths are relative to the workspace unless they start with the workspace path.
func ParseVolumeMount(vol string, workspace string) (hostPath, guestPath string, readonly bool, err error) {
	parts := strings.Split(vol, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return "", "", false, fmt.Errorf("expected format host:guest or host:guest:ro")
	}

	hostPath = parts[0]
	guestPath = parts[1]

	// Resolve to absolute path
	if !filepath.IsAbs(hostPath) {
		hostPath, err = filepath.Abs(hostPath)
		if err != nil {
			return "", "", false, fmt.Errorf("failed to resolve path: %w", err)
		}
	}

	// Verify host path exists
	if _, err := os.Stat(hostPath); err != nil {
		return "", "", false, fmt.Errorf("host path does not exist: %s", hostPath)
	}

	// Check for readonly flag
	if len(parts) == 3 {
		if parts[2] == "ro" || parts[2] == "readonly" {
			readonly = true
		} else {
			return "", "", false, fmt.Errorf("unknown option %q (use 'ro' for readonly)", parts[2])
		}
	}

	// Guest path handling:
	// - If path starts with workspace, use as-is
	// - If path is absolute but not under workspace, prefix with workspace
	// - If path is relative, make it relative to workspace
	if !filepath.IsAbs(guestPath) {
		guestPath = filepath.Join(workspace, guestPath)
	} else if !strings.HasPrefix(guestPath, workspace) {
		guestPath = filepath.Join(workspace, guestPath)
	}

	return hostPath, guestPath, readonly, nil
}
