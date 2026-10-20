//go:build linux

// Adversarial review harness for the icebox DNS rewrite spike (commit
// 1319c5d0f59872055873243e7344d9811c1eaf5e), written by the independent
// reviewer against the FROZEN contract (E0-CORRECTIONS.md + design v6 §4/§4a),
// vendored into the tree by the E0-FIX2 corrective pass (F1/F2 review fixes;
// see pkg/net/dns_rewrite.go for the fixed behavior).
//
// These tests assert the CONTRACT, not the implementation: a failing assertion
// is a REPRODUCED counterexample. The F1-family counterexamples below were RED
// on 1319c5d and are now the standing regressions for the ASCII-only
// canonicalization fix; the controls (ASCII case sharing, cache bounds,
// taxonomy) must stay green.
//
// Run: TMPDIR=/tmp go test -count=1 -run 'TestAdvRev' -v ./pkg/net/
package net

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// advRevRawName builds a dnsmessage.Name from raw wire bytes (no validation),
// so fixtures can carry labels strings.ToLower would alter.
func advRevRawName(wire string) dnsmessage.Name {
	var n dnsmessage.Name
	n.Length = uint8(len(wire))
	copy(n.Data[:], wire)
	return n
}

func advRevPack(t *testing.T, msg dnsmessage.Message) []byte {
	t.Helper()
	raw, err := msg.Pack()
	require.NoError(t, err)
	return raw
}

// advRevAnyQueryContains reports whether any recorded upstream query contains
// the byte sequence (exact-bytes fixture sanity; no normalization).
func advRevAnyQueryContains(t *testing.T, s *rwStub, seq []byte) bool {
	t.Helper()
	for i := 0; i < s.totalQueries(); i++ {
		if bytes.Contains(s.rawAt(i), seq) {
			return true
		}
	}
	return false
}

// advRevAQueryHasExactBytes reports whether a recorded A-TYPE query carries
// exactly these question-name bytes (no normalization, A queries only).
func advRevAQueryHasExactBytes(t *testing.T, s *rwStub, want []byte) bool {
	t.Helper()
	for i := 0; i < s.totalQueries(); i++ {
		raw := s.rawAt(i)
		var m dnsmessage.Message
		if err := m.Unpack(raw); err != nil || len(m.Questions) != 1 {
			continue
		}
		q := m.Questions[0]
		if q.Type != dnsmessage.TypeA {
			continue
		}
		if bytes.Equal(q.Name.Data[:q.Name.Length], want) {
			return true
		}
	}
	return false
}

// sentForExact returns the raw upstream response recorded for the first query
// whose parsed name matches EXACTLY (byte-for-byte, no lowering): the oracle
// must not reuse the implementation's normalization.
func (s *rwStub) sentForExact(qtype dnsmessage.Type, name string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sent := range s.sent {
		if sent.q.Type == qtype && sent.q.Name.String() == name {
			return append([]byte(nil), sent.raw...), true
		}
	}
	return nil, false
}

// ===========================================================================
// H1: name canonicalization folds UNICODE, not just ASCII.
//
// Authority: RFC 4343 (DNS case-insensitivity is ASCII-only) + E0-CORRECTIONS
// clarifications v2 ("A qualifies only if owner == qname or the final name of
// a validated CNAME chain from qname"; unattributable A => UNKNOWN) + design
// v6 §4 ("if no A exists (v6-only names), keep AAAA").
//
// Implementation under test (pre-fix): canonicalDNSName = strings.ToLower(
// name.String()) was the single funnel for answer attribution (lines 344/359),
// probe/query question matching (line 476) and the per-name cache key
// (line 653); its Unicode-aware folding is the F1 defect. The fix replaces the
// fold with a bytewise ASCII A-Z -> a-z map over the raw wire label bytes and
// nothing else, still at the one funnel every call site goes through.
// ===========================================================================

