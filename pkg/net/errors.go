package net

import "errors"

var (
	ErrNFTablesConn  = errors.New("nftables connection failed")
	ErrNFTablesApply = errors.New("nftables apply failed")
	ErrListen        = errors.New("listen failed")
	ErrSyscall       = errors.New("syscall conn failed")
	ErrOriginalDst   = errors.New("getsockopt SO_ORIGINAL_DST failed")

	// ErrDNSConfig marks an invalid DNS forwarder construction (for example
	// the ipv4-preferring mode without an allow_private name matcher, which
	// would silently disable the rewrite).
	ErrDNSConfig = errors.New("invalid DNS forwarder config")
	// ErrDNSRewrite marks a failure inside the ipv4-preferring rewrite
	// machinery (probe or NODATA synthesis). Every such failure fails safe:
	// the caller relays the upstream bytes verbatim.
	ErrDNSRewrite = errors.New("DNS rewrite failed")
	// ErrStackNoPolicy reports an attempt to bring up the userspace network
	// stack without a policy engine. The stack exists to enforce the allowlist
	// and the private-destination block; without an engine it could only pass
	// traffic unchecked, so construction fails closed.
	ErrStackNoPolicy = errors.New("userspace network stack requires a policy engine")
)
