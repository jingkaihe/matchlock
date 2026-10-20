//go:build linux

package net

import (
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/dns/dnsmessage"
)

// rwStub is a controllable upstream resolver for the ipv4-preferring rewrite
// tests. Every received query is answered by reply in its own goroutine — so a
// test can hold one probe in flight without stalling the stub's other traffic —
// and both the raw query bytes and the raw response bytes are recorded so
// verbatim-relay cases can assert byte-for-byte preservation.
type rwStub struct {
	t      *testing.T
	ln     *net.UDPConn
	reply  func(dnsmessage.Header, dnsmessage.Question) *dnsmessage.Message
	notify chan dnsmessage.Question

	mu      sync.Mutex
	queries []dnsmessage.Question
	raw     [][]byte
	sent    []rwSent
}

type rwSent struct {
	q   dnsmessage.Question
	raw []byte
}

func newRWStub(t *testing.T, reply func(dnsmessage.Header, dnsmessage.Question) *dnsmessage.Message) *rwStub {
	t.Helper()
	ln, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	require.NoError(t, err)
	s := &rwStub{t: t, ln: ln, reply: reply, notify: make(chan dnsmessage.Question, 32)}
	go s.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *rwStub) addr() string { return s.ln.LocalAddr().String() }

func (s *rwStub) serve() {
	buf := make([]byte, dnsPacketBufSize)
	for {
		n, addr, err := s.ln.ReadFromUDP(buf)
		if err != nil {
			return
		}
		raw := append([]byte(nil), buf[:n]...)
		var p dnsmessage.Parser
		hdr, err := p.Start(raw)
		if err != nil {
			continue
		}
		q, err := p.Question()
		if err != nil {
			continue
		}
		s.mu.Lock()
		s.queries = append(s.queries, q)
		s.raw = append(s.raw, raw)
		s.mu.Unlock()
		select {
		case s.notify <- q:
		default:
		}
		go s.respond(addr, hdr, q)
	}
}

func (s *rwStub) respond(addr *net.UDPAddr, hdr dnsmessage.Header, q dnsmessage.Question) {
	msg := s.reply(hdr, q)
	if msg == nil {
		return // drop: a black-holed resolver
	}
	raw, err := msg.Pack()
	if err != nil {
		return
	}
	s.mu.Lock()
	s.sent = append(s.sent, rwSent{q: q, raw: raw})
	s.mu.Unlock()
	_, _ = s.ln.WriteToUDP(raw, addr)
}

func (s *rwStub) totalQueries() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queries)
}

func (s *rwStub) aQueryCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, q := range s.queries {
		if q.Type == dnsmessage.TypeA {
			n++
		}
	}
	return n
}

func (s *rwStub) rawAt(i int) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	require.Less(s.t, i, len(s.raw))
	return append([]byte(nil), s.raw[i]...)
}

// sentFor returns the raw response the stub sent for the first qtype/name
// query, so a verbatim case can compare it byte-for-byte.
func (s *rwStub) sentFor(qtype dnsmessage.Type, name string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := strings.ToLower(name)
	for _, sent := range s.sent {
		if sent.q.Type == qtype && strings.ToLower(sent.q.Name.String()) == want {
			return append([]byte(nil), sent.raw...), true
		}
	}
	return nil, false
}

// rwEcho builds a stub response skeleton that echoes the query's ID and
// question, like a real resolver.
func rwEcho(hdr dnsmessage.Header, q dnsmessage.Question) *dnsmessage.Message {
	return &dnsmessage.Message{
		Header:    dnsmessage.Header{ID: hdr.ID, Response: true, RecursionDesired: hdr.RecursionDesired},
		Questions: []dnsmessage.Question{q},
	}
}

// rwTableReply answers A and AAAA queries from per-name tables (missing name =
// NOERROR with no answers).
func rwTableReply(a, aaaa map[string][]dnsmessage.Resource) func(dnsmessage.Header, dnsmessage.Question) *dnsmessage.Message {
	return func(hdr dnsmessage.Header, q dnsmessage.Question) *dnsmessage.Message {
		msg := rwEcho(hdr, q)
		name := strings.ToLower(q.Name.String())
		if q.Type == dnsmessage.TypeAAAA {
			msg.Answers = aaaa[name]
		} else {
			msg.Answers = a[name]
		}
		return msg
	}
}

func rwARes(t *testing.T, name, ip string, ttl uint32) dnsmessage.Resource {
	t.Helper()
	// No fatal assertion here: fixtures are static and this helper also runs
	// inside the stub's reply goroutines, where t.FailNow is invalid.
	var body dnsmessage.AResource
	copy(body.A[:], net.ParseIP(ip).To4())
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: ttl},
		Body:   &body,
	}
}

func rwAAAARes(t *testing.T, name, ip string, ttl uint32) dnsmessage.Resource {
	t.Helper()
	// See rwARes: no fatal assertion in a helper that reply goroutines call.
	var body dnsmessage.AAAAResource
	copy(body.AAAA[:], net.ParseIP(ip).To16())
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET, TTL: ttl},
		Body:   &body,
	}
}

func rwCNAMERes(t *testing.T, name, target string, ttl uint32) dnsmessage.Resource {
	t.Helper()
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET, TTL: ttl},
		Body:   &dnsmessage.CNAMEResource{CNAME: dnsmessage.MustNewName(target)},
	}
}

func rwSOARes(t *testing.T, name string, ttl, minTTL uint32) dnsmessage.Resource {
	t.Helper()
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeSOA, Class: dnsmessage.ClassINET, TTL: ttl},
		Body: &dnsmessage.SOAResource{
			NS:      dnsmessage.MustNewName("ns." + name),
			MBox:    dnsmessage.MustNewName("hostmaster." + name),
			Serial:  1,
			Refresh: 3600,
			Retry:   600,
			Expire:  86400,
			MinTTL:  minTTL,
		},
	}
}

