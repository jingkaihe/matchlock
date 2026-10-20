//go:build !(darwin && cgo)

package darwin

import (
	"context"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/vm"
)

// Create validates the config through the same SDK-independent checks the real
// backend performs first (swap/disk safety and, when interception is active,
// the IPv6 link derivation), then fails explicitly: a darwin binary built
// without cgo cannot link Virtualization.framework and can never boot a VM. It
// does not reproduce the file-existence, CPU or VZ-configuration validation of
// the cgo path, because no VM is ever created in this build.
func (b *DarwinBackend) Create(ctx context.Context, config *vm.VMConfig) (vm.Machine, error) {
	if err := vm.ValidateSwapDisks(config); err != nil {
		return nil, err
	}
	// The IPv6 link is a property of the interception path only (mirroring the
	// real Create): a plain-NAT or --no-network config may carry stale IPv6
	// fields and must keep booting exactly as before.
	if config.UseInterception && !config.NoNetwork {
		if _, _, err := vm.ParseIPv6Link(config); err != nil {
			return nil, errx.Wrap(ErrInvalidIPv6Link, err)
		}
	}
	return nil, ErrVZUnavailable
}
