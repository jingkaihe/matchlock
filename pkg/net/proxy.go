//go:build linux

package net

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/policy"
)

const (
	SO_ORIGINAL_DST = 80
	// IP6T_SO_ORIGINAL_DST is the IPv6 half of the transparent-proxy lookup: the
	// pre-DNAT destination of a connection an ip6 nftables redirect sent to the
	// proxy. It is queried at level IPPROTO_IPV6 and answers with a
	// sockaddr_in6, exactly as SO_ORIGINAL_DST answers with a sockaddr_in at
	// level SOL_IP. Both options share the number 80 in their own protocol.
	IP6T_SO_ORIGINAL_DST = 80

	// SO_DOMAIN (SOL_SOCKET) reports the address family a socket was CREATED
	// with — not the family of a peer address — so it is what picks the right
	// original-destination getsockopt level on a dual-stack proxy. It has no
	// constant in the syscall package.
	soDomain = 39

	// Kernel sockaddr wire sizes and the family values they carry:
	//   sockaddr_in : family(2) + port(2) + address(4)                     = 16
	//   sockaddr_in6: family(2) + port(2) + flowinfo(4) + addr(16) + scope(4) = 28
	sockaddrInLen  = 16
	sockaddrIn6Len = 28

	afInet  = 2
	afInet6 = 10
)

