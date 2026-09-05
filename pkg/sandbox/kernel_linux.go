//go:build linux

package sandbox

import (
	"context"
	"runtime"

	"github.com/jingkaihe/matchlock/internal/assets/qemu-kernel"
	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/lifecycle"
)

// resolveLinuxKernelForConfig is the Linux-only kernel resolution path.
//
// For the QEMU amd64 backend with no explicit kernel config, it selects the
// vendored QEMU bzImage (embedded in the binary, offline) so that
// MATCHLOCK_BACKEND=qemu works without a --kernel override. Every other case
// (Firecracker, arm64 QEMU, or an explicit --kernel file:// or OCI ref) falls
// through to the shared resolveKernelForConfig so its behavior is unchanged.
func resolveLinuxKernelForConfig(ctx context.Context, config *api.Config,
	opts *Options, store *lifecycle.Store, kind backendKind) (string, error) {

	hasOverride := opts != nil && opts.KernelPath != ""
	hasRef := config != nil && config.Kernel != nil && config.Kernel.Ref != ""

	// Only the vendored-kernel case is special: QEMU backend on amd64, with no
	// explicit kernel supplied. Everything else keeps the existing resolution.
	if kind != backendQEMU || runtime.GOARCH != "amd64" || hasOverride || hasRef {
		return resolveKernelForConfig(ctx, config, opts, store)
	}

	path, err := qemukernel.Ensure()
	if err != nil {
		return "", err
	}

	ref := qemukernel.EmbeddedRef()
	recordKernelResolution(store, ref, path)
	return path, nil
}
