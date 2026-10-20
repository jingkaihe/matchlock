//go:build linux

// Adversarial review harness, part 2: resolution-context identity (EDNS Client
// Subnet), VENDORED into the tree by the E0-FIX2 corrective pass and rewritten
// from an observation of the F2 loss into the PRESERVATION regression for the
// chosen fix policy.
//
// Provenance: this file was written by the independent reviewer of 1319c5d as
// the decisive counterexample attempt for the review question "can answers
// legitimately vary per-query such that name-keyed caching is unsafe
// in-scope?". It originally asserted the undesirable behavior (log:
// icebox/e0/rev-ecs.log): in ipv4 mode the guest AAAA+ECS answer was replaced
// by NODATA (the A probe found the default-context A while the guest's own
// A+ECS query came back empty), leaving the guest with ZERO usable addresses
// for that name, where the pre-rewrite behavior gave it the AAAA.
//
// FINDING F2 (as reviewed): the A-existence probe is a bare, context-free
// query; the relay leg forwards the guest query verbatim (ECS included); the
// cache/singleflight key is the name only — a guest whose AAAA query carries
// ECS got its answer judged by A-existence evidence from the DEFAULT
// resolution context.
//
// FIX (E0-FIX2), CHOSEN POLICY: a query whose EDNS OPT carries an EDNS Client
// Subnet option is resolution-context sensitive; an ECS-bearing AAAA query is
// relayed VERBATIM, the rewrite cache is neither read nor written and no A
// probe is sent (conservative bypass). The reviewer's constructed
// ECS-sensitive upstream model and the ipv6 verbatim control are kept:
//
//	upstream model for name ecsdec.example.:
//	  A    + ECS   -> NOERROR, no A records (in-context: no IPv4)
//	  AAAA + ECS   -> NOERROR, 1 AAAA record   (in-context: the only address)
//	  A    (no ECS)-> NOERROR, 1 A record      (default context)
//
// Pre-fix this file PASSED by asserting the loss; post-fix it PASSES by
// asserting preservation (byte-for-byte). The pre-fix run of this updated
// file is part of the red evidence recorded in icebox/e0/fix2-red-net.log.
package net

import (
	"net"
	"sync"
	"testing"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/stretchr/testify/require"
)

const advECSOptionCode = 8 // EDNS0 Client Subnet, RFC 7871

// ECS option data: family 1, source prefix 24, scope 0, address 203.0.113.0/24.
var (
	advECSQueryData = []byte{0, 1, 24, 0, 203, 0, 113}
	advECSRespData  = []byte{0, 1, 24, 24, 203, 0, 113} // scope 24 in the answer
)

func advECSOptionData(opts []dnsmessage.Option) ([]byte, bool) {
	for _, o := range opts {
		if o.Code == advECSOptionCode {
			return o.Data, true
		}
	}
	return nil, false
}

func advMsgECS(msg dnsmessage.Message) ([]byte, bool) {
	for _, r := range msg.Additionals {
		if r.Header.Type != dnsmessage.TypeOPT {
			continue
		}
		if body, ok := r.Body.(*dnsmessage.OPTResource); ok {
			return advECSOptionData(body.Options)
		}
	}
	return nil, false
}

// advECSResponse attaches an ECS-echoing OPT to a response when the query
// carried ECS, so the fixture cannot be dismissed as a malformed or
// context-ambiguous reply (RFC 7871 requires the response to echo ECS).
func advECSResponse(t *testing.T, q dnsmessage.Message, msg *dnsmessage.Message) *dnsmessage.Message {
	t.Helper()
	if _, ok := advMsgECS(q); !ok {
		return msg
	}
	var optHdr dnsmessage.ResourceHeader
	if err := optHdr.SetEDNS0(1232, 0, false); err != nil {
		return msg
	}
	msg.Additionals = []dnsmessage.Resource{{
		Header: optHdr,
		Body:   &dnsmessage.OPTResource{Options: []dnsmessage.Option{{Code: advECSOptionCode, Data: advECSRespData}}},
	}}
	return msg
}

// advECSLeg is one upstream exchange: the parsed query as seen by the upstream
// and the raw response bytes it sent back (the byte-for-byte oracle).
type advECSLeg struct {
	query dnsmessage.Message
	resp  []byte
}