// newRWForwarder is the production-shaped constructor: the same
// NewConfiguredDNSForwarder the sandbox uses.
func newRWForwarder(t *testing.T, servers []string, priority string, matcher func(string) bool) *DNSForwarder {
	t.Helper()
	d, err := NewConfiguredDNSForwarder("127.0.0.1", "", servers, DNSForwarderConfig{
		DNSPriority:      priority,
		AllowPrivateName: matcher,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// newInternalRWForwarder reaches the same constructor with the test knobs (a
// small probe budget, an injected clock).
func newInternalRWForwarder(t *testing.T, servers []string, settings dnsForwarderSettings) *DNSForwarder {
	t.Helper()
	ln, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	require.NoError(t, err)
	if settings.mode == dnsForwardModeIPv4Prefer && settings.allowPrivateName == nil {
		settings.allowPrivateName = func(string) bool { return false }
	}
	d := newDNSForwarder([]*net.UDPConn{ln}, "", servers, settings)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// rwClock is a mutex-guarded injectable clock for cache-expiry tests.
type rwClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *rwClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *rwClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func waitFor[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
	return *new(T)
}

// requireNODATAResponse asserts resp is the synthesized NODATA answer for
// query: ID and question echoed, rcode 0, no answers, AA/TC/AD clear, RA set.
func requireNODATAResponse(t *testing.T, query, resp []byte) {
	t.Helper()
	var qMsg, rMsg dnsmessage.Message
	require.NoError(t, qMsg.Unpack(query))
	require.NoError(t, rMsg.Unpack(resp))
	assert.Equal(t, qMsg.Header.ID, rMsg.Header.ID, "query ID must be echoed")
	assert.True(t, rMsg.Response, "QR must be set")
	assert.False(t, rMsg.Truncated, "TC must stay clear")
	assert.False(t, rMsg.Authoritative, "AA must stay clear")
	assert.False(t, rMsg.AuthenticData, "synthesized NODATA is unsigned: AD must stay clear")
	assert.True(t, rMsg.RecursionAvailable, "RA must be set")
	assert.Equal(t, qMsg.Header.RecursionDesired, rMsg.RecursionDesired, "RD must be copied from the query")
	assert.Equal(t, dnsmessage.RCodeSuccess, rMsg.RCode)
	require.Len(t, rMsg.Questions, 1, "QDCOUNT must be 1")
	assert.Equal(t, qMsg.Questions[0].Name, rMsg.Questions[0].Name, "question name echoed")
	assert.Equal(t, qMsg.Questions[0].Type, rMsg.Questions[0].Type, "question type echoed")
	assert.Equal(t, qMsg.Questions[0].Class, rMsg.Questions[0].Class, "question class echoed")
	assert.Empty(t, rMsg.Answers, "ANCOUNT must be 0")
	assert.Empty(t, rMsg.Authorities, "NSCOUNT must be 0")
}

// TestDNSRewriteSynthesizesNODATAWhenAExists covers the core rewrite: an AAAA
// query for a name with an A record is answered with a synthesized NODATA
// response (question echoed verbatim from the guest query), and exactly one A
// probe was issued.
func TestDNSRewriteSynthesizesNODATAWhenAExists(t *testing.T) {
	stub := newRWStub(t, rwTableReply(
		map[string][]dnsmessage.Resource{"e0.example.": {rwARes(t, "e0.example.", "203.0.113.7", 60)}},
		map[string][]dnsmessage.Resource{"e0.example.": {rwAAAARes(t, "e0.example.", "2001:db8::7", 60)}},
	))
	// The empty priority is the explicit default: ipv4-preferring.
	d := newRWForwarder(t, []string{stub.addr()}, "", func(string) bool { return false })

	query := buildQuery(0x4242, 28, "e0.example")
	resp := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), query)

	requireNODATAResponse(t, query, resp)
	assert.Equal(t, 1, stub.aQueryCount(), "exactly one A probe")
	assert.Equal(t, 2, stub.totalQueries(), "the relayed AAAA query plus one probe, no more")
	if upstream, ok := stub.sentFor(dnsmessage.TypeAAAA, "e0.example."); ok {
		assert.NotEqual(t, upstream, resp, "the upstream AAAA answer must not be relayed when A exists")
	}

	// The question section is echoed verbatim from the guest query bytes.
	qEnd := questionEndOffset(t, query)
	require.LessOrEqual(t, qEnd, len(resp))
	assert.Equal(t, query[12:qEnd], resp[12:qEnd], "question section must be echoed verbatim")
}

// TestDNSRewriteRelaysVerbatimWhenAAbsent proves the no-A path relays the
// upstream AAAA response byte-for-byte.
func TestDNSRewriteRelaysVerbatimWhenAAbsent(t *testing.T) {
	stub := newRWStub(t, rwTableReply(
		map[string][]dnsmessage.Resource{}, // no A at all
		map[string][]dnsmessage.Resource{"absent.example.": {rwAAAARes(t, "absent.example.", "2001:db8::8", 60)}},
	))
	d := newRWForwarder(t, []string{stub.addr()}, "ipv4", func(string) bool { return false })

	resp := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), buildQuery(0x4343, 28, "absent.example"))

	upstream, ok := stub.sentFor(dnsmessage.TypeAAAA, "absent.example.")
	require.True(t, ok, "the AAAA query must reach the upstream")
	assert.Equal(t, upstream, resp, "the AAAA response must be relayed byte-for-byte")
	assert.Equal(t, 1, stub.aQueryCount(), "the probe must have run once")
}

// TestDNSRewriteRelaysAQueryVerbatim proves a non-AAAA query is untouched in
// ipv4 mode: the upstream receives the guest bytes verbatim and the guest
// receives the upstream answer byte-for-byte.
func TestDNSRewriteRelaysAQueryVerbatim(t *testing.T) {
	stub := newRWStub(t, rwTableReply(
		map[string][]dnsmessage.Resource{"a.example.": {rwARes(t, "a.example.", "203.0.113.9", 60)}},
		nil,
	))
	d := newRWForwarder(t, []string{stub.addr()}, "ipv4", func(string) bool { return false })

	query := buildQuery(0x4444, 1, "a.example")
	resp := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), query)

	assert.Equal(t, 1, stub.totalQueries(), "an A query must not spawn a probe")
	assert.Equal(t, query, stub.rawAt(0), "the guest query must reach the upstream byte-for-byte")
	upstream, ok := stub.sentFor(dnsmessage.TypeA, "a.example.")
	require.True(t, ok)
	assert.Equal(t, upstream, resp, "the A answer must be relayed byte-for-byte")
}

// TestDNSRewriteIPv6ModeRelaysAAAAEvenWithA proves the verbatim mode: even
// though the name has an A record, the AAAA answer is relayed verbatim and NO A
// probe is sent.
func TestDNSRewriteIPv6ModeRelaysAAAAEvenWithA(t *testing.T) {
	stub := newRWStub(t, rwTableReply(
		map[string][]dnsmessage.Resource{"v6mode.example.": {rwARes(t, "v6mode.example.", "203.0.113.10", 60)}},
		map[string][]dnsmessage.Resource{"v6mode.example.": {rwAAAARes(t, "v6mode.example.", "2001:db8::10", 60)}},
	))
	// "ipv6" mode must not require the matcher: verbatim relay ignores it.
	d := newRWForwarder(t, []string{stub.addr()}, "ipv6", nil)

	resp := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), buildQuery(0x4545, 28, "v6mode.example"))

	upstream, ok := stub.sentFor(dnsmessage.TypeAAAA, "v6mode.example.")
	require.True(t, ok)
	assert.Equal(t, upstream, resp, "ipv6 mode must relay the AAAA answer verbatim")
	assert.Equal(t, 1, stub.totalQueries(), "ipv6 mode must send no probe")
	assert.Equal(t, 0, stub.aQueryCount(), "no A probe may be sent in ipv6 mode")
}

