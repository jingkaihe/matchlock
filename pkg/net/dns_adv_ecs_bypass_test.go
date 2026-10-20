//go:build linux

package net

// F2 regression suite: conservative ECS bypass for the ipv4-preferring AAAA
// rewrite, added by the F1/F2 fix pass (E0-FIX2) on top of the vendored
// adversarial-review harness (dns_adv_rev_test.go, dns_adv_ecs_test.go).
//
// CHOSEN POLICY (verbatim): contextAffecting(q) := the query's EDNS OPT carries
// an ECS option (option code 8). For an AAAA query where contextAffecting is
// true: relay the upstream response VERBATIM; do NOT consult the rewrite cache;
// do NOT populate it; do NOT send an A probe. All other queries keep today's
// behavior: plain OPT (no options), DO, CD are handled as before — DO/CD do NOT
// bypass.
//
// Comparability ruling (in-scope scope note, pinned by the tests below): DO and
// CD are NOT treated as context-affecting by this rewrite, so DO/CD-bearing
// queries continue to take the existing rewrite path with the same bare,
// context-free probe and name-based attribution as before. This closes only
// the ECS context question; it is not a claim of universal EDNS/DNSSEC context
// equivalence (a DNSSEC-validating resolver's outcome can still vary per query
// even though this rewrite's own A-existence attribution does not), and a
// synthesized NODATA stays unsigned with AD clear (documented limitation). ECS
// is the only context-affecting option in scope; every other OPT option (known
// or unknown code) is processed normally, also by deliberate decision.
//
// The ECS-bypass regressions in this file FAIL on 1319c5d (the ECS query was
// judged by a bare, context-free A probe: suppressed / probed / cached) and
// PASS after the F2 fix (red evidence: icebox/e0/fix2-red-net.log). The
// controls here — DO/CD non-bypass and the non-AAAA leg — passed both before
// and after; only the intended regressions flip.

import (
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// advECSFullOption is the ECS option as the guest queries below carry it.
var advECSFullOption = dnsmessage.Option{Code: advECSOptionCode, Data: advECSQueryData}

// advAAAAOPTQuery builds a single-question AAAA query carrying one OPT record
// with the given DO/CD flags and options.
func advAAAAOPTQuery(t *testing.T, id uint16, name string, do, cd bool, opts []dnsmessage.Option) []byte {
	t.Helper()
	var optHdr dnsmessage.ResourceHeader
	require.NoError(t, optHdr.SetEDNS0(1232, 0, do))
	return advRevPack(t, dnsmessage.Message{
		Header:    dnsmessage.Header{ID: id, RecursionDesired: true, CheckingDisabled: cd},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET}},
		Additionals: []dnsmessage.Resource{{
			Header: optHdr,
			Body:   &dnsmessage.OPTResource{Options: opts},
		}},
	})
}

// advRawForID returns the raw response bytes the stub sent for the first query
// whose recorded response echoes wantID (the raw bytes are the oracle; the
// test never reconstructs them).
func advRawForID(t *testing.T, s *rwStub, wantID uint16) ([]byte, bool) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sent := range s.sent {
		var m dnsmessage.Message
		if err := m.Unpack(sent.raw); err != nil {
			continue
		}
		if m.Header.ID == wantID {
			return append([]byte(nil), sent.raw...), true
		}
	}
	return nil, false
}

// advRewriteCacheSize snapshots the rewrite cache size under its lock.
func advRewriteCacheSize(d *DNSForwarder) int {
	d.rewriteMu.Lock()
	defer d.rewriteMu.Unlock()
	return len(d.rewriteCache)
}

// advRewriteCacheSnapshot copies the rewrite cache (entry content and expiry
// included) under its lock, so a before/after comparison proves that not only
// the size but every entry stayed untouched.
func advRewriteCacheSnapshot(d *DNSForwarder) map[string]dnsRewriteCacheEntry {
	d.rewriteMu.Lock()
	defer d.rewriteMu.Unlock()
	out := make(map[string]dnsRewriteCacheEntry, len(d.rewriteCache))
	for key, entry := range d.rewriteCache {
		out[key] = entry
	}
	return out
}

