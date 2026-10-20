//go:build linux

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/jingkaihe/matchlock/internal/errx"
)

const (
	// ipv6FieldPrefix is the kernel cmdline field that carries the guest side of
	// the per-VM IPv6 link. The kernel's own ip= argument is IPv4-only, so the
	// address and the default route are installed from userspace instead.
	ipv6FieldPrefix = "matchlock.ipv6="

	// netlinkRequestSeq is the sequence number of every rtnetlink request
	// guest-init sends; the kernel echoes it in the ACK. guest-init performs one
	// configuration exchange per boot, so a fixed value is enough to match the
	// reply to the request.
	netlinkRequestSeq = 1

	// netlinkRecvBufferLen is the receive buffer for a netlink ACK. The kernel
	// answers these requests with a single NLMSG_ERROR message.
	netlinkRecvBufferLen = 4096

	// netlinkRequestTimeout bounds the wait for the kernel's ACK so a lost reply
	// cannot hang the boot.
	netlinkRequestTimeout = 5 * time.Second

	// netlinkAddrAttrLen is one rtattr carrying a 128-bit IPv6 address: a 4-byte
	// rtattr header plus the 16 address bytes. The length is already 4-byte
	// aligned, which is all the rtattr padding rules require.
	netlinkAddrAttrLen = 4 + net.IPv6len

	// netlinkUint32AttrLen is one rtattr carrying a native-endian uint32, which
	// is how RTA_OIF encodes an interface index.
	netlinkUint32AttrLen = 4 + 4

	// ipv6GlobalScope is the rtnetlink scope of a routable address
	// (RT_SCOPE_UNIVERSE = 0). The guest address is a global unique-local
	// address, never link-local.
	ipv6GlobalScope = syscall.RT_SCOPE_UNIVERSE

	// ifaFlagNoDAD is IFA_F_NODAD: install the guest address without duplicate
	// address detection, so it is usable the moment it exists. DAD cannot find a
	// duplicate on this link (the host side of the per-VM /64 carries a different
	// address), and while DAD runs the address is TENTATIVE - the kernel then
	// refuses to use it as a source, so a workload that starts connecting right
	// after boot (the ready signal is served before DAD would finish) fails its
	// first IPv6 connections.
	ifaFlagNoDAD = 0x2
)

// netlinkByteOrder is the byte order of the rtnetlink wire format: unlike the
// socket address structures it is host byte order, not network byte order
// (verified against the messages iproute2 emits; see main_test.go).
var netlinkByteOrder binary.ByteOrder = binary.NativeEndian

// ipv6Link is the guest side of the per-VM IPv6 link the host announces with the
// matchlock.ipv6=<guest-address>/<prefix-length>,<gateway-address> kernel
// argument (generated in pkg/vm/linux). bootConfig.IPv6 stays nil when the host
// emitted no such field, which is what keeps an IPv4-only boot byte-identical
// to before.
type ipv6Link struct {
	Guest     net.IP
	PrefixLen int
	Gateway   net.IP
}

// parseIPv6LinkField parses the value of a matchlock.ipv6= field. Both halves are
// required: a missing address, prefix length or gateway is a malformed boot
// argument and fails the boot instead of silently leaving the guest without a
// route. Only IPv6 values are accepted for either address.
func parseIPv6LinkField(value string) (ipv6Link, error) {
	var link ipv6Link

	addrPart, gatewayPart, found := strings.Cut(value, ",")
	if !found {
		return link, errx.With(ErrInvalidIPv6Link, ": %q is missing the gateway address", value)
	}
	addrPart = strings.TrimSpace(addrPart)
	gatewayPart = strings.TrimSpace(gatewayPart)
	if addrPart == "" || gatewayPart == "" {
		return link, errx.With(ErrInvalidIPv6Link, ": %q has an empty address or gateway", value)
	}

	guest, guestNet, err := net.ParseCIDR(addrPart)
	if err != nil {
		return link, errx.With(ErrInvalidIPv6Link, ": %q: %w", addrPart, err)
	}
	prefixLen, bits := guestNet.Mask.Size()
	if bits != net.IPv6len*8 || guest.To4() != nil {
		return link, errx.With(ErrInvalidIPv6Link, ": %q is not an IPv6 address/prefix", addrPart)
	}

	gateway := net.ParseIP(gatewayPart)
	if gateway == nil || gateway.To4() != nil || gateway.To16() == nil {
		return link, errx.With(ErrInvalidIPv6Link, ": %q is not an IPv6 gateway address", gatewayPart)
	}

	return ipv6Link{Guest: guest, PrefixLen: prefixLen, Gateway: gateway}, nil
}

// String renders the link the way the kernel cmdline field spells it.
func (l ipv6Link) String() string {
	return fmt.Sprintf("%s/%d,%s", l.Guest, l.PrefixLen, l.Gateway)
}

// activeIPv6Link returns the IPv6 link to configure for this boot, or nil when
// the host announced no link or the boot has networking disabled. It is the
// single decision point for "does this boot configure IPv6 at all", so a
// matchlock.no_network=1 or v6-less boot cannot reach the netlink path.
func (c *bootConfig) activeIPv6Link() *ipv6Link {
	if c == nil || c.NoNetwork {
		return nil
	}
	return c.IPv6
}

