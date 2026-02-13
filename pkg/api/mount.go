package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ParseVolumeMount parses a volume mount string in format "host:guest" or "host:guest:ro".
// Guest paths are resolved within workspace; absolute guest paths must already be under workspace.
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

	cleanWorkspace := filepath.Clean(workspace)

	// Guest path handling:
	// - Relative guest paths are resolved from workspace
	// - Absolute guest paths must already be within workspace
	if !filepath.IsAbs(guestPath) {
		guestPath = filepath.Join(cleanWorkspace, guestPath)
	} else {
		guestPath = filepath.Clean(guestPath)
	}

	if err := ValidateGuestPathWithinWorkspace(guestPath, cleanWorkspace); err != nil {
		return "", "", false, err
	}

	return hostPath, guestPath, readonly, nil
}

// ValidateGuestPathWithinWorkspace checks that guestPath is absolute and inside workspace.
func ValidateGuestPathWithinWorkspace(guestPath string, workspace string) error {
	cleanGuestPath := filepath.Clean(guestPath)
	cleanWorkspace := filepath.Clean(workspace)

	if !filepath.IsAbs(cleanGuestPath) {
		return fmt.Errorf("guest path %q must be absolute", guestPath)
	}
	if !isWithinWorkspace(cleanGuestPath, cleanWorkspace) {
		return fmt.Errorf("guest path %q must be within workspace %q", cleanGuestPath, cleanWorkspace)
	}
	return nil
}

// ValidateVFSMountsWithinWorkspace checks that all VFS mount paths are valid
// guest paths under the configured workspace.
func ValidateVFSMountsWithinWorkspace(mounts map[string]MountConfig, workspace string) error {
	for guestPath := range mounts {
		if err := ValidateGuestPathWithinWorkspace(guestPath, workspace); err != nil {
			return err
		}
	}
	return nil
}

func isWithinWorkspace(path string, workspace string) bool {
	path = filepath.Clean(path)
	workspace = filepath.Clean(workspace)
	if workspace == "/" {
		return filepath.IsAbs(path)
	}
	return path == workspace || strings.HasPrefix(path, workspace+"/")
}
