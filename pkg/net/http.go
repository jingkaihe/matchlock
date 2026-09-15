package net

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/policy"
)

type HTTPInterceptor struct {
	policy   *policy.Engine
	events   chan api.Event
	caPool   *CAPool
	connPool *upstreamConnPool
	// gatewayIP is the guest-visible virtual gateway of the darwin userspace
	// network stack. On darwin that address is assigned to the netstack NIC but
	// is NOT bound on the host (there is no TAP), so a dial to it can never
	// connect. When it is set, a policy-verified dial address equal to it is
	// mapped to host loopback (see mapGatewayDialHost). It is left empty by
	// NewHTTPInterceptor, so the Linux TransparentProxy path dials every
	// destination literally: its TAP gateway IS a real host address.
	gatewayIP string
	// dial is used for upstream connections. It defaults to a 30s timeout dial
	// and is a seam for tests that need to observe the dial target or avoid real
	// network traffic.
	dial func(network, addr string) (net.Conn, error)
}

func NewHTTPInterceptor(pol *policy.Engine, events chan api.Event, caPool *CAPool) *HTTPInterceptor {
	return &HTTPInterceptor{
		policy:   pol,
		events:   events,
		caPool:   caPool,
		connPool: newUpstreamConnPool(),
		dial:     defaultDial,
	}
}

// defaultDial opens a TCP connection with the upstream dial timeout.
func defaultDial(network, addr string) (net.Conn, error) {
	return net.DialTimeout(network, addr, 30*time.Second)
}

// allowedDialIPForFamily resolves host (which may carry a port) through the
// policy engine and returns a single verified address that the proxy may dial,
// preferring the destination family of the intercepted connection. port is the
// real destination port of the intercepted connection, so a port-scoped
// allow_private exception is honored. The caller MUST dial this literal address
// rather than the hostname, so the dial cannot be redirected by a DNS change
// between the policy check and the dial. The returned address is drawn from the
// verified set via selectDialIPForFamily with preferV6 == dstPrefersV6(dstIP),
// so an IPv6-intercepted connection whose hostname is dual-stack is dialed over
// IPv6 and never silently falls back to an unrelated IPv4 literal.
func (i *HTTPInterceptor) allowedDialIPForFamily(host string, port int, preferV6 bool) (net.IP, bool) {
	ips, ok := i.policy.AllowedHostIPsPort(host, port)
	if !ok || len(ips) == 0 {
		return nil, false
	}
	return selectDialIPForFamily(ips, preferV6), true
}

// dstPrefersV6 reports whether an intercepted ORIGINAL destination is an IPv6
// literal, which is the family the proxy must dial out with so the guest's
// IPv6 connection is not answered over an unrelated IPv4 address. Anything that
// is not a literal IPv6 address (a hostname, an IPv4 literal, empty) keeps the
// previous IPv4 preference.
func dstPrefersV6(dstIP string) bool {
	ip := net.ParseIP(dstIP)
	return ip != nil && ip.To4() == nil
}

// selectDialIP picks the address to dial from a set that has already been
// verified against the policy, preferring IPv4. The transparent proxy can be
// IPv4-only (see selectDialIPForFamily for the family-aware form): a public
// hostname that resolves AAAA-first (Go's LookupIP returns AAAA before A for a
// dual-stack host) would otherwise be dialed at an IPv6 literal with no
// fallback, whereas the prior hostname dial would have tried all addresses
// (Happy Eyeballs) and fallen back to reachable IPv4. If the verified set is
// IPv6-only it falls back to the first address, since no IPv4 alternative
// exists. The chosen address is still a member of the policy-verified set, so
// this does not relax the privacy check.
func selectDialIP(ips []net.IP) net.IP {
	return selectDialIPForFamily(ips, false)
}

// selectDialIPForFamily picks the address to dial from a policy-verified set,
// preferring the family of the ORIGINAL destination. An IPv6-intercepted
// connection MUST dial an IPv6 member when the set has one: the IPv4
// preference above exists to keep an IPv6-only *dial* from failing on an
// IPv4-only proxy, and applying it to an IPv6 destination would answer the
// guest's IPv6 request by connecting to a different (unrelated) IPv4 address.
// Falls back to the first address when the set has no member of the preferred
// family, mirroring selectDialIP's IPv6-only fallback.
func selectDialIPForFamily(ips []net.IP, preferV6 bool) net.IP {
	for _, ip := range ips {
		if isIPv6(ip) == preferV6 {
			return ip
		}
	}
	return ips[0]
}

// isIPv6 reports whether ip is a native IPv6 address (an IPv4 or
// IPv4-mapped-IPv6 literal is not).
func isIPv6(ip net.IP) bool {
	return ip.To4() == nil
}

