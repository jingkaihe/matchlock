//go:build linux

package guestfused

// Real host-FUSE gate helpers shared by the FUSE regression tests.
//
// Cleanup contract (qualification requirement): every mount this package
// creates must be unmounted while the VFS server and the host backing are still
// responsive, and the unmount result must never be silently ignored. Unmount is
// therefore performed FIRST (before the transport/server is closed), with a
// short bounded retry for transient EBUSY, and a persistent failure is reported
// through the test (the driver runs these gates in an owned private mount+pid
// namespace, so a failed mount can never wedge the shared host namespace; the
// failure still must be visible, never papered over).

import (
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// unmountNow attempts a bounded unmount of srv and reports a persistent failure
// through the test. It is meant to be the FIRST cleanup step (LIFO t.Cleanup /
// deferred registration keeps transports open until after the unmount).
func unmountNow(t *testing.T, srv *fuse.Server) {
	t.Helper()
	if srv == nil {
		return
	}
	// Bounded retry: a transient EBUSY (a racing kernel flush while the last
	// client is still draining) is not a gate failure; a mount that stays up
	// after the bound IS one.
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := srv.Unmount()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("FUSE unmount failed after retry bound: %v", err)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}
