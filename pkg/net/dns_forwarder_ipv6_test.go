//go:build linux

package net

import (
	"bufio"
	"encoding/binary"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/policy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startStubAAAAUpstream answers every received query with a canned AAAA answer
// built from the query itself and reports the exact bytes it sent on sent, so a
// test can compare what the forwarder returns against the upstream response
// byte-for-byte.
func startStubAAAAUpstream(t *testing.T, answer string, sent chan<- []byte) string {
	t.Helper()

	ln, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = ln.Close()
	})

	go func() {
		buf := make([]byte, dnsPacketBufSize)
		for {
			n, addr, err := ln.ReadFromUDP(buf)
			if err != nil {
				return
			}
			resp := buildAddressAnswer(buf[:n], 28, []net.IP{net.ParseIP(answer)})
			if resp == nil {
				continue
			}
			sent <- resp
			_, _ = ln.WriteToUDP(resp, addr)
		}
	}()

	return ln.LocalAddr().String()
}

// responseAnswerIPv6 decodes the single AAAA answer record of a DNS response.
func responseAnswerIPv6(t *testing.T, resp []byte) net.IP {
	t.Helper()

	require.GreaterOrEqual(t, len(resp), 12, "response too short")
	ancount := binary.BigEndian.Uint16(resp[6:8])
	require.NotZero(t, ancount, "expected at least 1 answer")

	off := 12
	for off < len(resp) && resp[off] != 0 {
		off += 1 + int(resp[off])
	}
	off += 5 // null terminator + qtype(2) + qclass(2)
	// Answer record header: name pointer(2) + type(2) + class(2) + ttl(4).
	require.LessOrEqual(t, off+10+2+16, len(resp), "response too short to contain an AAAA record")
	require.Equal(t, uint16(28), binary.BigEndian.Uint16(resp[off+2:off+4]), "expected an AAAA record")
	off += 10
	rdlen := binary.BigEndian.Uint16(resp[off : off+2])
	off += 2
	require.Equal(t, uint16(16), rdlen, "expected an IPv6 answer")
	require.LessOrEqual(t, off+16, len(resp), "response too short for IPv6 answer")

	return net.IP(resp[off : off+16])
}

// queryForwarderOver sends query over the given family to the forwarder and
// returns the response bytes.
func queryForwarderOver(t *testing.T, network, host string, port int, query []byte) []byte {
	t.Helper()

	conn, err := net.DialUDP(network, nil, &net.UDPAddr{IP: net.ParseIP(host), Port: port})
	require.NoError(t, err)
	defer conn.Close()

	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	_, err = conn.Write(query)
	require.NoError(t, err)

	buf := make([]byte, dnsPacketBufSize)
	n, err := conn.Read(buf)
	require.NoError(t, err, "no DNS response over %s to %s:%d", network, host, port)
	return buf[:n]
}

// TestDNSForwarderAcceptsIPv6BindAddr proves the constructor takes an IPv6 bind
// address (the guest's IPv6 gateway), binds a udp6 socket and reports a usable
// port, forwarding a query that arrived over IPv6.
func TestDNSForwarderAcceptsIPv6BindAddr(t *testing.T) {
	requireIPv6Loopback(t)

	sent := make(chan []byte, 1)
	upstream := startStubAAAAUpstream(t, "2001:db8::5", sent)

	d, err := NewDNSForwarder("::1", []string{upstream})
	require.NoError(t, err)
	defer d.Close()

	require.NotZero(t, d.Port(), "an IPv6 bind must still report a usable port")
	assert.Empty(t, d.BindAddrV6(), "a single-bind forwarder is not dual-stack")

	require.Len(t, d.conns, 1)
	bound := d.conns[0].LocalAddr().(*net.UDPAddr)
	require.Nil(t, bound.IP.To4(), "the socket must be IPv6")
	assert.Equal(t, "::1", bound.IP.String())

	resp := queryForwarderOver(t, "udp6", "::1", d.Port(), buildQuery(0x0a01, 28, "v6.example"))
	assert.Equal(t, uint16(0x0a01), binary.BigEndian.Uint16(resp[0:2]), "query id not echoed")
	assert.Equal(t, "2001:db8::5", responseAnswerIPv6(t, resp).String())
}

