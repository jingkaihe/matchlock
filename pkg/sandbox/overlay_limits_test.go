package sandbox

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateOverlayDiskLayout(t *testing.T) {
	require.NoError(t, validateOverlayDiskLayout(20, 2))

	err := validateOverlayDiskLayout(21, 0)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrOverlayLayerLimit)

	err = validateOverlayDiskLayout(20, 3) // 1 root + 20 lowers + 1 upper + 3 extra = 25
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrOverlayDiskLimit)
}

func TestNormalizeOverlayLowerFSTypes(t *testing.T) {
	got := normalizeOverlayLowerFSTypes([]string{"a", "b", "c"}, []string{"erofs"})
	assert.Equal(t, []string{"erofs", "erofs", "erofs"}, got)
}
