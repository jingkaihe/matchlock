package policy

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
)

type Engine struct {
	mu           sync.RWMutex
	config       *api.NetworkConfig
	placeholders map[string]string
	networkRules []compiledNetworkRule
	networkHook  networkHookInvoker
	allowPrivate []allowPrivateEntry

	// allowPrivateNames holds the distinct host names carried by allow_private
	// NAME entries and addHosts snapshots the static network.add_hosts mapping.
	// Both are immutable after construction, so the name resolver in
	// allow_private_name.go reads them without e.mu.
	allowPrivateNames []string
	addHosts          map[string][]net.IP

	// nameMu guards nameCache, the TTL-bounded host-side resolution of the
	// allow_private NAME entries. The resolver deliberately owns this lock
	// instead of e.mu: the private-block decision already runs with e.mu held
	// for reading, and a recursive RLock deadlocks as soon as a writer waits
	// for the read lock while a lookup is in flight.
	nameMu      sync.Mutex
	nameCache   map[string]*nameResolution
	now         func() time.Time
	nameTTL     time.Duration
	nameTimeout time.Duration
	nameWarn    func(name string)
}

// allowPrivateEntry is a single compiled allow_private exception. Exactly one of
// name, literal or network is set. port is the entry's port scope; 0 means any
// port.
type allowPrivateEntry struct {
	name    string
	literal net.IP
	network *net.IPNet
	port    int
}

func NewEngine(config *api.NetworkConfig) *Engine {
	if config == nil {
		config = &api.NetworkConfig{}
	}

	allowPrivate := compileAllowPrivate(config.AllowPrivate)

	e := &Engine{
		config:            config,
		placeholders:      make(map[string]string),
		networkRules:      compileNetworkRules(config.Interception),
		networkHook:       newNetworkHookInvoker(config),
		allowPrivate:      allowPrivate,
		allowPrivateNames: compileAllowPrivateNames(allowPrivate),
		addHosts:          compileAddHosts(config.AddHosts),
		nameCache:         make(map[string]*nameResolution),
		now:               time.Now,
		nameTTL:           allowPrivateNameTTL,
		nameTimeout:       allowPrivateNameLookupTimeout,
	}

	for name, secret := range config.Secrets {
		if secret.Placeholder == "" {
			placeholder := generatePlaceholder()
			config.Secrets[name] = api.Secret{
				Value:       secret.Value,
				Placeholder: placeholder,
				Hosts:       secret.Hosts,
			}
		}
		e.placeholders[name] = config.Secrets[name].Placeholder
	}

	return e
}

// SetNetworkHookInvoker overrides the SDK-local network hook invoker.
// This is primarily intended for testing.
func (e *Engine) SetNetworkHookInvoker(invoker networkHookInvoker) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.networkHook = invoker
}

func generatePlaceholder() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return api.GeneratedSecretPlaceholderPrefix + hex.EncodeToString(b)
}

func (e *Engine) GetPlaceholder(name string) string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.placeholders[name]
}

func (e *Engine) GetPlaceholders() map[string]string {
	e.mu.RLock()
	defer e.mu.RUnlock()

	result := make(map[string]string)
	for k, v := range e.placeholders {
		result[k] = v
	}
	return result
}

func (e *Engine) AddAllowedHosts(hosts ...string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.config == nil {
		e.config = &api.NetworkConfig{}
	}

	existing := make(map[string]struct{}, len(e.config.AllowedHosts))
	for _, host := range e.config.AllowedHosts {
		host = strings.TrimSpace(host)
		if host == "" {
			continue
		}
		existing[host] = struct{}{}
	}

	added := make([]string, 0, len(hosts))
	for _, host := range hosts {
		host = strings.TrimSpace(host)
		if host == "" {
			continue
		}
		if _, ok := existing[host]; ok {
			continue
		}
		e.config.AllowedHosts = append(e.config.AllowedHosts, host)
		existing[host] = struct{}{}
		added = append(added, host)
	}

	return added
}

func (e *Engine) RemoveAllowedHosts(hosts ...string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.config == nil || len(e.config.AllowedHosts) == 0 {
		return nil
	}

	toRemove := make(map[string]struct{}, len(hosts))
	for _, host := range hosts {
		host = strings.TrimSpace(host)
		if host == "" {
			continue
		}
		toRemove[host] = struct{}{}
	}
	if len(toRemove) == 0 {
		return nil
	}

	next := make([]string, 0, len(e.config.AllowedHosts))
	removed := make([]string, 0, len(toRemove))
	removedSet := make(map[string]struct{}, len(toRemove))
	for _, host := range e.config.AllowedHosts {
		host = strings.TrimSpace(host)
		if host == "" {
			continue
		}
		if _, ok := toRemove[host]; ok {
			if _, seen := removedSet[host]; !seen {
				removed = append(removed, host)
				removedSet[host] = struct{}{}
			}
			continue
		}
		next = append(next, host)
	}
	e.config.AllowedHosts = next
	return removed
}

