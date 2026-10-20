package api

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/jingkaihe/matchlock/internal/errx"
)

// VolumeMountSpec is a parsed -v/--volume specification.
type VolumeMountSpec struct {
	HostPath  string
	GuestPath string
	Type      string
	Readonly  bool
	OwnerUID  *uint32
	OwnerGID  *uint32
}

// ParseVolumeMount parses a volume mount string in format:
// - "host:guest"
// - "host:guest:ro"
// - "host:guest:overlay"
// - "host:guest:host_fs"
// - "host:guest:host_fs,uid=1000,gid=1000"
// - "host:guest:host_fs,owner=1000:1000"
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
	parts := strings.SplitN(vol, ":", 3)
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
	var ownerUID *uint32
	var ownerGID *uint32

	// Parse optional comma-separated mount options.
	if len(parts) == 3 {
		for _, rawOption := range strings.Split(parts[2], ",") {
			option := strings.TrimSpace(rawOption)
			key, value, hasValue := strings.Cut(option, "=")
			key = strings.ToLower(strings.TrimSpace(key))
			value = strings.TrimSpace(value)

			switch key {
			case MountOptionReadonlyShort, MountOptionReadonly:
				if hasValue {
					return VolumeMountSpec{}, unknownVolumeMountOption(option)
				}
				// Keep explicit read-only behavior as a host mount.
				mountType = MountTypeHostFS
				readonly = true
			case MountTypeOverlay:
				if hasValue {
					return VolumeMountSpec{}, unknownVolumeMountOption(option)
				}
				mountType = MountTypeOverlay
			case MountTypeHostFS:
				if hasValue {
					return VolumeMountSpec{}, unknownVolumeMountOption(option)
				}
				mountType = MountTypeHostFS
			case "uid":
				if !hasValue {
					return VolumeMountSpec{}, unknownVolumeMountOption(option)
				}
				uid, err := parseMountOwnerID("uid", value)
				if err != nil {
					return VolumeMountSpec{}, err
				}
				ownerUID = &uid
			case "gid":
				if !hasValue {
					return VolumeMountSpec{}, unknownVolumeMountOption(option)
				}
				gid, err := parseMountOwnerID("gid", value)
				if err != nil {
					return VolumeMountSpec{}, err
				}
				ownerGID = &gid
			case "owner":
				if !hasValue {
					return VolumeMountSpec{}, unknownVolumeMountOption(option)
				}
				uidValue, gidValue, ok := strings.Cut(value, ":")
				if !ok {
					return VolumeMountSpec{}, errx.With(ErrInvalidMountOwner, ": owner must be UID:GID")
				}
				uid, err := parseMountOwnerID("uid", uidValue)
				if err != nil {
					return VolumeMountSpec{}, err
				}
				gid, err := parseMountOwnerID("gid", gidValue)
				if err != nil {
					return VolumeMountSpec{}, err
				}
				ownerUID = &uid
				ownerGID = &gid
			default:
				return VolumeMountSpec{}, unknownVolumeMountOption(option)
			}
		}
	}
	if (ownerUID != nil || ownerGID != nil) && mountType != MountTypeHostFS {
		return VolumeMountSpec{}, errx.With(ErrInvalidMountOwner, ": uid/gid owner options are only supported for %s mounts", MountTypeHostFS)
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
		OwnerUID:  ownerUID,
		OwnerGID:  ownerGID,
	}, nil
}

func unknownVolumeMountOption(option string) error {
	return errx.With(ErrUnknownMountOption, " %q (use '%s', '%s', '%s', '%s', 'uid=UID', 'gid=GID', or 'owner=UID:GID')", option, MountOptionReadonlyShort, MountOptionReadonly, MountTypeOverlay, MountTypeHostFS)
}

func parseMountOwnerID(name string, value string) (uint32, error) {
	if value == "" {
		return 0, errx.With(ErrInvalidMountOwner, ": %s cannot be empty", name)
	}
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0, errx.With(ErrInvalidMountOwner, ": %s %q must be an unsigned 32-bit integer: %w", name, value, err)
	}
	return uint32(parsed), nil
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

// prohibitedExactDestinations are guest paths that must never be shadowed by a
// host mount when exact destinations are enabled. Shadowing the guest root or
// a core system directory would replace part of the guest OS, which is
// explicitly disallowed. /opt/matchlock is the trusted guest runtime where the
// sandbox injects guest-init/guest-agent/guest-fused; mounting a host tree over
// it or a subpath would corrupt the trusted runtime. Lower-level directories
// such as /home, /root, /opt, /workspace and /opt/matchlock's siblings (for
// example /opt/project) remain valid exact destinations.
var prohibitedExactDestinations = []string{
	"/",
	"/etc",
	"/usr",
	"/bin",
	"/sbin",
	"/lib",
	"/lib64",
	"/boot",
	"/dev",
	"/proc",
	"/sys",
	"/run",
	"/opt/matchlock",
}