// F1 regression (was RED on 1319c5d): distinct wire names that Unicode
// folding maps to one string share one canonical form.
func TestAdvRevInvalidUTF8NameFold(t *testing.T) {
	// U+212A KELVIN SIGN is a distinct DNS label from ASCII "k" (wire bytes
	// E2 84 AA vs 6B); strings.ToLower maps it to "k".
	assert.NotEqual(t, canonicalDNSName(advRevRawName("\u212A.example.")),
		canonicalDNSName(advRevRawName("k.example.")),
		"CONTRACT: U+212A is a distinct DNS label from ASCII k")

	// Distinct invalid UTF-8 label bytes both decode to U+FFFD under ToLower.
	nameA := advRevRawName("\xc3\x28.example.")
	nameB := advRevRawName("\xc4\x28.example.")
	require.False(t, bytes.Equal(nameA.Data[:nameA.Length], nameB.Data[:nameB.Length]),
		"fixture sanity: the two wire names must differ byte-wise")
	assert.NotEqual(t, canonicalDNSName(nameA), canonicalDNSName(nameB),
		"CONTRACT: distinct wire labels must not fold to one canonical name")
}

// F1 regression (was RED on 1319c5d): an A record owned by ASCII
// "k.example" is accepted as attributable evidence for a probe response whose
// question is the DISTINCT Kelvin-sign name.
func TestAdvRevKelvinAttribution(t *testing.T) {
	kelvinQ := dnsmessage.Question{Name: advRevRawName("\u212A.example."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}
	msg := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 0x7101, Response: true},
		Questions: []dnsmessage.Question{kelvinQ},
		Answers:   []dnsmessage.Resource{rwARes(t, "k.example.", "203.0.113.2", 60)}, // ASCII owner
	}
	raw := advRevPack(t, msg)
	var check dnsmessage.Message
	require.NoError(t, check.Unpack(raw))
	require.Len(t, check.Answers, 1)
	require.Equal(t, "k.example.", check.Answers[0].Header.Name.String(),
		"fixture sanity: the A owner is the ASCII name")

	outcome := classifyAProbeResponse(raw, 0x7101, kelvinQ)
	assert.Equal(t, dnsProbeUnknown, outcome.evidence,
		"CONTRACT: an A record for a different wire name must NOT be positive evidence for the Kelvin qname")
	assert.Zero(t, outcome.ttl, "UNKNOWN evidence must carry no cache TTL")

	// Control (must stay green): a reply whose A owner differs from the query
	// only by ASCII CASE is genuinely attributable (RFC 4343).
	asciiQ := dnsmessage.Question{Name: dnsmessage.MustNewName("b.example."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}
	upper := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 0x7102, Response: true},
		Questions: []dnsmessage.Question{asciiQ},
		Answers:   []dnsmessage.Resource{rwARes(t, "B.Example.", "203.0.113.3", 60)},
	}
	got := classifyAProbeResponse(advRevPack(t, upper), 0x7102, asciiQ)
	assert.Equal(t, dnsProbeAExists, got.evidence,
		"control: ASCII case folding must stay case-insensitive")
}

// F1 regression (was RED on 1319c5d), end-to-end COLD — no cache warming at all:
// the upstream A-probe reply for the Kelvin name carries an A record owned by
// the DISTINCT ASCII name and NO CNAME chain. Per the contract this is
// unattributable evidence => UNKNOWN => the AAAA answer relays verbatim. The
// implementation folds owner and qname to one canonical form and accepts it as
// positive evidence: this proves the defect is not only a cache-key collision,
// it is also a live misclassification of unattributable evidence (distinguishes
// cache contamination from probe retargeting: the probe itself carries the
// exact Kelvin wire name; the fold happens at attribution time).
func TestAdvRevKelvinColdCase(t *testing.T) {
	const kelvin = "\u212A.example"

	stub := newRWStub(t, func(hdr dnsmessage.Header, q dnsmessage.Question) *dnsmessage.Message {
		msg := rwEcho(hdr, q)
		if q.Type == dnsmessage.TypeA {
			// Unattributable A: owner is the ASCII twin, no CNAME chain.
			msg.Answers = []dnsmessage.Resource{rwARes(t, "k.example.", "203.0.113.20", 60)}
			return msg
		}
		msg.Answers = []dnsmessage.Resource{rwAAAARes(t, q.Name.String(), "2001:db8::20", 60)}
		return msg
	})
	d := newRWForwarder(t, []string{stub.addr()}, "ipv4", func(string) bool { return false })

	r := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), buildQuery(0x7901, 28, kelvin))

	// Fixture sanity, A-query-specific: the recorded A probe must carry the
	// exact Kelvin wire name (checked against A-type queries only, so the
	// forwarded AAAA query cannot trivially satisfy it).
	require.True(t, advRevAQueryHasExactBytes(t, stub, []byte{0xE2, 0x84, 0xAA, '.', 'e', 'x', 'a', 'm', 'p', 'l', 'e', '.'}),
		"fixture sanity: the recorded A probe carried the exact Kelvin wire name")

	var m dnsmessage.Message
	_ = m.Unpack(r)
	t.Logf("observed cold: answers=%d rcode=%v | A-probes=%d", len(m.Answers), m.RCode, stub.aQueryCount())

	upstream, ok := stub.sentForExact(dnsmessage.TypeAAAA, "\u212A.example.")
	require.True(t, ok)
	assert.Equal(t, 1, stub.aQueryCount(), "sanity: exactly one probe on the cold path")
	assert.Equal(t, upstream, r,
		"CONTRACT: unattributable A evidence must relay the AAAA answer verbatim (cold path)")
}

