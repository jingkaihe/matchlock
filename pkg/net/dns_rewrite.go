//go:build linux

package net

// This file implements the ipv4-preferring AAAA rewrite of DNSForwarder: for a
// standard single-question AAAA query the forwarder probes the same upstream
// resolver list for an A record and, when the name demonstrably has one, answers
// with a synthesized NODATA response so a dual-stack guest falls back to IPv4.
// Every other query and every other outcome relays the upstream bytes verbatim;
// failures of the rewrite machinery always fail safe to the verbatim relay.
//
// Two identity/context rules shape what is rewritten:
//
//   - Name identity is ASCII-only case folding over the raw wire label bytes
//     (RFC 4343): canonicalDNSName is the single funnel for every name
//     comparison in this file and never Unicode-folds (see its comment). Two
//     names that differ in any byte other than the case of an ASCII letter
//     keep distinct identities, so they never share attribution evidence,
//     question echoes or cached verdicts.
//   - Resolution-context safety: an AAAA query whose EDNS OPT carries the
//     ECS option (code 8, RFC 7871) is relayed verbatim — the A-existence
//     probe is a bare, context-free query, so its evidence is not comparable
//     with an ECS-scoped resolution context. The gate sits in query parsing,
//     before any rewrite-cache lookup or probe; the rewrite cache is neither
//     read nor written and no A probe is sent for such a query.
//     Comparability ruling: DO and CD do NOT bypass — this corrective pass
//     leaves their prior behavior unchanged (same bare probe, same name-based
//     attribution; a synthesized NODATA stays unsigned with AD clear). That is
//     a scope statement for this rewrite, not a claim of universal EDNS/DNSSEC
//     context equivalence. ECS is the only context-affecting option in scope;
//     every other OPT option (known or unknown code) is processed normally by
//     deliberate decision.
//
// The synthesized NODATA answer is unsigned: DNSSEC AD is deliberately not set
// and no RRSIG is fabricated. It is local synthesis, not DNSSEC validation
// transparency.

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/jingkaihe/matchlock/internal/errx"
)

// dnsForwardMode selects how a DNSForwarder treats upstream answers.
type dnsForwardMode int

const (
	// dnsForwardModeVerbatim relays every upstream answer unchanged.
	dnsForwardModeVerbatim dnsForwardMode = iota
	// dnsForwardModeIPv4Prefer rewrites eligible AAAA answers for names that
	// demonstrably have an A record.
	dnsForwardModeIPv4Prefer
)

// DNSForwarderConfig carries the answer-mode settings of
// NewConfiguredDNSForwarder.
type DNSForwarderConfig struct {
	// DNSPriority is the requested resolution priority: "ipv4" (also the
	// default when empty) selects the ipv4-preferring rewrite; "ipv6" keeps
	// the verbatim relay. "ipv6" is verbatim DNS relay only: it does not
	// force applications to prefer IPv6, and direct IP-literal connections
	// are unaffected by either mode.
	DNSPriority string
	// AllowPrivateName reports whether a queried name is covered by an
	// allow_private entry; such names bypass the rewrite entirely. It is
	// REQUIRED whenever DNSPriority resolves to "ipv4": a nil matcher fails
	// construction (fail closed) rather than silently dropping the
	// allow_private carve-out.
	AllowPrivateName func(host string) bool
}

// dnsForwarderSettings are the internal knobs of a DNSForwarder. The public
// constructors derive them from their arguments; the tests reach the same
// constructor with a controlled clock and probe budget.
type dnsForwarderSettings struct {
	mode             dnsForwardMode
	allowPrivateName func(host string) bool
	// now is the cache clock; nil means time.Now.
	now func() time.Time
	// probeBudget bounds the TOTAL A-probe time, across the whole resolver
	// fallback list; 0 means dnsProbeBudget.
	probeBudget time.Duration
}

const (
	// dnsProbeBudget bounds the TOTAL time an A-existence probe may spend,
	// over the whole resolver fallback list: one absolute deadline covers
	// every dial and every socket wait, so a black-holed first resolver
	// cannot multiply the wait.
	dnsProbeBudget = 2500 * time.Millisecond
	// dnsRewriteMaxTTL clamps only the upper bound of a cached A-existence
	// verdict; a record TTL of 0 stays 0 and is never cached.
	dnsRewriteMaxTTL = 300 * time.Second
	// dnsRewriteCacheMaxEntries bounds the A-existence cache; the insert
	// path sweeps expired entries first and drops the oldest quarter when
	// still full.
	dnsRewriteCacheMaxEntries = 4096
	// dnsMaxCNAMEHops caps a CNAME chain walk; a deeper chain is malformed
	// evidence (UNKNOWN, fail safe to the verbatim relay).
	dnsMaxCNAMEHops = 8
	// dnsEDNSOptionCodeECS is EDNS Client Subnet (RFC 7871). It is the only
	// resolution-context-affecting option in scope: the A-existence probe is
	// a bare, context-free query, so its evidence is not comparable with a
	// query that names its own resolution context. An AAAA query carrying
	// this option is relayed verbatim (no probe, no cache read, no cache
	// write); DO, CD and every other option do NOT bypass.
	dnsEDNSOptionCodeECS = 8
)