// advInflightCount snapshots the number of in-flight rewrite probes under the
// lock (the singleflight table).
func advInflightCount(d *DNSForwarder) int {
	d.rewriteMu.Lock()
	defer d.rewriteMu.Unlock()
	return len(d.rewriteInflight)
}

// advAssertBareProbe asserts that a recorded A probe reached the upstream and
// that it stayed a bare, context-free query (no OPT, no CD): the comparability
// ruling for DO/CD-bearing guest queries.
func advAssertBareProbe(t *testing.T, s *rwStub) {
	t.Helper()
	found := false
	for i := 0; i < s.totalQueries(); i++ {
		var m dnsmessage.Message
		require.NoError(t, m.Unpack(s.rawAt(i)))
		if len(m.Questions) != 1 || m.Questions[0].Type != dnsmessage.TypeA {
			continue
		}
		found = true
		assert.Empty(t, m.Additionals, "the A probe must stay bare (no OPT)")
		assert.False(t, m.Header.CheckingDisabled, "the A probe must not copy the guest's CD flag")
	}
	require.True(t, found, "fixture sanity: an A probe reached the upstream")
}

// TestAdvRevECSBypassColdNoProbeNoCacheWrite: on a COLD cache an ECS-bearing
// AAAA query is relayed verbatim; the relayed leg is the ONLY upstream query
// (no A probe), the guest query reaches the upstream byte-for-byte, and the
// rewrite cache stays empty (no write).
func TestAdvRevECSBypassColdNoProbeNoCacheWrite(t *testing.T) {
	const name = "ecsbypass.example."
	stub := newRWStub(t, rwTableReply(
		map[string][]dnsmessage.Resource{name: {rwARes(t, name, "203.0.113.50", 60)}},
		map[string][]dnsmessage.Resource{name: {rwAAAARes(t, name, "2001:db8::50", 60)}},
	))
	d := newRWForwarder(t, []string{stub.addr()}, "ipv4", func(string) bool { return false })

	query := advAAAAOPTQuery(t, 0x7c01, name, false, false, []dnsmessage.Option{advECSFullOption})
	resp := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), query)

	upstream, ok := stub.sentFor(dnsmessage.TypeAAAA, name)
	require.True(t, ok)
	assert.Equal(t, upstream, resp, "CONTRACT: an ECS-bearing AAAA query must relay the upstream bytes verbatim")
	assert.Equal(t, 1, stub.totalQueries(), "the relayed AAAA+ECS leg must be the ONLY upstream query")
	assert.Equal(t, 0, stub.aQueryCount(), "CONTRACT: no A probe for an ECS-bearing query")
	assert.Equal(t, query, stub.rawAt(0), "the guest query must reach the upstream byte-for-byte")
	assert.Zero(t, advRewriteCacheSize(d), "CONTRACT: an ECS-bearing query must not populate the rewrite cache")
}

