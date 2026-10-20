//go:build linux

package main

import (
	"os"
	"strconv"
	"strings"
)

// processIsZombie reports whether pid is in the zombie (or otherwise dead)
// state. kill(pid, 0) succeeds for a zombie because the pid slot is still
// occupied, so it cannot be used on its own to decide that a process is alive.
func processIsZombie(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		// A vanished /proc entry means the process is gone. Other read
		// failures are inconclusive and left to the kill(pid, 0) fallback.
		return os.IsNotExist(err)
	}

	// The comm field is parenthesised and may itself contain spaces and
	// parentheses, so the process state is the first field after the LAST ')'.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 || end+2 >= len(data) {
		return false
	}
	switch data[end+2] {
	case 'Z', 'X', 'x':
		return true
	default:
		return false
	}
}
