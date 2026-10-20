//go:build linux

package net

import (
	"os"
	"syscall"
)

// setSocketBufferSizes increases the SO_SNDBUF and SO_RCVBUF on the socket pair
// FD. Larger buffers prevent frame drops when the VM sends bursts of packets
// faster than the gVisor stack can consume them. This Linux adapter exists so
// the userspace stack (stack_userspace.go, no build tag) compiles and its tests
// run on Linux hosts as well as on macOS.
func setSocketBufferSizes(f *os.File) {
	fd := int(f.Fd())
	syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_SNDBUF, socketBufSize)
	syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, socketBufSize)
}
