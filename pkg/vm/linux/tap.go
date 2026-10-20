//go:build linux

package linux

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
	"unsafe"

	"github.com/jingkaihe/matchlock/internal/errx"
)

const (
	tunDevice     = "/dev/net/tun"
	ifnameLen     = 16
	TUNSETPERSIST = 0x400454cb
)

type ifreq struct {
	name  [ifnameLen]byte
	flags uint16
	_     [22]byte
}

func CreateTAP(name string) (int, error) {
	return createTAPInternal(name, true)
}

// CreateNonPersistentTAP creates a TAP device WITHOUT TUNSETPERSIST, so the kernel
// destroys it automatically when the last open descriptor closes. This is used by
// backends that hand the fd to a child process (QEMU via ExtraFiles) and rely on
// the child's fd lifetime to reap the interface on crash/exit, eliminating the
// persistent-TAP leak that occurs when a child is killed without a clean Close().
func CreateNonPersistentTAP(name string) (int, error) {
	return createTAPInternal(name, false)
}

func createTAPInternal(name string, persistent bool) (int, error) {
	fd, err := syscall.Open(tunDevice, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return 0, errx.Wrap(ErrTUNOpen, err)
	}

	var ifr ifreq
	copy(ifr.name[:], name)
	ifr.flags = syscall.IFF_TAP | syscall.IFF_NO_PI

	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		uintptr(syscall.TUNSETIFF), uintptr(unsafe.Pointer(&ifr)))
	if errno != 0 {
		syscall.Close(fd)
		return 0, errx.Wrap(ErrTUNSETIFF, errno)
	}

	if persistent {
		// Make the TAP device persistent so it survives FD close (legacy behavior
		// for Firecracker/reconcile that expects a durable named interface).
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
			uintptr(TUNSETPERSIST), 1)
		if errno != 0 {
			syscall.Close(fd)
			return 0, errx.Wrap(ErrTUNSETPERSIST, errno)
		}
	}

	return fd, nil
}

func ConfigureInterface(name, cidr string) error {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return errx.With(ErrInterfaceNotFound, " %s: %w", name, err)
	}

	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return errx.With(ErrInvalidCIDR, " %s: %w", cidr, err)
	}

	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return errx.Wrap(ErrCreateSocket, err)
	}
	defer syscall.Close(fd)

	var ifr struct {
		name [ifnameLen]byte
		addr syscall.RawSockaddrInet4
		_    [8]byte
	}
	copy(ifr.name[:], name)
	ifr.addr.Family = syscall.AF_INET
	copy(ifr.addr.Addr[:], ip.To4())

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		syscall.SIOCSIFADDR, uintptr(unsafe.Pointer(&ifr))); errno != 0 {
		return errx.Wrap(ErrSIOCSIFADDR, errno)
	}

	var maskReq struct {
		name [ifnameLen]byte
		addr syscall.RawSockaddrInet4
		_    [8]byte
	}
	copy(maskReq.name[:], name)
	maskReq.addr.Family = syscall.AF_INET
	copy(maskReq.addr.Addr[:], ipNet.Mask)

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		syscall.SIOCSIFNETMASK, uintptr(unsafe.Pointer(&maskReq))); errno != 0 {
		return errx.Wrap(ErrSIOCSIFNETMASK, errno)
	}

	_ = iface
	return setLinkUp(name)
}

// setLinkUp sets IFF_UP|IFF_RUNNING on the named interface. It is shared by the
// IPv4 and IPv6 configuration paths (the flags ioctl pair is family
// independent) and keeps the same sentinels the IPv4 path has always returned.
func setLinkUp(name string) error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return errx.Wrap(ErrCreateSocket, err)
	}
	defer syscall.Close(fd)

	var flagReq struct {
		name  [ifnameLen]byte
		flags int16
		_     [22]byte
	}
	copy(flagReq.name[:], name)

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		syscall.SIOCGIFFLAGS, uintptr(unsafe.Pointer(&flagReq))); errno != 0 {
		return errx.Wrap(ErrSIOCGIFFLAGS, errno)
	}

	flagReq.flags |= syscall.IFF_UP | syscall.IFF_RUNNING

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		syscall.SIOCSIFFLAGS, uintptr(unsafe.Pointer(&flagReq))); errno != 0 {
		return errx.Wrap(ErrSIOCSIFFLAGS, errno)
	}

	return nil
}

func DeleteInterface(name string) error {
	// For persistent TAP devices, we need to open the device and clear TUNSETPERSIST
	fd, err := syscall.Open(tunDevice, syscall.O_RDWR, 0)
	if err != nil {
		return errx.Wrap(ErrTUNOpen, err)
	}
	defer syscall.Close(fd)

	var ifr ifreq
	copy(ifr.name[:], name)
	ifr.flags = syscall.IFF_TAP | syscall.IFF_NO_PI

	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		uintptr(syscall.TUNSETIFF), uintptr(unsafe.Pointer(&ifr)))
	if errno != 0 {
		return errx.Wrap(ErrTUNSETIFF, errno)
	}

	// Clear persistent flag to delete the device when FD is closed
	_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		uintptr(TUNSETPERSIST), 0)
	if errno != 0 {
		return errx.Wrap(ErrTUNSETPERSISTOff, errno)
	}

	return nil
}

