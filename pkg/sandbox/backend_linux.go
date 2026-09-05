//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/kvm"
	"github.com/jingkaihe/matchlock/pkg/vm"
	"github.com/jingkaihe/matchlock/pkg/vm/linux"
	"github.com/jingkaihe/matchlock/pkg/vm/qemu"
)

// backendKind identifies which VM backend a sandbox builds.
type backendKind int

const (
	// backendFirecracker is the KVM-accelerated Firecracker backend.
	backendFirecracker backendKind = iota
	// backendQEMU is the QEMU TCG backend, used as a fallback when KVM is
	// definitively unavailable.
	backendQEMU
)

func (k backendKind) String() string {
	switch k {
	case backendFirecracker:
		return "firecracker"
	case backendQEMU:
		return "qemu"
	default:
		return "unknown"
	}
}

// qemuPrereq reports whether the QEMU TCG fallback is actually usable.
type qemuPrereq func() (ok bool, reason string)

// selectBackendKind chooses the VM backend. It prefers the KVM-accelerated
// Firecracker backend and falls back to QEMU TCG only when KVM is definitively
// absent or unusable. Unexpected/resource failures are surfaced as errors and
// never disguised as missing hardware.
//
// MATCHLOCK_BACKEND honors an explicit user override: "firecracker" or "qemu".
// This is an escape hatch (e.g. to force the TCG fallback on a KVM host for
// testing or for environments where KVM acceleration is undesirable) and does
// not change the default Firecracker-first behavior when unset. An invalid
// value is an error, never silently ignored.
func selectBackendKind(res kvm.Result, qemu qemuPrereq) (backendKind, error) {
	if override := os.Getenv("MATCHLOCK_BACKEND"); override != "" {
		switch override {
		case "firecracker":
			return backendFirecracker, nil
		case "qemu":
			ok, reason := qemu()
			if !ok {
				return 0, fmt.Errorf("MATCHLOCK_BACKEND=qemu but qemu fallback unavailable: %s", reason)
			}
			return backendQEMU, nil
		default:
			return 0, fmt.Errorf("invalid MATCHLOCK_BACKEND %q (want \"firecracker\" or \"qemu\")", override)
		}
	}

	switch res.Status {
	case kvm.StatusAvailable:
		return backendFirecracker, nil
	case kvm.StatusNotPresent, kvm.StatusIncompatible, kvm.StatusUnusable, kvm.StatusPermissionDenied:
		ok, reason := qemu()
		if !ok {
			return 0, fmt.Errorf("kvm %s; qemu fallback unavailable: %s", res.Status, reason)
		}
		return backendQEMU, nil
	default:
		return 0, fmt.Errorf("kvm probe returned unexpected status; refusing to guess a backend: %s", res)
	}
}

// qemuAvailable reports whether the QEMU TCG fallback is actually usable on
// this host: the qemu-system binary for this arch must exist and /dev/vhost-vsock
// must be accessible to the calling user (refreshed kvm group membership).
func qemuAvailable() (bool, string) {
	switch runtime.GOARCH {
	case "arm64", "aarch64":
		if _, err := exec.LookPath("qemu-system-aarch64"); err != nil {
			return false, "qemu-system-aarch64 not in PATH"
		}
		if err := vhostVsockAccessible(); err != nil {
			return false, err.Error()
		}
		return true, ""
	case "amd64", "x86_64":
		if _, err := exec.LookPath("qemu-system-x86_64"); err != nil {
			return false, "qemu-system-x86_64 not in PATH"
		}
		if err := vhostVsockAccessible(); err != nil {
			return false, err.Error()
		}
		return true, ""
	default:
		return false, fmt.Sprintf("qemu tcg not validated for arch %q", runtime.GOARCH)
	}
}

// validateBackendConstraints rejects configs a selected backend cannot honor
// before any network/subnet/proxy provisioning. Firecracker accepts all modes.
// QEMU supports TAP networking, guest-side VFS (workspace/volumes), and — like
// Firecracker — the host-side transparent proxy / MITM / policy engine, since
// that machinery binds to the TAP gateway IP that both backends provide. There
// are no backend-specific constraints left to reject; the proxy, DNS forwarder,
// CA pool, and policy engine all operate on the host side before the machine
// starts, independent of the hypervisor.
func validateBackendConstraints(kind backendKind, config *api.Config) error {
	_ = kind
	_ = config
	return nil
}

// newVMBackend builds the backend selected by the probe result.
type vmBackendFactory func(kind backendKind) vm.Backend

func defaultVMBackendFactory(kind backendKind) vm.Backend {
	switch kind {
	case backendQEMU:
		return qemu.NewBackend()
	default:
		return linux.NewLinuxBackend()
	}
}

const vhostVsockPath = "/dev/vhost-vsock"

// vhostVsockAccessible checks that the vhost-vsock device exists and is usable
// by the current user (it is root:kvm, so this requires kvm group membership).
func vhostVsockAccessible() error {
	f, err := os.OpenFile(vhostVsockPath, os.O_RDWR, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return &vhostVsockError{"vhost-vsock device not present"}
		}
		if os.IsPermission(err) {
			return &vhostVsockError{"vhost-vsock permission denied; add user to kvm group and re-login"}
		}
		return &vhostVsockError{err.Error()}
	}
	_ = f.Close()
	return nil
}

type vhostVsockError struct{ msg string }

func (e *vhostVsockError) Error() string { return e.msg }

// vhostVsockOK reports whether vhost-vsock is accessible (used on QEMU path).
func vhostVsockOK() bool { return vhostVsockAccessible() == nil }