func (e *Engine) AllowedHosts() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if e.config == nil || len(e.config.AllowedHosts) == 0 {
		return nil
	}
	hosts := make([]string, 0, len(e.config.AllowedHosts))
	for _, host := range e.config.AllowedHosts {
		host = strings.TrimSpace(host)
		if host == "" {
			continue
		}
		hosts = append(hosts, host)
	}
	return hosts
}

// IsHostAllowed reports whether host is allowed by the configured policy,
// ignoring any allow_private port scope (a trailing numeric :port on host is
// parsed and honored; a bare host matches any port). It is a thin wrapper around
// IsHostAllowedPort.
func (e *Engine) IsHostAllowed(host string) bool {
	host, port := hostAndPort(host)
	return e.IsHostAllowedPort(host, port)
}

// IsHostAllowedPort is IsHostAllowed with an explicit destination port so the
// port scope of an allow_private entry can be enforced. port 0 means "no port
// information"; an allow_private address entry with a concrete port does not
// match port 0. A port embedded in host is ignored in favor of the explicit
// argument (host is stripped with stripHostPort).
func (e *Engine) IsHostAllowedPort(host string, port int) bool {
	// Resolve (or refresh) the allow_private NAME entries before taking the
	// read lock: a lookup is DNS I/O and must never run under e.mu.
	e.refreshAllowPrivateNames()

	e.mu.RLock()
	defer e.mu.RUnlock()

	host = stripHostPort(host)

	if e.config.NoNetwork {
		return false
	}

	if e.config.BlockPrivateIPs && e.privateBlocked(host, port) {
		return false
	}

	if len(e.config.AllowedHosts) == 0 {
		return true
	}

	for _, pattern := range e.config.AllowedHosts {
		if matchGlob(pattern, host) {
			return true
		}
	}

	return false
}

func (e *Engine) OnRequest(req *http.Request, host string) (*http.Request, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	// Strip a port (and the brackets of a bracketed IPv6 literal) so a v6
	// destination host matches host patterns exactly like a v4 one. A plain
	// strings.Split(host, ":") would mangle "[fd00:1::5]:8080" into "[".
	host = stripHostPort(host)

	if err := e.applyBeforeNetworkRules(req, host); err != nil {
		return nil, err
	}

	for name, secret := range e.config.Secrets {
		if !e.isSecretAllowedForHost(name, host) {
			if e.requestContainsPlaceholder(req, secret.Placeholder) {
				return nil, api.ErrSecretLeak
			}
			continue
		}
		e.replaceInRequest(req, secret.Placeholder, secret.Value)
	}

	return req, nil
}

func (e *Engine) OnResponse(resp *http.Response, req *http.Request, host string) (*http.Response, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	// See OnRequest: a v6 literal keeps its colons, only a port and IPv6
	// brackets are removed.
	host = stripHostPort(host)
	return e.applyAfterNetworkRules(resp, req, host)
}

func (e *Engine) isSecretAllowedForHost(secretName, host string) bool {
	secret, ok := e.config.Secrets[secretName]
	if !ok {
		return false
	}

	if len(secret.Hosts) == 0 {
		return true
	}

	for _, pattern := range secret.Hosts {
		if matchGlob(pattern, host) {
			return true
		}
	}

	return false
}

func (e *Engine) requestContainsPlaceholder(req *http.Request, placeholder string) bool {
	for _, values := range req.Header {
		for _, v := range values {
			if strings.Contains(v, placeholder) {
				return true
			}
			if basicValueContainsPlaceholder(v, placeholder) {
				return true
			}
		}
	}

	if req.URL != nil {
		if strings.Contains(req.URL.String(), placeholder) {
			return true
		}
	}

	return false
}

