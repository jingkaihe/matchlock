package dnsrewrite

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/dns/dnsmessage"
)

// ---------- F1: ASCII-only canonicalization ----------

// TestCanonicalizeNameASCIIOnly pins the F1 rule: exactly the ASCII bytes
// A-Z fold to a-z; every other byte (non-ASCII, invalid UTF-8, punctuation)
// is preserved verbatim so distinct wire names can NEVER collapse into one
// identity (and therefore one cached verdict). strings.ToLower would fold
// Unicode ('Ä' vs 'ä') and mangle invalid UTF-8 with U+FFFD; both are
// forbidden here.
func TestCanonicalizeNameASCIIOnly(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Example.COM.", "example.com."},
		{"MiXeD-CaSe.ExAmPlE.", "mixed-case.example."},
		{"already.lower.", "already.lower."},
		{".", "."},
		// Non-ASCII bytes are NOT case-folded or normalized: the UTF-8
		// sequences for 'Ä' (0xC3 0x84) and 'ä' (0xC3 0xA4) must stay
		// distinct, and neither may be rewritten.
		{"\u00c4xample.example.", "\u00c4xample.example."},
		{"\u00e4xample.example.", "\u00e4xample.example."},
		// Raw invalid UTF-8 (0xC3 alone) must be preserved byte-for-byte.
		{"\xc3\xff.example.", "\xc3\xff.example."},
		// Only the A-Z range folds; adjacent bytes like '@' (0x40), '[' and
		// '{' are untouched.
		{"@[\\]{.example.", "@[\\]{.example."},
	}
	for _, tc := range cases {
		got := CanonicalizeName(tc.in)
		assert.Equal(t, tc.want, got, "CanonicalizeName(%q)", tc.in)
	}
	assert.False(t, CanonicalNameEqual("\u00c4xample.example.", "\u00e4xample.example."),
		"non-ASCII case variants must not collide")
	assert.False(t, CanonicalNameEqual("\xc3\xff.example.", "\xc3.xml.example."),
		"invalid UTF-8 must not be replaced into a colliding form")
	assert.True(t, CanonicalNameEqual("ExAmPlE.com.", "eXaMpLe.COM."), "ASCII case variants must match")
}

// ---------- F2: context-affecting EDNS options ----------

// TestHasContextAffectingOption pins the F2 gate: ECS (option code 8) selects
// the conservative verbatim path, and NOTHING else does. DO and CD live in the
// OPT TTL/flags, not in options, so they cannot appear here at all.
func TestHasContextAffectingOption(t *testing.T) {
	assert.True(t, HasContextAffectingOption([]dnsmessage.Option{{Code: ECSOptionCode}}),
		"ECS must be context-affecting")
	assert.True(t, HasContextAffectingOption([]dnsmessage.Option{
		{Code: 12}, {Code: ECSOptionCode, Data: []byte{0, 1, 24, 0, 0}},
	}), "ECS among other options must still gate")
	assert.False(t, HasContextAffectingOption(nil), "no OPT options at all")
	assert.False(t, HasContextAffectingOption([]dnsmessage.Option{}), "empty option list")
	assert.False(t, HasContextAffectingOption([]dnsmessage.Option{{Code: 12, Data: []byte{1, 2, 3}}}),
		"an unrelated option code must not gate")
}

// ---------- Rewrite semantics with an injected transport ----------

// fakeExchange answers probes and relays from per-name tables and records the
// raw probe traffic, so every verbatim path can assert the exchange function
// was (or was not) invoked.
type fakeExchange struct {
	aips    map[string][]byte // name -> A record IPs
	failAll bool
	err     error

	calls atomic.Int32
}

