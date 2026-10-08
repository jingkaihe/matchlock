//go:build linux

package linux

import "errors"

// TAP / network interface errors
var (
	ErrTAPCreate         = errors.New("create TAP device")
	ErrTAPConfigure      = errors.New("configure TAP interface")
	ErrTAPConfigureIPv6  = errors.New("configure TAP IPv6 address")
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

// IPv6 rtnetlink errors (the SIOCSIFADDR ioctl used for IPv4 cannot carry a
// 128-bit address, so IPv6 addresses go through RTM_NEWADDR instead)
var (
	ErrInvalidInterfaceIndex   = errors.New("invalid interface index")
	ErrInvalidIPv6Address      = errors.New("invalid IPv6 address")
	ErrInvalidIPv6PrefixLength = errors.New("invalid IPv6 prefix length")
	ErrNetlinkSocket           = errors.New("create netlink socket")
	ErrNetlinkBind             = errors.New("bind netlink socket")
	ErrNetlinkSend             = errors.New("send netlink message")
	ErrNetlinkRecv             = errors.New("receive netlink response")
	ErrNetlinkAddrAdd          = errors.New("netlink RTM_NEWADDR")
)

// Firecracker lifecycle errors
var (
	ErrWriteConfig      = errors.New("write firecracker config")
	ErrCreateLogFile    = errors.New("create log file")
	ErrStartFirecracker = errors.New("start firecracker")
	ErrVMNotReady       = errors.New("VM failed to become ready")
	ErrVMReadyTimeout   = errors.New("timeout waiting for VM ready signal")
	ErrInvalidCPUCount  = errors.New("invalid cpu count")
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
