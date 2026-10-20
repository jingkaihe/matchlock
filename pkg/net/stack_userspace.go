package net

// This file is the userspace network stack the darwin (Virtualization.framework)
// interception path runs: the guest's frames arrive over a datagram socketpair
// and are delivered into a gVisor netstack, whose NIC terminations and
// forwarders feed the same HTTPInterceptor / policy engine the Linux path uses.
//
// It carries no platform build constraint on purpose: the stack itself is pure
// userspace (gVisor + os.File), so its bring-up logic, dual-stack addressing,
// DNS transport and policy ordering are unit-testable on every host. The only
// OS-specific piece is setSocketBufferSizes, which lives in small per-OS
// adapter files (sockopt_darwin.go / sockopt_linux.go).

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/dnsrewrite"
	"github.com/jingkaihe/matchlock/pkg/policy"
	"github.com/jingkaihe/matchlock/pkg/vm"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const (
	// tcpReceiveWindowSize is the receive window advertised to the guest.
	// Larger windows improve throughput on high-bandwidth paths.
	tcpReceiveWindowSize = 4 << 20 // 4MB

	// socketBufSize is the SO_SNDBUF/SO_RCVBUF size for the socket pair.
	// Larger buffers prevent frame drops under burst traffic between VM and host.
	socketBufSize = 2 << 20 // 2MB

	// writeBufSize is the capacity of pooled write buffers for outbound packets.
	writeBufSize = 64 * 1024

	// stackDNSUpstreamTimeout bounds a guest-triggered upstream DNS exchange.
	stackDNSUpstreamTimeout = 5 * time.Second

	// stackDNSMaxConcurrentQueries bounds in-flight guest DNS serves (each
	// holds one goroutine and up to one upstream socket), mirroring the Linux
	// forwarder's bounded worker pool. A query beyond the bound is dropped and
	// the guest resolver retries; the NIC dispatch path itself never blocks.
	stackDNSMaxConcurrentQueries = 32

	// stackDNSMaxQueriesPerEndpoint caps how many datagrams one guest DNS
	// endpoint serves before it is retired. Guests routinely send an A and an
	// AAAA query (and retries) through ONE source socket; reading a single
	// datagram per endpoint would silently discard the queued siblings.
	// Retirement is safe: the next datagram opens a fresh forwarder request.
	stackDNSMaxQueriesPerEndpoint = 16

	// stackDNSSiblingReadWindow bounds how long an endpoint that has already
	// answered stays open waiting for sibling datagrams (guests send an A and
	// an AAAA query together through one source socket). Keeping retirement
	// prompt is what stops idle endpoints from occupying the bounded admission
	// slots; a late query on a retired endpoint is served by a fresh forwarder
	// request.
	stackDNSSiblingReadWindow = 250 * time.Millisecond

	// stackDNSPacketBufSize is the guest-query and upstream-read buffer (4096),
	// matching the Linux forwarder's dnsPacketBufSize. A UDP read truncates a
	// larger datagram to this size and the excess is discarded — a documented
	// residual limitation identical on both platforms (there is no local
	// TC-flag synthesis and no TCP-fallback machinery; see the TCP-53 note in
	// handleTCPConnection).
	stackDNSPacketBufSize = 4096
)

