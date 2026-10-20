package sandbox

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateOverlayDiskLayout(t *testing.T) {
	require.NoError(t, validateOverlayDiskLayout(20, 2, false))

	err := validateOverlayDiskLayout(21, 0, false)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrOverlayLayerLimit)

	err = validateOverlayDiskLayout(20, 3, false) // 1 root + 20 lowers + 1 upper + 3 extra = 25
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrOverlayDiskLimit)
}

func TestValidateOverlayDiskLayoutCountsSwap(t *testing.T) {
	// 1 root + 20 lowers + 1 upper + 1 extra + swap = 24: exactly at the cap.
	require.NoError(t, validateOverlayDiskLayout(20, 1, true))

	// The same layout without swap is also within the cap, but adding swap to a
	// layout that already fills it must be rejected.
	require.NoError(t, validateOverlayDiskLayout(20, 2, false))
	err := validateOverlayDiskLayout(20, 2, true) // total 25
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrOverlayDiskLimit)

	// A layout that is overlarge regardless of swap still reports the layer
	// limit before the device-limit check.
	err = validateOverlayDiskLayout(21, 0, true)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrOverlayLayerLimit)
}

func TestNormalizeOverlayLowerFSTypes(t *testing.T) {
	got := normalizeOverlayLowerFSTypes([]string{"a", "b", "c"}, []string{"erofs"})
	assert.Equal(t, []string{"erofs", "erofs", "erofs"}, got)
}