// dnsModeForPriority maps a NetworkConfig.DNSPriority value to a forward mode.
func dnsModeForPriority(priority string) (dnsForwardMode, error) {
	switch priority {
	case "", "ipv4":
		return dnsForwardModeIPv4Prefer, nil
	case "ipv6":
		return dnsForwardModeVerbatim, nil
	default:
		return dnsForwardModeVerbatim, errx.With(ErrDNSConfig, ": unsupported DNS priority %q (want \"ipv4\" or \"ipv6\")", priority)
	}
}

// dnsProbeEvidence is the outcome of an A-existence probe.
type dnsProbeEvidence int

const (
	// dnsProbeUnknown is the fail-safe outcome: no decisive evidence was
	// obtained (transport error, timeout, unexpected response shape). It is
	// never cached and the AAAA answer is relayed verbatim.
	dnsProbeUnknown dnsProbeEvidence = iota
	// dnsProbeNoA means the name has no A record for this resolution
	// context. The AAAA answer is relayed verbatim.
	dnsProbeNoA
	// dnsProbeAExists means the name demonstrably has an A record; an
	// eligible AAAA answer is replaced with a synthesized NODATA response.
	dnsProbeAExists
)

// dnsProbeOutcome is the result of one A-existence probe.
type dnsProbeOutcome struct {
	evidence dnsProbeEvidence
	// ttl is the evidence's lifetime in seconds: for positive evidence the
	// minimum TTL across the validated answer chain (CNAME records and the
	// terminal A records), for negative evidence the SOA-derived negative
	// TTL. 0 means "decisive but not cacheable" (TTL 0 stays 0).
	ttl uint32
	// reason is set when evidence is dnsProbeUnknown.
	reason string
}

// dnsRewriteCacheEntry is one memoized A-existence verdict for a queried name.
type dnsRewriteCacheEntry struct {
	positive  bool
	expiresAt time.Time
}

// dnsProbeCall is one in-flight A-existence probe. Concurrent AAAA queries for
// the same name collapse onto the leader's call and wait for done (or for the
// forwarder's Close).
type dnsProbeCall struct {
	done    chan struct{}
	outcome dnsProbeOutcome
}

// dnsAAAAQuery is a parsed standard AAAA query eligible for the rewrite.
type dnsAAAAQuery struct {
	header   dnsmessage.Header
	question dnsmessage.Question
	// opt is the query's EDNS OPT record header, nil when the query carried
	// none.
	opt *dnsmessage.ResourceHeader
}