func (i *HTTPInterceptor) HandleHTTP(guestConn net.Conn, dstIP string, dstPort int) {
	defer guestConn.Close()

	guestReader := bufio.NewReader(guestConn)

	for {
		req, err := http.ReadRequest(guestReader)
		if err != nil {
			return
		}

		start := time.Now()

		host := req.Host
		if host == "" {
			host = dstIP
		}

		// Resolve the host once, verify EVERY resolved address against the
		// policy, then bind the dial to a specific verified address. The HTTP
		// Host header is left intact, so the upstream still sees the intended
		// host. This removes the time-of-check-to-time-of-use escape where the
		// dial re-resolves the hostname and may reach an address that was not
		// checked (DNS rebinding). dstPort is the real destination port, so a
		// port-scoped allow_private entry is enforced here.
		// The original destination's family is the family to dial: an
		// IPv6-intercepted request must not be answered over IPv4.
		dialIP, ok := i.allowedDialIPForFamily(host, dstPort, dstPrefersV6(dstIP))
		if !ok {
			i.emitBlockedEvent(req, host, "host not in allowlist")
			writeHTTPError(guestConn, http.StatusForbidden, "Blocked by policy")
			return
		}

		modifiedReq, err := i.policy.OnRequest(req, host)
		if err != nil {
			i.emitBlockedEvent(req, host, err.Error())
			writeHTTPError(guestConn, http.StatusForbidden, "Blocked by policy")
			return
		}

		// The policy decision above is on the guest-visible host; only the dial
		// target is mapped, and only on darwin (empty gatewayIP elsewhere), so an
		// allowlisted gateway reaches a host listener while an unallowlisted one
		// is still refused before this point.
		targetHost := resolvePassthroughTarget(dialIP.String(), dstPort, i.gatewayIP)

		// Try to reuse an existing upstream connection from the pool.
		pc := i.connPool.get(targetHost)
		if pc == nil {
			realConn, err := i.dial("tcp", targetHost)
			if err != nil {
				writeHTTPError(guestConn, http.StatusBadGateway, "Failed to connect")
				return
			}
			pc = &pooledConn{
				conn:   realConn,
				reader: bufio.NewReader(realConn),
			}
		}

		if err := modifiedReq.Write(pc.conn); err != nil {
			pc.conn.Close()
			writeHTTPError(guestConn, http.StatusBadGateway, "Failed to write request")
			return
		}

		resp, err := http.ReadResponse(pc.reader, modifiedReq)
		if err != nil {
			pc.conn.Close()
			return
		}

		modifiedResp, err := i.policy.OnResponse(resp, modifiedReq, host)
		if err != nil {
			i.emitBlockedEvent(modifiedReq, host, err.Error())
			writeHTTPError(guestConn, http.StatusForbidden, "Blocked by policy")
			resp.Body.Close()
			pc.conn.Close()
			return
		}

		if isStreamingResponse(modifiedResp) {
			i.emitEvent(modifiedReq, modifiedResp, host, time.Since(start))
			err := writeResponseHeadersAndStreamBody(guestConn, modifiedResp)
			resp.Body.Close()
			pc.conn.Close()
			if err != nil {
				return
			}
			return
		}

		duration := time.Since(start)
		i.emitEvent(modifiedReq, modifiedResp, host, duration)

		if err := writeResponse(guestConn, modifiedResp); err != nil {
			resp.Body.Close()
			pc.conn.Close()
			return
		}

		resp.Body.Close()

		// Return the connection to the pool if neither side requested close.
		if modifiedReq.Close || modifiedResp.Close {
			pc.conn.Close()
		} else {
			i.connPool.put(targetHost, pc)
		}

		if modifiedReq.Close || modifiedResp.Close {
			return
		}
	}
}

