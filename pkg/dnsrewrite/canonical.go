package dnsrewrite

import (
	"golang.org/x/net/dns/dnsmessage"
)

// This file holds the two shared DNS-identity helpers the corrected contract
// requires. They are deliberately exported so the LINUX resolver can adopt the
// exact same functions instead of its own copies (merge reconciliation: replace
// pkg/net/dns_rewrite.go's canonicalDNSName body with
// dnsrewrite.CanonicalizeName(name.String()) and its ECS check with
// dnsrewrite.HasContextAffectingOption). Reimplementing either rule per
// platform is the defect this factoring exists to prevent.

// CanonicalizeName returns the wire-name identity form used for EVERY DNS name
// comparison and cache key in this package: the name's bytes are preserved
// exactly except that the ASCII bytes 'A'..'Z' fold to 'a'..'z' (RFC 4343).
//
// There is deliberately NO Unicode case folding, NO IDNA conversion, NO
// invalid-UTF-8 replacement and NO whitespace trimming: a dnsmessage.Name is a
// raw wire label sequence, and reinterpreting its bytes could merge two
// distinct names into one identity (and therefore one cached verdict).
// strings.ToLower is NOT an acceptable implementation of this rule; distinct
// non-ASCII or non-UTF-8 names must never collide.
func CanonicalizeName(name string) string {
	b := []byte(name)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// CanonicalNameEqual reports whether two names are the same DNS name under the
// ASCII-only folding rule (see CanonicalizeName).
func CanonicalNameEqual(a, b string) bool {
	return CanonicalizeName(a) == CanonicalizeName(b)
}

// ECSOptionCode is the EDNS option code of EDNS Client Subnet (RFC 7871).
const ECSOptionCode uint16 = 8

// HasContextAffectingOption reports whether a query's EDNS OPT carries an
// option that changes the name-resolution context modeled by the resolver.
// Currently that is ECS (option code 8). A query carrying one MUST be relayed
// verbatim: the A-existence verdict is context-dependent, so neither probing
// nor cache reads/writes are valid. The option's payload is deliberately not
// interpreted — its mere presence selects the conservative path. DO=1 and CD=1
// do NOT qualify (they do not alter A-existence attribution), and a plain OPT
// without options remains eligible for the rewrite.
func HasContextAffectingOption(options []dnsmessage.Option) bool {
	for _, opt := range options {
		if opt.Code == ECSOptionCode {
			return true
		}
	}
	return false
}

// canonicalDNSName is the dnsmessage.Name form of CanonicalizeName, used by
// every name-identity comparison in this package.
func canonicalDNSName(name dnsmessage.Name) string {
	return CanonicalizeName(name.String())
}
