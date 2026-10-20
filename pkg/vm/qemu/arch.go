//go:build linux

package qemu

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/jingkaihe/matchlock/internal/errx"
)

// qemuSystemName returns the qemu-system binary name for this host, or "" if
// the architecture is not a validated QEMU TCG target. x86_64 and arm64 are
// both supported; other archs are not.
func qemuSystemName() string {
	switch runtime.GOARCH {
	case "arm64", "aarch64":
		return "qemu-system-aarch64"
	case "amd64", "x86_64":
		return "qemu-system-x86_64"
	default:
		return ""
	}
}

// machineType returns the QEMU -machine argument. arm64 uses the virt board;
// x86_64 uses the standard PC (i440fx) board which provides all virtio PCI
// devices matchlock needs (virtio-blk-pci, virtio-net-pci, vhost-vsock-pci).
func machineType() string {
	switch runtime.GOARCH {
	case "arm64", "aarch64":
		return "virt"
	default:
		return "pc"
	}
}

// consoleDevice returns the kernel console= argument for the guest arch. arm64
// uses the PL011 UART (ttyAMA0); x86_64 uses the 16550 UART (ttyS0).
func consoleDevice() string {
	switch runtime.GOARCH {
	case "arm64", "aarch64":
		return "ttyAMA0"
	default:
		return "ttyS0"
	}
}

// guestCPU returns the -cpu model for the guest architecture.
func guestCPU() string {
	return guestCPUFor(runtimeGOARCH())
}

// guestCPUFor maps a host architecture to the QEMU -cpu model for its guest.
//
// arm64 deliberately advertises the conservative cortex-a53 feature set rather
// than "max". The QEMU/TCG backend runs every guest under `-accel tcg`, and
// under swap memory pressure (-cpu max) intermittently corrupted guest runtime
// state: the guest agent died in Go runtime consistency fatals, workloads
// aborted with glibc "corrupted double-linked list" or SIGSEGV, and some
// children never exited (host-side vsock "connection reset by peer"). In an
// admission-gated differential that varied only the CPU model, cortex-a53
// produced 0/24 failures while -cpu max produced 15/36, and the pre-registered
// cell on an identical QEMU 11.0.1 binary measured cortex-a53 12/12 pass vs
// -cpu max 5/12 pass (Fisher two-sided p=0.0046). The underlying QEMU TCG
// emulation defect and the responsible CPU feature were NOT identified, so this
// is a conservative-advertised-feature WORKAROUND, not a root-cause fix, and it
// is not proven to eliminate the defect. Pinning the model also changes the
// advertised instruction/feature set and can affect application compatibility.
// x86_64 (and any unknown arch) keeps "max" unchanged.
func guestCPUFor(arch string) string {
	switch arch {
	case "arm64", "aarch64":
		return "cortex-a53"
	default:
		return "max"
	}
}

// prepareDisks validates the bootstrap and overlay disks and returns the set of
// virtio disks to attach, in the order the kernel args expect:
// vda = bootstrap (root), then overlay lowers, then overlay upper, then extras.
func (m *Machine) prepareDisks() (diskSet, error) {
	cleanup := func() error { return nil }

	// vda: bootstrap root disk (must exist, writable, ext4).
	if err := requireFile(m.config.RootfsPath); err != nil {
		return diskSet{}, errx.With(ErrRootfsNotFound, ": %s", m.config.RootfsPath)
	}

	disks := []diskMount{
		{HostPath: m.config.RootfsPath, ReadOnly: false},
	}

	if m.config.OverlayEnabled {
		for _, lower := range m.config.OverlayLowerPaths {
			if err := requireFile(lower); err != nil {
				return diskSet{}, errx.With(ErrRootfsNotFound, ": %s", lower)
			}
			disks = append(disks, diskMount{HostPath: lower, ReadOnly: true})
		}
		if err := requireFile(m.config.OverlayUpperPath); err != nil {
			return diskSet{}, errx.With(ErrRootfsNotFound, ": %s", m.config.OverlayUpperPath)
		}
		disks = append(disks, diskMount{HostPath: m.config.OverlayUpperPath, ReadOnly: false})
	}

	for _, d := range m.config.ExtraDisks {
		if err := requireFile(d.HostPath); err != nil {
			return diskSet{}, errx.With(ErrRootfsNotFound, ": %s", d.HostPath)
		}
		disks = append(disks, diskMount{HostPath: d.HostPath, ReadOnly: d.ReadOnly})
	}

	return diskSet{disks: disks, cleanup: cleanup}, nil
}

// diskArgs renders the -drive list for the attached virtio disks. Guest device
// names (vda, vdb, ...) appear only in kernel arguments; QEMU object IDs must be
// plain identifiers (no '/'), so we use disk0, disk1, ... in attachment order:
// bootstrap, overlay lowers, upper, then extras.
func (m *Machine) diskArgs(disks []diskMount) []string {
	var args []string
	for i, d := range disks {
		id := fmt.Sprintf("disk%d", i)
		fileArg := escapeDriveFile(d.HostPath)
		if d.ReadOnly {
			args = append(args,
				"-drive", fmt.Sprintf("if=none,id=%s,file=%s,format=raw,readonly=on", id, fileArg),
				"-device", fmt.Sprintf("virtio-blk-pci,drive=%s", id),
			)
			continue
		}
		args = append(args,
			"-drive", fmt.Sprintf("if=none,id=%s,file=%s,format=raw", id, fileArg),
			"-device", fmt.Sprintf("virtio-blk-pci,drive=%s", id),
		)
	}
	return args
}

// escapeDriveFile doubles any comma so a drive path containing one is parsed by
// QEMU's key-value syntax as a literal character rather than a field separator.
func escapeDriveFile(path string) string {
	return strings.ReplaceAll(path, ",", ",,")
}

func requireFile(path string) error {
	if path == "" {
		return errx.With(ErrRootfsNotFound, ": empty path")
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return errx.With(ErrRootfsNotFound, ": %s is a directory", path)
	}
	return nil
}

// lowerFSTypes matches the kernel arg string for the overlay lowers.
func (m *Machine) lowerFSTypes() string {
	if len(m.config.OverlayLowerFSTypes) == 0 {
		return ""
	}
	return strings.Join(m.config.OverlayLowerFSTypes, ",")
}