// F1 regression (was RED on 1319c5d): the response-question echo check
// accepts a Kelvin-sign question as an echo of an ASCII question.
func TestAdvRevQuestionFold(t *testing.T) {
	kelvin := dnsmessage.Question{Name: advRevRawName("\u212A.example."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}
	ascii := dnsmessage.Question{Name: dnsmessage.MustNewName("k.example."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}
	assert.False(t, questionMatches(kelvin, ascii),
		"CONTRACT: byte-distinct questions are not the same question")

	upper := dnsmessage.Question{Name: dnsmessage.MustNewName("K.Example."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}
	assert.True(t, questionMatches(upper, ascii),
		"control: ASCII case-insensitive question echo matching must keep working")
}

// F1 regression (was RED on 1319c5d), end-to-end: a positive verdict cached for
// ASCII "k.example" is reused for the DISTINCT v6-only Kelvin name, replacing a
// legitimate upstream AAAA answer with synthesized NODATA. Design v6 §4: "if no
// A exists (v6-only names), keep AAAA and resolve exactly as today".
func TestAdvRevKelvinCacheCollision(t *testing.T) {
	const kelvin = "\u212A.example"

	stub := newRWStub(t, func(hdr dnsmessage.Header, q dnsmessage.Question) *dnsmessage.Message {
		msg := rwEcho(hdr, q)
		if q.Type == dnsmessage.TypeA {
			// A exists ONLY for the ASCII name; the Kelvin name is genuinely
			// v6-only (NOERROR, no answers).
			if q.Name.String() == "k.example." {
				msg.Answers = []dnsmessage.Resource{rwARes(t, "k.example.", "203.0.113.1", 60)}
			}
			return msg
		}
		// The AAAA owner is the EXACT queried name: a legitimate upstream
		// answer for the Kelvin name.
		msg.Answers = []dnsmessage.Resource{rwAAAARes(t, q.Name.String(), "2001:db8::1", 60)}
		return msg
	})
	d := newRWForwarder(t, []string{stub.addr()}, "ipv4", func(string) bool { return false })

	// Query 1: ASCII twin warms the cache with positive evidence.
	r1 := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), buildQuery(0x7001, 28, "k.example"))
	var m1 dnsmessage.Message
	require.NoError(t, m1.Unpack(r1))
	require.Empty(t, m1.Answers, "sanity: the ASCII name with an A record is suppressed")
	require.Equal(t, 1, stub.aQueryCount(), "sanity: one probe for the ASCII name")

	// Query 2: the Kelvin name must keep its upstream AAAA verbatim.
	r2 := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), buildQuery(0x7002, 28, kelvin))

	require.True(t, advRevAnyQueryContains(t, stub, []byte{0xE2, 0x84, 0xAA}),
		"fixture sanity: the Kelvin-sign query bytes must reach the upstream")

	upstream, ok := stub.sentForExact(dnsmessage.TypeAAAA, "\u212A.example.")
	require.True(t, ok, "fixture sanity: the upstream answered the Kelvin AAAA query")

	var m2 dnsmessage.Message
	_ = m2.Unpack(r2)
	t.Logf("observed: Kelvin response answers=%d rcode=%v | upstream answers=%d | A-probes=%d",
		len(m2.Answers), m2.RCode, countAnswers(t, upstream), stub.aQueryCount())

	assert.Equal(t, upstream, r2,
		"CONTRACT: a v6-only name must keep its upstream AAAA answer verbatim")
	assert.Equal(t, 2, stub.aQueryCount(),
		"CONTRACT: a distinct wire name must be probed on its own, not reuse another name's verdict")
}