// parseAAAAQuery classifies a guest query for the ipv4-preferring rewrite.
//
// ok=false means the query is relayed verbatim; gate describes why for the
// cases the rewrite's safety rules say to log, and is empty for queries the
// rewrite simply does not apply to (non-AAAA types — the common case, which
// must not spam the log).
func parseAAAAQuery(query []byte) (dnsAAAAQuery, bool, string) {
	var parsed dnsAAAAQuery
	var p dnsmessage.Parser
	header, err := p.Start(query)
	if err != nil {
		return parsed, false, "unparsable query"
	}
	parsed.header = header
	if header.OpCode != 0 { // 0 is the standard QUERY opcode
		return parsed, false, fmt.Sprintf("unsupported opcode %d", header.OpCode)
	}
	if header.Response {
		return parsed, false, "message is a response, not a query"
	}
	question, err := p.Question()
	if err != nil {
		return parsed, false, "query without exactly one question"
	}
	if _, err := p.Question(); !errors.Is(err, dnsmessage.ErrSectionDone) {
		return parsed, false, "query without exactly one question"
	}
	if question.Type != dnsmessage.TypeAAAA {
		return parsed, false, ""
	}
	if question.Class != dnsmessage.ClassINET {
		return parsed, false, fmt.Sprintf("unsupported question class %d", question.Class)
	}
	parsed.question = question
	if err := p.SkipAllAnswers(); err != nil {
		return parsed, false, "unparsable query answer section"
	}
	if err := p.SkipAllAuthorities(); err != nil {
		return parsed, false, "unparsable query authority section"
	}
	optHeader, err := p.AdditionalHeader()
	switch {
	case errors.Is(err, dnsmessage.ErrSectionDone):
		return parsed, true, ""
	case err != nil:
		return parsed, false, "unparsable query additional section"
	}
	if optHeader.Type != dnsmessage.TypeOPT {
		return parsed, false, fmt.Sprintf("unexpected additional record type %d in query", optHeader.Type)
	}
	if canonicalDNSName(optHeader.Name) != "." {
		return parsed, false, "query OPT owner is not the root name"
	}
	// Parse the OPT body (not just skip it) so a malformed option encoding
	// gates the query instead of reaching the mirror path.
	optBody, err := p.OPTResource()
	if err != nil {
		return parsed, false, "unparsable query OPT record"
	}
	// Resolution-context bypass (conservative choice, review F2): a query
	// that carries ECS names a resolution context this rewrite cannot
	// reproduce — its A-existence probe is context-free — so it is relayed
	// verbatim, BEFORE any rewrite-cache lookup or singleflight/probe entry
	// point is reachable. Comparability ruling (scope statement for this
	// rewrite, not universal EDNS/DNSSEC context equivalence): DO and CD do
	// not alter the A-existence attribution this rewrite computes, so their
	// prior behavior is unchanged (same bare probe, same name-based
	// attribution; a synthesized NODATA stays unsigned with AD clear —
	// documented). ECS is the only context-affecting option in scope; every
	// other OPT option (known or unknown code) is processed normally by
	// deliberate decision.
	for _, option := range optBody.Options {
		if option.Code == dnsEDNSOptionCodeECS {
			return parsed, false, "query OPT carries an EDNS Client Subnet option; relaying AAAA verbatim"
		}
	}
	opt := optHeader
	parsed.opt = &opt
	if _, err := p.AdditionalHeader(); !errors.Is(err, dnsmessage.ErrSectionDone) {
		return parsed, false, "more than one additional record in query"
	}
	return parsed, true, ""
}

// dnsMessageView is a parsed DNS message with the sections the rewrite
// machinery reads.
type dnsMessageView struct {
	header      dnsmessage.Header
	questions   []dnsmessage.Question
	answers     []dnsmessage.Resource
	authorities []dnsmessage.Resource
	// opt is the message's OPT record header (from additionals), nil when
	// absent.
	opt *dnsmessage.ResourceHeader
}

// optVersion returns the EDNS version advertised by the message's OPT record
// (0 when there is none).
func (v *dnsMessageView) optVersion() uint8 {
	if v.opt == nil {
		return 0
	}
	return uint8(v.opt.TTL >> 16)
}

// rcode returns the response code including the OPT's extended bits. The
// extended bits are only interpreted for EDNS version 0; a nonzero version is
// rejected by the callers before this matters.
func (v *dnsMessageView) rcode() dnsmessage.RCode {
	if v.opt != nil && v.optVersion() == 0 {
		return v.opt.ExtendedRCode(v.header.RCode)
	}
	return v.header.RCode
}

// parseDNSMessage unpacks the sections the rewrite reads. Every record body is
// fully unpacked (including the OPT body), so a malformed encoding surfaces as
// a parse error instead of silently passing a classification.
func parseDNSMessage(msg []byte) (*dnsMessageView, error) {
	var p dnsmessage.Parser
	header, err := p.Start(msg)
	if err != nil {
		return nil, err
	}
	view := &dnsMessageView{header: header}
	if view.questions, err = p.AllQuestions(); err != nil {
		return nil, err
	}
	if view.answers, err = p.AllAnswers(); err != nil {
		return nil, err
	}
	if view.authorities, err = p.AllAuthorities(); err != nil {
		return nil, err
	}
	for {
		resource, err := p.Additional()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			return view, nil
		}
		if err != nil {
			return nil, err
		}
		if resource.Header.Type == dnsmessage.TypeOPT && view.opt == nil {
			opt := resource.Header
			view.opt = &opt
		}
	}
}

// canonicalDNSName returns the lower-cased, dot-terminated form of a wire name
// so owner, target and queried names compare case-insensitively, as DNS
// requires. It is the SINGLE funnel every name-identity comparison in the
// rewrite goes through: answer attribution owners and CNAME targets
// (walkAnswerSection), response-question echo matching (questionMatches) and
// the rewrite cache / singleflight key (probeAEvidence).
//
// The fold is ASCII-only over the raw wire label bytes (RFC 4343): 'A'-'Z'
// become 'a'-'z' and every other byte is preserved. Only names that differ
// exclusively in ASCII letter case are collapsed onto one identity; a
// difference in any other byte never collapses — including the UTF-8 encoding
// of U+212A KELVIN SIGN (which Unicode case folding, strings.ToLower, would
// collapse onto ASCII "k") and invalid UTF-8 label bytes (which strings.ToLower
// would rewrite to U+FFFD). The previous strings.ToLower implementation did
// both, so byte-distinct names shared attribution evidence and cached verdicts.
func canonicalDNSName(name dnsmessage.Name) string {
	raw := name.Data[:name.Length]
	folded := make([]byte, len(raw))
	for i, b := range raw {
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}
		folded[i] = b
	}
	return string(folded)
}

