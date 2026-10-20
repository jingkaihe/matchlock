//go:build linux

package net

import (
	"context"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jingkaihe/matchlock/internal/errx"
)

// DNSForwarder relays guest DNS queries to upstream resolvers.
//
// It serves the guest over IPv4 and — when the VM has an IPv6 link — over the
// IPv6 gateway address as well, on the SAME port: the ip6 nftables table
// DNATs guest port 53 to the port this forwarder reports through
// SetDNSForwarderPort, exactly as the ip table does for IPv4, so one port
// number has to answer for both families. A reply is written back on the
// socket that received its query.
//
// Two answer modes exist:
//
//   - Verbatim relay (the legacy NewDNSForwarder and NewDualStackDNSForwarder
//     constructors, and NewConfiguredDNSForwarder with priority "ipv6"):
//     whatever the upstream returns — record types, flags, TTLs — reaches the
//     guest unchanged. "Prioritizing IPv6 DNS resolution" means exactly this
//     verbatim relay; it does NOT force applications to prefer IPv6, and
//     direct IP-literal connections are unaffected by either mode.
//   - ipv4-preferring rewrite (NewConfiguredDNSForwarder with priority
//     "ipv4", the default): for a standard single-question AAAA query whose
//     name is not exempted by the allow_private matcher, the forwarder probes
//     the same upstream list for an A record. When the name demonstrably has
//     an A record, the guest receives a synthesized NODATA answer (no AAAA
//     records; unsigned — AD clear) instead of the upstream AAAA answer so a
//     dual-stack guest falls back to IPv4. Every other query and every other
//     upstream outcome relays the upstream bytes verbatim.
type DNSForwarder struct {
	// conns are the sockets the forwarder reads from, IPv4 first. Dual-stack
	// mode adds the IPv6 gateway socket bound on the same port. A reply is
	// written back on the socket that received its query, because a udp4 socket
	// cannot address an IPv6 client (and vice versa).
	conns []*net.UDPConn

	dnsServers []string
	// bindAddrV6 is the configured IPv6 bind address, empty when IPv4-only.
	bindAddrV6 string

	// mode selects the answer mode: dnsModeVerbatim relays every answer
	// unchanged; dnsModeIPv4Prefer rewrites eligible AAAA answers (see the
	// type comment). It is fixed at construction.
	mode dnsForwardMode
	// allowPrivateName reports whether a queried name matches an
	// allow_private entry. Names it matches are relayed verbatim with no
	// probe. In dnsModeIPv4Prefer it is required at construction (fail
	// closed); in dnsModeVerbatim it is unused.
	allowPrivateName func(host string) bool

	// stopCtx is cancelled by Close so an in-flight probe aborts promptly,
	// including a dial that has not completed yet.
	stopCtx    context.Context
	stopCancel context.CancelFunc

	// rewriteMu guards the rewrite cache and the in-flight probe map. No
	// network I/O ever happens while it is held.
	rewriteMu sync.Mutex
	// rewriteCache memoizes A-existence per queried name (see probeAExists).
	rewriteCache map[string]dnsRewriteCacheEntry
	// rewriteInflight collapses concurrent probes for the same name into one
	// upstream query (singleflight).
	rewriteInflight map[string]*dnsProbeCall
	// now is the clock the rewrite cache reads. It is a field so tests can
	// inject a clock; production constructors leave it at time.Now.
	now func() time.Time
	// probeSeq numbers A-probe queries so only a response that echoes the
	// probe's ID is accepted as evidence.
	probeSeq atomic.Uint32
	// probeBudget bounds the TOTAL A-probe time across the whole resolver
	// fallback list; 0 means dnsProbeBudget.
	probeBudget time.Duration

	requests  chan dnsRequest
	stopCh    chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup

	upstreamMu sync.Mutex
	upstreams  map[net.Conn]struct{}
}

type dnsRequest struct {
	query      []byte
	clientAddr *net.UDPAddr
	// conn is the guest-facing socket the query arrived on; the reply goes back
	// out the same socket so it is sent from the address family the guest used.
	conn *net.UDPConn
}

const (
	dnsUpstreamTimeout = 5 * time.Second
	dnsPacketBufSize   = 4096
	// Bound concurrent upstream exchanges so a guest cannot create an
	// unbounded number of goroutines or sockets by flooding DNS traffic.
	dnsWorkerCount = 32
	dnsQueueSize   = 128
)

