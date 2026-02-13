package net

import "errors"

var (
	ErrNFTablesConn  = errors.New("nftables connection failed")
	ErrNFTablesApply = errors.New("nftables apply failed")
	ErrListen        = errors.New("listen failed")
	ErrSyscall       = errors.New("syscall conn failed")
	ErrOriginalDst   = errors.New("getsockopt SO_ORIGINAL_DST failed")
)
