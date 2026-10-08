//go:build linux

package qemu

import (
	"strings"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/vm"
)

// TestBootArgsSwapDisk verifies a Swap:true disk is announced via
// matchlock.swap= (never matchlock.disk.<dev>=) and still advances the device
// letter for later disks.
func TestBootArgsSwapDisk(t *testing.T) {
	m := &Machine{config: &vm.VMConfig{
		ID: "vm-swap", DNSServers: []string{"8.8.8.8"}, NoNetwork: true, CPUs: 1,
		ExtraDisks: []vm.DiskConfig{
			{HostPath: "/data.ext4", GuestMount: "/mnt/data"},
			{HostPath: "/swap.raw", Swap: true},
			{HostPath: "/data2.ext4", GuestMount: "/mnt/data2"},
		},
	}}

	args := m.bootArgs()
	// vda is rootfs, so the first extra disk is vdb, the swap is vdc, and the
	// disk after it must advance to vdd.
	if !strings.Contains(args, " matchlock.disk.vdb=/mnt/data") {
		t.Fatalf("bootArgs missing the pre-swap disk:\n%s", args)
	}
	if !strings.Contains(args, " matchlock.swap=vdc") {
		t.Fatalf("bootArgs missing matchlock.swap=vdc:\n%s", args)
	}
	if strings.Contains(args, "matchlock.disk.vdc=") {
		t.Fatalf("bootArgs emitted a mount spec for the swap device:\n%s", args)
	}
	if !strings.Contains(args, " matchlock.disk.vdd=/mnt/data2") {
		t.Fatalf("bootArgs did not advance the device letter past swap:\n%s", args)
	}
}