// dnsAnswerAttribution is the result of walking the CNAME chain that starts at
// the queried name through a response's answer section.
type dnsAnswerAttribution struct {
	// matchTTLs are the TTLs of the requested-type records attributable to
	// the queried name (owner == qname, or the final name of the validated
	// CNAME chain starting at qname).
	matchTTLs []uint32
	// chainTTLs are the TTLs of every CNAME record traversed.
	chainTTLs []uint32
	// totalType counts requested-type records anywhere in the answer
	// section, attributable or not.
	totalType int
	// foreign counts non-INET records in the answer section: they never
	// contribute evidence, and their presence makes a response
	// uninterpretable (UNKNOWN).
	foreign int
	// malformed reports a cyclic chain, a chain deeper than
	// dnsMaxCNAMEHops, or an owner carrying both a CNAME and other data
	// (which RFC 1034/2181 forbid).
	malformed bool
}

// DNSSEC record types (RFC 4034) that legitimately share an owner name with its
// CNAME record in a signed response.
const (
	dnsTypeRRSIG dnsmessage.Type = 46
	dnsTypeNSEC  dnsmessage.Type = 47
)

// walkAnswerSection attributes the answer section to qname: it walks the CNAME
// chain starting at qname (independent of answer ordering) and collects the
// TTLs of the requested-type records on the final name, plus the traversed
// CNAME TTLs. Only INET-class records can contribute evidence.
//
// Malformed chains are detected conservatively: a cycle, a chain deeper than
// dnsMaxCNAMEHops, inconsistent CNAME targets for one owner, and an owner that
// carries a CNAME alongside other data (RFC 1034/2181 forbid the coexistence,
// except for the DNSSEC RRSIG/NSEC records that legitimately accompany a signed
// CNAME) all mark the attribution malformed. This is a targeted evidence
// walker, not a general DNS validator; a positive verdict still requires the A
// record itself to be attributable through the validated chain.
func walkAnswerSection(answers []dnsmessage.Resource, qname string, rtype dnsmessage.Type) dnsAnswerAttribution {
	var attr dnsAnswerAttribution
	byOwner := make(map[string][]uint32)
	type cnameLink struct {
		target string
		ttl    uint32
	}
	cnames := make(map[string][]cnameLink)
	otherTypes := make(map[string]map[dnsmessage.Type]bool)
	for _, answer := range answers {
		if answer.Header.Class != dnsmessage.ClassINET {
			attr.foreign++
			continue
		}
		owner := canonicalDNSName(answer.Header.Name)
		if otherTypes[owner] == nil {
			otherTypes[owner] = make(map[dnsmessage.Type]bool)
		}
		otherTypes[owner][answer.Header.Type] = true
		switch body := answer.Body.(type) {
		case *dnsmessage.AResource:
			if rtype == dnsmessage.TypeA {
				byOwner[owner] = append(byOwner[owner], answer.Header.TTL)
			}
		case *dnsmessage.AAAAResource:
			if rtype == dnsmessage.TypeAAAA {
				byOwner[owner] = append(byOwner[owner], answer.Header.TTL)
			}
		case *dnsmessage.CNAMEResource:
			cnames[owner] = append(cnames[owner], cnameLink{target: canonicalDNSName(body.CNAME), ttl: answer.Header.TTL})
		}
	}
	for _, ttls := range byOwner {
		attr.totalType += len(ttls)
	}

	current := qname
	visited := map[string]bool{qname: true}
	for hop := 0; ; hop++ {
		records := byOwner[current]
		links := cnames[current]
		if len(links) > 0 {
			if len(records) > 0 {
				attr.malformed = true
				return attr
			}
			for answerType := range otherTypes[current] {
				if answerType == dnsmessage.TypeCNAME || answerType == rtype {
					continue
				}
				if answerType == dnsTypeRRSIG || answerType == dnsTypeNSEC {
					continue
				}
				attr.malformed = true
				return attr
			}
		}
		if len(records) > 0 {
			attr.matchTTLs = records
			return attr
		}
		if len(links) == 0 {
			return attr
		}
		for _, link := range links[1:] {
			if link.target != links[0].target {
				attr.malformed = true
				return attr
			}
		}
		if hop >= dnsMaxCNAMEHops {
			attr.malformed = true
			return attr
		}
		next := links[0].target
		if visited[next] {
			attr.malformed = true
			return attr
		}
		visited[next] = true
		// Every copy of the chain's CNAME counts towards the minimum TTL.
		for _, link := range links {
			attr.chainTTLs = append(attr.chainTTLs, link.ttl)
		}
		current = next
	}
}