type NetworkStack struct {
	stack       *stack.Stack
	policy      *policy.Engine
	interceptor *HTTPInterceptor
	events      chan api.Event
	linkEP      *socketPairEndpoint
	// gatewayIP is the virtual IPv4 gateway address assigned to the gVisor NIC
	// (Config.GatewayIP). It is guest-visible only: macOS has no TAP, so the
	// address is not bound on the host. Passthrough dials to it are mapped to
	// host loopback by resolvePassthroughTarget.
	gatewayIP string
	// gatewayIPv6 is the virtual IPv6 gateway address assigned to the gVisor
	// NIC (Config.GatewayIPv6), when the sandbox wired the per-VM ULA link.
	// Like its IPv4 sibling it exists only inside the stack; passthrough dials
	// to it map to host ::1.
	gatewayIPv6 string
	// dnsRewriter applies the network.dns_priority answer-mode contract to
	// upstream DNS answers (ipv4-preferring rewrite by default; verbatim in
	// ipv6 mode). Always non-nil after NewNetworkStack; a nil value relays
	// verbatim, which is the same fail-safe direction.
	dnsRewriter *dnsrewrite.Rewriter
	// dnsServers is the immutable resolver list shared by the guest relay and
	// the rewriter's A probes (snapshotted at construction); both walk it IN
	// ORDER with transport-error fallback, so the relayed answer and any A
	// evidence are selected by the same policy.
	dnsServers []string
	// dnsSem bounds in-flight DNS serves (stackDNSMaxConcurrentQueries); dnsWG
	// tracks the workers so Close can wait for them after the stack is down.
	// stopCh is closed by Close; a worker checks it between datagrams and
	// closes its endpoint, which unblocks a blocked guest read.
	dnsSem chan struct{}
	dnsWG  sync.WaitGroup
	stopCh chan struct{}
	mu     sync.Mutex
	closed bool
}

type Config struct {
	FD         int
	File       *os.File // Use this instead of FD when available
	GatewayIP  string
	GuestIP    string
	MTU        uint32
	Policy     *policy.Engine
	Events     chan api.Event
	CAPool     *CAPool
	DNSServers []string
	// GatewayIPv6 / GuestIPv6 / Subnet6CIDR carry the same per-VM IPv6 ULA
	// link the guest is told to install via matchlock.ipv6= (see pkg/vm
	// IPv6Link). When they are empty the stack stays IPv4-only and the boot
	// remains byte-identical to the pre-IPv6 behavior; when they are set the
	// gateway is assigned to the NIC and an IPv6 default route is installed,
	// so guest IPv6 traffic terminates here and is intercepted like IPv4.
	GatewayIPv6 string
	GuestIPv6   string
	Subnet6CIDR string
	// DNSPriority is the network.dns_priority contract value ("ipv4", "ipv6",
	// or empty meaning the fork default "ipv4"): "ipv4" enables the
	// ipv4-preferring AAAA rewrite, "ipv6" relays answers verbatim. An
	// unsupported value fails construction rather than silently degrading to
	// verbatim.
	DNSPriority string
}

// writeBufPool provides reusable buffers for serializing outbound packets
// in WritePackets, which can be called from multiple goroutines concurrently.
var writeBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, writeBufSize)
		return &b
	},
}

// socketPairEndpoint implements stack.LinkEndpoint for Unix socket pairs
type socketPairEndpoint struct {
	file     *os.File
	mtu      uint32
	linkAddr tcpip.LinkAddress
	// dispatcher is read atomically in the hot path; only written on Attach.
	dispatcher    atomic.Pointer[stack.NetworkDispatcher]
	closed        atomic.Bool
	closeCh       chan struct{}
	mu            sync.Mutex // protects onCloseAction and linkAddr
	onCloseAction func()
}

func newSocketPairEndpoint(file *os.File, mtu uint32) *socketPairEndpoint {
	mac := tcpip.LinkAddress([]byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x01})
	return &socketPairEndpoint{
		file:     file,
		mtu:      mtu,
		linkAddr: mac,
		closeCh:  make(chan struct{}),
	}
}

func (e *socketPairEndpoint) MTU() uint32 {
	return e.mtu
}

func (e *socketPairEndpoint) SetMTU(mtu uint32) {
	e.mtu = mtu
}

func (e *socketPairEndpoint) MaxHeaderLength() uint16 {
	return header.EthernetMinimumSize
}

func (e *socketPairEndpoint) LinkAddress() tcpip.LinkAddress {
	return e.linkAddr
}

func (e *socketPairEndpoint) Capabilities() stack.LinkEndpointCapabilities {
	return stack.CapabilityResolutionRequired
}

func (e *socketPairEndpoint) Attach(dispatcher stack.NetworkDispatcher) {
	e.dispatcher.Store(&dispatcher)

	if dispatcher != nil {
		go e.readLoop()
	}
}