// TestDNSForwarderDualStackServesBothFamilies proves the dual-stack forwarder
// binds the IPv6 gateway on the SAME port the IPv4 socket was assigned (the
// pairing the ip6 DNAT rule relies on: one port number for both families, and
// that is what Port() reports) and that a query arriving on either family is
// forwarded and answered.
func TestDNSForwarderDualStackServesBothFamilies(t *testing.T) {
	requireIPv6Loopback(t)

	sent := make(chan []byte, 2)
	upstream := startStubAAAAUpstream(t, "2001:db8::9", sent)

	d, err := NewDualStackDNSForwarder("127.0.0.1", "::1", []string{upstream})
	require.NoError(t, err)
	defer d.Close()

	assert.Equal(t, "::1", d.BindAddrV6())
	require.Len(t, d.conns, 2, "one IPv4 socket and one IPv6 socket, no more")

	port := d.Port()
	require.NotZero(t, port)

	v4Addr := d.conns[0].LocalAddr().(*net.UDPAddr)
	v6Addr := d.conns[1].LocalAddr().(*net.UDPAddr)
	assert.Equal(t, "127.0.0.1", v4Addr.IP.String())
	assert.Equal(t, "::1", v6Addr.IP.String(), "the v6 socket is on the configured gateway address")
	assert.Equal(t, port, v4Addr.Port)
	assert.Equal(t, port, v6Addr.Port, "the v6 listener must answer on the port the DNS redirect targets")

	for _, tc := range []struct {
		network string
		host    string
		id      uint16
	}{
		{"udp4", "127.0.0.1", 0x0b01},
		{"udp6", "::1", 0x0b02},
	} {
		resp := queryForwarderOver(t, tc.network, tc.host, port, buildQuery(tc.id, 28, "both.example"))
		assert.Equal(t, tc.id, binary.BigEndian.Uint16(resp[0:2]), "%s: query id not echoed", tc.network)
		assert.Equal(t, "2001:db8::9", responseAnswerIPv6(t, resp).String(),
			"%s: query must be serviced on the socket it arrived on", tc.network)
	}
}

// TestDNSForwarderRelaysAAAAAnswerByteForByte proves an AAAA answer is relayed
// unchanged over the IPv6 path: no record stripping, no rewriting, not even a
// re-encoded answer.
func TestDNSForwarderRelaysAAAAAnswerByteForByte(t *testing.T) {
	requireIPv6Loopback(t)

	sent := make(chan []byte, 1)
	upstream := startStubAAAAUpstream(t, "2001:db8:aaaa::1", sent)

	d, err := NewDualStackDNSForwarder("127.0.0.1", "::1", []string{upstream})
	require.NoError(t, err)
	defer d.Close()

	query := buildQuery(0x0c01, 28, "aaaa.example")
	resp := queryForwarderOver(t, "udp6", "::1", d.Port(), query)

	var upstreamResp []byte
	select {
	case upstreamResp = <-sent:
	case <-time.After(3 * time.Second):
		t.Fatal("the forwarder never reached the upstream")
	}

	require.Equal(t, query, append([]byte(nil), query...), "the query itself must be forwarded unchanged")
	require.Equal(t, upstreamResp, resp, "the upstream response must be relayed byte-for-byte")
	assert.Equal(t, "2001:db8:aaaa::1", responseAnswerIPv6(t, resp).String(),
		"the AAAA record must survive the forwarder")

	// A response with the AAAA record intact is longer than the query by the
	// full 16-byte rdata plus the record header: stripping the record would
	// shorten it.
	assert.Greater(t, len(resp), len(query))
}