// ValidateExactDestinationMount validates a guest mount destination used when
// exact absolute work/repository paths are admitted. It rejects non-absolute
// or unsafe paths and any destination that equals or is a descendant of a
// prohibited guest OS root, so a mount cannot shadow the guest operating
// system.
func ValidateExactDestinationMount(guestPath string) error {
	clean := filepath.Clean(guestPath)
	if !filepath.IsAbs(clean) {
		return errx.With(ErrGuestPathNotAbs, ": %q", guestPath)
	}
	// Reject the guest OS root and core system directories before the path-shape
	// check, so these produce the unambiguous prohibited-destination error.
	for _, prohibited := range prohibitedExactDestinations {
		if clean == prohibited {
			return errx.With(ErrProhibitedDestination, ": %q shadows guest %s", clean, prohibited)
		}
		if strings.HasPrefix(clean, prohibited+"/") {
			return errx.With(ErrProhibitedDestination, ": %q is within guest %s", clean, prohibited)
		}
	}
	if err := ValidateGuestMount(clean); err != nil {
		return errx.With(ErrInvalidConfig, ": %v", err)
	}
	return nil
}

// ValidateExactDestinationMounts validates every mount destination for
// exact-destination mode. In addition to the per-destination checks performed
// by ValidateExactDestinationMount, it rejects two conditions that only the
// full mount set can reveal:
//   - a mount destination nested inside another (a collision), which would make
//     the delivered guest paths overlap and ambiguous;
//   - a host_fs source that is itself a symlink (an ambiguous source that
//     should be given as a real directory or file instead of a redirect).
func ValidateExactDestinationMounts(mounts map[string]MountConfig) error {
	if len(mounts) == 0 {
		return nil
	}

	destinations := make([]string, 0, len(mounts))
	for guestPath := range mounts {
		destinations = append(destinations, guestPath)
	}
	// Validate each destination independently, then check for set-level issues.
	// Sorting by length descending makes the collision scan below deterministic.
	sort.Slice(destinations, func(i, j int) bool {
		return len(destinations[i]) > len(destinations[j])
	})
	for _, guestPath := range destinations {
		if err := ValidateExactDestinationMount(guestPath); err != nil {
			return err
		}
	}

	// Collision: a destination nested inside another destination is unsafe in
	// exact-destination mode because the two mounts would overlap and the guest
	// path is ambiguous (for example /opt/project and /opt/project/sub).
	for i := range destinations {
		a := destinations[i]
		for j := range destinations {
			if i == j {
				continue
			}
			if strings.HasPrefix(a, destinations[j]+"/") {
				return errx.With(ErrInvalidConfig, ": mount destination %q collides with (is nested inside) %q", a, destinations[j])
			}
		}
	}

	// Source symlink: a host_fs host_path that is a symlink is ambiguous and
	// redirectable; require a real directory or file.
	for guestPath, mount := range mounts {
		if err := validateNoSourceSymlink(guestPath, mount); err != nil {
			return err
		}
	}
	return nil
}

// validateNoSourceSymlink rejects a host_fs source whose path is a symlink.
// The source should name a real directory or file; a symlink is ambiguous and
// preferred to be resolved to its target before admission.
func validateNoSourceSymlink(guestPath string, mount MountConfig) error {
	if mount.Type != MountTypeHostFS || mount.HostPath == "" {
		return nil
	}
	fi, err := os.Lstat(mount.HostPath)
	if err != nil {
		// Absent host source cannot be a symlink; its existence is enforced
		// elsewhere when the provider is created.
		return nil
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return errx.With(ErrInvalidConfig, ": %s: host_path %q is a symlink; a host_fs source must be a real directory or file", guestPath, mount.HostPath)
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

// ValidateVFSMountOwnership checks ownership override invariants for VFS mounts.
func ValidateVFSMountOwnership(mounts map[string]MountConfig) error {
	for guestPath, mount := range mounts {
		if (mount.OwnerUID != nil || mount.OwnerGID != nil) && mount.Type != MountTypeHostFS {
			return errx.With(ErrInvalidConfig, ": %s: owner_uid/owner_gid are only supported for host_fs mounts", guestPath)
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
