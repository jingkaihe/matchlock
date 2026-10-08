package policy

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
)

const (
	// allowPrivateNameTTL bounds how long a resolved allow_private NAME entry
	// stays usable before the engine resolves the name again. Resolution is
	// lazy (the first policy decision that needs it, not engine construction)
	// so building an engine never blocks on DNS, and the refresh is always
	// performed outside e.mu (see refreshAllowPrivateNames). A 60 s TTL keeps
	// DNS changes honoured within a minute while still issuing at most one
	// query per name per TTL even under heavy connection churn.
	allowPrivateNameTTL = 60 * time.Second

	// allowPrivateNameLookupTimeout bounds a single host-side lookup of a NAME
	// entry so a black-holed DNS server cannot stall a policy decision (and,
	// with it, the read lock the decision holds) indefinitely.
	allowPrivateNameLookupTimeout = 2 * time.Second
)

// nameResolution is the cached resolution of one allow_private NAME entry.
//
// addresses is the address set the name resolved to; it is EMPTY (never nil
// substituted for a wildcard) when the name does not resolve, so an
// unresolvable entry simply matches no destination. warned records that the
// "does not resolve" debug line was already emitted for this name, so a name
// that keeps failing is reported once instead of once per connection; a later
// successful resolution clears it so a subsequent failure is reported again.
type nameResolution struct {
	addresses  []net.IP
	resolvedAt time.Time
	warned     bool
}

// compileAddHosts snapshots the static network.add_hosts mappings into a
// name -> address lookup. The same mappings are what pkg/vm turns into the
// guest /etc/hosts entries (matchlock.add_host.<i>), so a fixture or internal
// name can be exempted without touching operator DNS: this mapping is
// authoritative for policy evaluation and needs no resolver at all.
func compileAddHosts(mappings []api.HostIPMapping) map[string][]net.IP {
	if len(mappings) == 0 {
		return nil
	}

	compiled := make(map[string][]net.IP, len(mappings))
	for _, mapping := range mappings {
		name := normalizeDNSName(mapping.Host)
		ip := net.ParseIP(strings.TrimSpace(mapping.IP))
		if name == "" || ip == nil {
			continue
		}
		compiled[name] = append(compiled[name], ip)
	}
	if len(compiled) == 0 {
		return nil
	}
	return compiled
}

// compileAllowPrivateNames lists the distinct NAME entries of a compiled
// allow_private list that can actually be resolved host-side. The list is fixed
// at construction and read without e.mu.
//
// A glob pattern (*.example.com) is deliberately excluded: it is matched by
// allowPrivateName against the destination name, but it is not itself a host
// name, so resolving it would issue a pointless query per TTL and log a bogus
// "does not resolve" warning. A glob entry therefore contributes no resolved
// address set (see allowPrivateNameAddress).
func compileAllowPrivateNames(entries []allowPrivateEntry) []string {
	var names []string
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		name := normalizeDNSName(entry.name)
		if name == "" {
			continue
		}
		if strings.Contains(name, "*") {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names
}

// normalizeDNSName canonicalizes a host name (lowercase, no trailing root dot,
// no surrounding whitespace) so an allow_private entry, an add_hosts mapping
// and a DNS answer key compare equal regardless of how they were written.
func normalizeDNSName(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}

// nameEntryAddresses returns the address set the allow_private NAME entry name
// currently resolves to, resolving it first when the cache entry is missing or
// older than the TTL. A name that does not resolve yields an empty set, so such
// an entry never matches a destination.
//
// It never touches e.mu — only nameMu — because it is reachable from
// privateBlocked/allowPrivateAddress, which run with e.mu already held for
// reading. Holding nameMu across the lookup additionally coalesces concurrent
// lookups of the same name into a single DNS query and lets the second caller
// observe the fresh cache entry instead of re-querying.
func (e *Engine) nameEntryAddresses(name string) []net.IP {
	key := normalizeDNSName(name)
	if key == "" {
		return nil
	}

	e.nameMu.Lock()
	if resolution, ok := e.nameCache[key]; ok && !e.nameResolutionExpiredLocked(resolution) {
		addresses := cloneIPs(resolution.addresses)
		e.nameMu.Unlock()
		return addresses
	}
	addresses, warn := e.resolveNameEntryLocked(key)
	e.nameMu.Unlock()

	// Log outside the lock: the sink is caller-supplied and must not be able
	// to re-enter the cache.
	if warn {
		e.warnUnresolvedNameEntry(key)
	}

	return cloneIPs(addresses)
}

// nameEntryAddressesCached returns the cached address set of name when the
// entry exists and is still inside the TTL, and ok=false otherwise. It performs
// no DNS work at all, so it is safe to call with e.mu held for reading: the
// private-block decision uses it with the cache warmed by
// refreshAllowPrivateNames.
func (e *Engine) nameEntryAddressesCached(name string) ([]net.IP, bool) {
	key := normalizeDNSName(name)
	if key == "" {
		return nil, false
	}

	e.nameMu.Lock()
	defer e.nameMu.Unlock()

	resolution, ok := e.nameCache[key]
	if !ok || e.nameResolutionExpiredLocked(resolution) {
		return nil, false
	}
	return cloneIPs(resolution.addresses), true
}

// destinationAddresses resolves a destination host for a policy decision. A host
// that is one of the allow_private NAME entries is served from the resolver's
// TTL cache, which refreshAllowPrivateNames has already warmed outside e.mu: the
// name is therefore resolved at most once per TTL (no per-connection lookup) and
// the address set used by the private-block check is exactly the set the caller
// binds its dial to. Every other host is resolved directly, as before.
//
// It is safe to call with e.mu held: the cache read takes only nameMu (the
// private-block decision runs with e.mu held for reading and the resolver must
// never touch e.mu), and a cold cache falls back to a plain lookup.
func (e *Engine) destinationAddresses(host string) ([]net.IP, bool) {
	if cached, ok := e.nameEntryAddressesCached(host); ok {
		return cached, true
	}

	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return nil, false
	}
	return ips, true
}