func countAnswers(t *testing.T, raw []byte) int {
	t.Helper()
	var m dnsmessage.Message
	if err := m.Unpack(raw); err != nil {
		return -1
	}
	return len(m.Answers)
}

// GREEN control: ASCII mixed-case queries share the ASCII-folded cache verdict
// (intended RFC 4343 case-insensitivity must keep working).
func TestAdvRevAsciiCaseControl(t *testing.T) {
	stub := newRWStub(t, func(hdr dnsmessage.Header, q dnsmessage.Question) *dnsmessage.Message {
		msg := rwEcho(hdr, q)
		if q.Type == dnsmessage.TypeA {
			if q.Name.String() == "k.example." {
				msg.Answers = []dnsmessage.Resource{rwARes(t, "k.example.", "203.0.113.12", 60)}
			}
			return msg
		}
		msg.Answers = []dnsmessage.Resource{rwAAAARes(t, q.Name.String(), "2001:db8::12", 60)}
		return msg
	})
	d := newRWForwarder(t, []string{stub.addr()}, "ipv4", func(string) bool { return false })

	_ = queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), buildQuery(0x7801, 28, "k.example"))
	r := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), buildQuery(0x7802, 28, "K.Example"))
	var m dnsmessage.Message
	require.NoError(t, m.Unpack(r))
	assert.Empty(t, m.Answers, "ASCII case-insensitive names must share the verdict (intended)")
	assert.Equal(t, 1, stub.aQueryCount(), "the mixed-case query must hit the cached verdict")
}

// ===========================================================================
// H2: EDNS extended rcodes / nonzero versions.
// Authority: E0-CORRECTIONS v2 ("probe rcode != NOERROR => UNKNOWN"; "never
// synthesize from error") + impl-report decision #3 (nonzero response EDNS
// version => UNKNOWN, conservative).
// ===========================================================================

// GREEN: probe reply, version-0 OPT with extended-rcode bits set (combined
// RCODE 16) plus an attributable A record => UNKNOWN, never positive.
func TestAdvRevProbeExtendedRcode(t *testing.T) {
	expected := dnsmessage.Question{Name: dnsmessage.MustNewName("ext.example."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}
	msg := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 0x7201, Response: true},
		Questions: []dnsmessage.Question{expected},
		Answers:   []dnsmessage.Resource{rwARes(t, "ext.example.", "203.0.113.5", 60)},
		Additionals: []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{
			Name: dnsmessage.MustNewName("."), Type: dnsmessage.TypeOPT, Class: 1232,
			TTL: uint32(1) << 24, // ext-rcode 1 => combined RCODE 16
		}, Body: &dnsmessage.OPTResource{}}},
	}
	raw := advRevPack(t, msg)
	var check dnsmessage.Message
	require.NoError(t, check.Unpack(raw))
	require.Len(t, check.Additionals, 1)
	require.NotZero(t, check.Additionals[0].Header.TTL&(uint32(0xff)<<24),
		"fixture sanity: the ext-rcode bits survive packing")

	outcome := classifyAProbeResponse(raw, 0x7201, expected)
	assert.Equal(t, dnsProbeUnknown, outcome.evidence,
		"CONTRACT: an extended rcode is not a clean NOERROR with A evidence")
	assert.Zero(t, outcome.ttl)
}

// GREEN: probe reply whose OPT advertises a nonzero EDNS version => UNKNOWN
// (conservative, per impl-report decision #3).
func TestAdvRevProbeResponseEDNSVersionNonzero(t *testing.T) {
	expected := dnsmessage.Question{Name: dnsmessage.MustNewName("ver.example."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}
	msg := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 0x7202, Response: true},
		Questions: []dnsmessage.Question{expected},
		Answers:   []dnsmessage.Resource{rwARes(t, "ver.example.", "203.0.113.30", 60)},
		Additionals: []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{
			Name: dnsmessage.MustNewName("."), Type: dnsmessage.TypeOPT, Class: 1232,
			TTL: uint32(1) << 16, // version 1
		}, Body: &dnsmessage.OPTResource{}}},
	}
	outcome := classifyAProbeResponse(advRevPack(t, msg), 0x7202, expected)
	assert.Equal(t, dnsProbeUnknown, outcome.evidence,
		"CONTRACT: a nonzero EDNS version is uninterpretable => UNKNOWN")
}