// TestDNSRewriteAllowPrivateNameSkipsProbe proves an allow_private name is
// relayed verbatim with NO probe, and that the matcher receives the normalized
// name (lowercase, no trailing dot).
func TestDNSRewriteAllowPrivateNameSkipsProbe(t *testing.T) {
	stub := newRWStub(t, rwTableReply(
		map[string][]dnsmessage.Resource{"private.example.": {rwARes(t, "private.example.", "203.0.113.11", 60)}},
		map[string][]dnsmessage.Resource{"private.example.": {rwAAAARes(t, "private.example.", "2001:db8::11", 60)}},
	))
	var mu sync.Mutex
	var seen []string
	matcher := func(name string) bool {
		mu.Lock()
		seen = append(seen, name)
		mu.Unlock()
		return name == "private.example"
	}
	d := newRWForwarder(t, []string{stub.addr()}, "ipv4", matcher)

	resp := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), buildQuery(0x4646, 28, "private.example"))

	upstream, ok := stub.sentFor(dnsmessage.TypeAAAA, "private.example.")
	require.True(t, ok)
	assert.Equal(t, upstream, resp, "an allow_private name must be relayed verbatim")
	assert.Equal(t, 0, stub.aQueryCount(), "an allow_private name must not be probed")
	mu.Lock()
	defer mu.Unlock()
	assert.Contains(t, seen, "private.example", "the matcher must receive the normalized name")
}

// TestDNSRewriteProbeBudgetIsBoundedAndNotMultiplied proves the probe deadline
// is a TOTAL budget across the resolver fallback list: with two black-holed A
// resolvers the probe finishes within the budget (not twice it), the second
// resolver is never probed, and the guest still gets the verbatim AAAA answer.
func TestDNSRewriteProbeBudgetIsBoundedAndNotMultiplied(t *testing.T) {
	dropA := func(aaaaIP string) func(dnsmessage.Header, dnsmessage.Question) *dnsmessage.Message {
		return func(hdr dnsmessage.Header, q dnsmessage.Question) *dnsmessage.Message {
			if q.Type == dnsmessage.TypeA {
				return nil // black hole
			}
			msg := rwEcho(hdr, q)
			msg.Answers = []dnsmessage.Resource{rwAAAARes(t, strings.ToLower(q.Name.String()), aaaaIP, 60)}
			return msg
		}
	}
	first := newRWStub(t, dropA("2001:db8::21"))
	second := newRWStub(t, dropA("2001:db8::22"))

	const budget = 300 * time.Millisecond
	d := newInternalRWForwarder(t, []string{first.addr(), second.addr()}, dnsForwarderSettings{
		mode:        dnsForwardModeIPv4Prefer,
		probeBudget: budget,
	})

	started := time.Now()
	resp := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), buildQuery(0x4747, 28, "hole.example"))
	elapsed := time.Since(started)

	upstream, ok := first.sentFor(dnsmessage.TypeAAAA, "hole.example.")
	require.True(t, ok)
	assert.Equal(t, upstream, resp, "an UNKNOWN probe must relay the AAAA answer verbatim")
	assert.Less(t, elapsed, 3*time.Second, "the probe must complete within its bounded total budget")
	assert.Greater(t, elapsed, budget/2, "the budget really was consumed by the black hole")
	assert.Equal(t, 1, first.aQueryCount(), "the first resolver must be probed")
	assert.Equal(t, 0, second.aQueryCount(), "the budget must not be re-spent on the second resolver")

	// The production budget must stay within the mandated bound.
	assert.LessOrEqual(t, dnsProbeBudget, 3*time.Second, "production probe budget must be <= 3s")
}

// TestDNSRewriteCachePositiveRespectedAndExpired proves a positive verdict is
// cached (no second probe) and that expiry re-probes, on an injected clock.
func TestDNSRewriteCachePositiveRespectedAndExpired(t *testing.T) {
	stub := newRWStub(t, rwTableReply(
		map[string][]dnsmessage.Resource{"cached.example.": {rwARes(t, "cached.example.", "203.0.113.12", 60)}},
		map[string][]dnsmessage.Resource{"cached.example.": {rwAAAARes(t, "cached.example.", "2001:db8::12", 60)}},
	))
	clock := &rwClock{now: time.Unix(1_700_000_000, 0)}
	d := newInternalRWForwarder(t, []string{stub.addr()}, dnsForwarderSettings{
		mode: dnsForwardModeIPv4Prefer,
		now:  clock.Now,
	})

	first := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), buildQuery(0x4848, 28, "cached.example"))
	requireNODATAResponse(t, buildQuery(0x4848, 28, "cached.example"), first)
	require.Equal(t, 1, stub.aQueryCount())

	second := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), buildQuery(0x4949, 28, "cached.example"))
	requireNODATAResponse(t, buildQuery(0x4949, 28, "cached.example"), second)
	assert.Equal(t, 1, stub.aQueryCount(), "a cached verdict must not be probed again")

	clock.Advance(61 * time.Second) // past the 60s record TTL
	third := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), buildQuery(0x4a4a, 28, "cached.example"))
	requireNODATAResponse(t, buildQuery(0x4a4a, 28, "cached.example"), third)
	assert.Equal(t, 2, stub.aQueryCount(), "an expired verdict must be re-probed")
}

// TestDNSRewriteZeroTTLPositiveNotCached proves a TTL-0 A record produces a
// one-shot verdict: the next AAAA query probes again.
func TestDNSRewriteZeroTTLPositiveNotCached(t *testing.T) {
	stub := newRWStub(t, rwTableReply(
		map[string][]dnsmessage.Resource{"zerottl.example.": {rwARes(t, "zerottl.example.", "203.0.113.13", 0)}},
		map[string][]dnsmessage.Resource{"zerottl.example.": {rwAAAARes(t, "zerottl.example.", "2001:db8::13", 60)}},
	))
	d := newRWForwarder(t, []string{stub.addr()}, "ipv4", func(string) bool { return false })

	q1 := buildQuery(0x4b4b, 28, "zerottl.example")
	first := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), q1)
	requireNODATAResponse(t, q1, first)
	require.Equal(t, 1, stub.aQueryCount())

	q2 := buildQuery(0x4c4c, 28, "zerottl.example")
	second := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), q2)
	requireNODATAResponse(t, q2, second)
	assert.Equal(t, 2, stub.aQueryCount(), "a TTL-0 positive must not be cached")
}