// TestAdvRevECSBypassDoesNotReadWarmCache: a warm POSITIVE verdict for the
// same name must not be consulted for an ECS-bearing query (that is the
// strongest no-read signal: the cache would otherwise suppress), the cached
// entry itself must stay untouched (content and expiry, not just size), and
// the verdict must keep serving plain queries afterwards.
func TestAdvRevECSBypassDoesNotReadWarmCache(t *testing.T) {
	const name = "ecswarm.example."
	stub := newRWStub(t, rwTableReply(
		map[string][]dnsmessage.Resource{name: {rwARes(t, name, "203.0.113.51", 60)}},
		map[string][]dnsmessage.Resource{name: {rwAAAARes(t, name, "2001:db8::51", 60)}},
	))
	d := newRWForwarder(t, []string{stub.addr()}, "ipv4", func(string) bool { return false })

	// Warm a positive verdict (A exists) via a plain AAAA query.
	q1 := buildQuery(0x7c10, 28, "ecswarm.example")
	requireNODATAResponse(t, q1, queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), q1))
	require.Equal(t, 1, stub.aQueryCount(), "sanity: the plain query probed")
	warm := advRewriteCacheSnapshot(d)
	require.Len(t, warm, 1, "sanity: the positive verdict is cached")

	// ECS query for the SAME name: a warm positive verdict would suppress a
	// rewrite-path query, so the verbatim relay below is the observable no-read
	// signal; the snapshot assertions prove the entry was not modified either.
	resp := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(),
		advAAAAOPTQuery(t, 0x7c11, name, false, false, []dnsmessage.Option{advECSFullOption}))
	upstream, ok := advRawForID(t, stub, 0x7c11)
	require.True(t, ok, "fixture sanity: the upstream answered the ECS query")
	assert.Equal(t, upstream, resp, "CONTRACT: warm positive evidence must NOT be consulted for an ECS-bearing query; upstream bytes relayed verbatim")
	assert.Equal(t, 1, stub.aQueryCount(), "no probe for the ECS query")
	assert.Equal(t, warm, advRewriteCacheSnapshot(d), "CONTRACT: the cached verdict must be neither read nor written")

	// The cached plain verdict still serves plain queries.
	q3 := buildQuery(0x7c12, 28, "ecswarm.example")
	requireNODATAResponse(t, q3, queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), q3))
	assert.Equal(t, 1, stub.aQueryCount(), "the plain verdict stays cached after ECS traffic")
	assert.Equal(t, warm, advRewriteCacheSnapshot(d), "the warm cache entry is untouched by the ECS query")
}

// TestAdvRevECSBypassWritesNoCacheThenPlainRulesApply: an ECS-bearing query
// must not write the cache; a following plain query for the same name must
// behave exactly per the plain rules (fresh probe, suppression, then normal
// caching).
func TestAdvRevECSBypassWritesNoCacheThenPlainRulesApply(t *testing.T) {
	const name = "ecswrite.example."
	stub := newRWStub(t, rwTableReply(
		map[string][]dnsmessage.Resource{name: {rwARes(t, name, "203.0.113.52", 60)}},
		map[string][]dnsmessage.Resource{name: {rwAAAARes(t, name, "2001:db8::52", 60)}},
	))
	d := newRWForwarder(t, []string{stub.addr()}, "ipv4", func(string) bool { return false })

	// ECS first: verbatim, no probe, nothing cached.
	resp := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(),
		advAAAAOPTQuery(t, 0x7c20, name, false, false, []dnsmessage.Option{advECSFullOption}))
	upstream, ok := advRawForID(t, stub, 0x7c20)
	require.True(t, ok)
	assert.Equal(t, upstream, resp, "CONTRACT: ECS query relays verbatim")
	assert.Equal(t, 0, stub.aQueryCount(), "CONTRACT: no probe for the ECS query")
	assert.Zero(t, advRewriteCacheSize(d), "CONTRACT: an ECS-bearing query must not populate the rewrite cache")

	// Plain query: must probe freshly and suppress per the plain rules.
	q2 := buildQuery(0x7c21, 28, "ecswrite.example")
	requireNODATAResponse(t, q2, queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), q2))
	assert.Equal(t, 1, stub.aQueryCount(), "the plain query probes freshly")

	// And the fresh plain verdict caches normally.
	q3 := buildQuery(0x7c22, 28, "ecswrite.example")
	requireNODATAResponse(t, q3, queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), q3))
	assert.Equal(t, 1, stub.aQueryCount(), "the fresh plain verdict caches normally")
}

