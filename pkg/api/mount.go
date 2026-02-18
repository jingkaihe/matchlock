package api

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/jingkaihe/matchlock/internal/errx"
)

// VolumeMountSpec is a parsed -v/--volume specification.
type VolumeMountSpec struct {
	HostPath  string
	GuestPath string
	Type      string
	Readonly  bool
}

// ParseVolumeMount parses a volume mount string in format:
// - "host:guest"
// - "host:guest:ro"
// - "host:guest:overlay"
// - "host:guest:host_fs"
//
// This is kept for backward compatibility with existing callers that only need
// host/guest/readonly. Use ParseVolumeMountSpec for mount type aware parsing.
//
// Guest paths are resolved within workspace; absolute guest paths must already be under workspace.
func ParseVolumeMount(vol string, workspace string) (hostPath, guestPath string, readonly bool, err error) {
	spec, err := ParseVolumeMountSpec(vol, workspace)
	if err != nil {
		return "", "", false, err
	}
	return spec.HostPath, spec.GuestPath, spec.Readonly, nil
}

// ParseVolumeMountSpec parses a volume mount string and returns a typed spec.
func ParseVolumeMountSpec(vol string, workspace string) (VolumeMountSpec, error) {
	parts := strings.Split(vol, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return VolumeMountSpec{}, ErrInvalidVolumeFormat
	}

	hostPath := parts[0]
	guestPath := parts[1]

	// Resolve to absolute path
	var err error
	if !filepath.IsAbs(hostPath) {
		hostPath, err = filepath.Abs(hostPath)
		if err != nil {
			return VolumeMountSpec{}, errx.Wrap(ErrResolvePath, err)
		}
	}

	// Verify host path exists
	if _, err := os.Stat(hostPath); err != nil {
		return VolumeMountSpec{}, errx.With(ErrHostPathNotExist, ": %s", hostPath)
	}

	// Default to overlay for safer snapshot-based isolation.
	mountType := MountTypeOverlay
	readonly := false

	// Parse optional mount option.
	if len(parts) == 3 {
		switch strings.ToLower(strings.TrimSpace(parts[2])) {
		case MountOptionReadonlyShort, MountOptionReadonly:
			// Keep explicit read-only behavior as a host mount.
			mountType = MountTypeHostFS
			readonly = true
		case MountTypeOverlay:
			mountType = MountTypeOverlay
		case MountTypeHostFS:
			mountType = MountTypeHostFS
		default:
			return VolumeMountSpec{}, errx.With(ErrUnknownMountOption, " %q (use '%s', '%s', or '%s')", parts[2], MountOptionReadonlyShort, MountTypeOverlay, MountTypeHostFS)
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
		return VolumeMountSpec{}, err
	}

	return VolumeMountSpec{
		HostPath:  hostPath,
		GuestPath: guestPath,
		Type:      mountType,
		Readonly:  readonly,
	}, nil
}

// ValidateGuestPathWithinWorkspace checks that guestPath is absolute and inside workspace.
func ValidateGuestPathWithinWorkspace(guestPath string, workspace string) error {
	cleanGuestPath := filepath.Clean(guestPath)
	cleanWorkspace := filepath.Clean(workspace)

	if !filepath.IsAbs(cleanGuestPath) {
		return errx.With(ErrGuestPathNotAbs, ": %q", guestPath)
	}
	if !isWithinWorkspace(cleanGuestPath, cleanWorkspace) {
		return errx.With(ErrGuestPathOutside, ": %q not in %q", cleanGuestPath, cleanWorkspace)
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