// minDNSRewriteTTL returns the smallest TTL across the groups, or 0 when all
// are empty.
func minDNSRewriteTTL(groups ...[]uint32) uint32 {
	var min uint32
	seen := false
	for _, group := range groups {
		for _, ttl := range group {
			if !seen || ttl < min {
				min = ttl
				seen = true
			}
		}
	}
	return min
}

// clampDNSRewriteTTL caps a cached verdict lifetime at dnsRewriteMaxTTL; only
// the upper bound is clamped (TTL 0 stays 0, meaning "not cacheable").
func clampDNSRewriteTTL(ttl uint32) uint32 {
	maxSeconds := uint32(dnsRewriteMaxTTL / time.Second)
	if ttl > maxSeconds {
		return maxSeconds
	}
	return ttl
}

// soaNegativeTTL derives the negative-cache TTL from the authorities' SOA
// record — the smaller of the record TTL and SOA.MINIMUM per RFC 2308, the
// smallest across SOA records. 0 means "no SOA / not cacheable".
func soaNegativeTTL(authorities []dnsmessage.Resource) uint32 {
	var min uint32
	found := false
	for _, resource := range authorities {
		if resource.Header.Class != dnsmessage.ClassINET {
			continue
		}
		body, ok := resource.Body.(*dnsmessage.SOAResource)
		if !ok {
			continue
		}
		ttl := resource.Header.TTL
		if body.MinTTL < ttl {
			ttl = body.MinTTL
		}
		if !found || ttl < min {
			min = ttl
			found = true
		}
	}
	if !found {
		return 0
	}
	return clampDNSRewriteTTL(min)
}

// questionMatches reports whether a response question echoes an expected
// question (case-insensitive name comparison).
func questionMatches(got, want dnsmessage.Question) bool {
	return canonicalDNSName(got.Name) == canonicalDNSName(want.Name) &&
		got.Type == want.Type &&
		got.Class == want.Class
}

// classifyAProbeResponse turns a raw A-probe response into evidence. It
// enforces the response envelope first: the response must be parseable, be a
// response, echo the probe's ID and single question (name/type/class), and not
// be truncated; anything else is UNKNOWN.
func classifyAProbeResponse(resp []byte, probeID uint16, expected dnsmessage.Question) dnsProbeOutcome {
	view, err := parseDNSMessage(resp)
	if err != nil {
		return dnsProbeOutcome{evidence: dnsProbeUnknown, reason: "unparsable probe response"}
	}
	if !view.header.Response {
		return dnsProbeOutcome{evidence: dnsProbeUnknown, reason: "probe response carries no response flag"}
	}
	if view.header.OpCode != 0 {
		return dnsProbeOutcome{evidence: dnsProbeUnknown, reason: fmt.Sprintf("probe response opcode %d", view.header.OpCode)}
	}
	if view.header.ID != probeID {
		return dnsProbeOutcome{evidence: dnsProbeUnknown, reason: "probe response ID mismatch"}
	}
	if len(view.questions) != 1 || !questionMatches(view.questions[0], expected) {
		return dnsProbeOutcome{evidence: dnsProbeUnknown, reason: "probe response question mismatch"}
	}
	if view.header.Truncated {
		return dnsProbeOutcome{evidence: dnsProbeUnknown, reason: "truncated probe response"}
	}
	if view.optVersion() != 0 {
		return dnsProbeOutcome{evidence: dnsProbeUnknown, reason: "probe response carries unsupported EDNS version"}
	}

	switch rcode := view.rcode(); rcode {
	case dnsmessage.RCodeSuccess:
		attr := walkAnswerSection(view.answers, canonicalDNSName(expected.Name), dnsmessage.TypeA)
		if attr.malformed {
			return dnsProbeOutcome{evidence: dnsProbeUnknown, reason: "malformed CNAME chain in probe response"}
		}
		if attr.foreign > 0 {
			return dnsProbeOutcome{evidence: dnsProbeUnknown, reason: "foreign class records in probe response"}
		}
		if len(attr.matchTTLs) > 0 {
			return dnsProbeOutcome{
				evidence: dnsProbeAExists,
				ttl:      clampDNSRewriteTTL(minDNSRewriteTTL(attr.matchTTLs, attr.chainTTLs)),
			}
		}
		if attr.totalType > 0 {
			return dnsProbeOutcome{evidence: dnsProbeUnknown, reason: "A records not attributable to the queried name"}
		}
		return dnsProbeOutcome{evidence: dnsProbeNoA, ttl: soaNegativeTTL(view.authorities)}
	case dnsmessage.RCodeNameError:
		// NXDOMAIN is negative evidence (spec taxonomy), but the mandated
		// malformed-chain guard applies first: a malformed/cyclic chain is
		// malformed evidence even on NXDOMAIN => UNKNOWN, fail safe.
		if walkAnswerSection(view.answers, canonicalDNSName(expected.Name), dnsmessage.TypeA).malformed {
			return dnsProbeOutcome{evidence: dnsProbeUnknown, reason: "malformed CNAME chain in probe response"}
		}
		return dnsProbeOutcome{evidence: dnsProbeNoA, ttl: soaNegativeTTL(view.authorities)}
	default:
		return dnsProbeOutcome{evidence: dnsProbeUnknown, reason: fmt.Sprintf("probe rcode %s", rcode)}
	}
}