// GREEN: upstream AAAA answer with extended-rcode bits and attributable AAAA
// records, to an OPT-bearing guest query => verbatim, no probe.
func TestAdvRevUpstreamExtendedRcodeVerbatim(t *testing.T) {
	stub := newRWStub(t, func(hdr dnsmessage.Header, q dnsmessage.Question) *dnsmessage.Message {
		msg := rwEcho(hdr, q)
		if q.Type == dnsmessage.TypeA {
			msg.Answers = []dnsmessage.Resource{rwARes(t, "extup.example.", "203.0.113.4", 60)}
			return msg
		}
		msg.Answers = []dnsmessage.Resource{rwAAAARes(t, "extup.example.", "2001:db8::4", 60)}
		msg.Additionals = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{
			Name: dnsmessage.MustNewName("."), Type: dnsmessage.TypeOPT, Class: 1232,
			TTL: uint32(1) << 24, // ext-rcode 1 => combined RCODE 16
		}, Body: &dnsmessage.OPTResource{}}}
		return msg
	})
	d := newRWForwarder(t, []string{stub.addr()}, "ipv4", func(string) bool { return false })

	var optHdr dnsmessage.ResourceHeader
	require.NoError(t, optHdr.SetEDNS0(1232, 0, true))
	query := advRevPack(t, dnsmessage.Message{
		Header:      dnsmessage.Header{ID: 0x7301, RecursionDesired: true},
		Questions:   []dnsmessage.Question{{Name: dnsmessage.MustNewName("extup.example."), Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET}},
		Additionals: []dnsmessage.Resource{{Header: optHdr, Body: &dnsmessage.OPTResource{}}},
	})
	resp := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), query)
	upstream, ok := stub.sentForExact(dnsmessage.TypeAAAA, "extup.example.")
	require.True(t, ok)
	assert.Equal(t, upstream, resp,
		"CONTRACT: an extended-rcode AAAA answer must be relayed verbatim")
	assert.Equal(t, 0, stub.aQueryCount(),
		"CONTRACT: an error-class AAAA answer must not trigger a probe")
}

// GREEN: an upstream error response that still carries address records stays
// verbatim (adversarial shape: records under RCODE != NOERROR).
func TestAdvRevUpstreamErrorWithRecordsVerbatim(t *testing.T) {
	stub := newRWStub(t, func(hdr dnsmessage.Header, q dnsmessage.Question) *dnsmessage.Message {
		msg := rwEcho(hdr, q)
		if q.Type == dnsmessage.TypeA {
			msg.Answers = []dnsmessage.Resource{rwARes(t, "err.example.", "203.0.113.8", 60)}
			return msg
		}
		msg.Header.RCode = dnsmessage.RCodeServerFailure
		msg.Answers = []dnsmessage.Resource{rwAAAARes(t, "err.example.", "2001:db8::8", 60)}
		return msg
	})
	d := newRWForwarder(t, []string{stub.addr()}, "ipv4", func(string) bool { return false })

	resp := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), buildQuery(0x7302, 28, "err.example"))
	upstream, ok := stub.sentForExact(dnsmessage.TypeAAAA, "err.example.")
	require.True(t, ok)
	assert.Equal(t, upstream, resp, "CONTRACT: SERVFAIL with records stays verbatim")
	assert.Equal(t, 0, stub.aQueryCount(), "CONTRACT: no probe for an ineligible AAAA answer")
}

// ===========================================================================
// H3: foreign-class records on an NXDOMAIN probe reply.
// Frozen taxonomy: NXDOMAIN = NEGATIVE (cache per SOA) — so the implementation
// behavior is CORRECT here; this test pins it and flags only that the E0 code
// report's blanket claim ("foreign-class => UNKNOWN") does not match.
// ===========================================================================