func (f *fakeExchange) exchange(query []byte, server string, deadline time.Time) ([]byte, error) {
	f.calls.Add(1)
	if f.failAll {
		return nil, fmt.Errorf("transport error: %w", f.err)
	}
	var p dnsmessage.Parser
	hdr, err := p.Start(query)
	if err != nil {
		return nil, err
	}
	q, err := p.Question()
	if err != nil {
		return nil, err
	}
	if _, err := p.Question(); err == nil {
		return nil, fmt.Errorf("multi-question probe")
	}
	msg := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: hdr.ID, Response: true, RecursionDesired: hdr.RecursionDesired},
		Questions: []dnsmessage.Question{q},
	}
	if q.Type == dnsmessage.TypeA {
		var body dnsmessage.AResource
		if ip := f.aips[CanonicalizeName(q.Name.String())]; len(ip) == 4 {
			copy(body.A[:], ip)
			msg.Answers = append(msg.Answers, dnsmessage.Resource{
				Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
				Body:   &body,
			})
		}
	}
	raw, err := msg.Pack()
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// aaaaQuery builds a standard single-question AAAA query, optionally with an
// EDNS OPT carrying the given options.
func aaaaQuery(t *testing.T, name string, opts []dnsmessage.Option) []byte {
	t.Helper()
	msg := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 0x1234, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET}},
	}
	if opts != nil {
		msg.Additionals = []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{
				Name:  dnsmessage.MustNewName("."),
				Type:  dnsmessage.TypeOPT,
				Class: 4096,
			},
			Body: &dnsmessage.OPTResource{Options: opts},
		}}
	}
	raw, err := msg.Pack()
	require.NoError(t, err)
	return raw
}

// aaaaResponse builds a normal upstream AAAA answer for the query name.
func aaaaResponse(t *testing.T, name string) []byte {
	t.Helper()
	var body dnsmessage.AAAAResource
	copy(body.AAAA[:], []byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1})
	msg := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 0x1234, Response: true, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET}},
		Answers: []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET, TTL: 60},
			Body:   &body,
		}},
	}
	raw, err := msg.Pack()
	require.NoError(t, err)
	return raw
}

