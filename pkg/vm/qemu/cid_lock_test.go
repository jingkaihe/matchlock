//go:build linux

package qemu

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// TestFlockExclusiveSerializes proves the CID-lock primitive: an exclusive
// non-blocking flock on a file fails while another holder retains it, and
// succeeds once the holder releases. This is the exact mutual-exclusion property
// the QEMU guest-CID allocator (reserveGuestCID) relies on to give distinct CIDs
// to concurrent sandboxes. Hardware-independent (no AF_VSOCK/vhost-vsock).
func TestFlockExclusiveSerializes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cid.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	require.NoError(t, err)
	defer f.Close()

	require.NoError(t, flockExclusive(f), "first exclusive flock must succeed")

	f2, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	require.NoError(t, err)
	defer f2.Close()
	require.Error(t, flockExclusive(f2), "a second flock on the same held file must fail")

	require.NoError(t, unix.Flock(int(f.Fd()), unix.LOCK_UN), "release first holder")
	require.NoError(t, flockExclusive(f2), "after release, the second flock must succeed")
	require.NoError(t, unix.Flock(int(f2.Fd()), unix.LOCK_UN))
}

// TestReserveGuestCIDConcurrentDistinct proves that concurrent reserveGuestCID
// calls hand out DISTINCT CIDs and that each reservation is released. It runs
// against an ISOLATED TMPDIR so it never touches the production /tmp CID
// namespace, and guarantees every reservation is closed even if an assertion
// fails. A finite concurrent test is specific evidence, not proof of universal
// race freedom; we also run it under -race and repeat.
func TestReserveGuestCIDConcurrentDistinct(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	const workers = 16
	type res struct {
		cid  uint32
		file *os.File
		err  error
	}
	results := make([]res, workers)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			cid, f, err := reserveGuestCID()
			results[idx] = res{cid: cid, file: f, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	t.Cleanup(func() {
		for i := range results {
			if results[i].file != nil {
				_ = results[i].file.Close()
			}
		}
	})

	seen := make(map[uint32]struct{}, workers)
	for i := 0; i < workers; i++ {
		require.NoError(t, results[i].err, "worker %d reserve failed", i)
		require.NotZero(t, results[i].cid, "worker %d got zero CID", i)
		_, dup := seen[results[i].cid]
		require.False(t, dup, "worker %d got duplicate CID %d", i, results[i].cid)
		seen[results[i].cid] = struct{}{}
	}
	require.Len(t, seen, workers, "all workers must get distinct CIDs")
}

// TestReserveGuestCIDSkipsHeldLock proves the allocator deterministically skips a
// CID whose lock is already held and hands out the next free one, and that when
// the holder releases, the CID becomes allocatable again.
func TestReserveGuestCIDSkipsHeldLock(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	cidA, lockA, err := reserveGuestCID()
	require.NoError(t, err, "reserve CID A")
	t.Cleanup(func() { _ = lockA.Close() })

	cidB, lockB, err := reserveGuestCID()
	require.NoError(t, err, "reserve CID B while A is held")
	t.Cleanup(func() { _ = lockB.Close() })
	require.NotEqual(t, cidA, cidB, "held CID A must be skipped")

	require.NoError(t, lockA.Close())
	path := filepath.Join(os.TempDir(), "matchlock-cid-"+strconv.FormatUint(uint64(cidA), 10)+".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	require.NoError(t, err)
	defer f.Close()
	require.NoError(t, flockExclusive(f), "released CID must be re-acquirable")
	require.NoError(t, unix.Flock(int(f.Fd()), unix.LOCK_UN))
}

// TestGuestCIDInheritedLockSurvivesParentClose proves the claim in
// reserveGuestCID's comment: the CID lock is held by the open-file DESCRIPTION,
// so a child that inherits the fd (QEMU via ExtraFiles) keeps the lock even after
// the parent closes its own copy. We use an explicit ready+release handshake so
// the child is GUARANTEED still holding the lock when the parent checks exclusion
// and after the parent releases — eliminating the ordering race a naive
// "child signals then exits" helper has.
func TestGuestCIDInheritedLockSurvivesParentClose(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	cidA, lockA, err := reserveGuestCID()
	require.NoError(t, err, "reserve CID A")
	// Do NOT t.Cleanup(close lockA): the test intentionally closes the parent's
	// copy and proves the child's inherited fd still holds the lock.

	// Ready pipe: child writes "ready" on readyW after re-flocking the inherited
	// fd. Release pipe: parent writes to releaseW; the child reads releaseR and
	// only then exits, so its fd (and thus the lock) is provably still held until
	// the parent chooses to release.
	readyR, readyW, err := os.Pipe()
	require.NoError(t, err)
	releaseR, releaseW, err := os.Pipe()
	require.NoError(t, err)

	cmd := exec.Command(os.Args[0], "-test.run=TestGuestCIDInheritedLockHelper", "--")
	// Child fd 3 = lock, fd 4 = readyW, fd 5 = releaseR.
	cmd.ExtraFiles = []*os.File{lockA, readyW, releaseR}
	cmd.Env = append(os.Environ(), "MATCHLOCK_CID_HELPER=1")
	cmd.Stdin = nil

	require.NoError(t, cmd.Start(), "start helper child")
	// Only now are the child's descriptors dupped; close the parent copies.
	_ = readyW.Close()
	_ = releaseR.Close()

	// Release the child: closing releaseW causes EOF on the child's releaseR, it
	// exits, and its fd close releases the lock. Wait for it exactly once, and
	// record the actual error so cleanup does not Wait() a second time or lose it.
	var childOnce sync.Once
	var childErr error
	childWait := func() error {
		childOnce.Do(func() { childErr = cmd.Wait() })
		return childErr
	}
	t.Cleanup(func() {
		// If we failed early, kill+reap the child (idempotent via childOnce) so
		// no fd/lock leaks.
		_ = cmd.Process.Kill()
		_ = childWait()
	})

	// Parent closes its own copy of lockA; the child's inherited fd (same open-file
	// description) must STILL hold the lock.
	_ = lockA.Close()

	// The child re-flocks the inherited fd and signals readiness.
	require.NoError(t, waitForReady(readyR), "child must become ready by re-flocking the inherited fd")
	_ = readyR.Close()

	// While the child holds the lock behind the release barrier, a fresh
	// reservation must skip CID A.
	cidB, lockB, err := reserveGuestCID()
	require.NoError(t, err, "reserve CID B while child holds A")
	t.Cleanup(func() { _ = lockB.Close() })
	require.NotEqual(t, cidA, cidB, "the child-inherited lock must keep CID A unavailable")

	// Release the child: closing releaseW causes EOF on the child's releaseR, it
	// exits, and its fd close releases the lock.
	_ = releaseW.Close()

	done := make(chan error, 1)
	go func() { done <- childWait() }()
	select {
	case err := <-done:
		require.NoError(t, err, "helper child should exit 0")
	case <-time.After(15 * time.Second):
		require.FailNow(t, "helper child did not exit")
	}

	// Now CID A must be reusable, while CID B is still held.
	path := filepath.Join(os.TempDir(), "matchlock-cid-"+strconv.FormatUint(uint64(cidA), 10)+".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	require.NoError(t, err)
	defer f.Close()
	require.NoError(t, flockExclusive(f), "CID A must be reusable after the inheriting child exits")
	require.NoError(t, unix.Flock(int(f.Fd()), unix.LOCK_UN))
	require.NotEqual(t, cidA, cidB, "CID B is still held, so it must differ from reusable A")
}

// TestGuestCIDInheritedLockHelper is the child process entrypoint. It inherits
// the CID lock at fd 3, re-flocks it (must succeed: same open-file description),
// writes "ready" on fd 4, then BLOCKS reading fd 5 until the parent closes the
// other end — so the lock is held until the parent explicitly releases.
func TestGuestCIDInheritedLockHelper(t *testing.T) {
	if os.Getenv("MATCHLOCK_CID_HELPER") != "1" {
		t.Skip("helper process only")
	}
	lock := os.NewFile(3, "inherited-lock")
	if lock == nil {
		os.Exit(2)
	}
	// The description is already flocked by the parent; re-flocking must succeed
	// for a fd referencing the same open-file description.
	if err := flockExclusive(lock); err != nil {
		os.Exit(3)
	}
	ready := os.NewFile(4, "ready")
	if ready == nil {
		os.Exit(4)
	}
	if _, err := ready.WriteString("ready\n"); err != nil {
		os.Exit(5)
	}
	release := os.NewFile(5, "release")
	if release == nil {
		os.Exit(6)
	}
	buf := make([]byte, 1)
	_, _ = release.Read(buf) // blocks until the parent closes releaseW (EOF)
	os.Exit(0)               // closes fd 3 -> releases the lock
}

// waitForReady reads "ready\n" from r, returning promptly on EOF (child failed)
// rather than spinning until the deadline.
func waitForReady(r *os.File) error {
	_ = r.SetReadDeadline(time.Now().Add(10 * time.Second))
	one := make([]byte, 1)
	var acc []byte
	for {
		n, err := r.Read(one)
		if n > 0 {
			acc = append(acc, one[:n]...)
			if string(acc) == "ready\n" {
				return nil
			}
		}
		if err != nil {
			if os.IsTimeout(err) {
				return os.ErrDeadlineExceeded
			}
			// EOF or a real error before the full "ready\n": the child failed.
			return err
		}
	}
}
