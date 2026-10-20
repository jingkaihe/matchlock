// Package dnsrewrite implements the ipv4-preferring AAAA rewrite as a
// transport-injected component.
//
// Behavioural contract (ported from the icebox/dns-rewrite lane's
// pkg/net/dns_rewrite.go at revision 1319c5d, which owns the Linux
// DNSForwarder): for a standard single-question AAAA query, probe the upstream
// resolver list for an A record and, when the name demonstrably has one, answer
// with a synthesized NODATA response so a dual-stack client falls back to IPv4.
// Every other query and every other outcome relays the upstream bytes
// verbatim; every failure of the rewrite machinery fails safe to the verbatim
// relay. allow_private names are always relayed verbatim. The synthesized
// NODATA answer is unsigned: DNSSEC AD is deliberately not set and no RRSIG is
// fabricated.
//
// It lives in its own package, with the upstream transport and the cache clock
// injected, so the darwin userspace network stack (pkg/net, gVisor) and the
// Linux DNS forwarder can share ONE implementation of the decision logic: the
// darwin stack owns socketpair-based upstream exchanges instead of the Linux
// forwarder's dual-stack UDP sockets, but the family-preference semantics must
// be identical on both platforms. The Linux forwarder still carries its own
// inline copy; adopting this package there is follow-up work for the DNS
// owner, and this package's API mirrors that file's semantics one-to-one so
// the adoption is mechanical.
package dnsrewrite

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/jingkaihe/matchlock/internal/errx"
)

var (
	// ErrConfig marks an invalid rewriter construction (for example the
	// ipv4-preferring mode without an allow_private name matcher, which would
	// silently disable the carve-out).
	ErrConfig = errors.New("invalid DNS rewrite config")
	// ErrRewrite marks a failure inside the rewrite machinery (probe or NODATA
	// synthesis). Every such failure fails safe: the caller relays the upstream
	// bytes verbatim.
	ErrRewrite = errors.New("DNS rewrite failed")
)

// Mode selects how the rewriter treats upstream answers.
type Mode int

const (
	// ModeVerbatim relays every upstream answer unchanged.
	ModeVerbatim Mode = iota
	// ModeIPv4Prefer rewrites eligible AAAA answers for names that demonstrably
	// have an A record.
	ModeIPv4Prefer
)

// ModeForPriority maps a network config dns_priority value ("ipv4", "ipv6", or
// empty meaning the ipv4-preferring default) to a rewrite mode. Any other value
// is an error.
func ModeForPriority(priority string) (Mode, error) {
	switch priority {
	case "", "ipv4":
		return ModeIPv4Prefer, nil
	case "ipv6":
		return ModeVerbatim, nil
	default:
		return ModeVerbatim, errx.With(ErrConfig, ": unsupported DNS priority %q (want \"ipv4\" or \"ipv6\")", priority)
	}
}

// ExchangeFunc sends one raw DNS query to one upstream server and returns the
// raw response. deadline is one ABSOLUTE instant: the implementation must bound
// every dial and socket wait by it.
type ExchangeFunc func(query []byte, server string, deadline time.Time) ([]byte, error)

// Config wires the rewriter. Mode is required; the rest have defaults.
type Config struct {
	// Mode is the resolved answer mode (see ModeForPriority).
	Mode Mode
	// AllowPrivateName reports whether a queried name is covered by an
	// allow_private entry; such names bypass the rewrite entirely. It is
	// REQUIRED whenever Mode is ModeIPv4Prefer: a nil matcher fails
	// construction (fail closed) rather than silently dropping the
	// allow_private carve-out.
	AllowPrivateName func(host string) bool
	// Servers is the upstream resolver list probes are sent to, in fallback
	// order. Snapshotted at construction.
	Servers []string
	// Exchange performs the probe I/O. Required in ModeIPv4Prefer.
	Exchange ExchangeFunc
	// Now is the cache clock; nil means time.Now. It is used ONLY for cache
	// expiry — network deadlines always use the real clock, so an injected
	// test clock cannot silently disable probe timeouts.
	Now func() time.Time
	// ProbeBudget bounds the TOTAL A-probe time across the whole resolver
	// fallback list; 0 means the default.
	ProbeBudget time.Duration
}

