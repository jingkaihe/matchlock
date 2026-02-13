//go:build darwin

package main

import (
	"encoding/binary"
	"fmt"
	"syscall"
)

func totalMemoryMB() (int, error) {
	raw, err := syscall.Sysctl("hw.memsize")
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrSysctlMemsize, err)
	}
	b := []byte(raw)
	if len(b) < 8 {
		padded := make([]byte, 8)
		copy(padded, b)
		b = padded
	}
	mem := binary.LittleEndian.Uint64(b[:8])
	return int(mem / (1024 * 1024)), nil
}