func SetMTU(name string, mtu int) error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)

	var ifr struct {
		name [ifnameLen]byte
		mtu  int32
		_    [20]byte
	}
	copy(ifr.name[:], name)
	ifr.mtu = int32(mtu)

	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		syscall.SIOCSIFMTU, uintptr(unsafe.Pointer(&ifr)))
	if errno != 0 {
		return errno
	}

	return nil
}

func GenerateMAC(vmID string) string {
	h := uint32(0)
	for _, c := range vmID {
		h = h*31 + uint32(c)
	}
	return fmt.Sprintf("AA:FC:%02X:%02X:%02X:%02X",
		byte(h>>24), byte(h>>16), byte(h>>8), byte(h))
}

func SetupIPForwarding() error {
	return os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644)
}

// netlinkByteOrder is the byte order of the rtnetlink wire format: unlike the
// network protocols it carries, netlink uses the host byte order.
var netlinkByteOrder binary.ByteOrder = binary.NativeEndian

const (
	// ipv6AddrRequestSeq is the netlink sequence number of the single
	// RTM_NEWADDR request sent per socket, so the ACK that matches it is
	// unambiguous.
	ipv6AddrRequestSeq = 1

	// ipv6NetlinkTimeout bounds how long applyIPv6Addr waits for the kernel's
	// ACK before giving up, so a lost reply cannot hang sandbox creation.
	ipv6NetlinkTimeout = 5 * time.Second

	// ipv6GlobalScope is the rtnetlink scope of a routable (including
	// unique-local) address: RT_SCOPE_UNIVERSE is 0, and only link-local
	// addresses carry RT_SCOPE_LINK.
	ipv6GlobalScope = 0

	// ifaddrAttrLen is one rtattr carrying a 128-bit address (4-byte rtattr
	// header + 16-byte address, already 4-byte aligned).
	ifaddrAttrLen = 4 + net.IPv6len

	// ifaFlagNoDAD is IFA_F_NODAD: the address is installed without duplicate
	// address detection. The TAP has no carrier until the VMM attaches to it, and
	// DAD cannot complete on a carrier-less link, so the address would stay
	// TENTATIVE — and a tentative address cannot be bound (bind(2) fails with
	// EADDRNOTAVAIL). The interception proxy binds the TAP's IPv6 gateway BEFORE
	// the VM starts, so the address must be usable the moment it is installed.
	// DAD is pointless on this link anyway: the guest side carries a different
	// address from a per-VM /64, so no duplicate can exist.
	ifaFlagNoDAD = 0x2
)

// ipv6AddrSender installs a serialized RTM_NEWADDR message. Production uses
// applyIPv6Addr; tests inject a recorder so the configuration path is covered
// without CAP_NET_ADMIN.
type ipv6AddrSender func(msg []byte) error

// buildIPv6AddrMessage serializes an rtnetlink RTM_NEWADDR request that adds
// ip/prefixLen to the interface with the given ifindex. It is a pure function of
// its arguments so the message layout is unit-testable without CAP_NET_ADMIN
// (see tap_ipv6_test.go).
//
// The request carries IFA_LOCAL and IFA_ADDRESS, both set to ip, exactly like
// `ip -6 addr add`/`ip -6 addr replace` (verified against the rtnetlink message
// iproute2 sends), and asks for NLM_F_CREATE|NLM_F_REPLACE without NLM_F_EXCL so
// the request is idempotent: LinuxMachine.Start re-applies the TAP
// configuration because the VMM resets the interface when it opens the device,
// and installing an address that is already present must not fail with EEXIST.
func buildIPv6AddrMessage(ifindex int, ip net.IP, prefixLen int) ([]byte, error) {
	if ifindex <= 0 {
		return nil, errx.With(ErrInvalidInterfaceIndex, ": %d", ifindex)
	}
	if ip == nil || ip.To4() != nil || ip.To16() == nil {
		return nil, errx.With(ErrInvalidIPv6Address, ": %q is not an IPv6 address", ip.String())
	}
	if prefixLen < 0 || prefixLen > 128 {
		return nil, errx.With(ErrInvalidIPv6PrefixLength, ": %d", prefixLen)
	}

	msg := make([]byte, syscall.SizeofNlMsghdr+syscall.SizeofIfAddrmsg+2*ifaddrAttrLen)

	netlinkByteOrder.PutUint32(msg[0:4], uint32(len(msg)))
	netlinkByteOrder.PutUint16(msg[4:6], syscall.RTM_NEWADDR)
	netlinkByteOrder.PutUint16(msg[6:8],
		syscall.NLM_F_REQUEST|syscall.NLM_F_ACK|syscall.NLM_F_CREATE|syscall.NLM_F_REPLACE)
	netlinkByteOrder.PutUint32(msg[8:12], ipv6AddrRequestSeq)
	netlinkByteOrder.PutUint32(msg[12:16], 0) // nlmsg_pid: the kernel fills it in

	off := syscall.SizeofNlMsghdr
	msg[off] = syscall.AF_INET6  // ifa_family
	msg[off+1] = byte(prefixLen) // ifa_prefixlen
	msg[off+2] = ifaFlagNoDAD    // ifa_flags: usable at once, DAD cannot run on the TAP
	msg[off+3] = ipv6GlobalScope // ifa_scope
	netlinkByteOrder.PutUint32(msg[off+4:off+8], uint32(ifindex))
	off += syscall.SizeofIfAddrmsg

	for _, attrType := range []uint16{syscall.IFA_LOCAL, syscall.IFA_ADDRESS} {
		netlinkByteOrder.PutUint16(msg[off:off+2], ifaddrAttrLen)
		netlinkByteOrder.PutUint16(msg[off+2:off+4], attrType)
		copy(msg[off+4:off+ifaddrAttrLen], ip.To16())
		off += ifaddrAttrLen
	}

	return msg, nil
}

