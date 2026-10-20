//go:build linux

package linux

import "github.com/jingkaihe/matchlock/pkg/vm"

// The IPv6 link derivation that used to live here moved to pkg/vm (see
// vm.IPv6Link) so the darwin backend derives its per-VM IPv6 link from the
// SAME code instead of a second copy. The Linux names below are kept as thin
// aliases/wrappers: every Linux caller (this package, pkg/vm/qemu) keeps its
// existing API, and the sentinels it checks with (ErrInvalidIPv6Address,
// ErrInvalidCIDR in errors.go) are aliases of the shared ones, so
// errors.Is/ErrorIs identity is preserved.

// IPv6Link is the shared per-VM IPv6 guest link (vm.IPv6Link).
type IPv6Link = vm.IPv6Link

// ParseIPv6Link derives the optional IPv6 guest link of a VMConfig through the
// shared pkg/vm derivation.
func ParseIPv6Link(cfg *vm.VMConfig) (IPv6Link, bool, error) {
	return vm.ParseIPv6Link(cfg)
}

// IPv6KernelArg returns the matchlock.ipv6= cmdline field for cfg, or "" when
// no IPv6 link is configured.
func IPv6KernelArg(cfg *vm.VMConfig) string {
	return vm.IPv6KernelArg(cfg)
}