// buildGuestAddrMessage serializes an rtnetlink RTM_NEWADDR request that adds
// ip/prefixLen to the interface with the given ifindex. It is a pure function of
// its arguments so the message layout is unit-testable without CAP_NET_ADMIN.
//
// The request carries IFA_LOCAL and IFA_ADDRESS, both set to ip, exactly like
// `ip -6 addr add` (verified against the rtnetlink message iproute2 sends), and
// asks for NLM_F_CREATE|NLM_F_REPLACE without NLM_F_EXCL so a re-applied
// configuration is idempotent instead of failing with EEXIST.
func buildGuestAddrMessage(ifindex int, ip net.IP, prefixLen int) ([]byte, error) {
	if ifindex <= 0 {
		return nil, errx.With(ErrInvalidIPv6Link, ": invalid interface index %d", ifindex)
	}
	if ip == nil || ip.To4() != nil || ip.To16() == nil {
		return nil, errx.With(ErrInvalidIPv6Link, ": %q is not an IPv6 address", ip)
	}
	if prefixLen < 0 || prefixLen > net.IPv6len*8 {
		return nil, errx.With(ErrInvalidIPv6Link, ": invalid IPv6 prefix length %d", prefixLen)
	}

	msg := make([]byte, syscall.SizeofNlMsghdr+syscall.SizeofIfAddrmsg+2*netlinkAddrAttrLen)

	netlinkByteOrder.PutUint32(msg[0:4], uint32(len(msg)))
	netlinkByteOrder.PutUint16(msg[4:6], syscall.RTM_NEWADDR)
	netlinkByteOrder.PutUint16(msg[6:8],
		syscall.NLM_F_REQUEST|syscall.NLM_F_ACK|syscall.NLM_F_CREATE|syscall.NLM_F_REPLACE)
	netlinkByteOrder.PutUint32(msg[8:12], netlinkRequestSeq)
	netlinkByteOrder.PutUint32(msg[12:16], 0) // nlmsg_pid: the kernel fills it in

	off := syscall.SizeofNlMsghdr
	msg[off] = syscall.AF_INET6  // ifa_family
	msg[off+1] = byte(prefixLen) // ifa_prefixlen
	msg[off+2] = ifaFlagNoDAD    // ifa_flags: usable at once, no duplicate can exist here
	msg[off+3] = ipv6GlobalScope // ifa_scope
	netlinkByteOrder.PutUint32(msg[off+4:off+8], uint32(ifindex))
	off += syscall.SizeofIfAddrmsg

	off = putNetlinkAttr(msg, off, syscall.IFA_LOCAL, ip.To16())
	off = putNetlinkAttr(msg, off, syscall.IFA_ADDRESS, ip.To16())
	if off != len(msg) {
		return nil, errx.With(ErrInvalidIPv6Link, ": address message built %d of %d bytes", off, len(msg))
	}

	return msg, nil
}

// buildGuestRouteMessage serializes an rtnetlink RTM_NEWROUTE request that
// installs the default route (::/0) through gateway on the interface with the
// given ifindex. The layout mirrors what `ip -6 route add default via <gateway>
// dev <iface>` sends: an rtmsg with family AF_INET6, dst_len 0, table main,
// protocol boot, scope universe and type unicast, followed by RTA_DST (::),
// RTA_GATEWAY and RTA_OIF.
//
// The gateway is on-link because it is covered by the guest address's prefix, so
// no separate route to the gateway is needed. NLM_F_CREATE|NLM_F_REPLACE without
// NLM_F_EXCL keeps a re-application idempotent.
func buildGuestRouteMessage(ifindex int, gateway net.IP) ([]byte, error) {
	if ifindex <= 0 {
		return nil, errx.With(ErrInvalidIPv6Link, ": invalid interface index %d", ifindex)
	}
	if gateway == nil || gateway.To4() != nil || gateway.To16() == nil {
		return nil, errx.With(ErrInvalidIPv6Link, ": %q is not an IPv6 gateway address", gateway)
	}

	msg := make([]byte, syscall.SizeofNlMsghdr+syscall.SizeofRtMsg+2*netlinkAddrAttrLen+netlinkUint32AttrLen)

	netlinkByteOrder.PutUint32(msg[0:4], uint32(len(msg)))
	netlinkByteOrder.PutUint16(msg[4:6], syscall.RTM_NEWROUTE)
	netlinkByteOrder.PutUint16(msg[6:8],
		syscall.NLM_F_REQUEST|syscall.NLM_F_ACK|syscall.NLM_F_CREATE|syscall.NLM_F_REPLACE)
	netlinkByteOrder.PutUint32(msg[8:12], netlinkRequestSeq)
	netlinkByteOrder.PutUint32(msg[12:16], 0) // nlmsg_pid: the kernel fills it in

	off := syscall.SizeofNlMsghdr
	msg[off] = syscall.AF_INET6                      // rtm_family
	msg[off+1] = 0                                   // rtm_dst_len: ::/0
	msg[off+2] = 0                                   // rtm_src_len
	msg[off+3] = 0                                   // rtm_tos
	msg[off+4] = syscall.RT_TABLE_MAIN               // rtm_table
	msg[off+5] = syscall.RTPROT_BOOT                 // rtm_protocol
	msg[off+6] = ipv6GlobalScope                     // rtm_scope: a route via a gateway
	msg[off+7] = syscall.RTN_UNICAST                 // rtm_type
	netlinkByteOrder.PutUint32(msg[off+8:off+12], 0) // rtm_flags
	off += syscall.SizeofRtMsg

	oif := make([]byte, 4)
	netlinkByteOrder.PutUint32(oif, uint32(ifindex))

	off = putNetlinkAttr(msg, off, syscall.RTA_DST, net.IPv6zero)
	off = putNetlinkAttr(msg, off, syscall.RTA_GATEWAY, gateway.To16())
	off = putNetlinkAttr(msg, off, syscall.RTA_OIF, oif)
	if off != len(msg) {
		return nil, errx.With(ErrInvalidIPv6Link, ": route message built %d of %d bytes", off, len(msg))
	}

	return msg, nil
}

