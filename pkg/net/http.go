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

	if !i.policy.IsHostAllowed(serverName) {
		i.emitBlockedEvent(nil, serverName, "host not in allowlist")
		return
	}

	realConn, err := tls.Dial("tcp", net.JoinHostPort(serverName, fmt.Sprintf("%d", dstPort)), &tls.Config{
		ServerName: serverName,
	})
	if err != nil {
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
