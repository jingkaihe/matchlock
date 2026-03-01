package api

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsValidCPUCount(t *testing.T) {
	assert.True(t, IsValidCPUCount(0.5))
	assert.True(t, IsValidCPUCount(1))
	assert.False(t, IsValidCPUCount(0))
	assert.False(t, IsValidCPUCount(-1))
	assert.False(t, IsValidCPUCount(math.NaN()))
	assert.False(t, IsValidCPUCount(math.Inf(1)))
	assert.False(t, IsValidCPUCount(math.Inf(-1)))
}

func TestVCPUCount(t *testing.T) {
	vcpus, ok := VCPUCount(0.5)
	assert.True(t, ok)
	assert.Equal(t, 1, vcpus)

	vcpus, ok = VCPUCount(1.01)
	assert.True(t, ok)
	assert.Equal(t, 2, vcpus)

	_, ok = VCPUCount(0)
	assert.False(t, ok)

	_, ok = VCPUCount(math.NaN())
	assert.False(t, ok)

	_, ok = VCPUCount(math.Inf(1))
	assert.False(t, ok)
}
