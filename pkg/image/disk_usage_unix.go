//go:build linux || darwin

package image

import (
	"os"
	"syscall"
)

// fileStoredBytes returns allocated bytes on disk for a file, falling back to
// logical file length when allocation metadata is unavailable.
func fileStoredBytes(fi os.FileInfo) int64 {
	if fi == nil {
		return 0
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st != nil && st.Blocks > 0 {
		return st.Blocks * 512
	}
	return fi.Size()
}