func (e *socketPairEndpoint) IsAttached() bool {
	d := e.dispatcher.Load()
	return d != nil && *d != nil
}

func (e *socketPairEndpoint) Wait() {}

func (e *socketPairEndpoint) SetLinkAddress(addr tcpip.LinkAddress) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.linkAddr = addr
}

func (e *socketPairEndpoint) ARPHardwareType() header.ARPHardwareType {
	return header.ARPHardwareEther
}

func (e *socketPairEndpoint) AddHeader(pkt *stack.PacketBuffer) {
	eth := header.Ethernet(pkt.LinkHeader().Push(header.EthernetMinimumSize))
	eth.Encode(&header.EthernetFields{
		SrcAddr: pkt.EgressRoute.LocalLinkAddress,
		DstAddr: pkt.EgressRoute.RemoteLinkAddress,
		Type:    pkt.NetworkProtocolNumber,
	})
}

func (e *socketPairEndpoint) ParseHeader(pkt *stack.PacketBuffer) bool {
	_, ok := pkt.LinkHeader().Consume(header.EthernetMinimumSize)
	return ok
}

func (e *socketPairEndpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
	if e.closed.Load() {
		return 0, &tcpip.ErrClosedForSend{}
	}

	// Grab a write buffer from the pool; safe for concurrent callers.
	bp := writeBufPool.Get().(*[]byte)
	wb := *bp

	var written int
	for _, pkt := range pkts.AsSlice() {
		views := pkt.AsSlices()
		wb = wb[:0]
		for _, v := range views {
			wb = append(wb, v...)
		}
		if _, err := e.file.Write(wb); err != nil {
			*bp = wb
			writeBufPool.Put(bp)
			return written, &tcpip.ErrAborted{}
		}
		written++
	}

	*bp = wb
	writeBufPool.Put(bp)
	return written, nil
}

func (e *socketPairEndpoint) readLoop() {
	buf := make([]byte, e.mtu+header.EthernetMinimumSize)
	for {
		select {
		case <-e.closeCh:
			return
		default:
		}

		n, err := e.file.Read(buf)
		if err != nil {
			if e.closed.Load() {
				return
			}
			continue
		}

		if n < header.EthernetMinimumSize {
			continue
		}

		eth := header.Ethernet(buf[:n])
		proto := eth.Type()

		// We must allocate per packet because gVisor's TCP stack may hold
		// the underlying buffer.Buffer chunk in its receive/reassembly queue
		// long after the PacketBuffer is released. Pooling the byte slice via
		// OnRelease would cause use-after-free corruption under sustained load.
		data := make([]byte, n)
		copy(data, buf[:n])

		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			Payload: buffer.MakeWithData(data),
		})

		if !e.ParseHeader(pkt) {
			pkt.DecRef()
			continue
		}

		dp := e.dispatcher.Load()
		if dp != nil && *dp != nil {
			(*dp).DeliverNetworkPacket(proto, pkt)
		}
		pkt.DecRef()
	}
}

func (e *socketPairEndpoint) SetOnCloseAction(action func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onCloseAction = action
}

func (e *socketPairEndpoint) Close() {
	if !e.closed.CompareAndSwap(false, true) {
		return
	}

	e.mu.Lock()
	onClose := e.onCloseAction
	e.mu.Unlock()

	if onClose != nil {
		onClose()
	}
	close(e.closeCh)
	e.file.Close()
}

// ipv6ProtocolFactory brings up IPv6 with duplicate address detection DISABLED
// (DupAddrDetectTransmits: 0). The stack's gateway address is unique by
// construction (a per-VM ULA derived from the subnet lease), so DAD could only
// hold the address TENTATIVE — unusable for NDP responses — during its
// retransmit window right after sandbox creation. The guest side mirrors this
// intent with IFA_F_NODAD on its ULA; the stack must be usable the moment the
// guest boots.
func ipv6ProtocolFactory() stack.NetworkProtocolFactory {
	return ipv6.NewProtocolWithOptions(ipv6.Options{
		DADConfigs: stack.DADConfigurations{
			DupAddrDetectTransmits: 0,
			RetransmitTimer:        time.Second,
		},
	})
}