// TestDNSRewritePositiveTTLIsMinAcrossChain proves the cached lifetime is the
// minimum TTL across the WHOLE validated CNAME chain (the CNAME's 5s wins over
// the terminal A's 60s).
func TestDNSRewritePositiveTTLIsMinAcrossChain(t *testing.T) {
	stub := newRWStub(t, rwTableReply(
		map[string][]dnsmessage.Resource{
			"chain.example.": {rwCNAMERes(t, "chain.example.", "target.example.", 5), rwARes(t, "target.example.", "203.0.113.14", 60)},
		},
		map[string][]dnsmessage.Resource{
			"chain.example.": {rwCNAMERes(t, "chain.example.", "target.example.", 30), rwAAAARes(t, "target.example.", "2001:db8::14", 30)},
		},
	))
	clock := &rwClock{now: time.Unix(1_700_000_000, 0)}
	d := newInternalRWForwarder(t, []string{stub.addr()}, dnsForwarderSettings{
		mode: dnsForwardModeIPv4Prefer,
		now:  clock.Now,
	})

	first := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), buildQuery(0x4d4d, 28, "chain.example"))
	requireNODATAResponse(t, buildQuery(0x4d4d, 28, "chain.example"), first)
	require.Equal(t, 1, stub.aQueryCount())

	clock.Advance(4 * time.Second)
	queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), buildQuery(0x4e4e, 28, "chain.example"))
	assert.Equal(t, 1, stub.aQueryCount(), "within the chain's minimum TTL the verdict stays cached")

	clock.Advance(2 * time.Second) // t=6s: past the CNAME's 5s
	queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), buildQuery(0x4f4f, 28, "chain.example"))
	assert.Equal(t, 2, stub.aQueryCount(), "past the chain's minimum TTL the verdict expires")
}

// TestDNSRewritePositiveTTLClampedAtMax proves only the upper bound is clamped:
// a 3600s record TTL is cached for 300s.
func TestDNSRewritePositiveTTLClampedAtMax(t *testing.T) {
	stub := newRWStub(t, rwTableReply(
		map[string][]dnsmessage.Resource{"clamp.example.": {rwARes(t, "clamp.example.", "203.0.113.15", 3600)}},
		map[string][]dnsmessage.Resource{"clamp.example.": {rwAAAARes(t, "clamp.example.", "2001:db8::15", 60)}},
	))
	clock := &rwClock{now: time.Unix(1_700_000_000, 0)}
	d := newInternalRWForwarder(t, []string{stub.addr()}, dnsForwarderSettings{
		mode: dnsForwardModeIPv4Prefer,
		now:  clock.Now,
	})

	queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), buildQuery(0x5050, 28, "clamp.example"))
	require.Equal(t, 1, stub.aQueryCount())

	clock.Advance(299 * time.Second)
	queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), buildQuery(0x5151, 28, "clamp.example"))
	assert.Equal(t, 1, stub.aQueryCount(), "within the 300s clamp the verdict stays cached")

	clock.Advance(2 * time.Second) // t=301s
	queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), buildQuery(0x5252, 28, "clamp.example"))
	assert.Equal(t, 2, stub.aQueryCount(), "past the 300s clamp the verdict expires")
}

// TestDNSRewriteNegativeTTLFromSOA proves an NXDOMAIN probe reply is NEGATIVE
// evidence with a SOA-derived cache lifetime (min of SOA record TTL and
// SOA.MINIMUM), and that the AAAA answer stays verbatim.
func TestDNSRewriteNegativeTTLFromSOA(t *testing.T) {
	stub := newRWStub(t, func(hdr dnsmessage.Header, q dnsmessage.Question) *dnsmessage.Message {
		msg := rwEcho(hdr, q)
		if q.Type == dnsmessage.TypeA {
			msg.Header.RCode = dnsmessage.RCodeNameError
			// min(SOA record TTL 200, SOA.MINIMUM 45) = 45s — deliberately
			// NOT 30s so the test cannot pass with a hardcoded 30s negative.
			msg.Authorities = []dnsmessage.Resource{rwSOARes(t, "example.", 200, 45)}
			return msg
		}
		msg.Answers = []dnsmessage.Resource{rwAAAARes(t, strings.ToLower(q.Name.String()), "2001:db8::16", 60)}
		return msg
	})
	clock := &rwClock{now: time.Unix(1_700_000_000, 0)}
	d := newInternalRWForwarder(t, []string{stub.addr()}, dnsForwarderSettings{
		mode: dnsForwardModeIPv4Prefer,
		now:  clock.Now,
	})

	resp := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), buildQuery(0x5353, 28, "nx.example"))
	upstream, ok := stub.sentFor(dnsmessage.TypeAAAA, "nx.example.")
	require.True(t, ok)
	assert.Equal(t, upstream, resp, "an NXDOMAIN probe verdict must relay the AAAA answer verbatim")
	require.Equal(t, 1, stub.aQueryCount())

	clock.Advance(44 * time.Second)
	queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), buildQuery(0x5454, 28, "nx.example"))
	assert.Equal(t, 1, stub.aQueryCount(), "inside the SOA-derived 45s negative TTL the verdict stays cached")

	clock.Advance(2 * time.Second) // t=46s: past the SOA-derived 45s
	queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), buildQuery(0x5555, 28, "nx.example"))
	assert.Equal(t, 2, stub.aQueryCount(), "past the SOA negative TTL the verdict expires")
}

// TestDNSRewriteNegativeWithoutSOANotCached proves a NOERROR-no-A verdict with
// no SOA in the authorities is decisive but NOT cacheable: the next AAAA query
// probes again.
func TestDNSRewriteNegativeWithoutSOANotCached(t *testing.T) {
	stub := newRWStub(t, rwTableReply(
		map[string][]dnsmessage.Resource{}, // NOERROR, no A, no SOA
		map[string][]dnsmessage.Resource{"nosoa.example.": {rwAAAARes(t, "nosoa.example.", "2001:db8::17", 60)}},
	))
	d := newRWForwarder(t, []string{stub.addr()}, "ipv4", func(string) bool { return false })

	first := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), buildQuery(0x5656, 28, "nosoa.example"))
	upstream, ok := stub.sentFor(dnsmessage.TypeAAAA, "nosoa.example.")
	require.True(t, ok)
	assert.Equal(t, upstream, first, "a no-A verdict must relay the AAAA answer verbatim")
	require.Equal(t, 1, stub.aQueryCount())

	queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), buildQuery(0x5757, 28, "nosoa.example"))
	assert.Equal(t, 2, stub.aQueryCount(), "a negative verdict without SOA must not be cached")
}

// TestDNSRewriteUnattributableAIsUnknown proves the CNAME-attribution rule: A
// records that belong to a different name (and are not reachable through a
// validated CNAME chain from qname) are NOT evidence; they are UNKNOWN, so the
// AAAA answer relays verbatim and nothing is cached.
func TestDNSRewriteUnattributableAIsUnknown(t *testing.T) {
	stub := newRWStub(t, rwTableReply(
		map[string][]dnsmessage.Resource{"unattr.example.": {rwARes(t, "elsewhere.example.", "203.0.113.18", 60)}},
		map[string][]dnsmessage.Resource{"unattr.example.": {rwAAAARes(t, "unattr.example.", "2001:db8::18", 60)}},
	))
	d := newRWForwarder(t, []string{stub.addr()}, "ipv4", func(string) bool { return false })

	first := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), buildQuery(0x5858, 28, "unattr.example"))
	upstream, ok := stub.sentFor(dnsmessage.TypeAAAA, "unattr.example.")
	require.True(t, ok)
	assert.Equal(t, upstream, first, "unattributable A records must relay the AAAA answer verbatim")

	queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), buildQuery(0x5959, 28, "unattr.example"))
	assert.Equal(t, 2, stub.aQueryCount(), "UNKNOWN evidence must never be cached")
}