// upstreamAAAAWarrantsSynthesis reports whether the upstream AAAA response is a
// normal answer the rewrite may replace: it must be the response to the guest's
// query (response flag, opcode, ID and question echo), not truncated, NOERROR
// with EDNS version 0 (extended code included), with at least one AAAA record
// attributable to the queried name. Anything else — an error rcode, an
// envelope mismatch, or an unattributable answer — stays verbatim: an upstream
// error or an A/AAAA disagreement is never converted into synthetic NODATA.
func upstreamAAAAWarrantsSynthesis(resp []byte, q dnsAAAAQuery) (bool, string) {
	view, err := parseDNSMessage(resp)
	if err != nil {
		return false, "unparsable upstream AAAA response"
	}
	if !view.header.Response {
		return false, "upstream AAAA answer carries no response flag"
	}
	if view.header.OpCode != 0 {
		return false, fmt.Sprintf("upstream AAAA answer opcode %d", view.header.OpCode)
	}
	if view.header.ID != q.header.ID {
		return false, "upstream AAAA answer ID mismatch"
	}
	if len(view.questions) != 1 || !questionMatches(view.questions[0], q.question) {
		return false, "upstream AAAA answer question mismatch"
	}
	if view.header.Truncated {
		return false, "truncated upstream AAAA response"
	}
	if view.optVersion() != 0 {
		return false, "upstream AAAA response carries unsupported EDNS version"
	}
	if rcode := view.rcode(); rcode != dnsmessage.RCodeSuccess {
		return false, fmt.Sprintf("upstream AAAA rcode %s", rcode)
	}
	attr := walkAnswerSection(view.answers, canonicalDNSName(q.question.Name), dnsmessage.TypeAAAA)
	if attr.malformed {
		return false, "malformed CNAME chain in upstream AAAA response"
	}
	if attr.foreign > 0 {
		return false, "foreign class records in upstream AAAA response"
	}
	if len(attr.matchTTLs) == 0 {
		// Zero attributable AAAA records: relaying the response verbatim
		// already answers the guest correctly.
		return false, "upstream AAAA response without attributable AAAA records"
	}
	return true, ""
}

// maybeRewriteAAAA returns the bytes to answer an AAAA query with: the upstream
// response verbatim, or — when the query is eligible, the name is not an
// allow_private entry, the upstream AAAA answer is a normal one, and the
// A-existence probe says the name has an A record — a synthesized NODATA
// response, so the guest prefers IPv4. Every failure path fails safe to the
// verbatim relay.
func (d *DNSForwarder) maybeRewriteAAAA(query, upstreamResp []byte) []byte {
	if d.mode != dnsForwardModeIPv4Prefer {
		return upstreamResp
	}

	parsed, ok, gate := parseAAAAQuery(query)
	if !ok {
		if gate != "" {
			slog.Debug("DNS rewrite skipped; relaying verbatim", "reason", gate)
		}
		return upstreamResp
	}

	qname := canonicalDNSName(parsed.question.Name)
	// A nil matcher is unreachable in ipv4 mode — the constructor refuses
	// it — and relay-verbatim keeps a hypothetical zero-value forwarder
	// fail-safe.
	if d.allowPrivateName == nil {
		slog.Debug("DNS rewrite skipped: no allow_private matcher; relaying AAAA verbatim", "name", qname)
		return upstreamResp
	}
	if d.allowPrivateName(strings.TrimSuffix(qname, ".")) {
		slog.Debug("DNS rewrite skipped: allow_private name; relaying AAAA verbatim", "name", qname)
		return upstreamResp
	}

	if ok, reason := upstreamAAAAWarrantsSynthesis(upstreamResp, parsed); !ok {
		slog.Debug("DNS AAAA answer relayed verbatim", "name", qname, "reason", reason)
		return upstreamResp
	}

	outcome := d.probeAEvidence(parsed.question)
	if outcome.evidence != dnsProbeAExists {
		if outcome.evidence == dnsProbeUnknown {
			slog.Debug("A probe unresolved; relaying AAAA verbatim", "name", qname, "reason", outcome.reason)
		}
		return upstreamResp
	}

	synthesized, err := synthesizeNODATA(parsed)
	if err != nil {
		slog.Debug("cannot synthesize NODATA; relaying AAAA verbatim", "name", qname, "error", err)
		return upstreamResp
	}
	return synthesized
}

