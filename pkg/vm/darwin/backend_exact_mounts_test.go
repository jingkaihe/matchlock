//go:build darwin

package darwin

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/jingkaihe/matchlock/pkg/vm"
)

// TestBuildKernelArgsExactMountsAllBranches pins that matchlock.exact.mounts=
// is emitted on every generated darwin command-line branch — dhcp, no-network
// and interception — matching the linux backend and the qemu machine. The
// interactive session line (and workflow runs) set vfs.exact_destinations=true
// with host-identical mounts; without this argument the guest never mounts
// those paths and the session child dies instantly with zero output
// (bead tamandua-03i). Formatting is byte-identical to the linux backend:
// a space-prefixed `matchlock.exact.mounts=` with comma-joined guest paths.
func TestBuildKernelArgsExactMountsAllBranches(t *testing.T) {
	exact := []string{"/home/me/work", "/home/me/.config"}
	want := " matchlock.exact.mounts=/home/me/work,/home/me/.config"

	branches := map[string]func(*vm.VMConfig){
		"dhcp": func(c *vm.VMConfig) {},
		"no-network": func(c *vm.VMConfig) {
			c.NoNetwork = true
		},
		"interception": func(c *vm.VMConfig) {
			c.UseInterception = true
		},
	}

	for name, mutate := range branches {
		t.Run(name, func(t *testing.T) {
			cfg := &vm.VMConfig{ID: "vm-exact-" + name, CPUs: 1, MemoryMB: 512, ExactMounts: exact}
			mutate(cfg)
			args := NewDarwinBackend().buildKernelArgs(cfg)
			assert.Contains(t, args, want)
		})
	}
}

// TestBuildKernelArgsExactMountsSinglePath proves the single-element form (the
// common interactive case: one exact path) is emitted without a separator.
func TestBuildKernelArgsExactMountsSinglePath(t *testing.T) {
	for _, cfg := range []*vm.VMConfig{
		{ID: "vm-one", CPUs: 1, MemoryMB: 512, ExactMounts: []string{"/srv/project"}},
		{ID: "vm-one-nonet", CPUs: 1, MemoryMB: 512, ExactMounts: []string{"/srv/project"}, NoNetwork: true},
		{ID: "vm-one-interception", CPUs: 1, MemoryMB: 512, ExactMounts: []string{"/srv/project"}, UseInterception: true},
	} {
		args := NewDarwinBackend().buildKernelArgs(cfg)
		assert.Contains(t, args, " matchlock.exact.mounts=/srv/project")
	}
}

// TestBuildKernelArgsNoExactMountsOmitsArg proves the argument is absent on
// every branch when the config carries no exact mounts (an empty list must not
// produce a bare matchlock.exact.mounts=).
func TestBuildKernelArgsNoExactMountsOmitsArg(t *testing.T) {
	cfgs := map[string]*vm.VMConfig{
		"dhcp":         {ID: "vm-plain", CPUs: 1, MemoryMB: 512},
		"no-network":   {ID: "vm-plain-nonet", CPUs: 1, MemoryMB: 512, NoNetwork: true},
		"interception": {ID: "vm-plain-interception", CPUs: 1, MemoryMB: 512, UseInterception: true},
		"empty-list":   {ID: "vm-plain-empty", CPUs: 1, MemoryMB: 512, ExactMounts: []string{}},
	}
	for name, cfg := range cfgs {
		t.Run(name, func(t *testing.T) {
			args := NewDarwinBackend().buildKernelArgs(cfg)
			assert.NotContains(t, args, "matchlock.exact.mounts")
		})
	}
}

// TestBuildKernelArgsCustomKernelArgsPrecedence pins the precedence rule: a
// custom KernelArgs string is returned verbatim and EVERY generated matchlock.*
// argument (workspace, exact mounts, disks, cpus, dns, hostname) is bypassed.
func TestBuildKernelArgsCustomKernelArgsPrecedence(t *testing.T) {
	custom := "console=hvc0 root=/dev/vda rw init=/init custom=1"
	cfg := &vm.VMConfig{
		ID:          "vm-custom",
		CPUs:        1,
		MemoryMB:    512,
		KernelArgs:  custom,
		Workspace:   "/workspace",
		ExactMounts: []string{"/work", "/cfg"},
		NoNetwork:   true,
	}
	got := NewDarwinBackend().buildKernelArgs(cfg)
	assert.Equal(t, custom, got)
	assert.NotContains(t, got, "matchlock.")
}

// TestBuildKernelArgsWorkspaceAndExactCoexist pins that the workspace argument
// and the exact-mounts argument are both present when a config carries both
// (they are independent matchlock.* parameters).
func TestBuildKernelArgsWorkspaceAndExactCoexist(t *testing.T) {
	cfg := &vm.VMConfig{
		ID:          "vm-both",
		CPUs:        1,
		MemoryMB:    512,
		Workspace:   "/workspace",
		ExactMounts: []string{"/work"},
	}
	args := NewDarwinBackend().buildKernelArgs(cfg)
	assert.Contains(t, args, " matchlock.workspace=/workspace")
	assert.Contains(t, args, " matchlock.exact.mounts=/work")
}
