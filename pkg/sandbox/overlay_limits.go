package sandbox

import (
	"github.com/jingkaihe/matchlock/internal/errx"
)

const (
	// Firecracker supports up to 24 block devices. Keep the same budget for
	// cross-platform parity.
	maxOverlayBlockDevices = 24
	// Root disk + upper disk + up to 20 lower disks fits within 24 even with
	// a couple of extra user-provided disks.
	maxOverlayLowerLayers = 20
)

func validateOverlayDiskLayout(lowerCount, extraCount int) error {
	if lowerCount > maxOverlayLowerLayers {
		return errx.With(ErrOverlayLayerLimit, ": lowers=%d max=%d", lowerCount, maxOverlayLowerLayers)
	}
	// root(vda) + lowers + upper + extra
	total := 1 + lowerCount + 1 + extraCount
	if total > maxOverlayBlockDevices {
		return errx.With(ErrOverlayDiskLimit, ": total=%d max=%d (root+lowers+upper+extra)", total, maxOverlayBlockDevices)
	}
	return nil
}

func normalizeOverlayLowerFSTypes(lowerPaths, fsTypes []string) []string {
	out := make([]string, len(lowerPaths))
	for i := range lowerPaths {
		if i < len(fsTypes) && fsTypes[i] != "" {
			out[i] = fsTypes[i]
			continue
		}
		out[i] = "erofs"
	}
	return out
}