// advStartContextUpstream answers from the ECS-sensitive model above. The
// datagram is copied before parsing, so retained Option.Data can never alias
// the reused receive buffer; the packed response is copied before it is sent,
// so the recorded bytes are exactly what went on the wire.
func advStartContextUpstream(t *testing.T) (*net.UDPConn, func() []advECSLeg) {
	t.Helper()
	ln, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	var mu sync.Mutex
	var seen []advECSLeg
	go func() {
		buf := make([]byte, dnsPacketBufSize)
		for {
			n, addr, err := ln.ReadFromUDP(buf)
			if err != nil {
				return
			}
			raw := append([]byte(nil), buf[:n]...) // copy: no buffer aliasing
			var q dnsmessage.Message
			if err := q.Unpack(raw); err != nil {
				continue
			}

			msg := &dnsmessage.Message{
				Header: dnsmessage.Header{
					ID:                 q.Header.ID,
					Response:           true,
					RecursionDesired:   q.Header.RecursionDesired,
					RecursionAvailable: true,
				},
				Questions: q.Questions,
			}
			if len(q.Questions) == 1 {
				question := q.Questions[0]
				_, hasECS := advMsgECS(q)
				switch question.Type {
				case dnsmessage.TypeAAAA:
					if hasECS {
						// In-context: the name's only address family is IPv6.
						msg.Answers = []dnsmessage.Resource{rwAAAARes(t, question.Name.String(), "2001:db8::40", 60)}
					}
				case dnsmessage.TypeA:
					if hasECS {
						// In-context: no IPv4. NOERROR/NODATA (no SOA needed:
						// this is a data answer, not a negative-cache case).
					} else {
						// Default context: the A record the bare probe sees.
						msg.Answers = []dnsmessage.Resource{rwARes(t, question.Name.String(), "203.0.113.40", 60)}
					}
				}
			}
			advECSResponse(t, q, msg)
			respRaw, err := msg.Pack()
			if err != nil {
				continue
			}
			mu.Lock()
			seen = append(seen, advECSLeg{query: q, resp: append([]byte(nil), respRaw...)})
			mu.Unlock()
			_, _ = ln.WriteToUDP(respRaw, addr)
		}
	}()
	snap := func() []advECSLeg {
		mu.Lock()
		defer mu.Unlock()
		out := make([]advECSLeg, len(seen))
		copy(out, seen)
		return out
	}
	return ln, snap
}

// advECSQuery builds a single-question A/AAAA query carrying the ECS option.
func advECSQuery(t *testing.T, id uint16, qtype dnsmessage.Type, name string) []byte {
	t.Helper()
	var optHdr dnsmessage.ResourceHeader
	if err := optHdr.SetEDNS0(1232, 0, false); err != nil {
		t.Fatalf("SetEDNS0: %v", err)
	}
	raw, err := (&dnsmessage.Message{
		Header:    dnsmessage.Header{ID: id, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(name), Type: qtype, Class: dnsmessage.ClassINET}},
		Additionals: []dnsmessage.Resource{{
			Header: optHdr,
			Body:   &dnsmessage.OPTResource{Options: []dnsmessage.Option{{Code: advECSOptionCode, Data: advECSQueryData}}},
		}},
	}).Pack()
	if err != nil {
		t.Fatalf("pack ECS query: %v", err)
	}
	return raw
}