type TransparentProxy struct {
	httpListener        net.Listener
	httpsListener       net.Listener
	passthroughListener net.Listener

	// IPv6 listeners, bound on ProxyConfig.BindAddrV6 and nil when no IPv6 bind
	// address was configured (the IPv4-only shape, which is the only shape
	// darwin uses). They share the handler functions with the IPv4 listeners so
	// both families run through one policy path, and they answer on the SAME
	// ports as the IPv4 listeners: one nftables DNAT rule per intercepted port
	// covers both families, so both families must listen on that one port.
	httpListenerV6        net.Listener
	httpsListenerV6       net.Listener
	passthroughListenerV6 net.Listener

	interceptor *HTTPInterceptor
	policy      *policy.Engine
	events      chan api.Event

	// dial opens the upstream TCP connection for the passthrough path. It defaults
	// to a 30s timeout dial and is a seam for tests (and, with the passthrough
	// path, mirrors the HTTP interceptor dial seam).
	dial func(network, addr string) (net.Conn, error)

	// originalDst resolves the pre-DNAT destination of an accepted connection.
	// nil selects getsockoptOriginalDst; tests inject a getter so the accept
	// path can be exercised on a loopback socket, which has no conntrack record
	// and therefore no original destination for the kernel to report.
	originalDst originalDstGetter

	httpPort        int
	httpsPort       int
	passthroughPort int
	bindAddr        string
	bindAddrV6      string

	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

type ProxyConfig struct {
	BindAddr string // Address to bind for IPv4 (e.g., "192.168.100.1")
	// BindAddrV6 is the guest-visible IPv6 gateway address (e.g. "fd00:100::1")
	// to bind the same HTTP, HTTPS and passthrough ports on for the guest's IPv6
	// link. Empty leaves the proxy IPv4-only (the previous behaviour). A bind
	// failure is fatal: the caller must not create a sandbox whose IPv6 traffic
	// could escape the policy, so there is no partial dual-stack mode.
	BindAddrV6      string
	HTTPPort        int // Port for HTTP interception (e.g., 8080)
	HTTPSPort       int // Port for HTTPS interception (e.g., 8443)
	PassthroughPort int // Port for policy-gated TCP passthrough (non-80/443). 0 = OS-assigned, negative = disabled
	Policy          *policy.Engine
	Events          chan api.Event
	CAPool          *CAPool
}

// proxyListeners is one address family's set of proxy listeners plus the
// concrete ports they ended up bound to.
type proxyListeners struct {
	http            net.Listener
	https           net.Listener
	passthrough     net.Listener // nil when the passthrough path is disabled
	httpPort        int
	httpsPort       int
	passthroughPort int
}

// close shuts down every listener this set owns, tolerating the ones that were
// never opened.
func (l *proxyListeners) close() {
	for _, ln := range []net.Listener{l.http, l.https, l.passthrough} {
		if ln != nil {
			ln.Close()
		}
	}
}

// openProxyListeners binds the HTTP, HTTPS and — unless passthroughPort is
// negative — passthrough listeners on bindAddr and reports the ports they were
// assigned (a requested port of 0 means "let the OS choose"). A later failure
// closes whatever this call already opened, so a failed call never leaks a
// listener.
func openProxyListeners(bindAddr string, httpPort, httpsPort, passthroughPort int) (*proxyListeners, error) {
	l := &proxyListeners{}

	httpAddr := net.JoinHostPort(bindAddr, strconv.Itoa(httpPort))
	httpLn, err := net.Listen("tcp", httpAddr)
	if err != nil {
		return nil, errx.With(ErrListen, " on HTTP port %s: %w", httpAddr, err)
	}
	l.http = httpLn
	l.httpPort = httpLn.Addr().(*net.TCPAddr).Port

	httpsAddr := net.JoinHostPort(bindAddr, strconv.Itoa(httpsPort))
	httpsLn, err := net.Listen("tcp", httpsAddr)
	if err != nil {
		l.close()
		return nil, errx.With(ErrListen, " on HTTPS port %s: %w", httpsAddr, err)
	}
	l.https = httpsLn
	l.httpsPort = httpsLn.Addr().(*net.TCPAddr).Port

	if passthroughPort >= 0 {
		ptAddr := net.JoinHostPort(bindAddr, strconv.Itoa(passthroughPort))
		ptLn, err := net.Listen("tcp", ptAddr)
		if err != nil {
			l.close()
			return nil, errx.With(ErrListen, " on passthrough port %s: %w", ptAddr, err)
		}
		l.passthrough = ptLn
		l.passthroughPort = ptLn.Addr().(*net.TCPAddr).Port
	}

	return l, nil
}

func NewTransparentProxy(cfg *ProxyConfig) (*TransparentProxy, error) {
	v4, err := openProxyListeners(cfg.BindAddr, cfg.HTTPPort, cfg.HTTPSPort, cfg.PassthroughPort)
	if err != nil {
		return nil, err
	}

	var v6 *proxyListeners
	if cfg.BindAddrV6 != "" {
		// Reuse the ports the IPv4 listeners were assigned (0 in the config means
		// "let the OS pick"): the nftables redirect targets one port number per
		// intercepted service, so the IPv6 listeners must answer on exactly the
		// ports the IPv4 rules and the proxy report use.
		ptPort := -1
		if v4.passthrough != nil {
			ptPort = v4.passthroughPort
		}
		v6, err = openProxyListeners(cfg.BindAddrV6, v4.httpPort, v4.httpsPort, ptPort)
		if err != nil {
			v4.close()
			return nil, err
		}
	}

	tp := &TransparentProxy{
		httpListener:        v4.http,
		httpsListener:       v4.https,
		passthroughListener: v4.passthrough,
		interceptor:         NewHTTPInterceptor(cfg.Policy, cfg.Events, cfg.CAPool),
		policy:              cfg.Policy,
		events:              cfg.Events,
		dial:                defaultDial,
		httpPort:            v4.httpPort,
		httpsPort:           v4.httpsPort,
		passthroughPort:     v4.passthroughPort,
		bindAddr:            cfg.BindAddr,
		bindAddrV6:          cfg.BindAddrV6,
	}
	if v6 != nil {
		tp.httpListenerV6 = v6.http
		tp.httpsListenerV6 = v6.https
		tp.passthroughListenerV6 = v6.passthrough
	}

	return tp, nil
}

// proxyAcceptLoop pairs a listener with the handler its connections run.
type proxyAcceptLoop struct {
	listener net.Listener
	handler  func(net.Conn, string, int)
}

// acceptLoops returns the accept loops to run, skipping the listeners that were
// not configured (no passthrough, no IPv6 bind address). Both families run the
// same handler functions, so an intercepted IPv6 connection is policed exactly
// like its IPv4 counterpart.
func (tp *TransparentProxy) acceptLoops() []proxyAcceptLoop {
	all := []proxyAcceptLoop{
		{tp.httpListener, tp.handleHTTP},
		{tp.httpsListener, tp.handleHTTPS},
		{tp.passthroughListener, tp.handlePassthrough},
		{tp.httpListenerV6, tp.handleHTTP},
		{tp.httpsListenerV6, tp.handleHTTPS},
		{tp.passthroughListenerV6, tp.handlePassthrough},
	}

	loops := make([]proxyAcceptLoop, 0, len(all))
	for _, l := range all {
		if l.listener != nil {
			loops = append(loops, l)
		}
	}
	return loops
}

func (tp *TransparentProxy) Start() {
	loops := tp.acceptLoops()

	// One WaitGroup slot per running accept loop, so Close waits for the IPv6
	// loops as well as the IPv4 ones before reporting the proxy stopped.
	tp.wg.Add(len(loops))
	for _, l := range loops {
		go tp.acceptLoop(l.listener, l.handler)
	}
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

		origDst, err := tp.originalDstOf(tcpConn)
		if err != nil {
			conn.Close()
			continue
		}

		go handler(conn, origDst.IP.String(), origDst.Port)
	}
}

