package image

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSquashPrefixCount(t *testing.T) {
	tests := []struct {
		total     int
		threshold int
		want      int
	}{
		{total: 1, threshold: 20, want: 0},
		{total: 20, threshold: 20, want: 0},
		{total: 21, threshold: 20, want: 2},
		{total: 30, threshold: 20, want: 11},
		{total: 5, threshold: 1, want: 5},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, squashPrefixCount(tc.total, tc.threshold))
	}
}

func TestNormalizeLayerFSType(t *testing.T) {
	fs, err := normalizeLayerFSType("")
	require.NoError(t, err)
	assert.Equal(t, layerFSTypeEROFS, fs)

	fs, err = normalizeLayerFSType("EROFS")
	require.NoError(t, err)
	assert.Equal(t, layerFSTypeEROFS, fs)

	_, err = normalizeLayerFSType("ext4")
	require.Error(t, err)

	_, err = normalizeLayerFSType("xfs")
	require.Error(t, err)
}

func TestDigestAndFSTypeFromBlobPath(t *testing.T) {
	d, fs, ok := digestAndFSTypeFromBlobPath("/tmp/sha256_abcd.erofs")
	require.True(t, ok)
	assert.Equal(t, "sha256:abcd", d)
	assert.Equal(t, layerFSTypeEROFS, fs)

	_, _, ok = digestAndFSTypeFromBlobPath("/tmp/sha256_abcd.ext4")
	assert.False(t, ok)
}