const (
	// dnsProbeBudget bounds the TOTAL time an A-existence probe may spend, over
	// the whole resolver fallback list: one absolute deadline covers every dial
	// and every socket wait, so a black-holed first resolver cannot multiply
	// the wait.
	dnsProbeBudget = 2500 * time.Millisecond
	// dnsRewriteMaxTTL clamps only the upper bound of a cached A-existence
	// verdict; a record TTL of 0 stays 0 and is never cached.
	dnsRewriteMaxTTL = 300 * time.Second
	// dnsRewriteCacheMaxEntries bounds the A-existence cache; the insert path
	// sweeps expired entries first and drops the oldest quarter when still
	// full.
	dnsRewriteCacheMaxEntries = 4096
	// dnsMaxCNAMEHops caps a CNAME chain walk; a deeper chain is malformed
	// evidence (UNKNOWN, fail safe to the verbatim relay).
	dnsMaxCNAMEHops = 8
)

// dnsProbeEvidence is the outcome of an A-existence probe.
type dnsProbeEvidence int

const (
	// dnsProbeUnknown is the fail-safe outcome: no decisive evidence was
	// obtained (transport error, timeout, unexpected response shape). It is
	// never cached and the AAAA answer is relayed verbatim.
	dnsProbeUnknown dnsProbeEvidence = iota
	// dnsProbeNoA means the name has no A record for this resolution context.
	// The AAAA answer is relayed verbatim.
	dnsProbeNoA
	// dnsProbeAExists means the name demonstrably has an A record; an eligible
	// AAAA answer is replaced with a synthesized NODATA response.
	dnsProbeAExists
)

// dnsProbeOutcome is the result of one A-existence probe.
type dnsProbeOutcome struct {
	evidence dnsProbeEvidence
	// ttl is the evidence's lifetime in seconds: for positive evidence the
	// minimum TTL across the validated answer chain (CNAME records and the
	// terminal A records), for negative evidence the SOA-derived negative TTL.
	// 0 means "decisive but not cacheable" (TTL 0 stays 0).
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
// rewriter's Close).
type dnsProbeCall struct {
	done    chan struct{}
	outcome dnsProbeOutcome
}

// Rewriter decides, per answered query, whether to relay the upstream response
// verbatim or replace it with a synthesized AAAA NODATA answer. It is safe for
// concurrent use; Close makes pending probes resolve to the verbatim relay and
// no later query can synthesize a rewritten answer.
type Rewriter struct {
	mode             Mode
	allowPrivateName func(host string) bool
	servers          []string
	exchange         ExchangeFunc

	// now is the cache clock; production leaves it at time.Now.
	now func() time.Time
	// probeBudget bounds the TOTAL A-probe time; 0 means dnsProbeBudget.
	probeBudget time.Duration
	// probeSeq numbers A-probe queries so only a response that echoes the
	// probe's ID is accepted as evidence.
	probeSeq atomic.Uint32

	// mu guards the cache, the in-flight probe map and closed. No network I/O
	// ever happens while it is held.
	mu       sync.Mutex
	cache    map[string]dnsRewriteCacheEntry
	inflight map[string]*dnsProbeCall
	closed   bool

	stopCh    chan struct{}
	closeOnce sync.Once
}

// New validates cfg and returns a ready Rewriter.
func New(cfg Config) (*Rewriter, error) {
	if cfg.Mode == ModeIPv4Prefer {
		if cfg.AllowPrivateName == nil {
			return nil, errx.With(ErrConfig, ": dns priority \"ipv4\" requires an allow_private name matcher")
		}
		if cfg.Exchange == nil {
			return nil, errx.With(ErrConfig, ": dns priority \"ipv4\" requires an upstream exchange function")
		}
	}
	r := &Rewriter{
		mode:             cfg.Mode,
		allowPrivateName: cfg.AllowPrivateName,
		servers:          append([]string(nil), cfg.Servers...),
		exchange:         cfg.Exchange,
		now:              cfg.Now,
		probeBudget:      cfg.ProbeBudget,
		cache:            make(map[string]dnsRewriteCacheEntry),
		inflight:         make(map[string]*dnsProbeCall),
		stopCh:           make(chan struct{}),
	}
	if r.now == nil {
		r.now = time.Now
	}
	return r, nil
}

// Mode returns the resolved answer mode.
func (r *Rewriter) Mode() Mode { return r.mode }

// Close releases the rewriter. Pending probes observe stop and resolve to the
// fail-safe verbatim relay, and any probe that finishes after Close (its
// transport bounded by its own deadline) still cannot cause a rewritten answer:
// Rewrite re-checks the closed state after every probe. The injected exchange
// transport is not owned here, so an in-flight dial finishes at its own
// absolute deadline.
func (r *Rewriter) Close() {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
		close(r.stopCh)
	})
}

