package acceptance

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// This file deliberately carries no `acceptance` build tag so the QEMU-backend
// gate is exercised by the ordinary unit suite (`mise run test`, i.e.
// `go test ./...`). The gate is a pure decision function; the QEMU-only
// acceptance tests that consume it live in acceptance-tagged files.

// qemuSystemBinaryName maps a GOARCH to the QEMU system emulator binary the QEMU
// backend requires, mirroring pkg/sandbox.qemuAvailable. It returns "" for
// architectures the QEMU backend does not support.
func qemuSystemBinaryName(goarch string) string {
	switch goarch {
	case "arm64", "aarch64":
		return "qemu-system-aarch64"
	case "amd64", "x86_64":
		return "qemu-system-x86_64"
	default:
		return ""
	}
}

// qemuBackendGate decides whether a QEMU-only acceptance test may run.
//
// QEMU-only tests derive a `qm-*` TAP name and assert on the QEMU TAP lifecycle,
// so they are meaningless (and fail) on the default Firecracker backend, which
// creates `fc-*` TAPs. They must therefore be gated on an explicit
// MATCHLOCK_BACKEND=qemu.
//
// run is false with a non-empty reason when:
//   - MATCHLOCK_BACKEND is not "qemu" (default Firecracker hosts must skip, not
//     fail on a `qm-` name that can never exist), or
//   - the arch needs a qemu-system-<arch> binary that is not in PATH (the test
//     skips with an explicit reason instead of failing a launch).
//
// lookPath is injectable so the decision is unit-testable without QEMU installed.
func qemuBackendGate(backendEnv, goarch string, lookPath func(string) (string, error)) (run bool, reason string) {
	if backendEnv != "qemu" {
		return false, fmt.Sprintf("QEMU-only test: requires MATCHLOCK_BACKEND=qemu (got %q); qm-* TAPs are created only by the QEMU backend", backendEnv)
	}
	bin := qemuSystemBinaryName(goarch)
	if bin == "" {
		return false, fmt.Sprintf("QEMU-only test: MATCHLOCK_BACKEND=qemu but no qemu-system binary is known for GOARCH %q", goarch)
	}
	if _, err := lookPath(bin); err != nil {
		return false, fmt.Sprintf("QEMU-only test: MATCHLOCK_BACKEND=qemu but %s is not in PATH: %v", bin, err)
	}
	return true, ""
}

func TestQEMUBackendGate(t *testing.T) {
	installed := func(string) (string, error) { return "/usr/bin/qemu-system-x86_64", nil }
	missing := func(string) (string, error) { return "", exec.ErrNotFound }

	cases := []struct {
		name       string
		backendEnv string
		goarch     string
		lookPath   func(string) (string, error)
		wantRun    bool
		wantReason string
	}{
		{"unset-backend-skips", "", "amd64", installed, false, "MATCHLOCK_BACKEND=qemu"},
		{"firecracker-backend-skips", "firecracker", "amd64", installed, false, "MATCHLOCK_BACKEND=qemu"},
		{"qemu-amd64-installed-runs", "qemu", "amd64", installed, true, ""},
		{"qemu-arm64-installed-runs", "qemu", "arm64", installed, true, ""},
		{"qemu-amd64-missing-binary-skips", "qemu", "amd64", missing, false, "qemu-system-x86_64"},
		{"qemu-arm64-missing-binary-skips", "qemu", "arm64", missing, false, "qemu-system-aarch64"},
		{"qemu-unsupported-arch-skips", "qemu", "riscv64", installed, false, "riscv64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run, reason := qemuBackendGate(tc.backendEnv, tc.goarch, tc.lookPath)
			if run != tc.wantRun {
				t.Fatalf("run = %v, want %v (reason %q)", run, tc.wantRun, reason)
			}
			if tc.wantRun {
				if reason != "" {
					t.Fatalf("expected empty reason when run=true, got %q", reason)
				}
				return
			}
			if !strings.Contains(reason, tc.wantReason) {
				t.Fatalf("reason %q does not contain %q", reason, tc.wantReason)
			}
		})
	}
}