// originalDstOf resolves the original destination of an accepted connection,
// using the injected getter when a test set one and the real getsockopt path
// otherwise. Either getter dispatches on the socket's own family, so an IPv6
// connection is looked up through IPPROTO_IPV6/IP6T_SO_ORIGINAL_DST and an IPv4
// one through SOL_IP/SO_ORIGINAL_DST.
func (tp *TransparentProxy) originalDstOf(conn *net.TCPConn) (*originalDst, error) {
	getter := tp.originalDst
	if getter == nil {
		getter = getsockoptOriginalDst
	}
	return getOriginalDstWith(conn, getter)
}

func (tp *TransparentProxy) handleHTTP(conn net.Conn, dstIP string, dstPort int) {
	// Don't pre-check policy on IP - the HTTP interceptor will check using the Host header
	tp.interceptor.HandleHTTP(conn, dstIP, dstPort)
}

func (tp *TransparentProxy) handleHTTPS(conn net.Conn, dstIP string, dstPort int) {
	// Don't pre-check policy on IP - the HTTPS interceptor will check using SNI
	tp.interceptor.HandleHTTPS(conn, dstIP, dstPort)
}

func (tp *TransparentProxy) handlePassthrough(conn net.Conn, dstIP string, dstPort int) {
	defer conn.Close()

	// The policy is always evaluated on the BARE IP the original-destination
	// lookup returned (IPv4 or IPv6 literal, never a bracketed or
	// port-suffixed form), so a v6 destination reaches the policy engine in the
	// same shape as a v4 one; the port travels separately.
	host := net.JoinHostPort(dstIP, fmt.Sprintf("%d", dstPort))
	// Evaluate the port too: an allow_private entry may be port-scoped. The
	// policy check is against the ORIGINAL SO_ORIGINAL_DST/IP6T_SO_ORIGINAL_DST
	// IP:port, never a re-resolved or mapped dial target.
	if !tp.policy.IsHostAllowedPort(dstIP, dstPort) {
		tp.emitBlockedEvent(host, "host not in allowlist")
		return
	}

	dial := tp.dial
	if dial == nil {
		dial = defaultDial
	}
	// The dial target is the literal original destination, so its family is
	// preserved by construction: a v6 destination is dialed as a v6 literal
	// (bracketed by JoinHostPort) and can never be redirected to an IPv4
	// address by a DNS change. The network stays "tcp" (dual-stack).
	realConn, err := dial("tcp", host)
	if err != nil {
		return
	}
	defer realConn.Close()

	done := make(chan struct{}, 2)
	go func() {
		io.Copy(realConn, conn)
		done <- struct{}{}
	}()
	go func() {
		io.Copy(conn, realConn)
		done <- struct{}{}
	}()

	<-done
	conn.SetDeadline(time.Now())
	realConn.SetDeadline(time.Now())
	<-done
}