// TestAdvRevECSPreservation is the preservation regression for the chosen F2
// policy: in ipv4 mode, an AAAA query carrying ECS keeps its in-context
// upstream AAAA answer byte-for-byte, no A probe is issued for it, and nothing
// is cached; the guest's A+ECS leg keeps its verbatim relay; the ipv6-mode
// control (same query, same upstream) agrees.
//
// Pre-fix this scenario was the F2 loss (AAAA suppressed => zero usable
// addresses); these assertions FAIL on 1319c5d and pass after the fix.
func TestAdvRevECSPreservation(t *testing.T) {
	const name = "ecsdec.example."
	ln, snap := advStartContextUpstream(t)

	// --- ipv4 mode (the fork default): the rewrite path, now bypassed ---
	d4 := newRWForwarder(t, []string{ln.LocalAddr().String()}, "ipv4", func(string) bool { return false })

	// Guest asks AAAA with ECS; the upstream has a valid in-context AAAA answer.
	rAAAA := queryForwarderOver(t, "udp4", "127.0.0.1", d4.Port(), advECSQuery(t, 0x7b01, dnsmessage.TypeAAAA, name))
	var mAAAA dnsmessage.Message
	require.NoError(t, mAAAA.Unpack(rAAAA))
	require.Len(t, mAAAA.Answers, 1,
		"CONTRACT: the in-context AAAA answer must be preserved (this WAS the F2 suppression)")
	require.Equal(t, dnsmessage.TypeAAAA, mAAAA.Answers[0].Header.Type)
	t.Logf("observed ipv4-mode AAAA+ECS response: answers=%d rcode=%v additionals=%d",
		len(mAAAA.Answers), mAAAA.RCode, len(mAAAA.Additionals))

	// Guest then asks A with the SAME ECS context: relayed verbatim as before.
	rA := queryForwarderOver(t, "udp4", "127.0.0.1", d4.Port(), advECSQuery(t, 0x7b02, dnsmessage.TypeA, name))
	var mA dnsmessage.Message
	require.NoError(t, mA.Unpack(rA))
	require.Empty(t, mA.Answers, "fixture sanity: in-context A+ECS has no records (the relay is verbatim)")

	// Recorded-query hygiene, keyed by the raw upstream response bytes:
	// - the ipv4 AAAA+ECS leg must be byte-identical to what the guest got;
	// - the A+ECS leg likewise;
	// - NO bare A probe may appear for the ECS-bearing name (the old loss
	//   was produced by exactly that context-free probe).
	var sawAAAAECS, sawAECS, sawBareA bool
	for _, leg := range snap() {
		if len(leg.query.Questions) != 1 {
			continue
		}
		q := leg.query.Questions[0]
		ecs, hasECS := advMsgECS(leg.query)
		switch {
		case q.Type == dnsmessage.TypeAAAA && hasECS:
			if string(ecs) != string(advECSQueryData) {
				t.Fatalf("fixture: AAAA ECS bytes = % x, want % x", ecs, advECSQueryData)
			}
			if leg.query.Header.ID == 0x7b01 {
				sawAAAAECS = true
				require.NotEmpty(t, leg.resp)
				require.Equal(t, leg.resp, rAAAA,
					"CONTRACT: the AAAA+ECS response must be byte-for-byte the upstream response")
			}
		case q.Type == dnsmessage.TypeA && hasECS:
			if leg.query.Header.ID == 0x7b02 {
				sawAECS = true
				require.NotEmpty(t, leg.resp)
				require.Equal(t, leg.resp, rA,
					"the A+ECS leg stays verbatim")
			}
		case q.Type == dnsmessage.TypeA && !hasECS:
			sawBareA = true
		}
	}
	require.True(t, sawAAAAECS, "fixture: the exact-ID AAAA+ECS leg reached the upstream and matched byte-for-byte")
	require.True(t, sawAECS, "fixture: the exact-ID A+ECS leg reached the upstream and matched byte-for-byte")
	require.False(t, sawBareA,
		"CONTRACT: an ECS-bearing AAAA query must not trigger an A probe")
	require.Zero(t, advRewriteCacheSize(d4),
		"CONTRACT: an ECS-bearing query must not populate the rewrite cache")

	// --- ipv6 mode: the verbatim control, SAME query, SAME upstream ---
	d6 := newRWForwarder(t, []string{ln.LocalAddr().String()}, "ipv6", nil)
	rCtrl := queryForwarderOver(t, "udp4", "127.0.0.1", d6.Port(), advECSQuery(t, 0x7b03, dnsmessage.TypeAAAA, name))
	var mCtrl dnsmessage.Message
	require.NoError(t, mCtrl.Unpack(rCtrl))
	require.Len(t, mCtrl.Answers, 1, "control: ipv6 mode relays the in-context AAAA answer verbatim")
	require.Equal(t, dnsmessage.TypeAAAA, mCtrl.Answers[0].Header.Type)

	ctrlSeen := false
	for _, leg := range snap() {
		if len(leg.query.Questions) != 1 || leg.query.Header.ID != 0x7b03 {
			continue
		}
		ctrlSeen = true
		require.NotEmpty(t, leg.resp)
		require.Equal(t, leg.resp, rCtrl, "control: bytes agree byte-for-byte")
	}
	require.True(t, ctrlSeen, "fixture: the ipv6 control leg reached the upstream")
}
