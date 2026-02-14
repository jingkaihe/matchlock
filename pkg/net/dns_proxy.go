package net

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jingkaihe/matchlock/internal/errx"
)

type DNSProxyConfig struct {
	BindAddr       string
	Port           int
	UpstreamServer []string
	UpstreamDialer UpstreamDialer
}

type DNSProxy struct {
	udpConn net.PacketConn
	tcpLn   net.Listener

	servers []string
	dialer  UpstreamDialer
	port    int

	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

func NewDNSProxy(cfg *DNSProxyConfig) (*DNSProxy, error) {
	if cfg == nil || strings.TrimSpace(cfg.BindAddr) == "" || len(cfg.UpstreamServer) == 0 {
		return nil, ErrDNSProxyConfig
	}

	dialer := cfg.UpstreamDialer
	if dialer == nil {
		dialer = NewSystemDialer()
	}

	udpConn, err := net.ListenPacket("udp", net.JoinHostPort(cfg.BindAddr, portString(cfg.Port)))
	if err != nil {
		return nil, errx.With(ErrDNSProxyListen, " UDP on %s:%d: %w", cfg.BindAddr, cfg.Port, err)
	}

	udpAddr, ok := udpConn.LocalAddr().(*net.UDPAddr)
	if !ok {
		udpConn.Close()
		return nil, ErrDNSProxyConfig
	}

	tcpLn, err := net.Listen("tcp", net.JoinHostPort(cfg.BindAddr, portString(udpAddr.Port)))
	if err != nil {
		_ = udpConn.Close()
		return nil, errx.With(ErrDNSProxyListen, " TCP on %s:%d: %w", cfg.BindAddr, udpAddr.Port, err)
	}

	return &DNSProxy{
		udpConn: udpConn,
		tcpLn:   tcpLn,
		servers: cfg.UpstreamServer,
		dialer:  dialer,
		port:    udpAddr.Port,
	}, nil
}

func (p *DNSProxy) Port() int {
	if p == nil {
		return 0
	}
	return p.port
}

func (p *DNSProxy) Start() {
	if p == nil {
		return
	}
	p.wg.Add(2)
	go p.udpLoop()
	go p.tcpLoop()
}

func (p *DNSProxy) Close() error {
	if p == nil {
		return nil
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()

	var errs []error
	if p.udpConn != nil {
		if err := p.udpConn.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if p.tcpLn != nil {
		if err := p.tcpLn.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	p.wg.Wait()
	return errors.Join(errs...)
}

func (p *DNSProxy) udpLoop() {
	defer p.wg.Done()

	buf := make([]byte, 4096)
	for {
		n, clientAddr, err := p.udpConn.ReadFrom(buf)
		if err != nil {
			if p.isClosed() {
				return
			}
			continue
		}

		packet := make([]byte, n)
		copy(packet, buf[:n])

		go p.handleUDPQuery(clientAddr, packet)
	}
}

func (p *DNSProxy) tcpLoop() {
	defer p.wg.Done()

	for {
		conn, err := p.tcpLn.Accept()
		if err != nil {
			if p.isClosed() {
				return
			}
			continue
		}
		go p.handleTCPQuery(conn)
	}
}

func (p *DNSProxy) handleUDPQuery(clientAddr net.Addr, query []byte) {
	servers := p.serverAddrs()
	var fallbackResp []byte

	for _, target := range servers {
		resp, err := p.queryUDP(target, query)
		if err != nil {
			continue
		}

		// SERVFAIL is commonly returned by quad100 when tailnet DNS config
		// lacks public resolvers. Try fallback DNS servers before returning.
		if dnsResponseRCode(resp) == 2 {
			fallbackResp = resp
			continue
		}

		_, _ = p.udpConn.WriteTo(resp, clientAddr)
		return
	}

	// If every upstream failed but one returned a DNS response (typically
	// SERVFAIL), return that response so the client gets a deterministic error.
	if len(fallbackResp) > 0 {
		_, _ = p.udpConn.WriteTo(fallbackResp, clientAddr)
	}
}

func (p *DNSProxy) handleTCPQuery(clientConn net.Conn) {
	defer clientConn.Close()

	target := p.serverAddrs()[0]

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	upstreamConn, err := p.dialer.DialContext(ctx, "tcp", target)
	cancel()
	if err != nil {
		return
	}
	defer upstreamConn.Close()

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstreamConn, clientConn)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(clientConn, upstreamConn)
		done <- struct{}{}
	}()

	<-done
	_ = clientConn.SetDeadline(time.Now())
	_ = upstreamConn.SetDeadline(time.Now())
	<-done
}

func (p *DNSProxy) serverAddrs() []string {
	out := make([]string, 0, len(p.servers))
	for _, server := range p.servers {
		if _, _, err := net.SplitHostPort(server); err == nil {
			out = append(out, server)
			continue
		}
		out = append(out, net.JoinHostPort(server, "53"))
	}
	return out
}

func (p *DNSProxy) queryUDP(target string, query []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	upstream, err := p.dialer.DialContext(ctx, "udp", target)
	cancel()
	if err != nil {
		return nil, err
	}
	defer upstream.Close()

	_ = upstream.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := upstream.Write(query); err != nil {
		return nil, err
	}

	resp := make([]byte, 4096)
	n, err := upstream.Read(resp)
	if err != nil {
		return nil, err
	}
	return resp[:n], nil
}

func dnsResponseRCode(resp []byte) byte {
	if len(resp) < 4 {
		return 0
	}
	return resp[3] & 0x0f
}

func (p *DNSProxy) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

func portString(port int) string {
	return strconv.Itoa(port)
}
