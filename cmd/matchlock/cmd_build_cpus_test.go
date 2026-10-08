package main

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jingkaihe/matchlock/pkg/api"
)

func TestResolveBuildCPUsDefaultCapsAtFirecrackerMax(t *testing.T) {
	// 80-CPU host, flag at its default (0 = all available) must resolve to 32.
	got, err := resolveBuildCPUs(false, 0, 80)
	require.NoError(t, err)
	assert.Equal(t, float64(api.MaxFirecrackerVCPUs), got)
}

func TestResolveBuildCPUsDefaultOnSmallHostUsesHostCPUs(t *testing.T) {
	got, err := resolveBuildCPUs(false, 0, 8)
	require.NoError(t, err)
	assert.Equal(t, float64(8), got)
}

func TestResolveBuildCPUsExplicitZeroMeansAllAvailableCapped(t *testing.T) {
	// An explicit `--build-cpus 0` keeps the documented "all available"
	// semantics; only the effective count is capped.
	got, err := resolveBuildCPUs(true, 0, 80)
	require.NoError(t, err)
	assert.Equal(t, float64(api.MaxFirecrackerVCPUs), got)
}

func TestResolveBuildCPUsExplicitWithinLimits(t *testing.T) {
	got, err := resolveBuildCPUs(true, 16, 80)
	require.NoError(t, err)
	assert.Equal(t, float64(16), got)
}

func TestResolveBuildCPUsExplicitOvershootFailsNamingLimit(t *testing.T) {
	for _, cpus := range []float64{api.MaxFirecrackerVCPUs + 1, 80, 33.5} {
		t.Run(fmt.Sprintf("%g", cpus), func(t *testing.T) {
			got, err := resolveBuildCPUs(true, cpus, 80)
			require.Error(t, err)
			assert.Zero(t, got)
			assert.Contains(t, err.Error(), fmt.Sprintf("%d", api.MaxFirecrackerVCPUs))
			assert.Contains(t, err.Error(), "Firecracker")
		})
	}
}

func TestResolveBuildCPUsExplicitAboveHostCPUsStillErrors(t *testing.T) {
	_, err := resolveBuildCPUs(true, 16, 8)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "host cpus")
}

func TestResolveBuildCPUsRejectsInvalidCount(t *testing.T) {
	_, err := resolveBuildCPUs(true, -1, 80)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "finite number > 0")
}