// listenDNS is the UDP socket constructor. It is a package-level variable so a
// test can observe the sockets a failed dual-stack bind opened: the IPv4
// socket's OS-assigned port is otherwise unknowable, which is exactly what a
// leaked listener would hide.
var listenDNS = net.ListenUDP

// NewDNSForwarder starts a UDP forwarder on bindAddr.
//
// bindAddr is normally the VM's IPv4 gateway. An IPv6 literal — with or without
// brackets, e.g. "[fd00:100::1]" — is accepted too and binds a udp6 socket, so
// a caller whose guest only has an IPv6 link can still serve it. Use
// NewDualStackDNSForwarder to serve both families at once.
func NewDNSForwarder(bindAddr string, dnsServers []string) (*DNSForwarder, error) {
	if len(dnsServers) == 0 {
		return nil, errx.With(ErrListen, " no DNS servers configured for forwarder")
	}
	conn, err := listenDNSAddr(bindAddr, 0)
	if err != nil {
		return nil, err
	}
	return newDNSForwarder([]*net.UDPConn{conn}, "", dnsServers, dnsForwarderSettings{mode: dnsForwardModeVerbatim}), nil
}

// NewDualStackDNSForwarder starts the forwarder on bindAddrV4 and on the SAME
// port on bindAddrV6 (the guest's IPv6 gateway address), so the single port the
// ip6 DNS redirect targets answers for both families and Port() stays one
// number. An empty bindAddrV6 is the IPv4-only forwarder. A failed IPv6 bind is
// fatal and releases the IPv4 socket, so there is no partial dual-stack
// forwarder whose IPv6 DNS traffic could go unanswered.
func NewDualStackDNSForwarder(bindAddrV4, bindAddrV6 string, dnsServers []string) (*DNSForwarder, error) {
	if bindAddrV6 == "" {
		return NewDNSForwarder(bindAddrV4, dnsServers)
	}
	if len(dnsServers) == 0 {
		return nil, errx.With(ErrListen, " no DNS servers configured for forwarder")
	}

	v4, err := listenDNSAddr(bindAddrV4, 0)
	if err != nil {
		return nil, err
	}
	v6, err := listenDNSAddr(bindAddrV6, v4.LocalAddr().(*net.UDPAddr).Port)
	if err != nil {
		_ = v4.Close()
		return nil, err
	}

	return newDNSForwarder([]*net.UDPConn{v4, v6}, bindAddrV6, dnsServers, dnsForwarderSettings{mode: dnsForwardModeVerbatim}), nil
}

// NewConfiguredDNSForwarder starts the mode-aware forwarder: bindAddrV4 plus —
// when bindAddrV6 is not empty — bindAddrV6 on the SAME port, exactly like
// NewDualStackDNSForwarder, but the answer mode comes from cfg. DNSPriority
// "ipv4" (also the default when empty) enables the ipv4-preferring rewrite of
// eligible AAAA answers; "ipv6" keeps the verbatim relay (see the DNSForwarder
// type comment). The ipv4 mode REQUIRES cfg.AllowPrivateName: a nil matcher is
// rejected here, before any socket is opened, because it would silently drop
// the allow_private carve-out.
func NewConfiguredDNSForwarder(bindAddrV4, bindAddrV6 string, dnsServers []string, cfg DNSForwarderConfig) (*DNSForwarder, error) {
	mode, err := dnsModeForPriority(cfg.DNSPriority)
	if err != nil {
		return nil, err
	}
	if mode == dnsForwardModeIPv4Prefer && cfg.AllowPrivateName == nil {
		priority := cfg.DNSPriority
		if priority == "" {
			priority = "ipv4"
		}
		return nil, errx.With(ErrDNSConfig, ": dns priority %q requires an allow_private name matcher", priority)
	}
	if len(dnsServers) == 0 {
		return nil, errx.With(ErrListen, " no DNS servers configured for forwarder")
	}

	v4, err := listenDNSAddr(bindAddrV4, 0)
	if err != nil {
		return nil, err
	}
	conns := []*net.UDPConn{v4}
	if bindAddrV6 != "" {
		v6, err := listenDNSAddr(bindAddrV6, v4.LocalAddr().(*net.UDPAddr).Port)
		if err != nil {
			_ = v4.Close()
			return nil, err
		}
		conns = append(conns, v6)
	}

	return newDNSForwarder(conns, bindAddrV6, dnsServers, dnsForwarderSettings{
		mode:             mode,
		allowPrivateName: cfg.AllowPrivateName,
	}), nil
}