func (tp *TransparentProxy) emitBlockedEvent(host, reason string) {
	if tp.events == nil {
		return
	}
	select {
	case tp.events <- api.Event{
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

func (tp *TransparentProxy) Close() error {
	tp.mu.Lock()
	if tp.closed {
		tp.mu.Unlock()
		return nil
	}
	tp.closed = true
	tp.mu.Unlock()

	// Close every listener, including the IPv6 ones, so all accept loops unblock
	// and the WaitGroup below actually drains.
	for _, ln := range []net.Listener{
		tp.httpListener,
		tp.httpsListener,
		tp.passthroughListener,
		tp.httpListenerV6,
		tp.httpsListenerV6,
		tp.passthroughListenerV6,
	} {
		if ln != nil {
			ln.Close()
		}
	}
	tp.wg.Wait()

	return nil
}

func (tp *TransparentProxy) HTTPPort() int        { return tp.httpPort }
func (tp *TransparentProxy) HTTPSPort() int       { return tp.httpsPort }
func (tp *TransparentProxy) PassthroughPort() int { return tp.passthroughPort }
func (tp *TransparentProxy) BindAddr() string     { return tp.bindAddr }

// BindAddrV6 is the address the IPv6 listeners were bound on; empty when the
// proxy is IPv4-only.
func (tp *TransparentProxy) BindAddrV6() string { return tp.bindAddrV6 }

// IPv6Enabled reports whether the proxy also accepts IPv6 connections.
func (tp *TransparentProxy) IPv6Enabled() bool {
	return tp.httpListenerV6 != nil && tp.httpsListenerV6 != nil
}

type originalDst struct {
	IP   net.IP
	Port int
}

// originalDstSockopt maps a socket address family to the getsockopt level and
// option that recover its pre-DNAT destination: SOL_IP/SO_ORIGINAL_DST for an
// IPv4 socket, IPPROTO_IPV6/IP6T_SO_ORIGINAL_DST for an IPv6 socket. ok is
// false for any other family, which the caller treats as a lookup failure
// (fail closed: the connection is closed instead of dialed).
func originalDstSockopt(domain int) (level, option int, ok bool) {
	switch domain {
	case afInet:
		return syscall.SOL_IP, SO_ORIGINAL_DST, true
	case afInet6:
		return syscall.IPPROTO_IPV6, IP6T_SO_ORIGINAL_DST, true
	}
	return 0, 0, false
}

// socketDomain reports the address family the socket was CREATED with
// (SO_DOMAIN). Unlike the local or peer address it cannot be confused by a
// dual-stack listener or an IPv4-mapped address, so the getsockopt dispatch
// above is exact.
func socketDomain(fd uintptr) (int, error) {
	domain, err := syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, soDomain)
	if err != nil {
		return 0, errx.Wrap(ErrOriginalDst, err)
	}
	return domain, nil
}

// getsockoptOriginalDst is the kernel-facing half of getOriginalDst. The
// socket's own family decides which sockaddr shape the kernel writes, so the
// answer for an IPv6 connection is parsed with parseSockaddrIn6 and the IPv4
// path keeps using parseSockaddrIn. A socket with no original-destination
// record (no NAT/conntrack entry) makes the kernel fail the call; that error is
// returned and the accept loop closes the connection.
func getsockoptOriginalDst(fd uintptr) (*originalDst, error) {
	domain, err := socketDomain(fd)
	if err != nil {
		return nil, err
	}

	level, option, ok := originalDstSockopt(domain)
	if !ok {
		return nil, errx.With(ErrOriginalDst, ": unsupported socket family %d", domain)
	}

	// Room for the larger sockaddr_in6; the kernel reports how much it wrote
	// (16 bytes for an IPv4 socket, 28 for an IPv6 one).
	var buf [sockaddrIn6Len]byte
	length := uint32(len(buf))

	_, _, errno := syscall.Syscall6(
		syscall.SYS_GETSOCKOPT,
		fd,
		uintptr(level),
		uintptr(option),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&length)),
		0,
	)
	if errno != 0 {
		return nil, errx.Wrap(ErrOriginalDst, errno)
	}
	if length > uint32(len(buf)) {
		return nil, errx.With(ErrOriginalDst, ": oversized sockaddr (%d bytes)", length)
	}

	return parseOriginalDst(domain, buf[:length])
}