// isStopping reports whether Close has been called.
func (r *Rewriter) isStopping() bool {
	select {
	case <-r.stopCh:
		return true
	default:
		return false
	}
}

// isClosed reads the closed flag (mu-protected) — the post-probe check that
// keeps a probe finishing after Close from producing a rewritten answer.
func (r *Rewriter) isClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

// Rewrite returns the bytes to answer the query with: the upstream response
// verbatim, or — when the query is eligible, the name is not an allow_private
// entry, the upstream AAAA answer is a normal one, and the A-existence probe
// says the name has an A record — a synthesized NODATA response. Every failure
// path fails safe to the verbatim relay.
func (r *Rewriter) Rewrite(query, upstreamResp []byte) []byte {
	if r.mode != ModeIPv4Prefer || r.isStopping() {
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
	// A nil matcher is unreachable in ipv4 mode — the constructor refuses it —
	// and relay-verbatim keeps a hypothetical zero-value rewriter fail-safe.
	if r.allowPrivateName == nil {
		slog.Debug("DNS rewrite skipped: no allow_private matcher; relaying AAAA verbatim", "name", qname)
		return upstreamResp
	}
	if r.allowPrivateName(strings.TrimSuffix(qname, ".")) {
		slog.Debug("DNS rewrite skipped: allow_private name; relaying AAAA verbatim", "name", qname)
		return upstreamResp
	}

	if ok, reason := upstreamAAAAWarrantsSynthesis(upstreamResp, parsed); !ok {
		slog.Debug("DNS AAAA answer relayed verbatim", "name", qname, "reason", reason)
		return upstreamResp
	}

	outcome := r.probeAEvidence(parsed.question)
	if outcome.evidence != dnsProbeAExists {
		if outcome.evidence == dnsProbeUnknown {
			slog.Debug("A probe unresolved; relaying AAAA verbatim", "name", qname, "reason", outcome.reason)
		}
		return upstreamResp
	}
	// A probe that finished after Close must not rewrite: the stop promise is
	// "everything after Close is verbatim", even for in-flight probes.
	if r.isClosed() {
		slog.Debug("DNS rewrite skipped: rewriter closed; relaying AAAA verbatim", "name", qname)
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
func (r *Rewriter) probeAEvidence(question dnsmessage.Question) dnsProbeOutcome {
	// Defense in depth at the probe/cache boundary: only an IN-class AAAA
	// question may reach the network or the cache (the guest-query parser
	// already rejects everything else).
	if question.Type != dnsmessage.TypeAAAA || question.Class != dnsmessage.ClassINET {
		return dnsProbeOutcome{evidence: dnsProbeUnknown, reason: "unsupported probe question"}
	}
	key := canonicalDNSName(question.Name)

	r.mu.Lock()
	if entry, ok := r.cache[key]; ok {
		if r.now().Before(entry.expiresAt) {
			r.mu.Unlock()
			evidence := dnsProbeNoA
			if entry.positive {
				evidence = dnsProbeAExists
			}
			return dnsProbeOutcome{evidence: evidence}
		}
		delete(r.cache, key)
	}
	if call, ok := r.inflight[key]; ok {
		r.mu.Unlock()
		select {
		case <-call.done:
			return call.outcome
		case <-r.stopCh:
			return dnsProbeOutcome{evidence: dnsProbeUnknown, reason: "rewriter closed while waiting for the A probe"}
		}
	}
	call := &dnsProbeCall{done: make(chan struct{})}
	r.inflight[key] = call
	r.mu.Unlock()

	outcome := r.runAProbe(question)

	r.mu.Lock()
	if outcome.evidence != dnsProbeUnknown && outcome.ttl > 0 && !r.closed {
		r.storeEntryLocked(key, outcome, r.now())
	}
	// Publish the outcome and close done in the SAME critical section that
	// removes the in-flight entry: no new leader for this name can be elected
	// before every waiter of the old call has been released, and waiters
	// observe the outcome through the happens-before edge of the closed
	// channel.
	call.outcome = outcome
	close(call.done)
	delete(r.inflight, key)
	r.mu.Unlock()

	return outcome
}

// storeEntryLocked memoizes a decisive outcome for key, keeping the cache
// bounded: expired entries are swept first, and when the cache is still full
// the oldest quarter (by expiry, ties broken by name) is dropped
// deterministically. The caller must hold r.mu.
func (r *Rewriter) storeEntryLocked(key string, outcome dnsProbeOutcome, now time.Time) {
	if r.cache == nil {
		r.cache = make(map[string]dnsRewriteCacheEntry)
	}
	if _, exists := r.cache[key]; !exists && len(r.cache) >= dnsRewriteCacheMaxEntries {
		for cached, entry := range r.cache {
			if !now.Before(entry.expiresAt) {
				delete(r.cache, cached)
			}
		}
	}
	if len(r.cache) >= dnsRewriteCacheMaxEntries {
		r.evictOldestLocked(len(r.cache) / 4)
	}
	r.cache[key] = dnsRewriteCacheEntry{
		positive:  outcome.evidence == dnsProbeAExists,
		expiresAt: now.Add(time.Duration(outcome.ttl) * time.Second),
	}
}

// evictOldestLocked drops count entries with the earliest expiry (ties broken
// by name) so the bound holds deterministically. The caller must hold r.mu.
func (r *Rewriter) evictOldestLocked(count int) {
	if count <= 0 {
		return
	}
	keys := make([]string, 0, len(r.cache))
	for key := range r.cache {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		left, right := r.cache[keys[i]], r.cache[keys[j]]
		if left.expiresAt.Equal(right.expiresAt) {
			return keys[i] < keys[j]
		}
		return left.expiresAt.Before(right.expiresAt)
	})
	for _, key := range keys[:min(count, len(keys))] {
		delete(r.cache, key)
	}
}

// runAProbe performs the actual A-existence probe against the upstream list,
// within ONE absolute deadline so a black-holed resolver cannot multiply the
// wait, and stopping early when the rewriter closes. Only a TRANSPORT error
// falls through to the next resolver; the first received response is FINAL — it
// is classified and returned, so a malformed or mismatched response can never
// be rescued into positive evidence by a later resolver.
//
// The deadline uses the real clock even when a test injected a cache clock:
// network timeouts must not be decoupled from wall time.
func (r *Rewriter) runAProbe(question dnsmessage.Question) dnsProbeOutcome {
	probeID := uint16(r.probeSeq.Add(1))
	expected := dnsmessage.Question{Name: question.Name, Type: dnsmessage.TypeA, Class: question.Class}
	query, err := (&dnsmessage.Message{
		Header:    dnsmessage.Header{ID: probeID, RecursionDesired: true},
		Questions: []dnsmessage.Question{expected},
	}).Pack()
	if err != nil {
		return dnsProbeOutcome{evidence: dnsProbeUnknown, reason: "cannot pack A probe query"}
	}

	deadline := time.Now().Add(r.probeBudgetValue())
	reason := "no upstream nameserver configured"
	for _, server := range r.servers {
		if r.isStopping() {
			return dnsProbeOutcome{evidence: dnsProbeUnknown, reason: "rewriter closed"}
		}
		if !time.Now().Before(deadline) {
			reason = "A probe budget exhausted"
			break
		}
		resp, err := r.exchange(query, server, deadline)
		if err != nil {
			reason = fmt.Sprintf("A probe transport error: %v", err)
			continue
		}
		return classifyAProbeResponse(resp, probeID, expected)
	}
	return dnsProbeOutcome{evidence: dnsProbeUnknown, reason: reason}
}

// probeBudgetValue returns the effective probe budget.
func (r *Rewriter) probeBudgetValue() time.Duration {
	if r.probeBudget > 0 {
		return r.probeBudget
	}
	return dnsProbeBudget
}
