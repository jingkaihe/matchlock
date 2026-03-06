//go:build linux

package diagnose

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHasCPUVirtualizationFlag(t *testing.T) {
	assert.True(t, hasCPUVirtualizationFlag([]byte("flags\t: fpu vmx sse")))
	assert.True(t, hasCPUVirtualizationFlag([]byte("flags\t: fpu svm sse")))
	assert.False(t, hasCPUVirtualizationFlag([]byte("flags\t: fpu sse sse2")))
}

func TestHasRequiredNetworkCapabilities(t *testing.T) {
	data := []byte{
		0x01, 0x00, 0x00, 0x00,
		0x00, 0x30, 0x00, 0x00,
		0x00, 0x30, 0x00, 0x00,
	}

	hasNetAdmin, hasNetRaw, err := hasRequiredNetworkCapabilities(data)
	require.NoError(t, err)
	assert.True(t, hasNetAdmin)
	assert.True(t, hasNetRaw)
}

func TestHasRequiredNetworkCapabilitiesRejectsShortData(t *testing.T) {
	_, _, err := hasRequiredNetworkCapabilities([]byte{0x01, 0x02})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too short")
}

func TestCapabilityBitSet(t *testing.T) {
	set := []byte{0x00, 0x30, 0x00, 0x00}
	assert.True(t, capabilityBitSet(set, 12))
	assert.True(t, capabilityBitSet(set, 13))
	assert.False(t, capabilityBitSet(set, 10))
}