func TestAdvRevNxdomainForeignClassObservation(t *testing.T) {
	expected := dnsmessage.Question{Name: dnsmessage.MustNewName("fgn.example."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}

	foreignA := rwARes(t, "fgn.example.", "203.0.113.9", 60)
	foreignA.Header.Class = 3 // CHAOS
	nxd := dnsmessage.Message{
		Header:      dnsmessage.Header{ID: 0x7401, Response: true, RCode: dnsmessage.RCodeNameError},
		Questions:   []dnsmessage.Question{expected},
		Answers:     []dnsmessage.Resource{foreignA},
		Authorities: []dnsmessage.Resource{rwSOARes(t, "example.", 60, 60)},
	}
	outcome := classifyAProbeResponse(advRevPack(t, nxd), 0x7401, expected)
	assert.Equal(t, dnsProbeNoA, outcome.evidence,
		"frozen taxonomy: NXDOMAIN is NEGATIVE regardless of foreign-class answer data")
	assert.Equal(t, uint32(60), outcome.ttl,
		"the negative TTL comes from the SOA even with foreign-class data")
	t.Log("NOTE: E0-CODE-REPORT decision #3 claimed foreign-class => UNKNOWN unqualified; on NXDOMAIN it is ignored (frozen taxonomy wins; the wording is corrected in the E0-CODE-REPORT AMENDMENTS).")

	// Control: on NOERROR the foreign-class guard does apply.
	noerr := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 0x7402, Response: true},
		Questions: []dnsmessage.Question{expected},
		Answers:   []dnsmessage.Resource{foreignA},
	}
	outcome = classifyAProbeResponse(advRevPack(t, noerr), 0x7402, expected)
	assert.Equal(t, dnsProbeUnknown, outcome.evidence,
		"NOERROR with foreign-class records must be UNKNOWN")
}

// ===========================================================================
// H4: zero-TTL CNAME link on an otherwise positive A chain.
// Authority: E0-CORRECTIONS item 1 + clarifications ("TTL = min over validated
// chain + terminal A"; "TTL 0 stays uncached").
// ===========================================================================

func TestAdvRevZeroTTLChainNotCached(t *testing.T) {
	stub := newRWStub(t, func(hdr dnsmessage.Header, q dnsmessage.Question) *dnsmessage.Message {
		msg := rwEcho(hdr, q)
		if q.Type == dnsmessage.TypeA {
			msg.Answers = []dnsmessage.Resource{
				rwCNAMERes(t, "zchain.example.", "ztarget.example.", 0), // zero-TTL link
				rwARes(t, "ztarget.example.", "203.0.113.10", 60),
			}
			return msg
		}
		msg.Answers = []dnsmessage.Resource{rwAAAARes(t, "zchain.example.", "2001:db8::10", 60)}
		return msg
	})
	d := newRWForwarder(t, []string{stub.addr()}, "ipv4", func(string) bool { return false })

	r1 := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), buildQuery(0x7501, 28, "zchain.example"))
	var m1 dnsmessage.Message
	require.NoError(t, m1.Unpack(r1))
	assert.Empty(t, m1.Answers, "a chain-TTL-0 positive still suppresses in this transaction")

	r2 := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), buildQuery(0x7502, 28, "zchain.example"))
	var m2 dnsmessage.Message
	require.NoError(t, m2.Unpack(r2))
	assert.Empty(t, m2.Answers, "suppression must still hold on the second query")
	assert.Equal(t, 2, stub.aQueryCount(),
		"CONTRACT: a TTL-0 chain verdict must not be cached (second query re-probes)")
}

// ===========================================================================
// H5: cache bounds. Contract: bounded storage; expired evidence never reused
// (reuse is covered by the implementation's own test suite). Eviction policy
// details are implementation freedom; only the bound is asserted.
// ===========================================================================