// refreshAllowPrivateNames resolves every allow_private NAME entry whose cache
// entry is missing or stale, so the private-block decision (which runs with
// e.mu held) can match a destination address against the already-resolved set
// without doing any DNS work under the lock.
//
// Callers MUST invoke it before taking e.mu: it may perform a bounded DNS
// lookup, and e.mu is only taken briefly to read the config flags. Without any
// NAME entry — the common case — it is a single length check: no lock, no DNS.
func (e *Engine) refreshAllowPrivateNames() {
	names := e.allowPrivateNames
	if len(names) == 0 {
		return
	}

	e.mu.RLock()
	enabled := e.config != nil && e.config.BlockPrivateIPs && !e.config.NoNetwork
	e.mu.RUnlock()
	if !enabled {
		return
	}

	for _, name := range names {
		if _, ok := e.nameEntryAddressesCached(name); ok {
			continue
		}
		e.nameEntryAddresses(name)
	}
}

// resolveNameEntryLocked resolves name and stores the result in the cache.
// The caller must hold e.nameMu; the DNS lookup itself is bounded by the
// engine's lookup timeout. It reports whether the "name does not resolve"
// warning should be emitted for this resolution (first time only).
func (e *Engine) resolveNameEntryLocked(name string) ([]net.IP, bool) {
	addresses := e.addHostsAddresses(name)
	if len(addresses) == 0 {
		addresses = e.lookupNameEntry(name)
	}

	warned := false
	if previous, ok := e.nameCache[name]; ok {
		warned = previous.warned
	}
	warn := len(addresses) == 0 && !warned

	if e.nameCache == nil {
		// Defensive: an Engine literal that skipped NewEngine still caches.
		e.nameCache = make(map[string]*nameResolution)
	}
	e.nameCache[name] = &nameResolution{
		addresses:  addresses,
		resolvedAt: e.clock(),
		warned:     warned || warn,
	}
	return addresses, warn
}

// addHostsAddresses returns the static add_hosts mapping for name, if any.
func (e *Engine) addHostsAddresses(name string) []net.IP {
	mapped := e.addHosts[name]
	if len(mapped) == 0 {
		return nil
	}
	return cloneIPs(mapped)
}

// lookupNameEntry resolves name through the host resolver (net.DefaultResolver,
// so a test can install a synthetic one) with a bounded timeout. Any failure —
// NXDOMAIN, timeout, transport error — yields an empty address set, which never
// matches a destination.
func (e *Engine) lookupNameEntry(name string) []net.IP {
	timeout := e.nameTimeout
	if timeout <= 0 {
		timeout = allowPrivateNameLookupTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	resolved, err := net.DefaultResolver.LookupIPAddr(ctx, name)
	if err != nil {
		return nil
	}

	addresses := make([]net.IP, 0, len(resolved))
	for _, address := range resolved {
		if address.IP == nil {
			continue
		}
		addresses = append(addresses, address.IP)
	}
	return dedupeIPs(addresses)
}

// nameResolutionExpiredLocked reports whether resolution is older than the TTL.
// The caller must hold e.nameMu.
func (e *Engine) nameResolutionExpiredLocked(resolution *nameResolution) bool {
	return e.clock().Sub(resolution.resolvedAt) >= e.nameTTLValue()
}

// nameTTLValue returns the configured TTL, falling back to the default so an
// Engine zero value keeps the documented 60 s window.
func (e *Engine) nameTTLValue() time.Duration {
	if e.nameTTL <= 0 {
		return allowPrivateNameTTL
	}
	return e.nameTTL
}

// clock returns the engine clock. Tests replace it to exercise TTL expiry
// without sleeping.
func (e *Engine) clock() time.Time {
	if e.now == nil {
		return time.Now()
	}
	return e.now()
}

// warnUnresolvedNameEntry reports, exactly once per name until it resolves
// again, that a NAME entry never matched because it does not resolve.
func (e *Engine) warnUnresolvedNameEntry(name string) {
	if e.nameWarn != nil {
		e.nameWarn(name)
		return
	}
	slog.Debug("allow_private name entry does not resolve; it will never match",
		"name", name)
}

// cloneIPs copies an address set so a cached slice can never be mutated by a
// caller (or a caller's slice by the cache).
func cloneIPs(ips []net.IP) []net.IP {
	if len(ips) == 0 {
		return nil
	}
	cloned := make([]net.IP, 0, len(ips))
	for _, ip := range ips {
		cloned = append(cloned, append(net.IP(nil), ip...))
	}
	return cloned
}

// dedupeIPs removes duplicate addresses from a DNS answer while keeping the
// resolver's order.
func dedupeIPs(ips []net.IP) []net.IP {
	if len(ips) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(ips))
	unique := make([]net.IP, 0, len(ips))
	for _, ip := range ips {
		key := ip.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, ip)
	}
	if len(unique) == 0 {
		return nil
	}
	return unique
}