// TestAdvRevDOAndCDDoNotBypass: DO=1, CD=1 and a non-ECS unknown OPT option
// are NOT context-affecting — the plain path (bare probe, suppression) stays
// in force. The probe itself stays bare and must not copy CD.
func TestAdvRevDOAndCDDoNotBypass(t *testing.T) {
	const name = "dontbypass.example."
	cases := []struct {
		name string
		do   bool
		cd   bool
		opts []dnsmessage.Option
	}{
		{"do-set", true, false, nil},
		{"cd-set", false, true, nil},
		{"do-cd-and-unknown-option", true, true, []dnsmessage.Option{{Code: 65001, Data: []byte{0xaa, 0x01}}}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newRWStub(t, rwTableReply(
				map[string][]dnsmessage.Resource{name: {rwARes(t, name, "203.0.113.53", 60)}},
				map[string][]dnsmessage.Resource{name: {rwAAAARes(t, name, "2001:db8::53", 60)}},
			))
			d := newRWForwarder(t, []string{stub.addr()}, "ipv4", func(string) bool { return false })

			query := advAAAAOPTQuery(t, uint16(0x7c30+i), name, tc.do, tc.cd, tc.opts)
			resp := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), query)

			requireNODATAResponse(t, query, resp)
			assert.Equal(t, 1, stub.aQueryCount(), "DO/CD/unknown options must not bypass: the probe must run")
			advAssertBareProbe(t, stub)
		})
	}
}

// TestAdvRevECSBypassDetectsECSRobustly: ECS presence alone bypasses — the
// option's position, repetition, flags and payload validation are irrelevant
// (only the option CODE matters; no address family, prefix length, nonempty or
// validated payload is required).
func TestAdvRevECSBypassDetectsECSRobustly(t *testing.T) {
	const name = "ecsrobust.example."
	cases := []struct {
		name string
		do   bool
		cd   bool
		opts []dnsmessage.Option
	}{
		{"ecs-with-do-cd", true, true, []dnsmessage.Option{advECSFullOption}},
		{"ecs-after-unknown-option", false, false, []dnsmessage.Option{{Code: 65001, Data: []byte{0x01}}, advECSFullOption}},
		{"ecs-repeated", false, false, []dnsmessage.Option{advECSFullOption, advECSFullOption}},
		{"ecs-empty-payload", false, false, []dnsmessage.Option{{Code: advECSOptionCode}}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newRWStub(t, rwTableReply(
				map[string][]dnsmessage.Resource{name: {rwARes(t, name, "203.0.113.54", 60)}},
				map[string][]dnsmessage.Resource{name: {rwAAAARes(t, name, "2001:db8::54", 60)}},
			))
			d := newRWForwarder(t, []string{stub.addr()}, "ipv4", func(string) bool { return false })

			id := uint16(0x7c40 + i)
			resp := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), advAAAAOPTQuery(t, id, name, tc.do, tc.cd, tc.opts))
			upstream, ok := advRawForID(t, stub, id)
			require.True(t, ok)
			assert.Equal(t, upstream, resp, "CONTRACT: any ECS-bearing AAAA query relays verbatim")
			assert.Equal(t, 1, stub.totalQueries(), "no second upstream leg may exist")
			assert.Equal(t, 0, stub.aQueryCount(), "no probe for an ECS-bearing query")
			assert.Zero(t, advRewriteCacheSize(d), "nothing cached for an ECS-bearing query")
		})
	}
}

// TestAdvRevECSNonAAAAQueriesUnaffected: ECS on a NON-AAAA query changes
// nothing — only AAAA queries are ever rewritten, so the query and its answer
// pass through byte-for-byte and no probe runs.
func TestAdvRevECSNonAAAAQueriesUnaffected(t *testing.T) {
	const name = "ecslegs.example."
	stub := newRWStub(t, rwTableReply(
		map[string][]dnsmessage.Resource{name: {rwARes(t, name, "203.0.113.55", 60)}},
		nil,
	))
	d := newRWForwarder(t, []string{stub.addr()}, "ipv4", func(string) bool { return false })

	query := advECSQuery(t, 0x7c50, dnsmessage.TypeA, name)
	resp := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), query)

	assert.Equal(t, 1, stub.totalQueries(), "a non-AAAA query must not spawn a probe")
	assert.Equal(t, query, stub.rawAt(0), "the A+ECS guest query must reach the upstream byte-for-byte")
	upstream, ok := stub.sentFor(dnsmessage.TypeA, name)
	require.True(t, ok)
	assert.Equal(t, upstream, resp, "the A+ECS answer must relay verbatim")

	// parse-level: ECS does not gate a non-AAAA query — it is simply not
	// eligible for the rewrite (silent, like every non-AAAA query).
	_, ok2, gate := parseAAAAQuery(query)
	assert.False(t, ok2)
	assert.Empty(t, gate)
}

