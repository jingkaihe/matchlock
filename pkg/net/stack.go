package net

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/policy"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/fdbased"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

type NetworkStack struct {
	stack       *stack.Stack
	policy      *policy.Engine
	interceptor *HTTPInterceptor
	events      chan api.Event
	linkEP      stack.LinkEndpoint
	mu          sync.Mutex
	closed      bool
}

type Config struct {
	FD        int
	GatewayIP string
	GuestIP   string
	MTU       uint32
	Policy    *policy.Engine
	Events    chan api.Event
}

func NewNetworkStack(cfg *Config) (*NetworkStack, error) {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})

	linkEP, err := fdbased.New(&fdbased.Options{
		FDs:            []int{cfg.FD},
		MTU:            cfg.MTU,
		EthernetHeader: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create link endpoint: %w", err)
	}

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

	ns.interceptor = NewHTTPInterceptor(cfg.Policy, cfg.Events)

	tcpForwarder := tcp.NewForwarder(s, 0, 65535, ns.handleTCPConnection)
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
	host := fmt.Sprintf("%s:%d", dstIP, dstPort)

	if !ns.policy.IsHostAllowed(host) {
		ns.emitBlockedEvent(host, "host not in allowlist")
		guestConn.Close()
		return
	}

	switch dstPort {
	case 80:
		go ns.interceptor.HandleHTTP(guestConn, dstIP, int(dstPort))
	case 443:
		go ns.interceptor.HandleHTTPS(guestConn, dstIP, int(dstPort))
	default:
		go ns.handlePassthrough(guestConn, dstIP, int(dstPort))
	}
}

func (ns *NetworkStack) handlePassthrough(guestConn net.Conn, dstIP string, dstPort int) {
	defer guestConn.Close()

	if !ns.policy.IsHostAllowed(dstIP) {
		ns.emitBlockedEvent(fmt.Sprintf("%s:%d", dstIP, dstPort), "host not in allowlist")
		return
	}

	realConn, err := net.Dial("tcp", fmt.Sprintf("%s:%d", dstIP, dstPort))
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

	ns.stack.Close()
	return nil
}

func (ns *NetworkStack) Stack() *stack.Stack {
	return ns.stack
}