func NewNetworkStack(cfg *Config) (*NetworkStack, error) {
	// Resolve the IPv6 link first: a malformed or partial triple must fail
	// before any resource is created, and the DHCP/darwin NAT path never
	// reaches here with IPv6 fields (the sandbox gates them on interception).
	var (
		link    vm.IPv6Link
		hasLink bool
	)
	if cfg.GatewayIPv6 != "" || cfg.GuestIPv6 != "" || cfg.Subnet6CIDR != "" {
		var err error
		link, hasLink, err = vm.ParseIPv6LinkFromStrings(cfg.GatewayIPv6, cfg.GuestIPv6, cfg.Subnet6CIDR)
		if err != nil {
			return nil, fmt.Errorf("invalid ipv6 link: %v", err)
		}
		if !hasLink {
			return nil, fmt.Errorf("invalid ipv6 link: gateway %q, guest %q and prefix %q must be set together",
				cfg.GatewayIPv6, cfg.GuestIPv6, cfg.Subnet6CIDR)
		}
	}

	// Fail closed on a missing policy engine: the stack exists to enforce the
	// allowlist and the private-destination block, so a stack without one
	// could only pass traffic unchecked. Production always supplies an
	// engine; rejecting here keeps every handler from inventing its own
	// nil-policy behavior.
	if cfg.Policy == nil {
		return nil, ErrStackNoPolicy
	}

	// ONE immutable resolver list serves both consumers: the guest query
	// relay and the A-existence probe. Snapshotting here (instead of sharing
	// the caller's slice) matches the Linux forwarder's contract and keeps
	// the original answer and any cached A evidence on the same resolver
	// configuration for the life of the stack.
	servers := append([]string(nil), cfg.DNSServers...)

	// Build the DNS answer-mode rewriter before the stack exists so a bad
	// priority cannot leave a half-started stack behind. ModeForPriority
	// rejects anything but "", "ipv4" and "ipv6"; the empty value is the fork
	// default (ipv4-preferring), pinned here, at the production constructor.
	mode, err := dnsrewrite.ModeForPriority(cfg.DNSPriority)
	if err != nil {
		return nil, err
	}
	// The allow_private carve-out source is the policy engine's normalized
	// name matcher, exactly the entries the policy path itself uses.
	dnsRewriter, err := dnsrewrite.New(dnsrewrite.Config{
		Mode:             mode,
		AllowPrivateName: cfg.Policy.MatchesAllowPrivateName,
		Servers:          servers,
		Exchange: func(query []byte, server string, deadline time.Time) ([]byte, error) {
			return exchangeDNSUntil(query, server, deadline)
		},
	})
	if err != nil {
		return nil, err
	}

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6ProtocolFactory(), arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	// fail tears down everything construction has already made: the rewriter
	// (whose probe goroutines are bounded but should be told to stop), the
	// stack itself, and — once it exists — the link endpoint, whose Close
	// stops its reader even if another owner (the sandbox's socket pair)
	// closes the underlying file first. linkEP is hoisted so the guard is
	// structural, not a matter of which statement happens to run first.
	var linkEP *socketPairEndpoint
	fail := func(err error) (*NetworkStack, error) {
		dnsRewriter.Close()
		if linkEP != nil {
			linkEP.Close()
		}
		s.Close()
		return nil, err
	}

	// Tune TCP buffer sizes: raise max to allow auto-tuning up to 16MB
	// for better throughput on high-bandwidth paths.
	tcpSendBuf := tcpip.TCPSendBufferSizeRangeOption{
		Min:     tcp.MinBufferSize,         // 4KB
		Default: tcp.DefaultSendBufferSize, // 1MB
		Max:     16 << 20,                  // 16MB
	}
	tcpRecvBuf := tcpip.TCPReceiveBufferSizeRangeOption{
		Min:     tcp.MinBufferSize,            // 4KB
		Default: tcp.DefaultReceiveBufferSize, // 1MB
		Max:     16 << 20,                     // 16MB
	}
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &tcpSendBuf)
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &tcpRecvBuf)

	file := cfg.File
	if file == nil {
		file = os.NewFile(uintptr(cfg.FD), "network")
	}

	// Increase socket pair buffer sizes to prevent frame drops under burst.
	setSocketBufferSizes(file)

	linkEP = newSocketPairEndpoint(file, cfg.MTU)

	if tcpipErr := s.CreateNIC(1, linkEP); tcpipErr != nil {
		return fail(fmt.Errorf("failed to create NIC: %v", tcpipErr))
	}

	gatewayAddr := tcpip.AddrFromSlice(net.ParseIP(cfg.GatewayIP).To4())
	protoAddr := tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: gatewayAddr.WithPrefix(),
	}
	if tcpipErr := s.AddProtocolAddress(1, protoAddr, stack.AddressProperties{}); tcpipErr != nil {
		return fail(fmt.Errorf("failed to add address: %v", tcpipErr))
	}

	// Dual-stack bring-up: the SAME per-VM ULA gateway the guest installs as
	// its default route (matchlock.ipv6=) is assigned to this NIC, so guest
	// IPv6 packets terminate here (NDP for the gateway is answered by the
	// stack itself) and are dispatched into the same TCP/UDP forwarders and
	// policy checks as IPv4. The guest address is not needed by the stack -
	// promiscuous+spoofing receive any destination - but the triple is
	// validated jointly above, exactly as the guest's boot argument derives
	// from it.
	if hasLink {
		gw := net.ParseIP(link.Gateway)
		protoAddr6 := tcpip.ProtocolAddress{
			Protocol: ipv6.ProtocolNumber,
			AddressWithPrefix: tcpip.AddressWithPrefix{
				Address:   tcpip.AddrFromSlice(gw.To16()),
				PrefixLen: link.PrefixLen,
			},
		}
		if tcpipErr := s.AddProtocolAddress(1, protoAddr6, stack.AddressProperties{}); tcpipErr != nil {
			return fail(fmt.Errorf("failed to add ipv6 address: %v", tcpipErr))
		}
	}

	routes := []tcpip.Route{{
		Destination: header.IPv4EmptySubnet,
		NIC:         1,
	}}
	if hasLink {
		routes = append(routes, tcpip.Route{
			Destination: header.IPv6EmptySubnet,
			NIC:         1,
		})
	}
	s.SetRouteTable(routes)

	if tcpipErr := s.SetPromiscuousMode(1, true); tcpipErr != nil {
		return fail(fmt.Errorf("failed to enable promiscuous mode: %v", tcpipErr))
	}
	if tcpipErr := s.SetSpoofing(1, true); tcpipErr != nil {
		return fail(fmt.Errorf("failed to enable spoofing: %v", tcpipErr))
	}

	ns := &NetworkStack{
		stack:       s,
		policy:      cfg.Policy,
		events:      cfg.Events,
		linkEP:      linkEP,
		dnsServers:  servers,
		gatewayIP:   cfg.GatewayIP,
		gatewayIPv6: cfg.GatewayIPv6,
		dnsRewriter: dnsRewriter,
		dnsSem:      make(chan struct{}, stackDNSMaxConcurrentQueries),
		stopCh:      make(chan struct{}),
	}

	ns.interceptor = NewHTTPInterceptor(cfg.Policy, cfg.Events, cfg.CAPool)
	// Only the userspace stack maps the guest-visible virtual gateways to host
	// loopback; the Linux TransparentProxy leaves these empty.
	ns.interceptor.gatewayIP = cfg.GatewayIP
	ns.interceptor.gatewayIPv6 = cfg.GatewayIPv6

	tcpForwarder := tcp.NewForwarder(s, tcpReceiveWindowSize, 65535, ns.handleTCPConnection)
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpForwarder.HandlePacket)

	udpForwarder := udp.NewForwarder(s, ns.handleUDPPacket)
	s.SetTransportProtocolHandler(udp.ProtocolNumber, udpForwarder.HandlePacket)

	return ns, nil
}

