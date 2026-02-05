package net

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/policy"
)

type HTTPInterceptor struct {
	policy    *policy.Engine
	events    chan api.Event
	tlsConfig *tls.Config
	caPool    *CAPool
}

func NewHTTPInterceptor(pol *policy.Engine, events chan api.Event) *HTTPInterceptor {
	caPool, _ := NewCAPool()

	return &HTTPInterceptor{
		policy: pol,
		events: events,
		caPool: caPool,
	}
}

func (i *HTTPInterceptor) CAPool() *CAPool {
	return i.caPool
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

		targetHost := fmt.Sprintf("%s:%d", host, dstPort)

		realConn, err := net.DialTimeout("tcp", targetHost, 30*time.Second)
		if err != nil {
			writeHTTPError(guestConn, http.StatusBadGateway, "Failed to connect")
			return
		}

		if err := modifiedReq.Write(realConn); err != nil {
			realConn.Close()
			return
		}

		resp, err := http.ReadResponse(bufio.NewReader(realConn), modifiedReq)
		if err != nil {
			realConn.Close()
			return
		}

		modifiedResp, err := i.policy.OnResponse(resp, modifiedReq, host)
		if err != nil {
			resp.Body.Close()
			realConn.Close()
			return
		}

		duration := time.Since(start)
		i.emitEvent(modifiedReq, modifiedResp, host, duration)

		if err := writeResponse(guestConn, modifiedResp); err != nil {
			resp.Body.Close()
			realConn.Close()
			return
		}

		resp.Body.Close()
		realConn.Close()

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

	realConn, err := tls.Dial("tcp", fmt.Sprintf("%s:%d", serverName, dstPort), &tls.Config{
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
