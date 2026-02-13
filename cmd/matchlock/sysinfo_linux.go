//go:build linux

package main

import (
	"fmt"
	"syscall"
)

func totalMemoryMB() (int, error) {
	var info syscall.Sysinfo_t
	if err := syscall.Sysinfo(&info); err != nil {
		return 0, fmt.Errorf("%w: %w", ErrSysinfo, err)
	}
	return int(info.Totalram * uint64(info.Unit) / (1024 * 1024)), nil
}
