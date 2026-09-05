//go:build linux

package qemu

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestGuestCPUFor pins the guest -cpu selection per architecture. arm64 must
// stay on cortex-a53: -cpu max intermittently corrupted guest runtime state
// under QEMU/TCG swap pressure (Go runtime consistency fatals, glibc heap
// corruption, SIGSEGV, vsock connection resets), while cortex-a53 measured
// 0/24 failures vs 15/36 for max in the admission-gated differential. amd64
// and any unknown arch must keep advertising "max" unchanged.
func TestGuestCPUFor(t *testing.T) {
	cases := map[string]string{
		"arm64":   "cortex-a53",
		"aarch64": "cortex-a53",
		"amd64":   "max",
		"x86_64":  "max",
		"riscv64": "max",
		"":        "max",
	}
	for arch, want := range cases {
		t.Run(arch, func(t *testing.T) {
			assert.Equal(t, want, guestCPUFor(arch))
		})
	}
}

// TestGuestCPUUsesHostArch guards the production delegation: guestCPU() must
// return the model selected for the host's own architecture.
func TestGuestCPUUsesHostArch(t *testing.T) {
	assert.Equal(t, guestCPUFor(runtimeGOARCH()), guestCPU())
}
