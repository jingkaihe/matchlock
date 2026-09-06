//go:build linux

package qemu

import "errors"

// Errors for the QEMU TCG backend.
var (
	// ErrUnsupportedPlatform indicates this backend is only valid on linux.
	ErrUnsupportedPlatform = errors.New("qemu backend requires linux")

	// ErrQEMUNotFound indicates the qemu-system-<arch> binary was not found.
	ErrQEMUNotFound = errors.New("qemu-system binary not found")

	// ErrVhostVsockUnavailable indicates /dev/vhost-vsock cannot be used.
	ErrVhostVsockUnavailable = errors.New("vhost-vsock unavailable")

	// ErrUnsupportedNetwork indicates a network mode QEMU cannot honor.
	ErrUnsupportedNetwork = errors.New("network mode unsupported by qemu backend")

	// ErrUnsupportedVFS indicates VFS/host-fs mounts QEMU cannot honor.
	ErrUnsupportedVFS = errors.New("vfs/host-fs unsupported by qemu backend")

	// ErrKernelNotFound indicates the pinned kernel image could not be found.
	ErrKernelNotFound = errors.New("kernel image not found")

	// ErrRootfsNotFound indicates the bootstrap/overlay disk is missing.
	ErrRootfsNotFound = errors.New("rootfs disk not found")

	// ErrCreate vs opErr.
	ErrCreate = errors.New("create qemu machine")

	// ErrStart indicates starting the QEMU process failed.
	ErrStart = errors.New("start qemu")

	// ErrNotReady indicates the guest did not become ready in time.
	ErrNotReady = errors.New("qemu guest not ready")

	// ErrStop indicates Stop failed.
	ErrStop = errors.New("stop qemu")

	// ErrWait indicates waiting for QEMU exited failed.
	ErrWait = errors.New("wait qemu")

	// ErrClose indicates Close failed.
	ErrClose = errors.New("close qemu")

	// ErrVsock indicates a vsock connect/dial failure.
	ErrVsock = errors.New("vsock")

	// ErrExec indicates an exec operation failed.
	ErrExec = errors.New("exec")

	// ErrVsockReset indicates the guest virtio-vsock transport reset an exec
	// connection (ECONNRESET) instead of the host closing it normally. It is
	// recorded distinctly so a QEMU-TCG acceptance failure can be told apart
	// from an agent protocol error, and so a guest teardown (panic / PID-1
	// OOM kill) is diagnosable from the captured serial log.
	ErrVsockReset = errors.New("vsock connection reset")

	// ErrWriteFile indicates a write_file operation failed.
	ErrWriteFile = errors.New("write_file")

	// ErrReadFile indicates a read_file operation failed.
	ErrReadFile = errors.New("read_file")

	// ErrListFiles indicates a list_files operation failed.
	ErrListFiles = errors.New("list_files")

	// ErrInvalidCID indicates an invalid guest CID.
	ErrInvalidCID = errors.New("invalid guest cid")

	// Network errors
	ErrCreateTAP    = errors.New("create tap")
	ErrTAPConfigure = errors.New("configure tap")
	// ErrTAPConfigureIPv6 covers installing the per-VM IPv6 guest link (gateway
	// address/prefix) on the TAP. It is separate from ErrTAPConfigure so a
	// missing IPv6 link, which the interception proxy and the ip6 table depend
	// on, is diagnosable without parsing the message text.
	ErrTAPConfigureIPv6 = errors.New("configure tap ipv6 address")
	ErrTAPSetMTU        = errors.New("set tap mtu")
	ErrTeardownTAP      = errors.New("teardown tap")
	// ErrNetworkUnsupported covers network modes the QEMU backend rejects.
	ErrNetworkUnsupported = errors.New("network mode unsupported")
)
