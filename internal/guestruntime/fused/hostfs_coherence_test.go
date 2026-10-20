//go:build linux

package guestfused

// Host-side coherence regressions for the guest FUSE daemon (real host FUSE,
// zero-model).
//
// The guest addresses the host by a cached path keyed only by the host inode
// number. A host-side rename, delete+recreate or atomic write-temp+rename is
// invisible to the guest, so a cached node can keep addressing a removed path
// (ENOENT on open/readdir) or a reused inode of a different type (persistent
// EIO). lookupOrReuse validates the cached path and file type before reuse;
// these tests mutate the host backing behind a live mount and assert the guest
// view converges to the host truth within the 1 s entry/attr timeout.
//
// The tests self-skip when FUSE/fusermount are unavailable (mountVFSWithBacking
// handles that), which is the case in an unprivileged sandbox.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// hostCoherenceBacking returns a host backing directory and whether the
// filesystem reuses a freed inode number immediately. The persistent
// wrong-type regression only fires when the host reuses the freed inode, which
// ext4/xfs do and tmpfs (often TMPDIR) does not, so prefer /var/tmp and fall
// back to t.TempDir() only when no reuse-capable location is available.
func hostCoherenceBacking(t *testing.T) (string, bool) {
	t.Helper()
	for _, base := range []string{"/var/tmp", os.TempDir()} {
		dir, err := os.MkdirTemp(base, "matchlock-coherence-*")
		if err != nil {
			continue
		}
		if hostReusesInodes(t, dir) {
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			return dir, true
		}
		_ = os.RemoveAll(dir)
	}
	return t.TempDir(), false
}

// hostReusesInodes reports whether the filesystem behind dir hands the just
// freed file inode to a directory created at the same name.
func hostReusesInodes(t *testing.T, dir string) bool {
	t.Helper()
	p := filepath.Join(dir, "reuse-probe")
	if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
		return false
	}
	before, err := os.Lstat(p)
	if err != nil {
		return false
	}
	if err := os.Remove(p); err != nil {
		return false
	}
	if err := os.Mkdir(p, 0755); err != nil {
		return false
	}
	after, err := os.Lstat(p)
	if err != nil {
		return false
	}
	_ = os.Remove(p)
	return inoOf(t, before) == inoOf(t, after)
}

// TestFuseMount_HostFileToDirectoryReplacement proves that a host-side
// delete+recreate of the same name with a different file type converges in the
// guest. On a reuse-capable filesystem the new directory gets the freed file
// inode, which used to coalesce onto the cached regular-file node and keep
// stamping S_IFREG into LOOKUP replies (persistent EIO on open/readdir) until
// FORGET. The guest must see a directory and read its child.
func TestFuseMount_HostFileToDirectoryReplacement(t *testing.T) {
	backing, reusesInodes := hostCoherenceBacking(t)
	require.NoError(t, os.MkdirAll(filepath.Join(backing, "c1"), 0755))
	hostTarget := filepath.Join(backing, "c1", "victim")
	require.NoError(t, os.WriteFile(hostTarget, []byte("old-file"), 0644))

	mnt, _ := mountVFSWithBacking(t, backing)
	guestTarget := filepath.Join(mnt, "c1", "victim")

	// Prime the guest: cache the regular-file node for this host inode.
	fi, err := os.Stat(guestTarget)
	require.NoError(t, err)
	require.True(t, fi.Mode().IsRegular(), "fixture must start as a regular file")

	hostBefore, err := os.Lstat(hostTarget)
	require.NoError(t, err)
	hostOldIno := inoOf(t, hostBefore)

	// Host deletes the file and creates a directory of the same name, so the
	// freed inode can be (and on ext4 is) reused for the directory.
	require.NoError(t, os.Remove(hostTarget))
	require.NoError(t, os.Mkdir(hostTarget, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(hostTarget, "inner"), []byte("inside"), 0644))

	hostAfter, err := os.Lstat(hostTarget)
	require.NoError(t, err)
	hostNewIno := inoOf(t, hostAfter)
	if reusesInodes {
		require.Equal(t, hostOldIno, hostNewIno,
			"reuse-capable backing must reuse the freed inode, otherwise the same-inode regression is not exercised")
	}
	t.Logf("host file->dir: host ino %d -> %d (reused=%v)", hostOldIno, hostNewIno, hostOldIno == hostNewIno)

	// The guest must converge on a directory and serve its entry and child.
	require.Eventually(t, func() bool {
		fi, err := os.Stat(guestTarget)
		if err != nil || !fi.IsDir() {
			return false
		}
		entries, err := os.ReadDir(guestTarget)
		if err != nil || len(entries) != 1 || entries[0].Name() != "inner" {
			return false
		}
		data, err := os.ReadFile(filepath.Join(guestTarget, "inner"))
		return err == nil && string(data) == "inside"
	}, 3*time.Second, 50*time.Millisecond, "guest must see the replacement directory and its child")
}