// replaceInRequest substitutes the placeholder with the real secret in headers
// and URL query params only. We intentionally skip the request body because the
// body is processed by the remote server's application layer, which may log or
// echo it back in responses — leaking the real secret into the VM.
func (e *Engine) replaceInRequest(req *http.Request, placeholder, value string) {
	for key, values := range req.Header {
		for i, v := range values {
			if strings.Contains(v, placeholder) {
				req.Header[key][i] = strings.ReplaceAll(v, placeholder, value)
				continue
			}
			if replaced, ok := replaceBasicAuthPlaceholder(v, placeholder, value); ok {
				req.Header[key][i] = replaced
			}
		}
	}

	if req.URL != nil {
		if strings.Contains(req.URL.RawQuery, placeholder) {
			req.URL.RawQuery = strings.ReplaceAll(req.URL.RawQuery, placeholder, value)
		}
	}

}

func basicValueContainsPlaceholder(value, placeholder string) bool {
	decoded, ok := decodeBasicAuthHeader(value)
	if !ok {
		return false
	}
	return strings.Contains(decoded, placeholder)
}

func replaceBasicAuthPlaceholder(value, placeholder, replacement string) (string, bool) {
	decoded, ok := decodeBasicAuthHeader(value)
	if !ok || !strings.Contains(decoded, placeholder) {
		return "", false
	}
	replaced := strings.ReplaceAll(decoded, placeholder, replacement)
	encoded := base64.StdEncoding.EncodeToString([]byte(replaced))
	return "Basic " + encoded, true
}

func decodeBasicAuthHeader(value string) (string, bool) {
	prefix, encoded, ok := strings.Cut(value, " ")
	if !ok || !strings.EqualFold(strings.TrimSpace(prefix), "Basic") {
		return "", false
	}
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(encoded)
	}
	if err != nil {
		return "", false
	}
	return string(decoded), true
}

func matchGlob(pattern, str string) bool {
	if pattern == "*" {
		return true
	}

	// Simple prefix wildcard: *.example.com
	if strings.HasPrefix(pattern, "*.") && !strings.Contains(pattern[2:], "*") {
		suffix := pattern[1:]
		return strings.HasSuffix(str, suffix)
	}

	// Simple suffix wildcard: example.*
	if strings.HasSuffix(pattern, ".*") && !strings.Contains(pattern[:len(pattern)-2], "*") {
		prefix := pattern[:len(pattern)-2]
		return strings.HasPrefix(str, prefix+".")
	}

	// General glob matching with * as wildcard
	if strings.Contains(pattern, "*") {
		return matchWildcard(pattern, str)
	}

	return pattern == str
}

// matchWildcard handles patterns with * wildcards anywhere
func matchWildcard(pattern, str string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == str
	}

	// Check prefix (before first *)
	if parts[0] != "" && !strings.HasPrefix(str, parts[0]) {
		return false
	}
	str = str[len(parts[0]):]

	// Check suffix (after last *)
	lastPart := parts[len(parts)-1]
	if lastPart != "" && !strings.HasSuffix(str, lastPart) {
		return false
	}
	if lastPart != "" {
		str = str[:len(str)-len(lastPart)]
	}

	// Check middle parts in order
	for i := 1; i < len(parts)-1; i++ {
		if parts[i] == "" {
			continue
		}
		idx := strings.Index(str, parts[i])
		if idx < 0 {
			return false
		}
		str = str[idx+len(parts[i]):]
	}

	return true
}

// stripHostPort removes a trailing port from a host string so IP range
// matching works for IPv4 and IPv6. It handles the bracketed IPv6 form
// ([::1]:80 -> ::1), a bare IPv6 literal, and an IPv4-host:port form
// (127.0.0.1:80 -> 127.0.0.1).
func stripHostPort(host string) string {
	// Bracketed IPv6: [::1]:80 -> ::1
	if strings.HasPrefix(host, "[") {
		if end := strings.Index(host, "]"); end > 1 {
			return host[1:end]
		}
		return host
	}

	// Bare IPv6 literal (multiple colons) without a port.
	if strings.Count(host, ":") > 1 {
		if net.ParseIP(host) != nil {
			return host
		}
	}

	// IPv4-host:port or hostname:port — strip the numeric port suffix.
	if i := strings.LastIndexByte(host, ':'); i >= 0 && i < len(host)-1 {
		if isNumeric(host[i+1:]) {
			return host[:i]
		}
	}

	return host
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// privateRanges are the address ranges that BlockPrivateIPs denies. A host is
// treated as private if ANY of its resolved addresses falls in these ranges.
//
// The IPv4 ranges are matched against the embedded IPv4 form of an address (see
// isPrivateAddr), which also covers IPv4-mapped IPv6 literals such as
// ::ffff:192.168.1.1. The IPv4-mapped prefix is deliberately NOT listed here as
// "::ffff:0:0/96": net.ParseCIDR normalizes that prefix to 0.0.0.0/0, which
// would mark every IPv4 address private. See isPrivateAddr.
var privateRanges = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"100.64.0.0/10", // CGNAT / Tailscale-style
	"::1/128",
	"fc00::/7",
	"fe80::/10",
	"200::/7", // Yggdrasil overlay
}

