package lifecycle

import "errors"

var (
	ErrCreateRecordDir = errors.New("create lifecycle record directory")
	ErrReadRecord      = errors.New("read lifecycle record")
	ErrDecodeRecord    = errors.New("decode lifecycle record")
	ErrEncodeRecord    = errors.New("encode lifecycle record")
	ErrWriteRecord     = errors.New("write lifecycle record")
	ErrRenameRecord    = errors.New("rename lifecycle record")
	ErrVMRunning       = errors.New("VM is running")
	ErrInvalidPhase    = errors.New("invalid lifecycle phase transition")
	ErrReconcileTable  = errors.New("reconcile nftables table")
	ErrReconcileTap    = errors.New("reconcile TAP interface")
	ErrReconcileRule   = errors.New("reconcile nftables rule")
)