// applyIPv6Addr sends a built RTM_NEWADDR request over rtnetlink and waits for
// the kernel's ACK. IPv6 addresses cannot be installed through the SIOCSIFADDR
// ioctl the IPv4 path uses (it has no room for a 128-bit address), so the host
// side needs the netlink round trip.
func applyIPv6Addr(msg []byte) error {
	fd, err := syscall.Socket(syscall.AF_NETLINK,
		syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_ROUTE)
	if err != nil {
		return errx.Wrap(ErrNetlinkSocket, err)
	}
	defer syscall.Close(fd)

	timeout := syscall.Timeval{Sec: int64(ipv6NetlinkTimeout / time.Second)}
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &timeout); err != nil {
		return errx.Wrap(ErrNetlinkSocket, err)
	}

	addr := &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}
	if err := syscall.Bind(fd, addr); err != nil {
		return errx.Wrap(ErrNetlinkBind, err)
	}
	if err := syscall.Sendto(fd, msg, 0, addr); err != nil {
		return errx.Wrap(ErrNetlinkSend, err)
	}

	buf := make([]byte, 4096)
	for {
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			return errx.Wrap(ErrNetlinkRecv, err)
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return errx.Wrap(ErrNetlinkRecv, err)
		}
		for i := range msgs {
			if msgs[i].Header.Type != syscall.NLMSG_ERROR {
				continue
			}
			if len(msgs[i].Data) < 4 {
				return errx.With(ErrNetlinkRecv, ": truncated NLMSG_ERROR payload (%d bytes)", len(msgs[i].Data))
			}
			// The payload is struct nlmsgerr: a negative errno (0 means ACK).
			errno := int(int32(netlinkByteOrder.Uint32(msgs[i].Data[:4])))
			if errno == 0 {
				return nil
			}
			// The request is a create-or-replace, so an address that is
			// already present must not surface as a failure: tolerate EEXIST
			// as success and keep the re-applied TAP configuration
			// idempotent.
			if errors.Is(syscall.Errno(-errno), syscall.EEXIST) {
				return nil
			}
			return errx.With(ErrNetlinkAddrAdd, ": %s", syscall.Errno(-errno))
		}
	}
}

// ConfigureInterfaceIPv6 adds the address/prefix given by cidr to the named
// interface and brings the link up. Unlike IPv4 it cannot go through the
// SIOCSIFADDR ioctl, so the address is installed with an rtnetlink RTM_NEWADDR
// request (see buildIPv6AddrMessage).
func ConfigureInterfaceIPv6(name, cidr string) error {
	return configureInterfaceIPv6(name, cidr, applyIPv6Addr, setLinkUp)
}

// configureInterfaceIPv6 is ConfigureInterfaceIPv6 with its two kernel-facing
// dependencies (the netlink sender and the link-up call) injected so tests can
// exercise the lookup, parsing and message-building path without CAP_NET_ADMIN.
func configureInterfaceIPv6(name, cidr string, send ipv6AddrSender, linkUp func(string) error) error {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return errx.With(ErrInterfaceNotFound, " %s: %w", name, err)
	}

	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return errx.With(ErrInvalidCIDR, " %s: %w", cidr, err)
	}
	prefixLen, bits := ipNet.Mask.Size()
	if bits != 128 {
		return errx.With(ErrInvalidCIDR, ": %s is not an IPv6 CIDR", cidr)
	}

	msg, err := buildIPv6AddrMessage(iface.Index, ip, prefixLen)
	if err != nil {
		return err
	}
	if err := send(msg); err != nil {
		return err
	}
	return linkUp(name)
}