// isPrivateIP reports whether host (a literal IP or a hostname) resolves to a
// private address. For a hostname, EVERY returned address is checked — not just
// the first — so a mixed public/private answer set cannot slip a private address
// through by ordering a public address first.
func isPrivateIP(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		ips, err := net.LookupIP(host)
		if err != nil || len(ips) == 0 {
			return false
		}
		for _, resolved := range ips {
			if isPrivateAddr(resolved) {
				return true
			}
		}
		return false
	}
	return isPrivateAddr(ip)
}

// isPrivateAddr reports whether ip falls in one of the privateRanges.
//
// IPv4 and IPv4-mapped IPv6 addresses (::ffff:a.b.c.d) are both matched using
// their embedded IPv4 form (ip.To4()), so ::ffff:192.168.1.1 is private while
// ::ffff:8.8.8.8 is public. Pure IPv6 addresses are matched against the IPv6
// ranges. The IPv4-mapped /96 prefix is intentionally not part of privateRanges
// because net.ParseCIDR("::ffff:0:0/96") normalizes to 0.0.0.0/0 and would
// otherwise mark every IPv4 address private.
func isPrivateAddr(ip net.IP) bool {
	v4 := ip.To4()
	for _, cidr := range privateRanges {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		if v4 != nil {
			// IPv4 literal or IPv4-mapped IPv6: only IPv4 ranges apply.
			if network.IP.To4() == nil {
				continue
			}
			if network.Contains(v4) {
				return true
			}
			continue
		}
		// Pure IPv6: only IPv6 ranges apply.
		if network.IP.To4() != nil {
			continue
		}
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// AllowedHostIPs resolves host to one or more IP addresses and returns the
// address set that satisfies the configured policy, or ok=false if the host is
// not allowed. It is a thin wrapper around AllowedHostIPsPort; a trailing
// numeric :port on host is parsed and honored (a bare host matches any port).
func (e *Engine) AllowedHostIPs(host string) ([]net.IP, bool) {
	host, port := hostAndPort(host)
	return e.AllowedHostIPsPort(host, port)
}

// AllowedHostIPsPort is AllowedHostIPs with an explicit destination port so the
// port scope of an allow_private entry can be enforced. Every resolved address
// is checked: under BlockPrivateIPs the host is rejected unless each private
// address is covered by an allow_private address entry (and, for a hostname,
// the name itself is listed). Callers MUST dial one of the returned addresses
// directly instead of re-resolving the hostname, so a DNS change between the
// policy check and the dial (rebinding / TOCTOU) cannot redirect the connection
// to an address that was not verified.
func (e *Engine) AllowedHostIPsPort(host string, port int) ([]net.IP, bool) {
	// See IsHostAllowedPort: warm the allow_private NAME cache off-lock so the
	// private-block check below never performs DNS while holding e.mu.
	e.refreshAllowPrivateNames()

	e.mu.RLock()
	defer e.mu.RUnlock()

	host = stripHostPort(host)

	if e.config.NoNetwork {
		return nil, false
	}

	ips, ok := e.destinationAddresses(host)
	if !ok {
		return nil, false
	}

	if e.config.BlockPrivateIPs {
		if literal := net.ParseIP(host); literal != nil {
			// An IP-literal destination needs only a covering address entry.
			if isPrivateAddr(literal) && !e.allowPrivateAddress(literal, port) {
				return nil, false
			}
		} else if e.privateBlockedResolved(host, ips, port) {
			return nil, false
		}
	}

	if len(e.config.AllowedHosts) == 0 {
		return ips, true
	}

	for _, pattern := range e.config.AllowedHosts {
		if matchGlob(pattern, host) {
			return ips, true
		}
	}

	return nil, false
}

// hostAndPort splits an optional trailing numeric port from host without
// resolving anything. It mirrors stripHostPort but also returns the port (0 when
// no numeric port suffix is present). The bracketed IPv6 form ([::1]:443) and a
// bare IPv6 literal (::1, 200::1) are handled explicitly so their colons are not
// mistaken for a port separator.
func hostAndPort(host string) (string, int) {
	host = strings.TrimSpace(host)

	if strings.HasPrefix(host, "[") {
		if end := strings.Index(host, "]"); end > 1 {
			name := host[1:end]
			rest := host[end+1:]
			if strings.HasPrefix(rest, ":") && isNumeric(rest[1:]) {
				if port, err := strconv.Atoi(rest[1:]); err == nil {
					return name, port
				}
			}
			return name, 0
		}
		return host, 0
	}

	// Bare IPv6 literal (multiple colons) without a port.
	if strings.Count(host, ":") > 1 && net.ParseIP(host) != nil {
		return host, 0
	}

	// IPv4-host:port or hostname:port — strip the numeric port suffix.
	if i := strings.LastIndexByte(host, ':'); i >= 0 && i < len(host)-1 {
		if isNumeric(host[i+1:]) {
			if port, err := strconv.Atoi(host[i+1:]); err == nil {
				return host[:i], port
			}
		}
	}

	return host, 0
}

// compileAllowPrivate parses the allow_private entries once, when the engine is
// built. Each entry is a host name, an IP literal or a CIDR with an optional
// trailing numeric port (host:port or [v6]:port); a bare entry matches any port.
// Malformed entries are kept as names so they simply never match, matching the
// permissive handling of AllowedHosts.
func compileAllowPrivate(entries []string) []allowPrivateEntry {
	if len(entries) == 0 {
		return nil
	}

	compiled := make([]allowPrivateEntry, 0, len(entries))
	for _, raw := range entries {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		compiled = append(compiled, compileAllowPrivateEntry(entry))
	}
	if len(compiled) == 0 {
		return nil
	}
	return compiled
}

func compileAllowPrivateEntry(entry string) allowPrivateEntry {
	var compiled allowPrivateEntry
	addr := entry

	// Bracketed form: [addr]:port (addr may be an IP literal, a CIDR or a name).
	if strings.HasPrefix(entry, "[") {
		if end := strings.Index(entry, "]"); end > 1 {
			addr = entry[1:end]
			rest := entry[end+1:]
			if strings.HasPrefix(rest, ":") && isNumeric(rest[1:]) {
				if port, err := strconv.Atoi(rest[1:]); err == nil {
					compiled.port = port
				}
			}
		}
		return finalizeAllowPrivateEntry(compiled, addr)
	}

	// Try the whole string first so a bare IPv6 literal (or IPv6 CIDR) is
	// recognized before any colon is treated as a port separator.
	if ip := net.ParseIP(entry); ip != nil {
		compiled.literal = ip
		return compiled
	}
	if _, network, err := net.ParseCIDR(entry); err == nil {
		compiled.network = network
		return compiled
	}

	// Then split an optional trailing numeric port from the remainder.
	if i := strings.LastIndexByte(entry, ':'); i >= 0 && i < len(entry)-1 {
		if isNumeric(entry[i+1:]) {
			if port, err := strconv.Atoi(entry[i+1:]); err == nil {
				compiled.port = port
				addr = entry[:i]
			}
		}
	}

	return finalizeAllowPrivateEntry(compiled, addr)
}

func finalizeAllowPrivateEntry(compiled allowPrivateEntry, addr string) allowPrivateEntry {
	if ip := net.ParseIP(addr); ip != nil {
		compiled.literal = ip
		return compiled
	}
	if _, network, err := net.ParseCIDR(addr); err == nil {
		compiled.network = network
		return compiled
	}
	compiled.name = addr
	return compiled
}

// privateBlocked reports whether BlockPrivateIPs denies host (an IP literal or a
// hostname) after applying the allow_private exemptions. It performs the DNS
// resolution itself and must be called with e.mu held (read lock).
func (e *Engine) privateBlocked(host string, port int) bool {
	// Fast path: without any exception list the legacy behavior applies
	// unchanged (any private resolved address blocks the destination).
	if len(e.allowPrivate) == 0 {
		return isPrivateIP(host)
	}

	if ip := net.ParseIP(host); ip != nil {
		if !isPrivateAddr(ip) {
			return false
		}
		return !e.allowPrivateAddress(ip, port)
	}

	ips, ok := e.destinationAddresses(host)
	if !ok {
		return false
	}
	return e.privateBlockedResolved(host, ips, port)
}

// privateBlockedResolved applies the private block to an already-resolved address
// set. Every private address must be individually allowed, and a hostname must
// additionally be covered by a matching name entry (the DNS rebinding guard: an
// address entry alone does not authorize an unlisted name, and a name entry
// alone does not authorize an unlisted address). An IP-literal host needs only
// a covering entry — see privateBlocked, which routes literals through
// allowPrivateAddress.
//
// Coverage here is deliberately pinned to allowPrivateAddressEntry: a NAME
// destination's private addresses must be covered by an address entry
// (literal/CIDR) in the same list, exactly as docs/network-interception.md
// documents ("a name that resolves to any unlisted private address is still
// refused — even when another of its addresses is listed"). The resolved set of
// a name entry covers a destination only on the literal-destination paths, where
// the client itself chose the address.
func (e *Engine) privateBlockedResolved(host string, ips []net.IP, port int) bool {
	privateSeen := false
	for _, ip := range ips {
		if !isPrivateAddr(ip) {
			continue
		}
		privateSeen = true
		if !e.allowPrivateAddressEntry(ip, port) {
			return true
		}
	}

	if !privateSeen {
		return false
	}

	// All private addresses are covered; a hostname still needs a matching name
	// entry. IP literals were handled by the caller and never reach here.
	return !e.allowPrivateName(host, port)
}

// allowPrivateAddress reports whether ip is covered by an allow_private entry
// whose port scope matches. Two entry forms cover an address: an address entry
// (IP literal or CIDR) that contains it, and a NAME entry whose host-side
// resolution contains it.
//
// The NAME arm is what makes "--allow-private name:port" work on the paths that
// observe the literal destination rather than the name: pkg/net/proxy.go
// handlePassthrough only ever sees the pre-DNAT destination IP:port, and an HTTP
// request whose Host header is an IP literal is seen the same way. Such a
// destination is not subject to DNS rebinding — the client picked that address —
// so an entry that resolves to it may cover it.
//
// It is deliberately NOT the coverage predicate for a NAME destination: see
// privateBlockedResolved and allowPrivateAddressEntry for that (documented)
// guard. The resolved set is served from the cache that
// refreshAllowPrivateNames warms before e.mu is taken — this function runs with
// e.mu held for reading and must not perform DNS itself. A name that does not
// resolve has an empty set, so it covers nothing.
func (e *Engine) allowPrivateAddress(ip net.IP, port int) bool {
	if e.allowPrivateAddressEntry(ip, port) {
		return true
	}
	return e.allowPrivateNameAddress(ip, port)
}

// allowPrivateAddressEntry reports whether ip is covered by an allow_private
// address entry (IP literal or CIDR) whose port scope matches. This is the
// pinned-address coverage the DNS rebinding guard requires for a NAME
// destination: an entry that names a host does not, on its own, authorize the
// address that host currently resolves to (see docs/network-interception.md).
func (e *Engine) allowPrivateAddressEntry(ip net.IP, port int) bool {
	if ip == nil {
		return false
	}

	for _, entry := range e.allowPrivate {
		if entry.port != 0 && entry.port != port {
			continue
		}
		if entry.literal != nil && entry.literal.Equal(ip) {
			return true
		}
		if entry.network != nil && entry.network.Contains(ip) {
			return true
		}
	}
	return false
}

// allowPrivateNameAddress reports whether ip is one of the addresses a NAME
// entry with a matching port scope currently resolves to. The match is against
// the resolved address set, never against the host string, so it authorizes the
// literal destination of the paths described in allowPrivateAddress without
// weakening allowPrivateName (the destination-name matcher used by the rebinding
// guard).
//
// Glob name entries (*.example.com) are matched by allowPrivateName only: a
// pattern is not a host name, so compileAllowPrivateNames never resolves it and
// it contributes no address here.
func (e *Engine) allowPrivateNameAddress(ip net.IP, port int) bool {
	for _, entry := range e.allowPrivate {
		if entry.name == "" {
			continue
		}
		if entry.port != 0 && entry.port != port {
			continue
		}
		addresses, ok := e.nameEntryAddressesCached(entry.name)
		if !ok {
			continue
		}
		for _, resolved := range addresses {
			if resolved.Equal(ip) {
				return true
			}
		}
	}
	return false
}

// allowPrivateName reports whether host matches an allow_private name entry with
// a matching port scope. Name matching uses matchGlob, consistent with
// AllowedHosts.
func (e *Engine) allowPrivateName(host string, port int) bool {
	for _, entry := range e.allowPrivate {
		if entry.name == "" {
			continue
		}
		if entry.port != 0 && entry.port != port {
			continue
		}
		if matchGlob(entry.name, host) {
			return true
		}
	}
	return false
}
