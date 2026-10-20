package dnsrewrite

// Message parsing, evidence classification and NODATA synthesis for the
// ipv4-preferring rewrite. Ported one-to-one from the icebox/dns-rewrite lane's
// pkg/net/dns_rewrite.go at revision 1319c5d (that lane owns the Linux
// DNSForwarder and its inline copy); this package exists so the darwin
// userspace stack can share the identical decision logic without reaching
// into a //go:build linux file. Linux adoption of this package is follow-up
// work for the DNS owner; until then the two implementations must be kept in
// lockstep, and any divergence is a defect in whichever copy moved.

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/jingkaihe/matchlock/internal/errx"
)

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
	// gates the query instead of reaching the mirror path, and so the
	// resolution-context rule below sees the actual options.
	optResource, err := p.OPTResource()
	if err != nil {
		return parsed, false, "unparsable query OPT record"
	}
	// F2 (resolution context): a query whose EDNS OPT carries a
	// context-affecting option (ECS) must take the conservative path — the
	// caller relays the upstream answer VERBATIM with NO A probe and NO cache
	// read/write — because the client has declared a resolution context the
	// host-side verdicts do not model. DO=1 and CD=1 are deliberately NOT
	// context-affecting, and a plain OPT without options stays eligible.
	if HasContextAffectingOption(optResource.Options) {
		return parsed, false, "query carries a context-affecting EDNS option (ECS)"
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
	// malformed reports a cyclic chain, a chain deeper than dnsMaxCNAMEHops,
	// or an owner carrying both a CNAME and other data (which RFC 1034/2181
	// forbid).
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
		return nil, errx.Wrap(ErrRewrite, err)
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