// TestFuseMount_HostAtomicReplaceAfterTmpLookup proves the write-temp+rename
// pattern (git refs, editors, node writeFileAtomic): the guest has looked up
// the temp name first (READDIRPLUS does that for every entry), the host renames
// the temp over the real name, and the guest must read the new content through
// the real name. Without the fix the cached node keeps addressing the removed
// temp path and open/read returns ENOENT until FORGET.
func TestFuseMount_HostAtomicReplaceAfterTmpLookup(t *testing.T) {
	backing, _ := hostCoherenceBacking(t)
	require.NoError(t, os.MkdirAll(filepath.Join(backing, "dir"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(backing, "dir", "b.txt"), []byte("old-content"), 0644))

	mnt, _ := mountVFSWithBacking(t, backing)

	hostTmp := filepath.Join(backing, "dir", ".b.tmp")
	hostReal := filepath.Join(backing, "dir", "b.txt")
	guestTmp := filepath.Join(mnt, "dir", ".b.tmp")
	guestReal := filepath.Join(mnt, "dir", "b.txt")

	// Stage the temp file and let the guest observe the temp name first.
	require.NoError(t, os.WriteFile(hostTmp, []byte("new-content"), 0644))
	fi, err := os.Stat(guestTmp)
	require.NoError(t, err)
	require.True(t, fi.Mode().IsRegular(), "temp fixture must be a regular file")

	// Publish atomically: the temp inode is now live only under b.txt.
	require.NoError(t, os.Rename(hostTmp, hostReal))

	// The guest must repair the node's cached path to b.txt and read the new
	// content instead of registering b.txt as an alias of the dead temp path.
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(guestReal)
		return err == nil && string(data) == "new-content"
	}, 3*time.Second, 50*time.Millisecond, "guest must read the atomically replaced file through b.txt")
}

// TestFuseMount_HostDirectoryRenameWithCachedChild proves that a host-side
// directory rename behind the guest's back does not leave the directory's
// cached path (or a cached child's path) dead. Without the fix the guest sees
// the renamed directory as empty and d2/inner as ENOENT for the lifetime of the
// cached nodes.
func TestFuseMount_HostDirectoryRenameWithCachedChild(t *testing.T) {
	backing, _ := hostCoherenceBacking(t)
	require.NoError(t, os.MkdirAll(filepath.Join(backing, "d1"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(backing, "d1", "inner"), []byte("inner-content"), 0644))

	mnt, _ := mountVFSWithBacking(t, backing)

	// Prime the guest directory and child nodes through the old name.
	fi, err := os.Stat(filepath.Join(mnt, "d1"))
	require.NoError(t, err)
	require.True(t, fi.IsDir(), "fixture must start as a directory")
	data, err := os.ReadFile(filepath.Join(mnt, "d1", "inner"))
	require.NoError(t, err)
	require.Equal(t, "inner-content", string(data))

	// Host renames the directory behind the guest's back.
	require.NoError(t, os.Rename(filepath.Join(backing, "d1"), filepath.Join(backing, "d2")))

	// The cached dir/child paths are dead; both must be repaired lazily through
	// the new name.
	require.Eventually(t, func() bool {
		fi, err := os.Stat(filepath.Join(mnt, "d2"))
		if err != nil || !fi.IsDir() {
			return false
		}
		data, err := os.ReadFile(filepath.Join(mnt, "d2", "inner"))
		return err == nil && string(data) == "inner-content"
	}, 3*time.Second, 50*time.Millisecond, "guest must resolve the renamed directory and its cached child")

	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(mnt, "d1"))
		return os.IsNotExist(err)
	}, 3*time.Second, 50*time.Millisecond, "the old directory name must no longer resolve")
}
