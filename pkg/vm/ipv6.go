package vm

import (
	"errors"
	"fmt"
	"net"

	"github.com/jingkaihe/matchlock/internal/errx"
)

// ErrInvalidIPv6Address is returned when a VMConfig carries a fully-set IPv6
// link whose gateway or guest address does not parse as an IPv6 address.
var ErrInvalidIPv6Address = errors.New("invalid IPv6 address")

// ErrInvalidCIDR is returned when a VMConfig carries a fully-set IPv6 link
// whose subnet prefix does not parse as a 128-bit CIDR.
var ErrInvalidCIDR = errors.New("invalid CIDR")

// IPv6Link is the per-VM IPv6 guest link derived from a VMConfig: the
// guest-visible gateway address, the guest address, and the prefix length of
// the unique-local /64 the subnet allocator hands the VM.
//
// It is the ONE derivation every backend consumes. The Linux backends install
// the gateway on their own TAP (pkg/vm/linux, pkg/vm/qemu); the darwin backend
// emits the same KernelArg on its interception boot line and assigns the same
// gateway to the gVisor netstack NIC (pkg/net). Deriving it once is what keeps
// the guest's boot argument and the host-side addressing from drifting apart: a
// backend that skips the install leaves sandbox-provisioned IPv6 state (the ip6
// table on Linux, the userspace stack addressing on darwin) pointed at nothing.
type IPv6Link struct {
	Gateway   string
	Guest     string
	PrefixLen int
}

// TapCIDR is the address/prefix to configure on the host-side link. It always
// carries the gateway address (never a network address), so a Subnet6CIDR
// supplied in network form still puts the gateway on the link.
func (l IPv6Link) TapCIDR() string {
	return fmt.Sprintf("%s/%d", l.Gateway, l.PrefixLen)
}

// KernelArg is the matchlock.ipv6=<guest>/<prefixlen>,<gateway> cmdline field.
// guest-init uses it to configure the guest address and its ::/0 route, because
// the kernel ip= boot argument configures IPv4 only.
func (l IPv6Link) KernelArg() string {
	return fmt.Sprintf(" matchlock.ipv6=%s/%d,%s", l.Guest, l.PrefixLen, l.Gateway)
}

// ParseIPv6Link derives the optional IPv6 guest link of a VMConfig. It is the
// shared entry point of every backend (see IPv6Link).
//
// hasLink is false when the config carries no IPv6 link: either no IPv6 field
// at all, or only some of the three fields set. That partially-set case is
// deliberately NOT an error — it is the pre-existing Linux behavior (pinned by
// pkg/vm/linux tests) and is what keeps an IPv4-only sandbox or a --no-network
// sandbox byte-identical to before: nothing is configured. A MALFORMED
// fully-set link (three fields present, one of them unparseable) IS an error,
// so half a link can never be smuggled into a boot arg or a NIC.
func ParseIPv6Link(cfg *VMConfig) (IPv6Link, bool, error) {
	if cfg == nil {
		return IPv6Link{}, false, nil
	}
	return ParseIPv6LinkFromStrings(cfg.GatewayIPv6, cfg.GuestIPv6, cfg.Subnet6CIDR)
}

// ParseIPv6LinkFromStrings is ParseIPv6Link on the raw triple, so consumers
// that hold the three fields without a VMConfig (the darwin userspace stack
// config) validate through the same code path instead of a second parser.
func ParseIPv6LinkFromStrings(gatewayIPv6, guestIPv6, subnet6CIDR string) (IPv6Link, bool, error) {
	if gatewayIPv6 == "" || guestIPv6 == "" || subnet6CIDR == "" {
		return IPv6Link{}, false, nil
	}
	for _, addr := range []string{gatewayIPv6, guestIPv6} {
		ip := net.ParseIP(addr)
		if ip == nil || ip.To4() != nil {
			return IPv6Link{}, false, errx.With(ErrInvalidIPv6Address, ": %q is not an IPv6 address", addr)
		}
	}
	_, ipNet, err := net.ParseCIDR(subnet6CIDR)
	if err != nil {
		return IPv6Link{}, false, errx.With(ErrInvalidCIDR, " %s: %w", subnet6CIDR, err)
	}
	prefixLen, bits := ipNet.Mask.Size()
	if bits != 128 {
		return IPv6Link{}, false, errx.With(ErrInvalidCIDR, ": %s is not an IPv6 CIDR", subnet6CIDR)
	}
	return IPv6Link{Gateway: gatewayIPv6, Guest: guestIPv6, PrefixLen: prefixLen}, true, nil
}

// IPv6KernelArg returns the matchlock.ipv6= cmdline field for cfg, or "" when
// no IPv6 link is configured. A malformed link is validated by every backend's
// Create before any host resource exists; on a bypass path it emits no field
// rather than half a link.
func IPv6KernelArg(cfg *VMConfig) string {
	link, hasLink, err := ParseIPv6Link(cfg)
	if err != nil || !hasLink {
		return ""
	}
	return link.KernelArg()
}