// TestDNSRewriteUpstreamAAAAErrorsRelayVerbatim proves an upstream AAAA error
// or truncation is never converted into synthetic NODATA (even when an A
// record exists): the upstream bytes are relayed unchanged, and no probe runs
// because the AAAA answer itself is not a normal eligible answer.
func TestDNSRewriteUpstreamAAAAErrorsRelayVerbatim(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(msg *dnsmessage.Message, name string)
	}{
		{"servfail", func(msg *dnsmessage.Message, name string) { msg.Header.RCode = dnsmessage.RCodeServerFailure }},
		{"nxdomain-disagreement", func(msg *dnsmessage.Message, name string) { msg.Header.RCode = dnsmessage.RCodeNameError }},
		{"truncated", func(msg *dnsmessage.Message, name string) { msg.Truncated = true }},
		{"zero-answers", func(msg *dnsmessage.Message, name string) { msg.Answers = nil }},
		{"unattributable-aaaa", func(msg *dnsmessage.Message, name string) {
			msg.Answers = []dnsmessage.Resource{rwAAAARes(t, "other.example.", "2001:db8::ff", 60)}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := "up.example."
			stub := newRWStub(t, func(hdr dnsmessage.Header, q dnsmessage.Question) *dnsmessage.Message {
				msg := rwEcho(hdr, q)
				if q.Type == dnsmessage.TypeA {
					msg.Answers = []dnsmessage.Resource{rwARes(t, name, "203.0.113.19", 60)}
					return msg
				}
				msg.Answers = []dnsmessage.Resource{rwAAAARes(t, name, "2001:db8::19", 60)}
				tc.mutate(msg, name)
				return msg
			})
			d := newRWForwarder(t, []string{stub.addr()}, "ipv4", func(string) bool { return false })

			resp := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), buildQuery(0x5a5a, 28, "up.example"))
			upstream, ok := stub.sentFor(dnsmessage.TypeAAAA, "up.example.")
			require.True(t, ok)
			assert.Equal(t, upstream, resp, "the upstream AAAA response must be relayed verbatim")
			assert.Equal(t, 0, stub.aQueryCount(), "an ineligible AAAA answer must not trigger a probe")
		})
	}
}

// TestDNSRewriteProbeEnvelopeGuards proves the mandated probe-response envelope
// checks: a truncated or ID-mismatched probe reply is UNKNOWN => the AAAA
// answer is relayed verbatim, never synthesized.
func TestDNSRewriteProbeEnvelopeGuards(t *testing.T) {
	cases := []struct {
		name   string
		replyA func(hdr dnsmessage.Header, q dnsmessage.Question, name string) *dnsmessage.Message
	}{
		{"probe-truncated", func(hdr dnsmessage.Header, q dnsmessage.Question, name string) *dnsmessage.Message {
			msg := rwEcho(hdr, q)
			msg.Truncated = true
			msg.Answers = []dnsmessage.Resource{rwARes(t, name, "203.0.113.20", 60)}
			return msg
		}},
		{"probe-id-mismatch", func(hdr dnsmessage.Header, q dnsmessage.Question, name string) *dnsmessage.Message {
			msg := rwEcho(hdr, q)
			msg.Header.ID = hdr.ID + 1
			msg.Answers = []dnsmessage.Resource{rwARes(t, name, "203.0.113.21", 60)}
			return msg
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := "env.example."
			stub := newRWStub(t, func(hdr dnsmessage.Header, q dnsmessage.Question) *dnsmessage.Message {
				if q.Type == dnsmessage.TypeA {
					return tc.replyA(hdr, q, name)
				}
				msg := rwEcho(hdr, q)
				msg.Answers = []dnsmessage.Resource{rwAAAARes(t, strings.ToLower(q.Name.String()), "2001:db8::20", 60)}
				return msg
			})
			d := newRWForwarder(t, []string{stub.addr()}, "ipv4", func(string) bool { return false })

			resp := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), buildQuery(0x5b5b, 28, "env.example"))
			upstream, ok := stub.sentFor(dnsmessage.TypeAAAA, "env.example.")
			require.True(t, ok)
			assert.Equal(t, upstream, resp, "an envelope-invalid probe reply is UNKNOWN: relay verbatim")
			assert.Equal(t, 1, stub.aQueryCount(), "the probe ran")
		})
	}
}

// TestDNSRewriteProbeCNAMEDeadEndNotCached proves a probe reply whose CNAME
// chain dead-ends without an A record and without an SOA is a non-cacheable
// negative: the next AAAA query probes again.
func TestDNSRewriteProbeCNAMEDeadEndNotCached(t *testing.T) {
	stub := newRWStub(t, func(hdr dnsmessage.Header, q dnsmessage.Question) *dnsmessage.Message {
		msg := rwEcho(hdr, q)
		if q.Type == dnsmessage.TypeA {
			// CNAME qname -> target, but target has no A record and the
			// response carries no SOA.
			msg.Answers = []dnsmessage.Resource{rwCNAMERes(t, "deadend.example.", "target.example.", 30)}
			return msg
		}
		msg.Answers = []dnsmessage.Resource{rwAAAARes(t, strings.ToLower(q.Name.String()), "2001:db8::21", 60)}
		return msg
	})
	d := newRWForwarder(t, []string{stub.addr()}, "ipv4", func(string) bool { return false })

	first := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), buildQuery(0x5c5c, 28, "deadend.example"))
	upstream, ok := stub.sentFor(dnsmessage.TypeAAAA, "deadend.example.")
	require.True(t, ok)
	assert.Equal(t, upstream, first, "a no-A verdict must relay the AAAA answer verbatim")
	require.Equal(t, 1, stub.aQueryCount())

	queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), buildQuery(0x5d5d, 28, "deadend.example"))
	assert.Equal(t, 2, stub.aQueryCount(), "a dead-end chain without SOA must not be cached")
}

