//go:build linux

package linux

import "errors"

// TAP / network interface errors
var (
	ErrTAPCreate         = errors.New("create TAP device")
	ErrTAPConfigure      = errors.New("configure TAP interface")
	ErrTAPSetMTU         = errors.New("set MTU")
	ErrTAPDelete         = errors.New("delete interface")
	ErrTUNOpen           = errors.New("open TUN device")
	ErrTUNSETIFF         = errors.New("TUNSETIFF")
	ErrTUNSETPERSIST     = errors.New("TUNSETPERSIST")
	ErrTUNSETPERSISTOff  = errors.New("TUNSETPERSIST(0)")
	ErrInterfaceNotFound = errors.New("interface not found")
	ErrInvalidCIDR       = errors.New("invalid CIDR")
	ErrCreateSocket      = errors.New("create socket")
	ErrSIOCSIFADDR       = errors.New("SIOCSIFADDR")
	ErrSIOCSIFNETMASK    = errors.New("SIOCSIFNETMASK")
	ErrSIOCGIFFLAGS      = errors.New("SIOCGIFFLAGS")
	ErrSIOCSIFFLAGS      = errors.New("SIOCSIFFLAGS")
)

// Firecracker lifecycle errors
var (
	ErrWriteConfig      = errors.New("write firecracker config")
	ErrCreateLogFile    = errors.New("create log file")
	ErrStartFirecracker = errors.New("start firecracker")
	ErrVMNotReady       = errors.New("VM failed to become ready")
	ErrVMReadyTimeout   = errors.New("timeout waiting for VM ready signal")
)

// Vsock errors
var (
	ErrVsockNotConfigured = errors.New("vsock not configured")
	ErrVsockConnect       = errors.New("connect to vsock UDS")
	ErrVsockSendConnect   = errors.New("send CONNECT command")
	ErrVsockReadResponse  = errors.New("read CONNECT response")
	ErrVsockConnectFailed = errors.New("vsock CONNECT failed")
	ErrVsockNoDirectFD    = errors.New("vsock not implemented for direct FD access; use VsockPath for UDS")
)

// Exec errors
var (
	ErrExecConnect        = errors.New("connect to exec service")
	ErrExecEncodeRequest  = errors.New("encode exec request")
	ErrExecWriteHeader    = errors.New("write header")
	ErrExecWriteRequest   = errors.New("write request")
	ErrExecReadRespHeader = errors.New("read response header")
	ErrExecReadRespData   = errors.New("read response data")
	ErrExecDecodeResponse = errors.New("decode exec response")
	ErrExecRemote         = errors.New("exec error")
)

// Close errors
var (
	ErrStop       = errors.New("stop")
	ErrCloseTapFD = errors.New("close tap fd")
)
