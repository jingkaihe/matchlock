//go:build darwin

package net

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/policy"

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
)

type NetworkStack struct {
	stack       *stack.Stack
	policy      *policy.Engine
	interceptor *HTTPInterceptor
	events      chan api.Event
	linkEP      *socketPairEndpoint
	mu          sync.Mutex
	closed      bool
}

type Config struct {
	FD        int
	File      *os.File // Use this instead of FD when available
	GatewayIP string
	GuestIP   string
	MTU       uint32
	Policy    *policy.Engine
	Events    chan api.Event
	CAPool    *CAPool
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
	dispatcher atomic.Pointer[stack.NetworkDispatcher]
	closed     atomic.Bool
	closeCh    chan struct{}
	mu         sync.Mutex // protects onCloseAction and linkAddr
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

func NewNetworkStack(cfg *Config) (*NetworkStack, error) {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})

	// Tune TCP buffer sizes: raise max to allow auto-tuning up to 16MB
	// for better throughput on high-bandwidth paths.
	tcpSendBuf := tcpip.TCPSendBufferSizeRangeOption{
		Min:     tcp.MinBufferSize,          // 4KB
		Default: tcp.DefaultSendBufferSize,  // 1MB
		Max:     16 << 20,                   // 16MB
	}
	tcpRecvBuf := tcpip.TCPReceiveBufferSizeRangeOption{
		Min:     tcp.MinBufferSize,             // 4KB
		Default: tcp.DefaultReceiveBufferSize,  // 1MB
		Max:     16 << 20,                      // 16MB
	}
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &tcpSendBuf)
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &tcpRecvBuf)

	file := cfg.File
	if file == nil {
		file = os.NewFile(uintptr(cfg.FD), "network")
	}

	// Increase socket pair buffer sizes to prevent frame drops under burst.
	setSocketBufferSizes(file)

	linkEP := newSocketPairEndpoint(file, cfg.MTU)

	if tcpipErr := s.CreateNIC(1, linkEP); tcpipErr != nil {
		return nil, fmt.Errorf("failed to create NIC: %v", tcpipErr)
	}

	gatewayAddr := tcpip.AddrFromSlice(net.ParseIP(cfg.GatewayIP).To4())
	protoAddr := tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: gatewayAddr.WithPrefix(),
	}
	if tcpipErr := s.AddProtocolAddress(1, protoAddr, stack.AddressProperties{}); tcpipErr != nil {
		return nil, fmt.Errorf("failed to add address: %v", tcpipErr)
	}

	s.SetRouteTable([]tcpip.Route{{
		Destination: header.IPv4EmptySubnet,
		NIC:         1,
	}})

	s.SetPromiscuousMode(1, true)
	s.SetSpoofing(1, true)

	ns := &NetworkStack{
		stack:  s,
		policy: cfg.Policy,
		events: cfg.Events,
		linkEP: linkEP,
	}

	ns.interceptor = NewHTTPInterceptor(cfg.Policy, cfg.Events, cfg.CAPool)

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
		host := fmt.Sprintf("%s:%d", dstIP, dstPort)
		if !ns.policy.IsHostAllowed(host) {
			ns.emitBlockedEvent(host, "host not in allowlist")
			guestConn.Close()
			return
		}
		go ns.handlePassthrough(guestConn, dstIP, int(dstPort))
	}
}

func (ns *NetworkStack) handlePassthrough(guestConn net.Conn, dstIP string, dstPort int) {
	defer guestConn.Close()

	if !ns.policy.IsHostAllowed(dstIP) {
		ns.emitBlockedEvent(net.JoinHostPort(dstIP, fmt.Sprintf("%d", dstPort)), "host not in allowlist")
		return
	}

	realConn, err := net.Dial("tcp", net.JoinHostPort(dstIP, fmt.Sprintf("%d", dstPort)))
	if err != nil {
		return
	}
	defer realConn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		copyWithCancel(ctx, realConn, guestConn)
		cancel()
	}()
	go func() {
		copyWithCancel(ctx, guestConn, realConn)
		cancel()
	}()

	<-ctx.Done()
}

func copyWithCancel(ctx context.Context, dst, src net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, err := src.Read(buf)
		if n > 0 {
			dst.Write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func (ns *NetworkStack) handleUDPPacket(r *udp.ForwarderRequest) bool {
	id := r.ID()

	if id.LocalPort == 53 {
		ns.handleDNS(r)
		return true
	}

	r.CreateEndpoint(nil)
	return true
}

func (ns *NetworkStack) handleDNS(r *udp.ForwarderRequest) {
	var wq waiter.Queue
	ep, tcpipErr := r.CreateEndpoint(&wq)
	if tcpipErr != nil {
		return
	}

	guestConn := gonet.NewUDPConn(&wq, ep)
	defer guestConn.Close()

	buf := make([]byte, 512)
	n, _, err := guestConn.ReadFrom(buf)
	if err != nil {
		return
	}

	dnsConn, err := net.Dial("udp", "8.8.8.8:53")
	if err != nil {
		return
	}
	defer dnsConn.Close()

	dnsConn.Write(buf[:n])

	resp := make([]byte, 512)
	respN, err := dnsConn.Read(resp)
	if err != nil {
		return
	}

	guestConn.Write(resp[:respN])
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
	defer ns.mu.Unlock()

	if ns.closed {
		return nil
	}
	ns.closed = true

	ns.linkEP.Close()
	ns.stack.Close()
	return nil
}

func (ns *NetworkStack) Stack() *stack.Stack {
	return ns.stack
}