// parseOriginalDst decodes the sockaddr the kernel wrote, dispatching on the
// socket family. It is pure, so the wire decoding is unit-tested with crafted
// buffers.
func parseOriginalDst(domain int, buf []byte) (*originalDst, error) {
	switch domain {
	case afInet:
		return parseSockaddrIn(buf)
	case afInet6:
		return parseSockaddrIn6(buf)
	}
	return nil, errx.With(ErrOriginalDst, ": unsupported socket family %d", domain)
}

// parseSockaddrIn decodes a sockaddr_in: family(2, host byte order) +
// port(2, network byte order) + address(4). A buffer that is too short or
// carries the wrong family is rejected rather than guessed at.
func parseSockaddrIn(buf []byte) (*originalDst, error) {
	if len(buf) < sockaddrInLen {
		return nil, errx.With(ErrOriginalDst, ": short sockaddr_in (%d bytes)", len(buf))
	}
	if family := int(binary.NativeEndian.Uint16(buf[0:2])); family != afInet {
		return nil, errx.With(ErrOriginalDst, ": unexpected sockaddr_in family %d", family)
	}

	// sockaddr_in: family(2) + port(2, big-endian) + ip(4)
	port := int(binary.BigEndian.Uint16(buf[2:4]))
	ip := net.IPv4(buf[4], buf[5], buf[6], buf[7])
	return &originalDst{IP: ip, Port: port}, nil
}

// parseSockaddrIn6 decodes a sockaddr_in6: family(2, host byte order) +
// port(2, network byte order) + flowinfo(4) + address(16) + scope_id(4).
// flowinfo and scope_id are ignored: the recovered address is used only as a
// literal dial target and a policy lookup, neither of which carries a link, and
// a scoped link-local destination (fe80::/10) is refused by policy anyway.
func parseSockaddrIn6(buf []byte) (*originalDst, error) {
	if len(buf) < sockaddrIn6Len {
		return nil, errx.With(ErrOriginalDst, ": short sockaddr_in6 (%d bytes)", len(buf))
	}
	if family := int(binary.NativeEndian.Uint16(buf[0:2])); family != afInet6 {
		return nil, errx.With(ErrOriginalDst, ": unexpected sockaddr_in6 family %d", family)
	}

	port := int(binary.BigEndian.Uint16(buf[2:4]))
	ip := make(net.IP, net.IPv6len)
	copy(ip, buf[8:24])

	return &originalDst{IP: ip, Port: port}, nil
}

// originalDstGetter is the sockopt half of getOriginalDst, injected so the
// dispatch and parse path can be exercised without a NATed socket.
type originalDstGetter func(fd uintptr) (*originalDst, error)

func getOriginalDst(conn *net.TCPConn) (*originalDst, error) {
	return getOriginalDstWith(conn, getsockoptOriginalDst)
}

// getOriginalDstWith is getOriginalDst with the getsockopt call injected. The
// guest-facing connection is a real *net.TCPConn in production; tests supply a
// getter that answers with crafted sockaddr bytes.
func getOriginalDstWith(conn *net.TCPConn, getter originalDstGetter) (*originalDst, error) {
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return nil, errx.Wrap(ErrSyscall, err)
	}

	var origDst *originalDst
	var controlErr error

	err = rawConn.Control(func(fd uintptr) {
		origDst, controlErr = getter(fd)
	})

	if err != nil {
		return nil, err
	}
	if controlErr != nil {
		return nil, controlErr
	}

	return origDst, nil
}