// TestDNSRewriteEDNSMirrored proves the synthesized NODATA mirrors the query's
// EDNS OPT (payload size class, version, DO bit; zero options; extended rcode
// 0) and keeps AD clear for a DO-bearing query.
func TestDNSRewriteEDNSMirrored(t *testing.T) {
	for _, do := range []bool{true, false} {
		t.Run(map[bool]string{true: "do-set", false: "do-clear"}[do], func(t *testing.T) {
			stub := newRWStub(t, rwTableReply(
				map[string][]dnsmessage.Resource{"edns.example.": {rwARes(t, "edns.example.", "203.0.113.22", 60)}},
				map[string][]dnsmessage.Resource{"edns.example.": {rwAAAARes(t, "edns.example.", "2001:db8::22", 60)}},
			))
			d := newRWForwarder(t, []string{stub.addr()}, "ipv4", func(string) bool { return false })

			var optHdr dnsmessage.ResourceHeader
			require.NoError(t, optHdr.SetEDNS0(1232, 0, do))
			queryMsg := dnsmessage.Message{
				Header:    dnsmessage.Header{ID: 0x5e5e, RecursionDesired: true},
				Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("edns.example."), Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET}},
				Additionals: []dnsmessage.Resource{{
					Header: optHdr,
					Body:   &dnsmessage.OPTResource{},
				}},
			}
			query, err := queryMsg.Pack()
			require.NoError(t, err)

			resp := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), query)
			requireNODATAResponse(t, query, resp)

			var rMsg dnsmessage.Message
			require.NoError(t, rMsg.Unpack(resp))
			require.Len(t, rMsg.Additionals, 1, "the query's OPT must be mirrored")
			opt := rMsg.Additionals[0]
			assert.Equal(t, dnsmessage.TypeOPT, opt.Header.Type)
			assert.Equal(t, dnsmessage.Class(1232), opt.Header.Class, "the UDP payload size class must be mirrored")
			assert.Equal(t, uint8(0), uint8(opt.Header.TTL>>16), "the EDNS version must be mirrored (0)")
			assert.Equal(t, do, opt.Header.DNSSECAllowed(), "the DO bit must be mirrored")
			assert.Equal(t, dnsmessage.RCodeSuccess, opt.Header.ExtendedRCode(rMsg.Header.RCode), "extended rcode must be 0")
			body, ok := opt.Body.(*dnsmessage.OPTResource)
			require.True(t, ok)
			assert.Empty(t, body.Options, "no options may be synthesized")
		})
	}

	t.Run("nonzero-version-and-options", func(t *testing.T) {
		stub := newRWStub(t, rwTableReply(
			map[string][]dnsmessage.Resource{"edns2.example.": {rwARes(t, "edns2.example.", "203.0.113.40", 60)}},
			map[string][]dnsmessage.Resource{"edns2.example.": {rwAAAARes(t, "edns2.example.", "2001:db8::40", 60)}},
		))
		d := newRWForwarder(t, []string{stub.addr()}, "ipv4", func(string) bool { return false })

		// Hand-built OPT: version 1, DO set, extended rcode 0, and a nonempty
		// NON-ECS query option (code 65001; an option code 8 = ECS would take
		// the conservative bypass and never reach synthesis — see
		// dns_adv_ecs_test.go). The version bits and the raw DO bit are
		// mirrored as-is; the synthesized options are empty.
		const (
			doBit      = uint32(1) << 15
			versionOne = uint32(1) << 16
		)
		optHdr := dnsmessage.ResourceHeader{
			Name:  dnsmessage.MustNewName("."),
			Type:  dnsmessage.TypeOPT,
			Class: 1400,
			TTL:   versionOne | doBit,
		}
		queryMsg := dnsmessage.Message{
			Header:    dnsmessage.Header{ID: 0x6060, RecursionDesired: true},
			Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("edns2.example."), Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET}},
			Additionals: []dnsmessage.Resource{{
				Header: optHdr,
				Body:   &dnsmessage.OPTResource{Options: []dnsmessage.Option{{Code: 65001, Data: []byte{0, 1, 24, 0}}}},
			}},
		}
		query, err := queryMsg.Pack()
		require.NoError(t, err)

		resp := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), query)
		requireNODATAResponse(t, query, resp)

		var rMsg dnsmessage.Message
		require.NoError(t, rMsg.Unpack(resp))
		require.Len(t, rMsg.Additionals, 1)
		opt := rMsg.Additionals[0].Header
		assert.Equal(t, uint8(1), uint8(opt.TTL>>16), "the EDNS version bits must be mirrored as-is")
		assert.Equal(t, doBit, opt.TTL&doBit, "the raw DO bit must be mirrored (not version-gated)")
		assert.Zero(t, opt.TTL>>24, "the extended rcode must be 0")
		assert.Equal(t, dnsmessage.Class(1400), opt.Class, "the UDP payload size class must be mirrored")
		body, ok := rMsg.Additionals[0].Body.(*dnsmessage.OPTResource)
		require.True(t, ok)
		assert.Empty(t, body.Options, "no options may be synthesized")
	})
}

// TestDNSRewriteSingleflightCoalescesConcurrentProbes proves concurrent AAAA
// queries for one uncached name collapse to a single upstream A probe: the
// second caller JOINS the in-flight probe (it receives the leader's complete
// outcome, TTL included — the cache-hit path would return no TTL) and only ONE
// A query is sent.
func TestDNSRewriteSingleflightCoalescesConcurrentProbes(t *testing.T) {
	aSeen := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseProbe := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseProbe) // never leak the blocked stub callback on early failure

	stub := newRWStub(t, func(hdr dnsmessage.Header, q dnsmessage.Question) *dnsmessage.Message {
		msg := rwEcho(hdr, q)
		if q.Type == dnsmessage.TypeA {
			select {
			case aSeen <- struct{}{}:
			default:
			}
			<-release
			msg.Answers = []dnsmessage.Resource{rwARes(t, "sf.example.", "203.0.113.23", 60)}
			return msg
		}
		msg.Answers = []dnsmessage.Resource{rwAAAARes(t, strings.ToLower(q.Name.String()), "2001:db8::23", 60)}
		return msg
	})
	d := newInternalRWForwarder(t, []string{stub.addr()}, dnsForwarderSettings{mode: dnsForwardModeIPv4Prefer})

	q := dnsmessage.Question{Name: dnsmessage.MustNewName("sf.example."), Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET}
	results := make([]dnsProbeOutcome, 2)
	followerStarted := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		results[0] = d.probeAEvidence(q)
	}()
	waitFor(t, aSeen, "the first A probe to reach the upstream")
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(followerStarted)
		results[1] = d.probeAEvidence(q)
	}()
	<-followerStarted
	// The leader cannot complete until releaseProbe, so this sleep only lets
	// the follower reach (and block on) the in-flight probe.
	time.Sleep(150 * time.Millisecond)
	releaseProbe()
	wg.Wait()

	assert.Equal(t, dnsProbeAExists, results[0].evidence)
	assert.Equal(t, uint32(60), results[0].ttl)
	assert.Equal(t, results[0], results[1],
		"the follower must receive the leader's COMPLETE outcome (evidence and TTL), proving it joined the in-flight probe")
	assert.Equal(t, 1, stub.aQueryCount(), "concurrent probes for one name must collapse to ONE upstream query")
}