// listenDNSAddr opens a UDP socket for one bind address literal on port (0 =
// OS-assigned). The network follows the address family of the literal: an IPv6
// address needs udp6, everything else keeps the udp4 socket the forwarder has
// always used.
func listenDNSAddr(bindAddr string, port int) (*net.UDPConn, error) {
	host := dnsBindHost(bindAddr)
	network := "udp4"
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		network = "udp6"
	}

	addr, err := net.ResolveUDPAddr(network, net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, errx.With(ErrListen, " resolving DNS bind addr %s: %w", bindAddr, err)
	}
	conn, err := listenDNS(network, addr)
	if err != nil {
		return nil, errx.With(ErrListen, " on DNS port %s: %w", addr, err)
	}
	return conn, nil
}

// dnsBindHost strips the brackets of a bracketed IPv6 literal so it can be
// handed to net.JoinHostPort, which adds them back for the IPv6 family.
func dnsBindHost(bindAddr string) string {
	if strings.HasPrefix(bindAddr, "[") && strings.HasSuffix(bindAddr, "]") {
		return bindAddr[1 : len(bindAddr)-1]
	}
	return bindAddr
}

// newDNSForwarder wires the sockets, the answer mode from settings and the
// worker pool. dnsServers is snapshotted here, and never mutated afterwards,
// so the rewrite cache is scoped to exactly one immutable resolver list (a
// name's verdict is never reused across differently-configured forwarders).
func newDNSForwarder(conns []*net.UDPConn, bindAddrV6 string, dnsServers []string, settings dnsForwarderSettings) *DNSForwarder {
	stopCtx, stopCancel := context.WithCancel(context.Background())
	d := &DNSForwarder{
		conns:            conns,
		dnsServers:       append([]string(nil), dnsServers...),
		bindAddrV6:       bindAddrV6,
		mode:             settings.mode,
		allowPrivateName: settings.allowPrivateName,
		stopCtx:          stopCtx,
		stopCancel:       stopCancel,
		rewriteCache:     make(map[string]dnsRewriteCacheEntry),
		rewriteInflight:  make(map[string]*dnsProbeCall),
		now:              settings.now,
		probeBudget:      settings.probeBudget,
		requests:         make(chan dnsRequest, dnsQueueSize),
		stopCh:           make(chan struct{}),
		upstreams:        make(map[net.Conn]struct{}),
	}
	if settings.now != nil {
		d.now = settings.now
	} else {
		d.now = time.Now
	}
	for _, conn := range conns {
		d.wg.Add(1)
		go d.serve(conn)
	}
	for range dnsWorkerCount {
		d.wg.Add(1)
		go d.worker()
	}
	return d
}

// Port returns the ephemeral port chosen by the kernel. In dual-stack mode both
// sockets are bound on it, so it is the one port the DNS redirect targets.
func (d *DNSForwarder) Port() int {
	return d.conns[0].LocalAddr().(*net.UDPAddr).Port
}

// BindAddrV6 returns the IPv6 address the forwarder also listens on, or "" when
// it is IPv4-only.
func (d *DNSForwarder) BindAddrV6() string {
	return d.bindAddrV6
}

// Close stops the server and releases every socket.
func (d *DNSForwarder) Close() error {
	d.closeOnce.Do(func() {
		close(d.stopCh)
		// Cancel the stop context too: this aborts a probe dial that has not
		// completed yet (the sockets a probe already connected are released
		// by closeUpstreams below), so Close never waits on the probe budget.
		d.stopCancel()
		for _, conn := range d.conns {
			_ = conn.Close()
		}
		d.closeUpstreams()
	})
	d.wg.Wait()
	return nil
}

