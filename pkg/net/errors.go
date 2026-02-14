package net

import "errors"

var (
	ErrNFTablesConn    = errors.New("nftables connection failed")
	ErrNFTablesApply   = errors.New("nftables apply failed")
	ErrListen          = errors.New("listen failed")
	ErrSyscall         = errors.New("syscall conn failed")
	ErrOriginalDst     = errors.New("getsockopt SO_ORIGINAL_DST failed")
	ErrDNSProxyConfig  = errors.New("invalid DNS proxy config")
	ErrDNSProxyListen  = errors.New("DNS proxy listen failed")
	ErrTailscaleConfig = errors.New("invalid tailscale dialer config")
	ErrTailscaleStart  = errors.New("tailscale start failed")
	ErrTailscaleUp     = errors.New("tailscale bring-up failed")
	ErrTailscaleDial   = errors.New("tailscale dial failed")
	ErrTailscaleCLI    = errors.New("tailscale CLI failed")
)