// TestDNSRewriteCloseCancelsInFlightProbe proves Close wakes an in-flight probe
// and a FOLLOWER waiting on that same probe, promptly, even though the probe
// budget is much longer: the leader's exchange is released through the stop
// context / upstream teardown, and the follower is released through the stop
// channel it selects on. Close blocks until every worker exits, so it can only
// return once both paths have unwound, and nothing may stay tracked afterwards.
func TestDNSRewriteCloseCancelsInFlightProbe(t *testing.T) {
	stub := newRWStub(t, func(hdr dnsmessage.Header, q dnsmessage.Question) *dnsmessage.Message {
		if q.Type == dnsmessage.TypeA {
			return nil // hold the probe forever
		}
		msg := rwEcho(hdr, q)
		msg.Answers = []dnsmessage.Resource{rwAAAARes(t, strings.ToLower(q.Name.String()), "2001:db8::24", 60)}
		return msg
	})
	d := newInternalRWForwarder(t, []string{stub.addr()}, dnsForwarderSettings{
		mode:        dnsForwardModeIPv4Prefer,
		probeBudget: 30 * time.Second, // would hang for 30s without cancellation
	})

	conn, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: d.Port()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_, err = conn.Write(buildQuery(0x5f5f, 28, "close.example"))
	require.NoError(t, err)

	// Wait until the probe (the A query) reached the upstream.
	deadline := time.After(5 * time.Second)
waitProbe:
	for {
		select {
		case q := <-stub.notify:
			if q.Type == dnsmessage.TypeA {
				break waitProbe
			}
		case <-deadline:
			t.Fatal("the A probe never reached the upstream")
		}
	}

	// A second guest query for the SAME name joins the in-flight probe. Its
	// AAAA leg still reaches the upstream (the relay runs before the rewrite),
	// so the only query that must NOT appear is a SECOND A probe — the
	// follower has to join the leader's in-flight probe instead.
	followerConn, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: d.Port()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = followerConn.Close() })
	_, err = followerConn.Write(buildQuery(0x6060, 28, "close.example"))
	require.NoError(t, err)

	// Drain stub notifications for a grace period: the follower's AAAA query
	// is expected (the relay runs before the rewrite); a second A probe is
	// not — the follower must join the leader's in-flight probe.
	sawFollowerAAAA := false
	grace := time.After(300 * time.Millisecond)
drain:
	for {
		select {
		case q := <-stub.notify:
			switch q.Type {
			case dnsmessage.TypeA:
				t.Fatalf("a second A probe must not be sent (the follower must join the in-flight probe): %v", q)
			case dnsmessage.TypeAAAA:
				sawFollowerAAAA = true
			}
		case <-grace:
			break drain
		}
	}
	require.True(t, sawFollowerAAAA,
		"the follower's AAAA query must reach the upstream before Close (otherwise this test never exercised the probe path)")

	done := make(chan struct{})
	go func() { _ = d.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close must cancel the in-flight probe (and its follower) promptly")
	}

	// Close waited for every worker to exit, so nothing may remain in flight
	// and no upstream socket may still be tracked. The state is snapshotted
	// under the locks and asserted after unlocking.
	d.rewriteMu.Lock()
	inflight := len(d.rewriteInflight)
	d.rewriteMu.Unlock()
	d.upstreamMu.Lock()
	tracked := len(d.upstreams)
	d.upstreamMu.Unlock()
	require.Zero(t, inflight, "no probe may stay in flight after Close")
	require.Zero(t, tracked, "no upstream socket may stay tracked after Close")
}