func (i *HTTPInterceptor) HandleHTTPS(guestConn net.Conn, dstIP string, dstPort int) {
	defer guestConn.Close()

	tlsConn := tls.Server(guestConn, &tls.Config{
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return i.caPool.GetCertificate(hello.ServerName)
		},
		InsecureSkipVerify: true,
	})

	if err := tlsConn.Handshake(); err != nil {
		return
	}
	defer tlsConn.Close()

	serverName := tlsConn.ConnectionState().ServerName
	if serverName == "" {
		serverName = dstIP
	}

	// Resolve the SNI host once, verify every resolved address against the
	// policy, then bind the TLS dial to a specific verified address while
	// preserving the SNI (ServerName) so certificate verification and the
	// upstream TLS handshake still target the intended hostname. This prevents
	// a DNS re-resolution at dial time from redirecting the TLS connection to
	// an address that was not checked. The interceptor only serves the HTTPS
	// destination port (443), and dstPort carries it, so a port-scoped
	// allow_private entry is enforced here too.
	//
	// The original destination's family is the family to dial, so an
	// IPv6-intercepted TLS session is not answered over an unrelated IPv4
	// literal. Dialing stays on the "tcp" network (dual-stack) as before.
	dialIP, ok := i.allowedDialIPForFamily(serverName, dstPort, dstPrefersV6(dstIP))
	if !ok {
		i.emitBlockedEvent(nil, serverName, "host not in allowlist")
		return
	}

	// Map the policy-verified dial address only: the TLS handshake below keeps
	// the original serverName as SNI, and the blocked event above names the
	// original SNI host. On darwin an allowlisted gateway maps to host loopback;
	// elsewhere (empty gatewayIP) the literal verified address is dialed.
	rawConn, err := i.dial("tcp", resolvePassthroughTarget(dialIP.String(), dstPort, i.gatewayIP))
	if err != nil {
		return
	}
	realConn := tls.Client(rawConn, &tls.Config{
		ServerName: serverName,
	})
	if err := realConn.Handshake(); err != nil {
		realConn.Close()
		return
	}
	defer realConn.Close()

	guestReader := bufio.NewReader(tlsConn)
	serverReader := bufio.NewReader(realConn)

	for {
		req, err := http.ReadRequest(guestReader)
		if err != nil {
			return
		}

		start := time.Now()

		modifiedReq, err := i.policy.OnRequest(req, serverName)
		if err != nil {
			i.emitBlockedEvent(req, serverName, err.Error())
			writeHTTPError(tlsConn, http.StatusForbidden, "Blocked by policy")
			return
		}

		if err := modifiedReq.Write(realConn); err != nil {
			return
		}

		resp, err := http.ReadResponse(serverReader, modifiedReq)
		if err != nil {
			return
		}

		modifiedResp, err := i.policy.OnResponse(resp, modifiedReq, serverName)
		if err != nil {
			i.emitBlockedEvent(modifiedReq, serverName, err.Error())
			writeHTTPError(tlsConn, http.StatusForbidden, "Blocked by policy")
			resp.Body.Close()
			return
		}

		if isStreamingResponse(modifiedResp) {
			i.emitEvent(modifiedReq, modifiedResp, serverName, time.Since(start))
			if err := writeResponseHeadersAndStreamBody(tlsConn, modifiedResp); err != nil {
				resp.Body.Close()
				return
			}
			resp.Body.Close()
			return
		}

		duration := time.Since(start)
		i.emitEvent(modifiedReq, modifiedResp, serverName, duration)

		if err := writeResponse(tlsConn, modifiedResp); err != nil {
			resp.Body.Close()
			return
		}

		resp.Body.Close()

		if modifiedReq.Close || modifiedResp.Close {
			return
		}
	}
}

func (i *HTTPInterceptor) emitEvent(req *http.Request, resp *http.Response, host string, duration time.Duration) {
	if i.events == nil {
		return
	}

	var reqBytes, respBytes int64
	if req.ContentLength > 0 {
		reqBytes = req.ContentLength
	}
	if resp.ContentLength > 0 {
		respBytes = resp.ContentLength
	}

	scheme := "http"
	if req.TLS != nil {
		scheme = "https"
	}

	select {
	case i.events <- api.Event{
		Type:      "network",
		Timestamp: time.Now().Unix(),
		Network: &api.NetworkEvent{
			Method:        req.Method,
			URL:           fmt.Sprintf("%s://%s%s", scheme, host, req.URL.Path),
			Host:          host,
			StatusCode:    resp.StatusCode,
			RequestBytes:  reqBytes,
			ResponseBytes: respBytes,
			DurationMS:    duration.Milliseconds(),
			Blocked:       false,
		},
	}:
	default:
	}
}

func (i *HTTPInterceptor) emitBlockedEvent(req *http.Request, host, reason string) {
	if i.events == nil {
		return
	}

	event := api.Event{
		Type:      "network",
		Timestamp: time.Now().Unix(),
		Network: &api.NetworkEvent{
			Host:        host,
			Blocked:     true,
			BlockReason: reason,
		},
	}

	if req != nil {
		event.Network.Method = req.Method
		event.Network.URL = req.URL.String()
	}

	select {
	case i.events <- event:
	default:
	}
}

func writeHTTPError(conn net.Conn, status int, message string) {
	resp := fmt.Sprintf("HTTP/1.1 %d %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, http.StatusText(status), len(message), message)
	io.WriteString(conn, resp)
}

func writeResponse(conn net.Conn, resp *http.Response) error {
	bw := bufio.NewWriterSize(conn, 64*1024)
	if err := resp.Write(bw); err != nil {
		return err
	}
	return bw.Flush()
}

func isStreamingResponse(resp *http.Response) bool {
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/event-stream") {
		return true
	}
	for _, te := range resp.TransferEncoding {
		if te == "chunked" {
			return true
		}
	}
	if resp.ContentLength == -1 && resp.ProtoMajor == 1 && resp.ProtoMinor == 1 {
		return true
	}
	return false
}

func writeResponseHeadersAndStreamBody(conn net.Conn, resp *http.Response) error {
	bw := bufio.NewWriterSize(conn, 4*1024)

	statusLine := fmt.Sprintf("HTTP/%d.%d %d %s\r\n", resp.ProtoMajor, resp.ProtoMinor, resp.StatusCode, http.StatusText(resp.StatusCode))
	if _, err := bw.WriteString(statusLine); err != nil {
		return err
	}

	if err := resp.Header.Write(bw); err != nil {
		return err
	}
	if _, err := bw.WriteString("\r\n"); err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return err
	}

	buf := make([]byte, 4*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := conn.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return readErr
		}
	}
}