// probeAEvidence returns A-existence evidence for the question, memoized per
// name and collapsed across concurrent queries (singleflight): the first caller
// probes, later callers either join the in-flight probe or read the cached
// verdict. It performs network I/O only in the leader.
func (d *DNSForwarder) probeAEvidence(question dnsmessage.Question) dnsProbeOutcome {
	// Defense in depth at the probe/cache boundary: only an IN-class AAAA
	// question may reach the network or the cache (the guest-query parser
	// already rejects everything else).
	if question.Type != dnsmessage.TypeAAAA || question.Class != dnsmessage.ClassINET {
		return dnsProbeOutcome{evidence: dnsProbeUnknown, reason: "unsupported probe question"}
	}
	key := canonicalDNSName(question.Name)

	d.rewriteMu.Lock()
	if entry, ok := d.rewriteCache[key]; ok {
		if d.now().Before(entry.expiresAt) {
			d.rewriteMu.Unlock()
			evidence := dnsProbeNoA
			if entry.positive {
				evidence = dnsProbeAExists
			}
			return dnsProbeOutcome{evidence: evidence}
		}
		delete(d.rewriteCache, key)
	}
	if call, ok := d.rewriteInflight[key]; ok {
		d.rewriteMu.Unlock()
		select {
		case <-call.done:
			return call.outcome
		case <-d.stopCh:
			return dnsProbeOutcome{evidence: dnsProbeUnknown, reason: "forwarder closed while waiting for the A probe"}
		}
	}
	call := &dnsProbeCall{done: make(chan struct{})}
	d.rewriteInflight[key] = call
	d.rewriteMu.Unlock()

	outcome := d.runAProbe(question)

	d.rewriteMu.Lock()
	if outcome.evidence != dnsProbeUnknown && outcome.ttl > 0 {
		d.storeRewriteEntryLocked(key, outcome, d.now())
	}
	// Publish the outcome and close done in the SAME critical section that
	// removes the in-flight entry: no new leader for this name can be
	// elected before every waiter of the old call has been released, and
	// waiters observe the outcome through the happens-before edge of the
	// closed channel.
	call.outcome = outcome
	close(call.done)
	delete(d.rewriteInflight, key)
	d.rewriteMu.Unlock()

	return outcome
}

// storeRewriteEntryLocked memoizes a decisive outcome for key, keeping the
// cache bounded: expired entries are swept first, and when the cache is still
// full the oldest quarter (by expiry, ties broken by name) is dropped
// deterministically. The caller must hold d.rewriteMu.
func (d *DNSForwarder) storeRewriteEntryLocked(key string, outcome dnsProbeOutcome, now time.Time) {
	if d.rewriteCache == nil {
		d.rewriteCache = make(map[string]dnsRewriteCacheEntry)
	}
	if _, exists := d.rewriteCache[key]; !exists && len(d.rewriteCache) >= dnsRewriteCacheMaxEntries {
		for cached, entry := range d.rewriteCache {
			if !now.Before(entry.expiresAt) {
				delete(d.rewriteCache, cached)
			}
		}
	}
	if len(d.rewriteCache) >= dnsRewriteCacheMaxEntries {
		d.evictOldestRewriteLocked(len(d.rewriteCache) / 4)
	}
	d.rewriteCache[key] = dnsRewriteCacheEntry{
		positive:  outcome.evidence == dnsProbeAExists,
		expiresAt: now.Add(time.Duration(outcome.ttl) * time.Second),
	}
}