// TestDNSRewriteClassifyProbeEdgeCases pins the probe classifier's envelope
// rules directly (the wire-level tests cover the paths that matter for relay).
func TestDNSRewriteClassifyProbeEdgeCases(t *testing.T) {
	expected := dnsmessage.Question{Name: dnsmessage.MustNewName("gate.example."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}
	pack := func(msg dnsmessage.Message) []byte {
		raw, err := msg.Pack()
		require.NoError(t, err)
		return raw
	}
	okReply := func() dnsmessage.Message {
		return dnsmessage.Message{
			Header:    dnsmessage.Header{ID: 7, Response: true},
			Questions: []dnsmessage.Question{expected},
		}
	}

	// Unparsable bytes and non-responses are UNKNOWN.
	assert.Equal(t, dnsProbeUnknown, classifyAProbeResponse([]byte{1, 2, 3}, 7, expected).evidence)
	noQR := okReply()
	noQR.Header.Response = false
	assert.Equal(t, dnsProbeUnknown, classifyAProbeResponse(pack(noQR), 7, expected).evidence)
	wrongOpcode := okReply()
	wrongOpcode.Header.OpCode = 5 // STATUS
	assert.Equal(t, dnsProbeUnknown, classifyAProbeResponse(pack(wrongOpcode), 7, expected).evidence)
	questionMismatch := okReply()
	questionMismatch.Questions = []dnsmessage.Question{{Name: dnsmessage.MustNewName("other.example."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}
	assert.Equal(t, dnsProbeUnknown, classifyAProbeResponse(pack(questionMismatch), 7, expected).evidence)

	// A foreign-class answer record makes the response uninterpretable.
	foreign := okReply()
	res := rwARes(t, "gate.example.", "203.0.113.30", 60)
	res.Header.Class = 3 // CHAOS
	foreign.Answers = []dnsmessage.Resource{res}
	assert.Equal(t, dnsProbeUnknown, classifyAProbeResponse(pack(foreign), 7, expected).evidence)

	// Error rcodes are UNKNOWN; NXDOMAIN is negative.
	servfail := okReply()
	servfail.Header.RCode = dnsmessage.RCodeServerFailure
	assert.Equal(t, dnsProbeUnknown, classifyAProbeResponse(pack(servfail), 7, expected).evidence)
	nx := okReply()
	nx.Header.RCode = dnsmessage.RCodeNameError
	outcome := classifyAProbeResponse(pack(nx), 7, expected)
	assert.Equal(t, dnsProbeNoA, outcome.evidence)
	assert.Zero(t, outcome.ttl, "NXDOMAIN without SOA must not be cacheable")

	// The positive clamp applies to the classifier too.
	positive := okReply()
	positive.Answers = []dnsmessage.Resource{rwARes(t, "gate.example.", "203.0.113.31", 3600)}
	outcome = classifyAProbeResponse(pack(positive), 7, expected)
	assert.Equal(t, dnsProbeAExists, outcome.evidence)
	assert.Equal(t, uint32(300), outcome.ttl, "positive TTL must be clamped at 300s")

	// Malformed (cyclic) CNAME chains are UNKNOWN even when data exists.
	cyclic := okReply()
	cyclic.Answers = []dnsmessage.Resource{
		rwCNAMERes(t, "gate.example.", "loop.example.", 10),
		rwCNAMERes(t, "loop.example.", "gate.example.", 10),
	}
	assert.Equal(t, dnsProbeUnknown, classifyAProbeResponse(pack(cyclic), 7, expected).evidence)

	// A chain deeper than the hop cap is malformed (UNKNOWN).
	deep := okReply()
	name := "gate.example."
	for i := 0; i < 9; i++ {
		next := "hop" + string(rune('a'+i)) + ".example."
		deep.Answers = append(deep.Answers, rwCNAMERes(t, name, next, 10))
		name = next
	}
	deep.Answers = append(deep.Answers, rwARes(t, name, "203.0.113.32", 10))
	assert.Equal(t, dnsProbeUnknown, classifyAProbeResponse(pack(deep), 7, expected).evidence)
}

// TestDNSRewriteNegativeEvidenceDerivation pins the negative-TTL derivation
// directly: both SOA orderings (SOA.MINIMUM wins / record TTL wins), the upper
// clamp, the no-SOA case, and the NOERROR-with-no-A negative path.
func TestDNSRewriteNegativeEvidenceDerivation(t *testing.T) {
	assert.Zero(t, soaNegativeTTL(nil), "no SOA => not cacheable")
	assert.Equal(t, uint32(45), soaNegativeTTL([]dnsmessage.Resource{rwSOARes(t, "example.", 200, 45)}),
		"SOA.MINIMUM below the record TTL must win")
	assert.Equal(t, uint32(40), soaNegativeTTL([]dnsmessage.Resource{rwSOARes(t, "example.", 40, 300)}),
		"a record TTL below SOA.MINIMUM must win")
	assert.Equal(t, uint32(300), soaNegativeTTL([]dnsmessage.Resource{rwSOARes(t, "example.", 3600, 3600)}),
		"the clamp caps only above 300s")

	expected := dnsmessage.Question{Name: dnsmessage.MustNewName("gate.example."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}
	noA := dnsmessage.Message{
		Header:      dnsmessage.Header{ID: 9, Response: true},
		Questions:   []dnsmessage.Question{expected},
		Authorities: []dnsmessage.Resource{rwSOARes(t, "gate.example.", 120, 60)},
	}
	raw, err := noA.Pack()
	require.NoError(t, err)
	outcome := classifyAProbeResponse(raw, 9, expected)
	assert.Equal(t, dnsProbeNoA, outcome.evidence, "NOERROR with zero attributable A records is negative evidence")
	assert.Equal(t, uint32(60), outcome.ttl, "the NOERROR-no-A negative TTL comes from the SOA")
}

// TestDNSRewriteParseQueryGates pins the safety gates that decide whether a
// guest query is even eligible for the rewrite.
func TestDNSRewriteParseQueryGates(t *testing.T) {
	aaaa := dnsmessage.Question{Name: dnsmessage.MustNewName("g.example."), Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET}
	pack := func(msg dnsmessage.Message) []byte {
		raw, err := msg.Pack()
		require.NoError(t, err)
		return raw
	}

	plain, err := queryMsgBytes(0x6001, aaaa, nil)
	require.NoError(t, err)
	_, ok, gate := parseAAAAQuery(plain)
	assert.True(t, ok)
	assert.Empty(t, gate)

	optHdr := dnsmessage.ResourceHeader{}
	require.NoError(t, optHdr.SetEDNS0(4096, 0, false))
	withOPT, err := queryMsgBytes(0x6002, aaaa, []dnsmessage.Resource{{Header: optHdr, Body: &dnsmessage.OPTResource{}}})
	require.NoError(t, err)
	parsed, ok, _ := parseAAAAQuery(withOPT)
	assert.True(t, ok)
	assert.NotNil(t, parsed.opt, "an OPT-bearing query must be eligible with its OPT captured")

	// Non-AAAA types are silently ineligible (no log spam).
	_, ok, gate = parseAAAAQuery(buildQuery(0x6003, 1, "g.example"))
	assert.False(t, ok)
	assert.Empty(t, gate)

	// Non-IN class is gated.
	chaos := aaaa
	chaos.Class = 3
	_, ok, gate = parseAAAAQuery(pack(dnsmessage.Message{Header: dnsmessage.Header{ID: 0x6004, RecursionDesired: true}, Questions: []dnsmessage.Question{chaos}}))
	assert.False(t, ok)
	assert.NotEmpty(t, gate)

	// Two questions are gated.
	two := dnsmessage.Message{Header: dnsmessage.Header{ID: 0x6005, RecursionDesired: true}, Questions: []dnsmessage.Question{aaaa, aaaa}}
	_, ok, gate = parseAAAAQuery(pack(two))
	assert.False(t, ok)
	assert.NotEmpty(t, gate)

	// A non-QUERY opcode is gated.
	status := dnsmessage.Message{Header: dnsmessage.Header{ID: 0x6006, OpCode: 5}, Questions: []dnsmessage.Question{aaaa}}
	_, ok, gate = parseAAAAQuery(pack(status))
	assert.False(t, ok)
	assert.NotEmpty(t, gate)

	// Additionals beyond a single OPT are gated (TSIG-style records included).
	extra := dnsmessage.Message{
		Header:      dnsmessage.Header{ID: 0x6007, RecursionDesired: true},
		Questions:   []dnsmessage.Question{aaaa},
		Additionals: []dnsmessage.Resource{{Header: optHdr, Body: &dnsmessage.OPTResource{}}, rwARes(t, "x.example.", "203.0.113.33", 60)},
	}
	_, ok, gate = parseAAAAQuery(pack(extra))
	assert.False(t, ok)
	assert.NotEmpty(t, gate)
}

// TestDNSRewriteConstructorGuards proves the fail-closed construction rules:
// ipv4 mode demands a matcher, an unknown priority is rejected, and ipv6 mode
// does not require a matcher.
func TestDNSRewriteConstructorGuards(t *testing.T) {
	_, err := NewConfiguredDNSForwarder("127.0.0.1", "", []string{"127.0.0.1:53"}, DNSForwarderConfig{})
	require.ErrorIs(t, err, ErrDNSConfig, "ipv4 mode without a matcher must fail closed")

	_, err = NewConfiguredDNSForwarder("127.0.0.1", "", []string{"127.0.0.1:53"}, DNSForwarderConfig{DNSPriority: "IPv6", AllowPrivateName: func(string) bool { return false }})
	require.ErrorIs(t, err, ErrDNSConfig, "an unsupported priority must be rejected")

	// ipv6 mode tolerates a nil matcher: it never rewrites.
	d, err := NewConfiguredDNSForwarder("127.0.0.1", "", []string{"127.0.0.1:53"}, DNSForwarderConfig{DNSPriority: "ipv6"})
	require.NoError(t, err)
	require.NoError(t, d.Close())
}

// queryMsgBytes packs a query message for the parse-gate tests.
func queryMsgBytes(id uint16, q dnsmessage.Question, additionals []dnsmessage.Resource) ([]byte, error) {
	return (&dnsmessage.Message{
		Header:      dnsmessage.Header{ID: id, RecursionDesired: true},
		Questions:   []dnsmessage.Question{q},
		Additionals: additionals,
	}).Pack()
}

// questionEndOffset returns the byte offset just past the first question of a
// wire query (name + qtype + qclass), for verbatim question-echo assertions.
func questionEndOffset(t *testing.T, msg []byte) int {
	t.Helper()
	off := 12
	for {
		require.Less(t, off, len(msg), "unterminated name in query")
		l := int(msg[off])
		if l == 0 {
			off++
			break
		}
		off += l + 1
	}
	return off + 4
}
