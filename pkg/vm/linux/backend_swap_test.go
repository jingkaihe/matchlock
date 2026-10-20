//go:build linux

package linux

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jingkaihe/matchlock/pkg/vm"
)

// TestGenerateFirecrackerConfigSwapDisk verifies a Swap:true disk is announced
// via matchlock.swap= (never matchlock.disk.<dev>=) and still consumes a device
// letter so later disks keep correct names.
func TestGenerateFirecrackerConfigSwapDisk(t *testing.T) {
	m := &LinuxMachine{
		id: "vm-swap-cmdline",
		config: &vm.VMConfig{
			ID:         "vm-swap-cmdline",
			CPUs:       1,
			MemoryMB:   512,
			NoNetwork:  true,
			KernelPath: "/kernel",
			RootfsPath: "/rootfs",
			ExtraDisks: []vm.DiskConfig{
				{HostPath: "/data.ext4", GuestMount: "/mnt/data"},
				{HostPath: "/swap.raw", Swap: true},
				{HostPath: "/data2.ext4", GuestMount: "/mnt/data2"},
			},
		},
	}

	cfg := decodeFirecrackerConfigForTest(t, m.generateFirecrackerConfig())
	args := cfg.BootSource.BootArgs

	// vda is rootfs, so the first extra disk is vdb, the swap is vdc, and the
	// disk after it must advance to vdd.
	assert.Contains(t, args, " matchlock.disk.vdb=/mnt/data")
	assert.Contains(t, args, " matchlock.swap=vdc")
	assert.NotContains(t, args, "matchlock.disk.vdc=")
	assert.Contains(t, args, " matchlock.disk.vdd=/mnt/data2")
}

// TestCreateRejectsSwapWithCustomKernelArgs proves the Linux backend fails fast
// (before creating a TAP) when custom kernel args would drop matchlock.swap=.
func TestCreateRejectsSwapWithCustomKernelArgs(t *testing.T) {
	_, err := NewLinuxBackend().Create(context.Background(), &vm.VMConfig{
		ID:         "vm-swap-custom-args",
		CPUs:       1,
		MemoryMB:   512,
		NoNetwork:  true,
		KernelPath: "/nonexistent/kernel",
		RootfsPath: "/nonexistent/rootfs",
		KernelArgs: "console=ttyS0",
		ExtraDisks: []vm.DiskConfig{{HostPath: "/swap.raw", Swap: true}},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, vm.ErrSwapCustomKernelArgs)
}

// TestCreateAllowsSwapWithoutCustomKernelArgs ensures a swap disk alone (no
// custom KernelArgs) is not rejected by the guard.
func TestCreateAllowsSwapWithoutCustomKernelArgs(t *testing.T) {
	m, err := NewLinuxBackend().Create(context.Background(), &vm.VMConfig{
		ID:         "vm-swap-ok",
		CPUs:       1,
		MemoryMB:   512,
		NoNetwork:  true,
		KernelPath: "/nonexistent/kernel",
		RootfsPath: "/nonexistent/rootfs",
		ExtraDisks: []vm.DiskConfig{{HostPath: "/swap.raw", Swap: true}},
	})
	require.NoError(t, err)
	require.NotNil(t, m)
}