// TestDNSForwarderDualStackCloseReleasesIPv6Socket proves Close releases the
// IPv6 socket as well as the IPv4 one: after Close both addresses can be
// re-bound on the port the forwarder reported, which is only possible if the
// listeners are really gone.
func TestDNSForwarderDualStackCloseReleasesIPv6Socket(t *testing.T) {
	requireIPv6Loopback(t)

	d, err := NewDualStackDNSForwarder("127.0.0.1", "::1", []string{"127.0.0.1:53"})
	require.NoError(t, err)
	port := d.Port()
	require.NotZero(t, port)

	require.NoError(t, d.Close())

	v4, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	require.NoError(t, err, "Close must release the IPv4 socket")
	defer v4.Close()

	v6, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.ParseIP("::1"), Port: port})
	require.NoError(t, err, "Close must release the IPv6 socket (no leaked listener)")
	defer v6.Close()

	// Nothing is serving that port any more: a query gets no answer.
	conn, err := net.DialUDP("udp6", nil, &net.UDPAddr{IP: net.ParseIP("::1"), Port: port})
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(300*time.Millisecond)))
	_, _ = conn.Write(buildQuery(0x0d01, 28, "closed.example"))
	buf := make([]byte, dnsPacketBufSize)
	_, err = conn.Read(buf)
	require.Error(t, err, "a closed forwarder must not answer")
}

// TestDNSForwarderDualStackIPv6BindFailureReleasesIPv4Socket proves a failed
// IPv6 bind does not leave the IPv4 socket behind: the port is OS-assigned, so
// the constructor's socket is observed through the listenDNS seam and its
// closure is asserted directly (a leaked socket would still be readable).
func TestDNSForwarderDualStackIPv6BindFailureReleasesIPv4Socket(t *testing.T) {
	requireIPv6Loopback(t)

	prev := listenDNS
	var opened []*net.UDPConn
	listenDNS = func(network string, laddr *net.UDPAddr) (*net.UDPConn, error) {
		conn, err := net.ListenUDP(network, laddr)
		if err == nil && network == "udp4" {
			opened = append(opened, conn)
		}
		return conn, err
	}
	t.Cleanup(func() { listenDNS = prev })

	// ::2 is not assigned to any interface: the IPv6 bind always fails.
	_, err := NewDualStackDNSForwarder("127.0.0.1", "::2", []string{"127.0.0.1:53"})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrListen)
	require.Len(t, opened, 1, "the IPv4 socket was opened before the IPv6 bind failed")

	// The socket is already closed, so a read must fail with net.ErrClosed
	// rather than block or return data.
	_, _, readErr := opened[0].ReadFromUDP(make([]byte, 16))
	require.ErrorIs(t, readErr, net.ErrClosed, "the IPv4 socket leaked after the failed dual-stack bind")
}

// TestHTTPInterceptorRefusesNameThatResolvesOnlyToPrivateIPv6 qualifies that a
// name whose only answer is an unlisted private IPv6 address (fc00::/7) is
// refused at connection time: the guest's connection carries the name, the
// policy engine resolves it, and the private answer blocks it before any dial.
func TestHTTPInterceptorRefusesNameThatResolvesOnlyToPrivateIPv6(t *testing.T) {
	dns := newSyntheticDNSWithAAAA(t,
		syntheticDNSConfig{}, // no A answer at all: the name is IPv6-only
		syntheticDNSConfig{"v6only.test.": {{"fc00::7"}}},
	)
	dns.install()
	defer dns.uninstall()

	pol := policy.NewEngine(&api.NetworkConfig{BlockPrivateIPs: true})
	events := make(chan api.Event, 10)
	interceptor := NewHTTPInterceptor(pol, events, nil)

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go interceptor.HandleHTTP(server, "v6only.test", 80)

	req := "GET / HTTP/1.1\r\nHost: v6only.test\r\nConnection: close\r\n\r\n"
	_, err := client.Write([]byte(req))
	require.NoError(t, err)

	require.NoError(t, client.SetReadDeadline(time.Now().Add(3*time.Second)))
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"a name resolving only to an unlisted private IPv6 address must be refused")

	select {
	case ev := <-events:
		require.NotNil(t, ev.Network)
		assert.True(t, ev.Network.Blocked)
		assert.Equal(t, "v6only.test", ev.Network.Host)
	default:
		assert.Fail(t, "expected a blocked network event")
	}
}