func (ns *NetworkStack) handleTCPConnection(r *tcp.ForwarderRequest) {
	id := r.ID()
	dstPort := id.LocalPort

	var wq waiter.Queue
	ep, tcpipErr := r.CreateEndpoint(&wq)
	if tcpipErr != nil {
		r.Complete(true)
		return
	}

	r.Complete(false)
	guestConn := gonet.NewTCPConn(&wq, ep)

	dstIP := id.LocalAddress.String()

	switch dstPort {
	case 80:
		go ns.interceptor.HandleHTTP(guestConn, dstIP, int(dstPort))
	case 443:
		go ns.interceptor.HandleHTTPS(guestConn, dstIP, int(dstPort))
	default:
		// TCP DNS (port 53) deliberately stays on this policy-gated
		// passthrough path instead of being redirected into the resolver: the
		// resolver's dns_priority answer-mode semantics apply to UDP DNS only
		// (the Linux forwarder is UDP-only as well), so over TCP an ALLOWED
		// destination's AAAA records are returned unchanged even in the
		// default ipv4 mode. That is a documented transport gap in the DNS
		// preference contract, not a policy bypass — the allowlist check
		// below still runs first, against the ORIGINAL destination, before
		// any dial. Making TCP-53 resolver-backed is a separate, deliberate
		// change (it would need a TCP DNS listener and the same rewrite
		// semantics), not a silent widening of access.
		host := fmt.Sprintf("%s:%d", dstIP, dstPort)
		// Port-aware policy check so a port-scoped allow_private entry is
		// honored; the destination is the ORIGINAL guest-visible one. A nil
		// policy is treated as deny (fail closed): production always supplies
		// an engine, and a stack without one must not pass traffic.
		if ns.policy == nil || !ns.policy.IsHostAllowedPort(dstIP, int(dstPort)) {
			ns.emitBlockedEvent(host, "host not in allowlist")
			guestConn.Close()
			return
		}
		go ns.handlePassthrough(guestConn, dstIP, int(dstPort))
	}
}