func newTestRewriter(t *testing.T, fx *fakeExchange, mode Mode, allowPrivate func(string) bool) *Rewriter {
	t.Helper()
	r, err := New(Config{
		Mode:             mode,
		AllowPrivateName: allowPrivate,
		Servers:          []string{"192.0.2.53"},
		Exchange:         fx.exchange,
		ProbeBudget:      500 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(r.Close)
	return r
}

// TestRewriteIPv4PreferSynthesizesNODATAWhenAExists is the core ipv4 mode
// contract: an eligible AAAA query for a name with an A record is answered
// with an unsigned NODATA response (ID/question echo, NOERROR, zero answers,
// AD clear) so a dual-stack client falls back to IPv4.
func TestRewriteIPv4PreferSynthesizesNODATAWhenAExists(t *testing.T) {
	canon := CanonicalizeName("dual.example.")
	fx := &fakeExchange{aips: map[string][]byte{canon: {10, 0, 0, 7}}}
	r := newTestRewriter(t, fx, ModeIPv4Prefer, func(string) bool { return false })

	query := aaaaQuery(t, "dual.example.", nil)
	upstream := aaaaResponse(t, "dual.example.")
	got := r.Rewrite(query, upstream)

	require.NotEqual(t, upstream, got, "an eligible AAAA answer must be replaced")
	var p dnsmessage.Parser
	hdr, err := p.Start(got)
	require.NoError(t, err)
	assert.True(t, hdr.Response)
	assert.Equal(t, uint16(0x1234), hdr.ID, "the synthesized response must echo the query ID")
	assert.Equal(t, dnsmessage.RCodeSuccess, hdr.RCode)
	assert.False(t, hdr.AuthenticData, "synthesized NODATA must not claim DNSSEC authentication")
	assert.False(t, hdr.Truncated)
	qs, err := p.AllQuestions()
	require.NoError(t, err)
	require.Len(t, qs, 1)
	assert.Equal(t, dnsmessage.TypeAAAA, qs[0].Type)
	answers, err := p.AllAnswers()
	require.NoError(t, err)
	assert.Empty(t, answers, "synthesized NODATA has no answers")
	authorities, err := p.AllAuthorities()
	require.NoError(t, err)
	assert.Empty(t, authorities, "synthesized NODATA has no authority records")
	assert.GreaterOrEqual(t, fx.calls.Load(), int32(1), "the A-existence probe must have run")
}

// TestRewriteIPv4PreferVerbatimWhenNoA pins the v6-only-name case: when the
// probe finds no A record, the upstream AAAA answer is relayed byte-for-byte.
func TestRewriteIPv4PreferVerbatimWhenNoA(t *testing.T) {
	fx := &fakeExchange{aips: map[string][]byte{}}
	r := newTestRewriter(t, fx, ModeIPv4Prefer, func(string) bool { return false })

	query := aaaaQuery(t, "v6only.example.", nil)
	upstream := aaaaResponse(t, "v6only.example.")
	got := r.Rewrite(query, upstream)
	assert.Equal(t, upstream, got, "no A record -> verbatim relay (v6-only names keep working)")
}

// TestRewriteVerbatimModeNeverProbes pins the ipv6 mode: answers relay
// unchanged and the probe transport is never touched.
func TestRewriteVerbatimModeNeverProbes(t *testing.T) {
	fx := &fakeExchange{aips: map[string][]byte{CanonicalizeName("dual.example."): {10, 0, 0, 7}}}
	r := newTestRewriter(t, fx, ModeVerbatim, nil)

	query := aaaaQuery(t, "dual.example.", nil)
	upstream := aaaaResponse(t, "dual.example.")
	got := r.Rewrite(query, upstream)
	assert.Equal(t, upstream, got, "ipv6 mode relays verbatim")
	assert.Zero(t, fx.calls.Load(), "ipv6 mode must not probe")
}

// TestRewriteAllowPrivateNameVerbatim pins the carve-out: an allow_private
// name is relayed verbatim AND the exchange function is never invoked (no
// probe at all), regardless of A-record existence.
func TestRewriteAllowPrivateNameVerbatim(t *testing.T) {
	fx := &fakeExchange{aips: map[string][]byte{CanonicalizeName("ai.iasylum.net."): {10, 1, 2, 3}}}
	r := newTestRewriter(t, fx, ModeIPv4Prefer, func(host string) bool {
		return host == "ai.iasylum.net"
	})

	query := aaaaQuery(t, "ai.iasylum.net.", nil)
	upstream := aaaaResponse(t, "ai.iasylum.net.")
	got := r.Rewrite(query, upstream)
	assert.Equal(t, upstream, got, "allow_private names are always verbatim")
	assert.Zero(t, fx.calls.Load(), "allow_private names must not be probed")
}

// TestRewriteECSVerbatimNoProbeNoCache pins F2: a query carrying an ECS option
// is relayed verbatim with NO probe AND no cache read/write; a later
// ECS-free query for the same name must still probe (nothing was cached from
// the ECS query).
func TestRewriteECSVerbatimNoProbeNoCache(t *testing.T) {
	canon := CanonicalizeName("dual.example.")
	fx := &fakeExchange{aips: map[string][]byte{canon: {10, 0, 0, 7}}}
	r := newTestRewriter(t, fx, ModeIPv4Prefer, func(string) bool { return false })

	upstream := aaaaResponse(t, "dual.example.")

	ecsQuery := aaaaQuery(t, "dual.example.", []dnsmessage.Option{{Code: ECSOptionCode, Data: []byte{0, 1, 24, 0, 0}}})
	got := r.Rewrite(ecsQuery, upstream)
	assert.Equal(t, upstream, got, "an ECS query must relay verbatim")
	assert.Zero(t, fx.calls.Load(), "an ECS query must not probe")

	// The same name WITHOUT ECS is still eligible and must probe afresh.
	plainQuery := aaaaQuery(t, "dual.example.", nil)
	got = r.Rewrite(plainQuery, upstream)
	require.NotEqual(t, upstream, got, "an ECS-free query for the same name is still eligible")
	assert.Equal(t, int32(1), fx.calls.Load(), "the ECS query left no cached verdict behind")

	// DO=1 / CD=1 are not context-affecting: an OPT with an unrelated option
	// (here a hypothetical code 12) still rewrites.
	otherOptQuery := aaaaQuery(t, "dual.example.", []dnsmessage.Option{{Code: 12, Data: []byte{1}}})
	got = r.Rewrite(otherOptQuery, upstream)
	require.NotEqual(t, upstream, got, "a plain OPT without ECS stays eligible")
}

// TestRewriteFailSafeOnProbeError pins the fail-safe direction: a transport
// error yields the upstream bytes verbatim.
func TestRewriteFailSafeOnProbeError(t *testing.T) {
	fx := &fakeExchange{failAll: true, err: fmt.Errorf("blackholed")}
	r := newTestRewriter(t, fx, ModeIPv4Prefer, func(string) bool { return false })

	query := aaaaQuery(t, "dual.example.", nil)
	upstream := aaaaResponse(t, "dual.example.")
	got := r.Rewrite(query, upstream)
	assert.Equal(t, upstream, got, "probe failure -> verbatim")
}

// TestRewriteCacheAvoidsSecondProbe pins the memoization: two eligible queries
// for the same name collapse to one upstream probe within the TTL.
func TestRewriteCacheAvoidsSecondProbe(t *testing.T) {
	canon := CanonicalizeName("dual.example.")
	fx := &fakeExchange{aips: map[string][]byte{canon: {10, 0, 0, 7}}}
	r := newTestRewriter(t, fx, ModeIPv4Prefer, func(string) bool { return false })

	query := aaaaQuery(t, "dual.example.", nil)
	upstream := aaaaResponse(t, "dual.example.")
	_ = r.Rewrite(query, upstream)
	_ = r.Rewrite(query, upstream)
	assert.Equal(t, int32(1), fx.calls.Load(), "the second query must be served from the cache")
}

// TestRewriteAfterCloseVerbatim pins the shutdown contract: after Close every
// query relays verbatim, including one whose probe had already resolved.
func TestRewriteAfterCloseVerbatim(t *testing.T) {
	canon := CanonicalizeName("dual.example.")
	fx := &fakeExchange{aips: map[string][]byte{canon: {10, 0, 0, 7}}}
	r := newTestRewriter(t, fx, ModeIPv4Prefer, func(string) bool { return false })

	query := aaaaQuery(t, "dual.example.", nil)
	upstream := aaaaResponse(t, "dual.example.")
	require.NotEqual(t, upstream, r.Rewrite(query, upstream))

	r.Close()
	assert.Equal(t, upstream, r.Rewrite(query, upstream), "after Close every answer is verbatim")
}

// TestModeForPriority pins the wire-value mapping and the loud rejection of
// anything else (a bad value must never silently degrade to verbatim).
func TestModeForPriority(t *testing.T) {
	m, err := ModeForPriority("")
	require.NoError(t, err)
	assert.Equal(t, ModeIPv4Prefer, m, "empty means the ipv4-preferring default")
	m, err = ModeForPriority("ipv4")
	require.NoError(t, err)
	assert.Equal(t, ModeIPv4Prefer, m)
	m, err = ModeForPriority("ipv6")
	require.NoError(t, err)
	assert.Equal(t, ModeVerbatim, m)
	_, err = ModeForPriority("IPv6")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "IPv6")
}

// TestNewRejectsMissingPrerequisites pins the fail-closed constructor: ipv4
// mode without an allow_private matcher or without a transport must fail.
func TestNewRejectsMissingPrerequisites(t *testing.T) {
	_, err := New(Config{Mode: ModeIPv4Prefer, Exchange: func([]byte, string, time.Time) ([]byte, error) { return nil, nil }})
	require.Error(t, err, "ipv4 mode without an allow_private matcher must fail closed")

	_, err = New(Config{Mode: ModeIPv4Prefer, AllowPrivateName: func(string) bool { return false }})
	require.Error(t, err, "ipv4 mode without an exchange transport must fail closed")

	r, err := New(Config{Mode: ModeVerbatim})
	require.NoError(t, err, "verbatim mode needs no matcher or transport")
	require.NotNil(t, r)
	r.Close()
}