// evictOldestRewriteLocked drops count entries with the earliest expiry (ties
// broken by name) so the bound holds deterministically. The caller must hold
// d.rewriteMu.
func (d *DNSForwarder) evictOldestRewriteLocked(count int) {
	if count <= 0 {
		return
	}
	keys := make([]string, 0, len(d.rewriteCache))
	for key := range d.rewriteCache {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		left, right := d.rewriteCache[keys[i]], d.rewriteCache[keys[j]]
		if left.expiresAt.Equal(right.expiresAt) {
			return keys[i] < keys[j]
		}
		return left.expiresAt.Before(right.expiresAt)
	})
	for _, key := range keys[:min(count, len(keys))] {
		delete(d.rewriteCache, key)
	}
}

// runAProbe performs the actual A-existence probe against the upstream list,
// within ONE absolute deadline so a black-holed resolver cannot multiply the
// wait, and stopping early when the forwarder closes. Only a TRANSPORT error
// falls through to the next resolver, mirroring the relay path in forward();
// the first received response is FINAL — it is classified and returned, so a
// malformed or mismatched response can never be rescued into positive evidence
// by a later resolver.
func (d *DNSForwarder) runAProbe(question dnsmessage.Question) dnsProbeOutcome {
	probeID := uint16(d.probeSeq.Add(1))
	expected := dnsmessage.Question{Name: question.Name, Type: dnsmessage.TypeA, Class: question.Class}
	query, err := (&dnsmessage.Message{
		Header:    dnsmessage.Header{ID: probeID, RecursionDesired: true},
		Questions: []dnsmessage.Question{expected},
	}).Pack()
	if err != nil {
		return dnsProbeOutcome{evidence: dnsProbeUnknown, reason: "cannot pack A probe query"}
	}

	deadline := time.Now().Add(d.probeBudgetValue())
	reason := "no upstream nameserver configured"
	for _, server := range d.dnsServers {
		if d.isStopping() {
			return dnsProbeOutcome{evidence: dnsProbeUnknown, reason: "forwarder closed"}
		}
		if !time.Now().Before(deadline) {
			reason = "A probe budget exhausted"
			break
		}
		resp, err := d.exchangeUntil(query, server, deadline)
		if err != nil {
			reason = fmt.Sprintf("A probe transport error: %v", err)
			continue
		}
		return classifyAProbeResponse(resp, probeID, expected)
	}
	return dnsProbeOutcome{evidence: dnsProbeUnknown, reason: reason}
}

// probeBudgetValue returns the effective probe budget.
func (d *DNSForwarder) probeBudgetValue() time.Duration {
	if d.probeBudget > 0 {
		return d.probeBudget
	}
	return dnsProbeBudget
}

// synthesizeNODATA builds the unsigned NODATA answer for an eligible AAAA
// query: the query's ID and question, QR=1, RD copied, RA=1, RCODE=NOERROR,
// and zero answer/authority records — except that an EDNS OPT from the query is
// mirrored (same UDP payload size class, version and DO bit; zero options;
// extended rcode 0). The response is unsigned: AD is deliberately not set and
// no RRSIG is fabricated — this is local synthesis, not DNSSEC validation
// transparency.
func synthesizeNODATA(query dnsAAAAQuery) ([]byte, error) {
	message := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID:                 query.header.ID,
			Response:           true,
			RecursionDesired:   query.header.RecursionDesired,
			RecursionAvailable: true,
		},
		Questions: []dnsmessage.Question{query.question},
	}
	if query.opt != nil {
		message.Additionals = []dnsmessage.Resource{{
			Header: mirrorOPTHeader(*query.opt),
			Body:   &dnsmessage.OPTResource{},
		}}
	}
	raw, err := message.Pack()
	if err != nil {
		return nil, errx.Wrap(ErrDNSRewrite, err)
	}
	return raw, nil
}

// mirrorOPTHeader mirrors the query's OPT flags into the response OPT header:
// the same UDP payload size class, the same version bits and the same DO bit,
// with an empty extended rcode and no options (RFC 6891 §6.1.3).
//
// The header is built by hand instead of via ResourceHeader.SetEDNS0, which
// overwrites the whole TTL field and would zero the query's version bits.
func mirrorOPTHeader(query dnsmessage.ResourceHeader) dnsmessage.ResourceHeader {
	const (
		// OPT TTL layout (RFC 6891 §6.1.3): bits 24-31 = extended rcode,
		// bits 16-23 = version, bit 15 = DO.
		optVersionShift = 16
		optDOFlag       = uint32(1) << 15
	)
	mirrored := dnsmessage.ResourceHeader{
		Name:  dnsmessage.MustNewName("."), // RFC 6891 §6.1.2: the OPT name is root
		Type:  dnsmessage.TypeOPT,
		Class: query.Class,
		TTL:   (query.TTL >> optVersionShift & 0xff) << optVersionShift,
	}
	if query.TTL&optDOFlag != 0 {
		mirrored.TTL |= optDOFlag
	}
	return mirrored
}