// TestAdvRevECSBypassDoesNotJoinInFlightProbe: an ECS-bearing AAAA query
// arriving while a plain query for the SAME name holds its A probe in flight
// must answer immediately, verbatim and unchanged, without joining or
// creating any rewrite work (no cache read/write, no second probe). The held
// plain probe then completes normally and still caches its verdict.
//
// The discriminator is structural, not a latency threshold: the stub refuses
// to answer the A probe until releaseProbe is called, and the probe is given a
// 30s budget, so on a broken implementation (which would join the in-flight
// probe) the ECS query could not be answered before the probe is released —
// not even by probe expiry. The test proves the ECS answer arrived while the
// plain probe was demonstrably still in flight.
func TestAdvRevECSBypassDoesNotJoinInFlightProbe(t *testing.T) {
	const name = "ecsinflight.example."

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
			msg.Answers = []dnsmessage.Resource{rwARes(t, name, "203.0.113.56", 60)}
			return msg
		}
		msg.Answers = []dnsmessage.Resource{rwAAAARes(t, name, "2001:db8::56", 60)}
		return msg
	})
	// A generous probe budget: a broken implementation that JOINS the in-flight
	// probe cannot be rescued by probe expiry either.
	d := newInternalRWForwarder(t, []string{stub.addr()}, dnsForwarderSettings{
		mode:        dnsForwardModeIPv4Prefer,
		probeBudget: 30 * time.Second,
	})

	// Query 1 (plain AAAA): reaches the upstream, then blocks in its A probe.
	conn, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: d.Port()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	_, err = conn.Write(buildQuery(0x7d01, 28, "ecsinflight.example"))
	require.NoError(t, err)
	waitFor(t, aSeen, "the plain query's A probe to reach the upstream")
	require.Equal(t, 1, advInflightCount(d), "sanity: the plain query's probe is in flight")

	// Query 2 (AAAA+ECS, same name): must answer before the held probe is
	// released. It never joins the in-flight call.
	ecsResp := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(),
		advAAAAOPTQuery(t, 0x7d02, name, false, false, []dnsmessage.Option{advECSFullOption}))
	upstream, ok := advRawForID(t, stub, 0x7d02)
	require.True(t, ok)
	assert.Equal(t, upstream, ecsResp, "CONTRACT: the ECS answer must be the verbatim upstream response")
	assert.Equal(t, 1, stub.aQueryCount(), "the ECS query must neither create nor join rewrite work")
	require.Equal(t, 1, advInflightCount(d),
		"the plain query's probe must still be in flight when the ECS answer arrived (no join)")
	assert.Empty(t, advRewriteCacheSnapshot(d),
		"CONTRACT: an ECS-bearing query must neither read nor write the rewrite cache (nothing may exist yet)")

	// Release the held probe: the plain query completes and caches normally.
	releaseProbe()
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	buf := make([]byte, dnsPacketBufSize)
	n, err := conn.Read(buf)
	require.NoError(t, err, "the plain query must still be answered after the probe completes")
	requireNODATAResponse(t, buildQuery(0x7d01, 28, "ecsinflight.example"), buf[:n])
	assert.Equal(t, 1, stub.aQueryCount(), "the plain path probed exactly once")
	assert.Equal(t, 1, advRewriteCacheSize(d), "the plain verdict caches normally")
}
