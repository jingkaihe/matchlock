package net

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"log"
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
}

func NewHTTPInterceptor(pol *policy.Engine, events chan api.Event, caPool *CAPool) *HTTPInterceptor {
	return &HTTPInterceptor{
		policy:   pol,
		events:   events,
		caPool:   caPool,
		connPool: newUpstreamConnPool(),
	}
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

		if !i.policy.IsHostAllowed(host) {
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

		targetHost := net.JoinHostPort(host, fmt.Sprintf("%d", dstPort))

		// Try to reuse an existing upstream connection from the pool.
		pc := i.connPool.get(targetHost)
		if pc == nil {
			realConn, err := net.DialTimeout("tcp", targetHost, 30*time.Second)
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
			resp.Body.Close()
			pc.conn.Close()
			return
		}

		// Buffer the entire body so we can inspect it and avoid broken
		// chunked re-encoding for streaming responses (SSE / LLM APIs).
		body, err := io.ReadAll(modifiedResp.Body)
		resp.Body.Close()
		if err != nil {
			pc.conn.Close()
			return
		}

		modifiedResp.Body = io.NopCloser(strings.NewReader(string(body)))
		modifiedResp.ContentLength = int64(len(body))
		modifiedResp.TransferEncoding = nil
		modifiedResp.Header.Del("Transfer-Encoding")
		modifiedResp.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))

		duration := time.Since(start)
		i.emitEvent(modifiedReq, modifiedResp, host, duration)

		if err := writeResponse(guestConn, modifiedResp); err != nil {
			pc.conn.Close()
			return
		}

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

	if !i.policy.IsHostAllowed(serverName) {
		i.emitBlockedEvent(nil, serverName, "host not in allowlist")
		return
	}

	guestReader := bufio.NewReader(tlsConn)

	for {
		req, err := http.ReadRequest(guestReader)
		if err != nil {
			return
		}

		start := time.Now()

		// Routing decision
		routeDirective, err := i.policy.RouteRequest(req, serverName)
		if err != nil {
			i.emitBlockedEvent(req, serverName, err.Error())
			writeHTTPError(tlsConn, http.StatusBadGateway, "Routing error")
			return
		}

		// Determine effective host for secret injection
		effectiveHost := serverName
		if routeDirective != nil {
			effectiveHost = routeDirective.Host
			log.Printf("[matchlock] route: %s %s%s -> %s:%d (local-backend)", req.Method, serverName, req.URL.Path, routeDirective.Host, routeDirective.Port)
		}

		// Secret injection using effective host
		modifiedReq, err := i.policy.OnRequest(req, effectiveHost)
		if err != nil {
			i.emitBlockedEvent(req, serverName, err.Error())
			writeHTTPError(tlsConn, http.StatusForbidden, "Blocked by policy")
			return
		}

		// Connect to backend per-request
		var upstreamConn net.Conn

		if routeDirective != nil {
			target := net.JoinHostPort(routeDirective.Host, fmt.Sprintf("%d", routeDirective.Port))
			if routeDirective.UseTLS {
				upstreamConn, err = tls.Dial("tcp", target, &tls.Config{
					ServerName: routeDirective.Host,
				})
			} else {
				upstreamConn, err = net.DialTimeout("tcp", target, 30*time.Second)
			}
			if err != nil {
				writeHTTPError(tlsConn, http.StatusBadGateway, "Failed to connect to routed backend")
				return
			}
		} else {
			target := net.JoinHostPort(serverName, fmt.Sprintf("%d", dstPort))
			upstreamConn, err = tls.Dial("tcp", target, &tls.Config{
				ServerName: serverName,
			})
			if err != nil {
				return
			}
		}

		// Forward request to upstream
		if err := modifiedReq.Write(upstreamConn); err != nil {
			upstreamConn.Close()
			return
		}

		// Read response
		upstreamReader := bufio.NewReader(upstreamConn)
		resp, err := http.ReadResponse(upstreamReader, modifiedReq)
		if err != nil {
			upstreamConn.Close()
			return
		}

		// Inject X-Routed-Via header on routed responses
		if routeDirective != nil {
			resp.Header.Set("X-Routed-Via", "local-backend")
		}

		// OnResponse
		modifiedResp, err := i.policy.OnResponse(resp, modifiedReq, serverName)
		if err != nil {
			resp.Body.Close()
			upstreamConn.Close()
			return
		}

		// Buffer full response body
		body, err := io.ReadAll(modifiedResp.Body)
		resp.Body.Close()
		upstreamConn.Close()
		if err != nil {
			return
		}

		modifiedResp.Body = io.NopCloser(strings.NewReader(string(body)))
		modifiedResp.ContentLength = int64(len(body))
		modifiedResp.TransferEncoding = nil
		modifiedResp.Header.Del("Transfer-Encoding")
		modifiedResp.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))

		duration := time.Since(start)
		if routeDirective != nil {
			log.Printf("[matchlock] route complete: %s %s%s -> %d %s (%dms, %d bytes)", req.Method, serverName, req.URL.Path, modifiedResp.StatusCode, effectiveHost, duration.Milliseconds(), len(body))
		}
		i.emitEvent(modifiedReq, modifiedResp, serverName, duration)

		if err := writeResponse(tlsConn, modifiedResp); err != nil {
			return
		}

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

	// Go's http.ReadResponse strips Transfer-Encoding and decodes the body.
	// Re-add chunked encoding so the guest HTTP parser can process the
	// streamed body incrementally (required for SSE / text/event-stream).
	resp.Header.Set("Transfer-Encoding", "chunked")
	resp.Header.Del("Content-Length")

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
			// Write chunk: hex size, CRLF, data, CRLF
			if _, err := fmt.Fprintf(conn, "%x\r\n", n); err != nil {
				return err
			}
			if _, err := conn.Write(buf[:n]); err != nil {
				return err
			}
			if _, err := conn.Write([]byte("\r\n")); err != nil {
				return err
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				// Write terminal chunk: 0-length chunk + trailing CRLF
				_, err := conn.Write([]byte("0\r\n\r\n"))
				return err
			}
			return readErr
		}
	}
}