func TestAdvRevCacheBound(t *testing.T) {
	clock := &rwClock{now: time.Unix(1_800_000_000, 0)}
	d := newInternalRWForwarder(t, []string{"192.0.2.1:53"}, dnsForwarderSettings{
		mode: dnsForwardModeIPv4Prefer,
		now:  clock.Now,
	})

	// storeRewriteEntryLocked's contract: caller holds rewriteMu.
	store := func(key string, ttl uint32) {
		d.rewriteMu.Lock()
		d.storeRewriteEntryLocked(key, dnsProbeOutcome{evidence: dnsProbeAExists, ttl: ttl}, d.now())
		d.rewriteMu.Unlock()
	}
	size := func() int {
		d.rewriteMu.Lock()
		defer d.rewriteMu.Unlock()
		return len(d.rewriteCache)
	}

	// Fill exactly to the bound (finite), then insert one more unexpired key.
	for i := 0; i < dnsRewriteCacheMaxEntries; i++ {
		store(fmt.Sprintf("fill%05d.example.", i), 300)
	}
	require.Equal(t, dnsRewriteCacheMaxEntries, size(), "sanity: the cache filled to its bound")

	store("overflow.example.", 300)
	t.Logf("observed size after overflow insert: %d (bound %d)", size(), dnsRewriteCacheMaxEntries)
	assert.LessOrEqual(t, size(), dnsRewriteCacheMaxEntries, "CONTRACT: the cache must stay bounded")

	d.rewriteMu.Lock()
	_, present := d.rewriteCache["overflow.example."]
	d.rewriteMu.Unlock()
	assert.True(t, present, "the just-stored key must survive eviction")

	// Expire everything, then hammer with unexpired inserts; the bound must
	// hold at all times (any reclaim policy qualifies).
	clock.Advance(301 * time.Second)
	for i := 0; i < 12000; i++ {
		store(fmt.Sprintf("hammer%05d.example.", i), 300)
	}
	t.Logf("observed size after 12000 unexpired inserts: %d (bound %d)", size(), dnsRewriteCacheMaxEntries)
	assert.LessOrEqual(t, size(), dnsRewriteCacheMaxEntries, "CONTRACT: no unbounded growth path")
}

// ===========================================================================
// Informational: probe/response shape facts tied to the review questions.
// ===========================================================================

// Observed: the A probe is a bare EDNS-less query (ID + RD only) even when the
// guest query carried OPT/DO/CD; the synthesized response mirrors the guest
// OPT header (class/version/DO, ext-rcode 0, zero options) and keeps AD clear.
func TestAdvRevProbeAndSynthesisShape(t *testing.T) {
	stub := newRWStub(t, func(hdr dnsmessage.Header, q dnsmessage.Question) *dnsmessage.Message {
		msg := rwEcho(hdr, q)
		if q.Type == dnsmessage.TypeA {
			msg.Answers = []dnsmessage.Resource{rwARes(t, "shape.example.", "203.0.113.11", 60)}
			return msg
		}
		msg.Answers = []dnsmessage.Resource{rwAAAARes(t, "shape.example.", "2001:db8::11", 60)}
		return msg
	})
	d := newRWForwarder(t, []string{stub.addr()}, "ipv4", func(string) bool { return false })

	var optHdr dnsmessage.ResourceHeader
	require.NoError(t, optHdr.SetEDNS0(1232, 0, true)) // DO set
	query := advRevPack(t, dnsmessage.Message{
		Header:      dnsmessage.Header{ID: 0x7601, RecursionDesired: true, CheckingDisabled: true},
		Questions:   []dnsmessage.Question{{Name: dnsmessage.MustNewName("shape.example."), Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET}},
		Additionals: []dnsmessage.Resource{{Header: optHdr, Body: &dnsmessage.OPTResource{}}},
	})
	resp := queryForwarderOver(t, "udp4", "127.0.0.1", d.Port(), query)

	var m dnsmessage.Message
	require.NoError(t, m.Unpack(resp))
	require.Empty(t, m.Answers, "sanity: suppression happened for this dual-stack name")
	require.Len(t, m.Additionals, 1, "the query's OPT must be mirrored")
	assert.Equal(t, dnsmessage.Class(1232), m.Additionals[0].Header.Class, "payload size class mirrored")
	assert.True(t, m.Additionals[0].Header.DNSSECAllowed(), "DO bit mirrored")
	assert.Zero(t, m.Additionals[0].Header.TTL>>24, "ext-rcode mirrored as 0")
	assert.False(t, m.AuthenticData, "AD must stay clear on the unsigned NODATA")
	assert.True(t, m.RecursionAvailable, "RA set")

	// The probe itself: observe (log) what it carries.
	seen := false
	for i := 0; i < stub.totalQueries(); i++ {
		raw := stub.rawAt(i)
		var q dnsmessage.Message
		if err := q.Unpack(raw); err != nil || len(q.Questions) != 1 || q.Questions[0].Type != dnsmessage.TypeA {
			continue
		}
		seen = true
		t.Logf("observed probe: additionals=%d cd=%v rd=%v opcode=%d",
			len(q.Additionals), q.Header.CheckingDisabled, q.Header.RecursionDesired, q.Header.OpCode)
	}
	require.True(t, seen, "sanity: an A probe reached the upstream")
}
