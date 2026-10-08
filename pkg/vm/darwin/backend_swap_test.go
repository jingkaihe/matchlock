//go:build darwin

package darwin

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jingkaihe/matchlock/pkg/vm"
)

// TestBuildKernelArgsSwapDisk verifies a Swap:true disk is announced via
// matchlock.swap= (never matchlock.disk.<dev>=) and still consumes a device
// letter so later disks keep correct names. vda is the rootfs, so the first
// extra disk is vdb, the swap is vdc, and the disk after it advances to vdd.
func TestBuildKernelArgsSwapDisk(t *testing.T) {
	args := NewDarwinBackend().buildKernelArgs(&vm.VMConfig{
		ID:        "vm-swap-cmdline",
		CPUs:      1,
		MemoryMB:  512,
		NoNetwork: true,
		ExtraDisks: []vm.DiskConfig{
			{HostPath: "/data.ext4", GuestMount: "/mnt/data"},
			{HostPath: "/swap.raw", Swap: true},
			{HostPath: "/data2.ext4", GuestMount: "/mnt/data2"},
		},
	})

	assert.Contains(t, args, " matchlock.disk.vdb=/mnt/data")
	assert.Contains(t, args, " matchlock.swap=vdc")
	assert.NotContains(t, args, "matchlock.disk.vdc=")
	assert.Contains(t, args, " matchlock.disk.vdd=/mnt/data2")
}

// TestBuildKernelArgsSwapDiskAfterOverlay proves the device letter counter
// carries across the overlay lower/upper devices: with one lower and an upper
// (vdb, vdc), the first extra disk is vdd, swap is vde and the following disk
// is vdf.
func TestBuildKernelArgsSwapDiskAfterOverlay(t *testing.T) {
	args := NewDarwinBackend().buildKernelArgs(&vm.VMConfig{
		ID:                  "vm-swap-overlay",
		CPUs:                1,
		MemoryMB:            512,
		NoNetwork:           true,
		OverlayEnabled:      true,
		OverlayLowerPaths:   []string{"/lower.erofs"},
		OverlayUpperPath:    "/upper.ext4",
		OverlayLowerFSTypes: []string{"erofs"},
		ExtraDisks: []vm.DiskConfig{
			{HostPath: "/data.ext4", GuestMount: "/mnt/data"},
			{HostPath: "/swap.raw", Swap: true},
			{HostPath: "/data2.ext4", GuestMount: "/mnt/data2"},
		},
	})

	assert.Contains(t, args, " matchlock.disk.vdd=/mnt/data")
	assert.Contains(t, args, " matchlock.swap=vde")
	assert.NotContains(t, args, "matchlock.disk.vde=")
	assert.Contains(t, args, " matchlock.disk.vdf=/mnt/data2")
}

// TestBuildKernelArgsCustomKernelArgsVerbatim documents why Create must guard
// the custom-args path: buildKernelArgs returns caller-provided KernelArgs
// unchanged, dropping every generated matchlock.* argument.
func TestBuildKernelArgsCustomKernelArgsVerbatim(t *testing.T) {
	args := NewDarwinBackend().buildKernelArgs(&vm.VMConfig{
		ID:         "vm-custom-args",
		CPUs:       1,
		MemoryMB:   512,
		NoNetwork:  true,
		KernelArgs: "console=hvc0 root=/dev/vda rw",
		ExtraDisks: []vm.DiskConfig{{HostPath: "/swap.raw", Swap: true}},
	})

	assert.Equal(t, "console=hvc0 root=/dev/vda rw", args)
	assert.NotContains(t, args, "matchlock.swap=")
}

// TestDarwinCreateRejectsSwapWithCustomKernelArgs proves the darwin backend
// fails fast when custom kernel args would silently drop matchlock.swap=
// instead of attaching a swap device that is never enabled.
func TestDarwinCreateRejectsSwapWithCustomKernelArgs(t *testing.T) {
	_, err := NewDarwinBackend().Create(context.Background(), &vm.VMConfig{
		ID:         "vm-swap-custom-args",
		CPUs:       1,
		MemoryMB:   512,
		NoNetwork:  true,
		KernelPath: "/nonexistent/kernel",
		RootfsPath: "/nonexistent/rootfs",
		KernelArgs: "console=hvc0",
		ExtraDisks: []vm.DiskConfig{{HostPath: "/swap.raw", Swap: true}},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, vm.ErrSwapCustomKernelArgs)
}