func (ns *NetworkStack) handlePassthrough(guestConn net.Conn, dstIP string, dstPort int) {
	defer guestConn.Close()

	// Policy is always evaluated against the ORIGINAL guest-visible destination,
	// never the mapped dial target: removing the gateway IP from the allowlist
	// must still close alt-port TCP. This holds for IPv4 and IPv6 destinations
	// alike. A nil policy is treated as deny (fail closed).
	if ns.policy == nil || !ns.policy.IsHostAllowedPort(dstIP, int(dstPort)) {
		ns.emitBlockedEvent(net.JoinHostPort(dstIP, fmt.Sprintf("%d", dstPort)), "host not in allowlist")
		return
	}

	// The guest's default routes point at the gVisor netstack's virtual
	// gateways, which are not host addresses. Dial host loopback for those two
	// addresses (127.0.0.1 for the IPv4 gateway, ::1 for the IPv6 one); every
	// other destination dials literally.
	dialTarget := resolvePassthroughTarget(dstIP, dstPort, ns.gatewayIP, ns.gatewayIPv6)

	realConn, err := net.Dial("tcp", dialTarget)
	if err != nil {
		slog.Debug("passthrough dial failed",
			"destination", net.JoinHostPort(dstIP, fmt.Sprintf("%d", dstPort)),
			"dialTarget", dialTarget,
			"error", err,
		)
		return
	}
	defer realConn.Close()

	// Relay both directions through the shared half-close relay: each direction
	// is copied independently and its EOF is propagated as a write-side
	// half-close instead of tearing the connection down the moment the first
	// direction sees EOF. A guest client that sends a request and immediately
	// half-closes (`echo PAYLOAD | nc HOST PORT` does exactly this) must still
	// receive the upstream response; the previous context.WithCancel version
	// cancelled the pending upstream->guest copy on the guest's first EOF, which
	// surfaced on the wire as a clean `RC=0` with no payload. guestConn is a
	// *gonet.TCPConn and realConn is a *net.TCPConn, both of which implement
	// CloseWrite, so the half-close propagates end-to-end.
	relayHalfClose(guestConn, realConn)
}

