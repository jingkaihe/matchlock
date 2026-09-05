//go:build linux

package qemu

import (
	"strings"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/vm"
)

func TestNextLetter(t *testing.T) {
	cases := map[string]string{"a": "b", "b": "c", "z": "{"}
	for in, want := range cases {
		if got := nextLetter(in); got != want {
			t.Fatalf("nextLetter(%q)=%q want %q", in, got, want)
		}
	}
}

func TestBootArgsNoOverlay(t *testing.T) {
	m := &Machine{config: &vm.VMConfig{
		ID: "vm-1", Hostname: "h", DNSServers: []string{"1.1.1.1"},
		NoNetwork: true, CPUs: 1, MTU: 1500,
	}}
	args := m.bootArgs()
	for _, want := range []string{
		"console=" + consoleDevice(), "root=/dev/vda rw", "init=/init", "ip=off",
		"matchlock.no_network=1", "hostname=h", "matchlock.dns=1.1.1.1",
		"matchlock.mtu=1500", "matchlock.cpus=1",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("bootArgs missing %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "matchlock.overlay") {
		t.Fatalf("bootArgs unexpectedly has overlay args:\n%s", args)
	}
}

func TestBootArgsOverlayDiskNames(t *testing.T) {
	m := &Machine{config: &vm.VMConfig{
		ID: "vm-2", DNSServers: []string{"8.8.8.8"}, NoNetwork: true, CPUs: 2,
		OverlayEnabled:      true,
		OverlayLowerPaths:   []string{"low1", "low2"},
		OverlayLowerFSTypes: []string{"erofs", "erofs"},
		OverlayUpperPath:    "up", ExtraDisks: []vm.DiskConfig{{GuestMount: "/mnt/x"}},
	}}
	args := m.bootArgs()
	// The vda root is followed by lowers vdb, vdc, upper vdd, then extra vde.
	if !strings.Contains(args, "matchlock.overlay.lower=vdb,vdc") {
		t.Fatalf("lower device names wrong:\n%s", args)
	}
	if !strings.Contains(args, "matchlock.overlay.upper=vdd") {
		t.Fatalf("upper device name wrong:\n%s", args)
	}
	if !strings.Contains(args, "matchlock.disk.vde=/mnt/x") {
		t.Fatalf("extra disk device name wrong:\n%s", args)
	}
}

func TestDiskArgsOrdering(t *testing.T) {
	disks := []diskMount{{HostPath: "/root"}, {HostPath: "/low", ReadOnly: true}, {HostPath: "/up"}}
	got := (&Machine{}).diskArgs(disks)
	joined := strings.Join(got, "\n")
	for _, want := range []string{
		"id=disk0", "file=/root", "format=raw",
		"id=disk1", "file=/low", "readonly=on",
		"id=disk2", "file=/up",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("diskArgs missing %q:\n%s", want, joined)
		}
	}
	// Object IDs must be QEMU identifiers: no '/'.
	if strings.Contains(joined, "/dev/vd") {
		t.Fatalf("diskArgs used a /dev/vd path as an id:\n%s", joined)
	}
}

func TestEffectiveMemVCPUs(t *testing.T) {
	if effectiveMem(0) != 512 || effectiveMem(1024) != 1024 {
		t.Fatalf("effectiveMem wrong")
	}
	if effectiveVCPUs(1) != 1 || effectiveVCPUs(2.5) != 3 || effectiveVCPUs(0.4) != 1 {
		t.Fatalf("effectiveVCPUs wrong")
	}
}