// putNetlinkAttr writes one rtattr (length, type, 4-byte aligned payload) at off
// and returns the offset just past it.
func putNetlinkAttr(msg []byte, off int, attrType uint16, payload []byte) int {
	length := 4 + len(payload)
	netlinkByteOrder.PutUint16(msg[off:off+2], uint16(length))
	netlinkByteOrder.PutUint16(msg[off+2:off+4], attrType)
	copy(msg[off+4:off+length], payload)
	return off + length
}

// netlinkSender sends one composed rtnetlink request and returns the kernel's
// verdict on it.
type netlinkSender func(msg []byte) error

// applyNetlinkRequest sends msg over rtnetlink and waits for the kernel's ACK.
// IPv6 addresses cannot be installed through the SIOCSIFADDR ioctl the IPv4 path
// uses (it has no room for a 128-bit address), and the IPv4 ip= boot argument
// installs no route at all, so both requests take the netlink round trip.
func applyNetlinkRequest(msg []byte) error {
	fd, err := syscall.Socket(syscall.AF_NETLINK,
		syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_ROUTE)
	if err != nil {
		return errx.Wrap(ErrConfigureGuestIPv6, err)
	}
	defer syscall.Close(fd)

	timeout := syscall.Timeval{Sec: int64(netlinkRequestTimeout / time.Second)}
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &timeout); err != nil {
		return errx.Wrap(ErrConfigureGuestIPv6, err)
	}

	addr := &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}
	if err := syscall.Bind(fd, addr); err != nil {
		return errx.Wrap(ErrConfigureGuestIPv6, err)
	}
	if err := syscall.Sendto(fd, msg, 0, addr); err != nil {
		return errx.Wrap(ErrConfigureGuestIPv6, err)
	}

	buf := make([]byte, netlinkRecvBufferLen)
	for {
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			return errx.Wrap(ErrConfigureGuestIPv6, err)
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return errx.Wrap(ErrConfigureGuestIPv6, err)
		}
		for i := range msgs {
			if msgs[i].Header.Type != syscall.NLMSG_ERROR {
				continue
			}
			if len(msgs[i].Data) < 4 {
				return errx.With(ErrConfigureGuestIPv6, ": truncated NLMSG_ERROR payload (%d bytes)", len(msgs[i].Data))
			}
			// The payload is struct nlmsgerr: a negative errno, 0 means ACK.
			errno := int(int32(netlinkByteOrder.Uint32(msgs[i].Data[:4])))
			if errno == 0 {
				return nil
			}
			// The requests are create-or-replace, so an address or route that
			// is already configured must not surface as a failure.
			if errors.Is(syscall.Errno(-errno), syscall.EEXIST) {
				return nil
			}
			return errx.With(ErrConfigureGuestIPv6, ": %s", syscall.Errno(-errno))
		}
	}
}

// configureGuestIPv6 installs the guest address and the ::/0 default route on
// iface. The route is installed second: it needs the on-link prefix the address
// request creates, and the workload must only ever start with both in place.
func configureGuestIPv6(iface string, link ipv6Link) error {
	return configureGuestIPv6With(iface, link, applyNetlinkRequest)
}

// configureGuestIPv6With is configureGuestIPv6 with the kernel-facing sender
// injected, so the interface lookup, parsing and message building are testable
// without CAP_NET_ADMIN.
func configureGuestIPv6With(iface string, link ipv6Link, send netlinkSender) error {
	device, err := net.InterfaceByName(iface)
	if err != nil {
		return errx.With(ErrConfigureGuestIPv6, " %s: %w", iface, err)
	}

	addrMsg, err := buildGuestAddrMessage(device.Index, link.Guest, link.PrefixLen)
	if err != nil {
		return err
	}
	if err := send(addrMsg); err != nil {
		return err
	}

	routeMsg, err := buildGuestRouteMessage(device.Index, link.Gateway)
	if err != nil {
		return err
	}
	return send(routeMsg)
}