func (ns *NetworkStack) handleUDPPacket(r *udp.ForwarderRequest) bool {
	id := r.ID()

	if id.LocalPort == 53 {
		// The DNS exchange (upstream dial plus, in ipv4-preferring mode, an
		// A-existence probe) is performed on a bounded worker goroutine: this
		// callback runs on the NIC's packet-dispatch path, and blocking it on
		// upstream I/O would stall every other packet, including the NDP
		// exchanges the link needs. The endpoint is created synchronously so
		// the guest's socket is served right away; the worker owns it and
		// closes it when done, bounded by deadlines.
		//
		// Admission is refused once the stack is closing, and a query beyond
		// the concurrency bound is dropped (the guest resolver retries),
		// mirroring the Linux forwarder's bounded pool.
		var wq waiter.Queue
		ep, tcpipErr := r.CreateEndpoint(&wq)
		if tcpipErr != nil {
			return true
		}
		negotiated := gonet.NewUDPConn(&wq, ep)

		ns.mu.Lock()
		if ns.closed {
			ns.mu.Unlock()
			negotiated.Close()
			return true
		}
		select {
		case ns.dnsSem <- struct{}{}:
			ns.dnsWG.Add(1)
			ns.mu.Unlock()
			go func() {
				defer func() {
					<-ns.dnsSem
					ns.dnsWG.Done()
				}()
				ns.serveDNSQueries(negotiated)
			}()
		default:
			ns.mu.Unlock()
			negotiated.Close()
		}
		return true
	}

	// Non-DNS UDP: silently drop by not creating an endpoint.
	//
	// There is deliberately no UDP passthrough here, so there is no passthrough
	// dial target to map for the virtual gateway. The DNS serve path dials the
	// configured upstream DNS servers (real host addresses) directly, never the
	// guest's gateway.
	return true
}

// serveDNSQueries serves datagrams from ONE guest DNS endpoint until its
// retirement cap is reached, the idle read deadline expires, or the stack is
// closing. Guests routinely send an A and an AAAA query (and retries) through
// one source socket, so reading a single datagram per endpoint would silently
// discard the queued siblings; the cap only bounds how long one endpoint is
// held open. Every datagram is answered through the same upstream exchange and
// the same dns_priority contract.
func (ns *NetworkStack) serveDNSQueries(guestConn *gonet.UDPConn) {
	defer guestConn.Close()

	for served := 0; served < stackDNSMaxQueriesPerEndpoint; served++ {
		select {
		case <-ns.stopCh:
			return
		default:
		}

		// The first read waits the full upstream timeout for the query that
		// brought this endpoint into existence; later reads only wait for the
		// short sibling window (an A/AAAA pair arrives within milliseconds),
		// so an endpoint that has already answered cannot hold a bounded
		// admission slot for the full timeout while doing no work.
		readTimeout := stackDNSUpstreamTimeout
		if served > 0 {
			readTimeout = stackDNSSiblingReadWindow
		}
		_ = guestConn.SetReadDeadline(time.Now().Add(readTimeout))
		buf := make([]byte, stackDNSPacketBufSize)
		n, _, err := guestConn.ReadFrom(buf)
		if err != nil {
			return
		}
		ns.answerDNS(guestConn, buf[:n])
	}
}