func (d *DNSForwarder) serve(conn *net.UDPConn) {
	defer d.wg.Done()
	buf := make([]byte, dnsPacketBufSize)
	for {
		n, clientAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			if d.isStopping() {
				return
			}
			continue
		}

		query := make([]byte, n)
		copy(query, buf[:n])
		select {
		case d.requests <- dnsRequest{query: query, clientAddr: clientAddr, conn: conn}:
		case <-d.stopCh:
			return
		}
	}
}

func (d *DNSForwarder) worker() {
	defer d.wg.Done()
	for {
		select {
		case req := <-d.requests:
			d.forward(req)
		case <-d.stopCh:
			return
		}
	}
}

func (d *DNSForwarder) forward(req dnsRequest) {
	for _, server := range d.dnsServers {
		if d.isStopping() {
			return
		}
		resp, err := d.exchange(req.query, server)
		if err != nil {
			continue
		}
		if d.isStopping() {
			return
		}

		// The answer is written back on the socket the query arrived on:
		// verbatim by default, or through the ipv4-preferring AAAA rewrite
		// when that mode is active (every failure inside the rewrite relays
		// the upstream bytes verbatim).
		_, _ = req.conn.WriteToUDP(d.maybeRewriteAAAA(req.query, resp), req.clientAddr)
		return
	}
	// Every upstream server failed on transport: there is no response to
	// relay and none is fabricated (the rewrite never runs without upstream
	// bytes, so it cannot synthesize evidence of its own). The guest sees a
	// DNS timeout and retries, exactly as before.
	slog.Debug("DNS query failed on every upstream server; no answer sent", "servers", d.dnsServers)
}

func (d *DNSForwarder) exchange(query []byte, server string) ([]byte, error) {
	return d.exchangeUntil(query, server, time.Now().Add(dnsUpstreamTimeout))
}

// exchangeUntil is exchange under one ABSOLUTE deadline (and the forwarder's
// stop context): it is the transport of the A-existence probe, whose total
// budget covers every dial and every socket wait across the whole resolver
// fallback list. The connection is registered in d.upstreams like any other
// upstream, so Close releases a probe blocked in Read, and the dial itself
// aborts the moment the forwarder stops.
func (d *DNSForwarder) exchangeUntil(query []byte, server string, deadline time.Time) ([]byte, error) {
	if d.isStopping() {
		return nil, net.ErrClosed
	}
	dialCtx, cancel := context.WithDeadline(d.stopCtx, deadline)
	defer cancel()

	var dialer net.Dialer
	upstream, err := dialer.DialContext(dialCtx, "udp", dnsServerAddr(server))
	if err != nil {
		return nil, err
	}
	if !d.trackUpstream(upstream) {
		_ = upstream.Close()
		return nil, net.ErrClosed
	}
	defer d.untrackUpstream(upstream)
	defer upstream.Close()

	// The deadline is the probe's enforcement point: a failure to install it
	// must surface as a transport error, not as an unbounded wait.
	if err := upstream.SetDeadline(deadline); err != nil {
		return nil, err
	}
	if _, err := upstream.Write(query); err != nil {
		return nil, err
	}

	resp := make([]byte, dnsPacketBufSize)
	n, err := upstream.Read(resp)
	if err != nil {
		return nil, err
	}
	return resp[:n], nil
}

func (d *DNSForwarder) isStopping() bool {
	select {
	case <-d.stopCh:
		return true
	default:
		return false
	}
}

func (d *DNSForwarder) trackUpstream(upstream net.Conn) bool {
	d.upstreamMu.Lock()
	defer d.upstreamMu.Unlock()

	if d.isStopping() {
		return false
	}
	d.upstreams[upstream] = struct{}{}
	return true
}

func (d *DNSForwarder) untrackUpstream(upstream net.Conn) {
	d.upstreamMu.Lock()
	defer d.upstreamMu.Unlock()
	delete(d.upstreams, upstream)
}

func (d *DNSForwarder) closeUpstreams() {
	d.upstreamMu.Lock()
	upstreams := make([]net.Conn, 0, len(d.upstreams))
	for upstream := range d.upstreams {
		upstreams = append(upstreams, upstream)
	}
	d.upstreamMu.Unlock()

	for _, upstream := range upstreams {
		_ = upstream.Close()
	}
}

func dnsServerAddr(server string) string {
	if _, _, err := net.SplitHostPort(server); err == nil {
		return server
	}
	return net.JoinHostPort(server, "53")
}
