//go:build linux

package qemu

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/vm"
)

// Backend is the QEMU TCG VM backend.
type Backend struct{}

// NewBackend returns a QEMU backend.
func NewBackend() *Backend { return &Backend{} }

// Name returns the stable backend identifier.
func (b *Backend) Name() string { return "qemu" }

// Supported reports whether the QEMU binary for this host architecture is
// available. arm64 and amd64 are validated; other architectures are rejected so
// we never silently apply untested machine settings.
func Supported() error {
	switch runtimeGOARCH() {
	case "arm64", "aarch64":
		if _, err := exec.LookPath("qemu-system-aarch64"); err != nil {
			return errx.Wrap(ErrQEMUNotFound, err)
		}
		return nil
	case "amd64", "x86_64":
		if _, err := exec.LookPath("qemu-system-x86_64"); err != nil {
			return errx.Wrap(ErrQEMUNotFound, err)
		}
		return nil
	default:
		return errx.With(ErrUnsupportedPlatform, ": %q is not a validated qemu target", runtimeGOARCH())
	}
}

// Create builds a QEMU machine for the given config. It validates platform
// support, allocates a unique guest CID, and (for a networked guest) creates
// the host TAP. The TAP must exist before Start so the sandbox can read
// TapName() and provision the host-side firewall/NAT. Start performs the boot.
func (b *Backend) Create(_ context.Context, config *vm.VMConfig) (vm.Machine, error) {
	if config == nil {
		return nil, errx.Wrap(ErrCreate, fmt.Errorf("nil config"))
	}
	if err := Supported(); err != nil {
		return nil, err
	}

	cid, lock, err := reserveGuestCID()
	if err != nil {
		return nil, errx.Wrap(ErrCreate, err)
	}

	m := &Machine{
		id:      config.ID,
		config:  config,
		cid:     cid,
		cidLock: lock,
		done:    make(chan struct{}),
	}

	// Create the host TAP now (before Start) so the sandbox sees a valid
	// TapName() and can set up nftables NAT for it. NoNetwork guests get none.
	if !config.NoNetwork {
		net, netErr := m.setupNetwork()
		if netErr != nil {
			_ = lock.Close()
			return nil, errx.Wrap(ErrCreate, netErr)
		}
		m.net = net
	}

	return m, nil
}

// reserveGuestCID allocates the first guest CID whose lock file can be
// exclusively locked, holding the lock for the lifetime of the returned file
// handle (and, via ExtraFiles, for the lifetime of the QEMU child). This makes
// the reservation safe across concurrent matchlock processes and survives a
// parent crash because the child keeps the lock until it exits.
func reserveGuestCID() (uint32, *os.File, error) {
	const base, max = uint32(3), uint32(65535)
	for cid := base; cid <= max; cid++ {
		path := filepath.Join(os.TempDir(), fmt.Sprintf("matchlock-cid-%d.lock", cid))
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			continue
		}
		if err := flockExclusive(f); err != nil {
			_ = f.Close()
			continue
		}
		return cid, f, nil
	}
	return 0, nil, errx.With(ErrInvalidCID, ": no free guest cid in %d..%d", base, max)
}
