//go:build linux

package linux

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/vm"
)

func TestCreateRejectsVCPUsAboveFirecrackerMax(t *testing.T) {
	_, err := NewLinuxBackend().Create(context.Background(), &vm.VMConfig{
		ID:         "vm-cpu-cap-create",
		CPUs:       api.MaxFirecrackerVCPUs + 1,
		MemoryMB:   512,
		NoNetwork:  true,
		KernelPath: "/nonexistent/kernel",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidCPUCount)
	assert.Contains(t, err.Error(), fmt.Sprintf("%d", api.MaxFirecrackerVCPUs))
	assert.Contains(t, err.Error(), "Firecracker")
}

func TestGenerateFirecrackerConfigCapsVCPUsAndBootArg(t *testing.T) {
	// 80 requested on a large host: the config must clamp both the VMM
	// machine-config and the guest boot arg to the Firecracker maximum.
	m := &LinuxMachine{
		id: "vm-cpu-cap-gen",
		config: &vm.VMConfig{
			ID:         "vm-cpu-cap-gen",
			CPUs:       float64(api.MaxFirecrackerVCPUs + 48),
			MemoryMB:   1024,
			NoNetwork:  true,
			KernelPath: "/kernel",
			RootfsPath: "/rootfs",
		},
	}

	cfg := decodeFirecrackerConfigForTest(t, m.generateFirecrackerConfig())
	assert.Equal(t, api.MaxFirecrackerVCPUs, cfg.MachineConfig.VCPUCount)
	assert.Contains(t, cfg.BootSource.BootArgs, fmt.Sprintf("matchlock.cpus=%d", api.MaxFirecrackerVCPUs))
	assert.NotContains(t, cfg.BootSource.BootArgs, fmt.Sprintf("matchlock.cpus=%d", api.MaxFirecrackerVCPUs+48))
}

func TestGenerateFirecrackerConfigBootArgMatchesRoundedVCPUs(t *testing.T) {
	// A fractional request below the cap must round up consistently in the
	// machine-config and in matchlock.cpus=.
	m := &LinuxMachine{
		id: "vm-cpu-round",
		config: &vm.VMConfig{
			ID:         "vm-cpu-round",
			CPUs:       2.5,
			MemoryMB:   512,
			NoNetwork:  true,
			KernelPath: "/kernel",
			RootfsPath: "/rootfs",
		},
	}

	cfg := decodeFirecrackerConfigForTest(t, m.generateFirecrackerConfig())
	assert.Equal(t, 3, cfg.MachineConfig.VCPUCount)
	assert.Contains(t, cfg.BootSource.BootArgs, "matchlock.cpus=3")
}

func TestEffectiveVCPUs(t *testing.T) {
	assert.Equal(t, 1, effectiveVCPUs(0.4))
	assert.Equal(t, 2, effectiveVCPUs(1.01))
	assert.Equal(t, api.MaxFirecrackerVCPUs, effectiveVCPUs(float64(api.MaxFirecrackerVCPUs)))
	assert.Equal(t, api.MaxFirecrackerVCPUs, effectiveVCPUs(float64(api.MaxFirecrackerVCPUs+1)))
	assert.Equal(t, api.MaxFirecrackerVCPUs, effectiveVCPUs(80))
}

type fcConfigForTest struct {
	BootSource struct {
		BootArgs string `json:"boot_args"`
	} `json:"boot-source"`
	MachineConfig struct {
		VCPUCount int `json:"vcpu_count"`
	} `json:"machine-config"`
}

func decodeFirecrackerConfigForTest(t *testing.T, data []byte) fcConfigForTest {
	t.Helper()
	var cfg fcConfigForTest
	require.NoError(t, json.Unmarshal(data, &cfg))
	return cfg
}
