//go:build linux

package linux

import (
	"net"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestConfigureInterfaceIPv6InstallsAddress is the real-kernel counterpart of
// the unit tests: it drives ConfigureInterfaceIPv6 (netlink round trip included)
// against a live interface. It needs CAP_NET_ADMIN, so it self-SKIPs unless run
// as root with MATCHLOCK_PRIVILEGED_TESTS=1 — a skip is not a pass.
//
// A host root shell is a bad place to run it: it adds addresses to whatever
// interface the test is pointed at. The intended recipe (same shape as the FUSE
// and TAP lifetime gates in AGENTS.md) compiles the package statically and runs
// it in a throwaway privileged container, whose network namespace and loopback
// device are discarded with the container:
//
//	CGO_ENABLED=0 go test -c -o /tmp/vmipv6.test ./pkg/vm/linux/
//	docker run --rm --privileged -v /tmp/vmipv6.test:/vmipv6.test:ro \
//	  -e MATCHLOCK_PRIVILEGED_TESTS=1 alpine:latest \
//	  /vmipv6.test -test.run TestConfigureInterfaceIPv6InstallsAddress -test.v
func TestConfigureInterfaceIPv6InstallsAddress(t *testing.T) {
	requirePrivileged(t)

	const (
		ifaceName = "lo"
		addr1     = "fd00:200::1"
		addr2     = "fd00:201::1"
		prefixLen = 64
	)

	iface, err := net.InterfaceByName(ifaceName)
	require.NoError(t, err)

	t.Cleanup(func() {
		uninstallIPv6Addr(t, iface.Index, addr2, prefixLen)
		uninstallIPv6Addr(t, iface.Index, addr1, prefixLen)
	})

	require.NoError(t, ConfigureInterfaceIPv6(ifaceName, addr1+"/64"))

	got, err := net.InterfaceByName(ifaceName)
	require.NoError(t, err)
	require.NotZero(t, got.Flags&net.FlagUp, "%s must be up after configuration", ifaceName)
	require.Contains(t, ifaceAddrs(t, got), addr1, "the configured address must be on the interface")

	// The address must be USABLE the moment it is installed: the interception
	// proxy binds the TAP's IPv6 gateway before the VM starts, and a tentative
	// address (DAD in flight, which never completes on a carrier-less TAP) is
	// rejected by bind(2) with EADDRNOTAVAIL. This is the regression lock for
	// IFA_F_NODAD - without the flag this immediate bind fails.
	ln, err := net.Listen("tcp", net.JoinHostPort(addr1, "0"))
	require.NoError(t, err, "the configured address must be bindable immediately (IFA_F_NODAD)")
	require.NoError(t, ln.Close())

	// LinuxMachine.Start re-applies the TAP configuration after the VMM opens
	// the device, so the second call must succeed instead of failing with
	// EEXIST (the reason the request carries NLM_F_REPLACE and not NLM_F_EXCL).
	require.NoError(t, ConfigureInterfaceIPv6(ifaceName, addr1+"/64"), "re-applying the same address must be idempotent")
	require.Contains(t, ifaceAddrs(t, iface), addr1, "the address must survive the re-application")

	// A second address on the same link must be installable too.
	require.NoError(t, ConfigureInterfaceIPv6(ifaceName, addr2+"/64"))
	require.Contains(t, ifaceAddrs(t, iface), addr2)
}

// TestConfigureInterfaceIPv6TAPAddressIsImmediatelyUsable is the behavioural
// regression lock for IFA_F_NODAD on the interface that actually matters. A TAP
// has no carrier until the VMM attaches to it, so duplicate address detection
// cannot complete: without IFA_F_NODAD the address stays TENTATIVE and bind(2)
// fails with EADDRNOTAVAIL - which is exactly how sandbox creation failed at
// "create transparent proxy: listen failed on HTTP port [fd00:N::1]:...: cannot
// assign requested address", because the proxy binds the TAP's IPv6 gateway
// before the VM starts. `lo` cannot show this (the kernel skips DAD there), and
// the unit tests only pin the flag byte, so this test is what proves the flag
// has the intended EFFECT against a real kernel.
func TestConfigureInterfaceIPv6TAPAddressIsImmediatelyUsable(t *testing.T) {
	requirePrivileged(t)

	const (
		ifaceName = "mlv6nodad"
		addr      = "fd00:2f1::1"
	)

	tapFD, err := CreateTAP(ifaceName)
	require.NoError(t, err, "create the persistent test TAP")
	t.Cleanup(func() {
		_ = syscall.Close(tapFD)
		_ = DeleteInterface(ifaceName)
	})

	require.NoError(t, ConfigureInterfaceIPv6(ifaceName, addr+"/64"))

	// No carrier on this TAP: without IFA_F_NODAD the address would still be
	// TENTATIVE here and this bind would fail.
	ln, err := net.Listen("tcp", net.JoinHostPort(addr, "0"))
	require.NoError(t, err, "the TAP address must be bindable the moment it is installed")
	require.NoError(t, ln.Close())
}

// ifaceAddrs returns the bare IP strings assigned to iface.
func ifaceAddrs(t *testing.T, iface *net.Interface) []string {
	t.Helper()
	addrs, err := iface.Addrs()
	require.NoError(t, err)
	out := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok {
			out = append(out, ipNet.IP.String())
			continue
		}
		out = append(out, addr.String())
	}
	return out
}

// uninstallIPv6Addr removes an address by reusing the wire format
// buildIPv6AddrMessage produces with the message type flipped to RTM_DELADDR, so
// the privileged test leaves the interface as it found it.
func uninstallIPv6Addr(t *testing.T, ifindex int, addr string, prefixLen int) {
	t.Helper()
	msg, err := buildIPv6AddrMessage(ifindex, net.ParseIP(addr), prefixLen)
	require.NoError(t, err)
	netlinkByteOrder.PutUint16(msg[4:6], syscall.RTM_DELADDR)
	netlinkByteOrder.PutUint16(msg[6:8], syscall.NLM_F_REQUEST|syscall.NLM_F_ACK)
	require.NoError(t, applyIPv6Addr(msg), "cleanup must be able to remove %s", addr)
}