// answerDNS relays one guest DNS query upstream, applies the dns_priority
// answer-mode contract to the response, and writes it back on the same socket.
func (ns *NetworkStack) answerDNS(guestConn *gonet.UDPConn, query []byte) {
	if len(ns.dnsServers) == 0 {
		slog.Debug("dropping DNS query; no upstream servers configured")
		return
	}
	// Resolver selection matches the Linux resolver contract AND the
	// rewriter's probe path: walk the immutable list IN ORDER, falling through
	// to the next server only on a TRANSPORT error; the first received
	// response is final. One absolute deadline covers the whole fallback
	// list, and a server that is reached after the budget expired is never
	// dialed (its dial would use a non-positive timeout).
	deadline := time.Now().Add(stackDNSUpstreamTimeout)
	var resp []byte
	var err error
	server := ""
	for _, candidate := range ns.dnsServers {
		if !time.Now().Before(deadline) {
			err = fmt.Errorf("DNS fallback budget exhausted before %s", candidate)
			break
		}
		server = candidate
		resp, err = exchangeDNSUntil(query, candidate, deadline)
		if err == nil {
			break
		}
	}
	if err != nil {
		slog.Debug("DNS upstream exchange failed", "server", server, "error", err)
		return
	}

	// The network.dns_priority contract (same semantics as the Linux
	// resolver): "ipv4" (default) rewrites an eligible AAAA answer into a
	// synthesized NODATA response when the name demonstrably has an A record;
	// "ipv6" relays verbatim. allow_private names are always verbatim, a query
	// carrying a context-affecting EDNS option (ECS) is always verbatim, and
	// every uncertainty inside the rewriter fails safe to the verbatim relay;
	// a nil rewriter (pre-construction test stacks) relays verbatim too.
	if ns.dnsRewriter != nil {
		resp = ns.dnsRewriter.Rewrite(query, resp)
	}

	_ = guestConn.SetWriteDeadline(time.Now().Add(stackDNSUpstreamTimeout))
	_, _ = guestConn.Write(resp)
}

// exchangeDNSUntil sends query to server and returns the raw upstream
// response, bounded by ONE absolute deadline that covers the dial and the
// socket wait. It is the transport seam the DNS rewriter probes and the guest
// query path both use.
func exchangeDNSUntil(query []byte, server string, deadline time.Time) ([]byte, error) {
	dnsConn, err := net.DialTimeout("udp", stackDNSServerAddr(server), time.Until(deadline))
	if err != nil {
		return nil, err
	}
	defer dnsConn.Close()

	// Bound write+read so host-side UDP disruption cannot pin this goroutine.
	if err := dnsConn.SetDeadline(deadline); err != nil {
		slog.Warn("set DNS deadline failed", "server", server, "error", err)
		return nil, err
	}

	if _, err := dnsConn.Write(query); err != nil {
		return nil, err
	}

	resp := make([]byte, stackDNSPacketBufSize)
	respN, err := dnsConn.Read(resp)
	if err != nil {
		return nil, err
	}

	return resp[:respN], nil
}

// exchangeDNS is exchangeDNSUntil with a relative timeout.
func exchangeDNS(query []byte, server string, timeout time.Duration) ([]byte, error) {
	return exchangeDNSUntil(query, server, time.Now().Add(timeout))
}

func stackDNSServerAddr(server string) string {
	if _, _, err := net.SplitHostPort(server); err == nil {
		return server
	}
	return net.JoinHostPort(server, "53")
}

func (ns *NetworkStack) emitBlockedEvent(host, reason string) {
	if ns.events != nil {
		select {
		case ns.events <- api.Event{
			Type: "network",
			Network: &api.NetworkEvent{
				Host:        host,
				Blocked:     true,
				BlockReason: reason,
			},
		}:
		default:
		}
	}
}

func (ns *NetworkStack) Close() error {
	ns.mu.Lock()
	if ns.closed {
		ns.mu.Unlock()
		return nil
	}
	// Mark closed BEFORE releasing the lock so no new DNS worker can be
	// admitted after this point, then release the lock before any blocking
	// operation: closing the link endpoint unblocks the read loop, and the
	// DNS workers observe stopCh between datagrams.
	ns.closed = true
	close(ns.stopCh)
	ns.mu.Unlock()

	if ns.dnsRewriter != nil {
		ns.dnsRewriter.Close()
	}
	ns.linkEP.Close()
	ns.stack.Close()
	ns.dnsWG.Wait()
	return nil
}

func (ns *NetworkStack) Stack() *stack.Stack {
	return ns.stack
}
