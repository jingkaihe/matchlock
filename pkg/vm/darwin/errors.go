//go:build darwin

package darwin

import "errors"

// Config errors
var (
	ErrKernelNotFound   = errors.New("kernel not found")
	ErrRootfsNotFound   = errors.New("rootfs not found")
	ErrInitramfsNotFound = errors.New("initramfs not found")
	ErrVMConfigInvalid  = errors.New("VM configuration validation failed")
)

// VM lifecycle errors
var (
	ErrVMStart    = errors.New("failed to start VM")
	ErrVMNotReady = errors.New("VM failed to become ready")
	ErrVMReady    = errors.New("timeout waiting for VM ready signal")
	ErrVMCreate   = errors.New("failed to create virtual machine")
)

// Storage errors
var (
	ErrCopyRootfs       = errors.New("failed to copy rootfs")
	ErrDiskAttachment   = errors.New("failed to create disk attachment")
	ErrStorageConfig    = errors.New("failed to create storage config")
	ErrExtraDiskAttach  = errors.New("failed to create extra disk attachment")
	ErrExtraDiskConfig  = errors.New("failed to create extra disk config")
)

// Network errors
var (
	ErrSocketPair        = errors.New("failed to create socket pair")
	ErrFileHandleAttach  = errors.New("failed to create file handle network attachment")
	ErrNATAttach         = errors.New("failed to create NAT network attachment")
	ErrNetworkConfig     = errors.New("failed to create network config")
	ErrMACAddress        = errors.New("failed to generate MAC address")
)

// Device errors
var (
	ErrBootLoader    = errors.New("failed to create boot loader")
	ErrVMConfig      = errors.New("failed to create VM configuration")
	ErrVsockConfig   = errors.New("failed to create vsock config")
	ErrEntropyConfig = errors.New("failed to create entropy config")
	ErrNoVsockDevice = errors.New("no vsock device available")
	ErrVsockFD       = errors.New("vsock FD not available; use SocketDevice() for native vsock")
)

// Console errors
var (
	ErrConsoleLog     = errors.New("failed to create console log")
	ErrConsoleConfig  = errors.New("failed to configure console")
	ErrDevNull        = errors.New("failed to open /dev/null for reading")
	ErrSerialAttach   = errors.New("failed to create serial attachment")
	ErrConsoleDevice  = errors.New("failed to create console config")
)

// Exec errors
var (
	ErrExecConnect      = errors.New("failed to connect to exec service")
	ErrExecEncode       = errors.New("failed to encode exec request")
	ErrExecEncodeReq    = errors.New("failed to encode request")
	ErrExecWriteHeader  = errors.New("failed to write header")
	ErrExecWriteReq     = errors.New("failed to write request")
	ErrExecReadHeader   = errors.New("failed to read response header")
	ErrExecReadData     = errors.New("failed to read response data")
	ErrExecDecode       = errors.New("failed to decode exec response")
	ErrExecRemote       = errors.New("exec error")
)

// Close errors
var (
	ErrStop             = errors.New("stop")
	ErrCloseVFSListener = errors.New("close vfs listener")
	ErrCloseSocketPair  = errors.New("close socket pair")
)
