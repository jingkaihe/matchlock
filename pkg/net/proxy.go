//go:build linux

package net

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"syscall"
	"unsafe"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/policy"
)

const (
	SO_ORIGINAL_DST = 80
)

type TransparentProxy struct {
	httpListener  net.Listener
	httpsListener net.Listener
	interceptor   *HTTPInterceptor
	policy        *policy.Engine
	events        chan api.Event

	httpPort  int
	httpsPort int
	bindAddr  string

	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

type ProxyConfig struct {
	BindAddr  string // Address to bind (e.g., "192.168.100.1")
	HTTPPort  int    // Port for HTTP interception (e.g., 8080)
	HTTPSPort int    // Port for HTTPS interception (e.g., 8443)
	Policy    *policy.Engine
	Events    chan api.Event
	CAPool    *CAPool
}

func NewTransparentProxy(cfg *ProxyConfig) (*TransparentProxy, error) {
	httpAddr := fmt.Sprintf("%s:%d", cfg.BindAddr, cfg.HTTPPort)
	httpsAddr := fmt.Sprintf("%s:%d", cfg.BindAddr, cfg.HTTPSPort)

	httpLn, err := net.Listen("tcp", httpAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on HTTP port %s: %w", httpAddr, err)
	}

	httpsLn, err := net.Listen("tcp", httpsAddr)
	if err != nil {
		httpLn.Close()
		return nil, fmt.Errorf("failed to listen on HTTPS port %s: %w", httpsAddr, err)
	}

	tp := &TransparentProxy{
		httpListener:  httpLn,
		httpsListener: httpsLn,
		interceptor:   NewHTTPInterceptor(cfg.Policy, cfg.Events, cfg.CAPool),
		policy:        cfg.Policy,
		events:        cfg.Events,
		httpPort:      cfg.HTTPPort,
		httpsPort:     cfg.HTTPSPort,
		bindAddr:      cfg.BindAddr,
	}

	return tp, nil
}

func (tp *TransparentProxy) Start() {
	tp.wg.Add(2)
	go tp.acceptLoop(tp.httpListener, tp.handleHTTP)
	go tp.acceptLoop(tp.httpsListener, tp.handleHTTPS)
}

func (tp *TransparentProxy) acceptLoop(ln net.Listener, handler func(net.Conn, string, int)) {
	defer tp.wg.Done()

	for {
		conn, err := ln.Accept()
		if err != nil {
			tp.mu.Lock()
			closed := tp.closed
			tp.mu.Unlock()
			if closed {
				return
			}
			continue
		}

		tcpConn, ok := conn.(*net.TCPConn)
		if !ok {
			conn.Close()
			continue
		}

		origDst, err := getOriginalDst(tcpConn)
		if err != nil {
			conn.Close()
			continue
		}

		go handler(conn, origDst.IP.String(), origDst.Port)
	}
}

func (tp *TransparentProxy) handleHTTP(conn net.Conn, dstIP string, dstPort int) {
	// Don't pre-check policy on IP - the HTTP interceptor will check using the Host header
	tp.interceptor.HandleHTTP(conn, dstIP, dstPort)
}

func (tp *TransparentProxy) handleHTTPS(conn net.Conn, dstIP string, dstPort int) {
	// Don't pre-check policy on IP - the HTTPS interceptor will check using SNI
	tp.interceptor.HandleHTTPS(conn, dstIP, dstPort)
}



func (tp *TransparentProxy) Close() error {
	tp.mu.Lock()
	if tp.closed {
		tp.mu.Unlock()
		return nil
	}
	tp.closed = true
	tp.mu.Unlock()

	tp.httpListener.Close()
	tp.httpsListener.Close()
	tp.wg.Wait()

	return nil
}

func (tp *TransparentProxy) HTTPPort() int    { return tp.httpPort }
func (tp *TransparentProxy) HTTPSPort() int   { return tp.httpsPort }
func (tp *TransparentProxy) BindAddr() string { return tp.bindAddr }

type originalDst struct {
	IP   net.IP
	Port int
}

func getOriginalDst(conn *net.TCPConn) (*originalDst, error) {
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return nil, fmt.Errorf("failed to get syscall conn: %w", err)
	}

	var origDst *originalDst
	var controlErr error

	err = rawConn.Control(func(fd uintptr) {
		// sockaddr_in structure: family(2) + port(2) + ip(4) + zero(8) = 16 bytes
		var addr [16]byte
		addrLen := uint32(len(addr))

		_, _, errno := syscall.Syscall6(
			syscall.SYS_GETSOCKOPT,
			fd,
			syscall.SOL_IP,
			SO_ORIGINAL_DST,
			uintptr(unsafe.Pointer(&addr)),
			uintptr(unsafe.Pointer(&addrLen)),
			0,
		)
		if errno != 0 {
			controlErr = fmt.Errorf("getsockopt SO_ORIGINAL_DST failed: %w", errno)
			return
		}

		// Parse sockaddr_in: family(2) + port(2, big-endian) + ip(4)
		port := int(binary.BigEndian.Uint16(addr[2:4]))
		ip := net.IPv4(addr[4], addr[5], addr[6], addr[7])
		origDst = &originalDst{IP: ip, Port: port}
	})

	if err != nil {
		return nil, err
	}
	if controlErr != nil {
		return nil, controlErr
	}

	return origDst, nil
}
