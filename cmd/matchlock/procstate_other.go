//go:build !linux

package main

// processIsZombie is a no-op on platforms without /proc. The reaper's exit
// report is the primary cross-platform liveness signal, so an early child exit
// is still detected promptly there.
func processIsZombie(pid int) bool {
	return false
}
