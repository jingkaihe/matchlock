//go:build linux

package guestfused

// Lookup-discovered hard-link regressions (real host FUSE, zero-model).
//
// The Link-based regressions in git_commit_repro_test.go create hard links
// through the guest (os.Link across the mount), so go-fuse registers those
// names on the shared node from the Link handler. The kernel can equally see a
// hard link that was created on the HOST before the mount (or by another guest
// view of the same host tree): two names then share one host inode, go-fuse
// coalesces the second Lookup onto the first name's node, and the looked-up
// name is never registered as an alternate of the shared node. Unlinking the
// cached (first-looked-up) name then leaves the surviving alias resolving to a
// removed path (ENOENT) in one direction, and a rename-overwrite can leave the
// surviving alias silently resolving to the WRONG file. These tests lock the
// Lookup/rename bookkeeping for such pre-existing host hard links.

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFuseMount_PreExistingHostHardLinks_BothUnlinkDirections proves, for host
// hard links created BEFORE the mount (discovered through Lookup rather than
// guest Link), that unlinking either alias in either order keeps the surviving
// alias readable and writable, including through file handles opened before the
// unlink.
func TestFuseMount_PreExistingHostHardLinks_BothUnlinkDirections(t *testing.T) {
	backing := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(backing, "one"), 0755))
	require.NoError(t, os.MkdirAll(filepath.Join(backing, "two"), 0755))
	require.NoError(t, os.MkdirAll(filepath.Join(backing, "three"), 0755))
	// Host-pre-created hard-link pairs, before the guest ever sees them.
	require.NoError(t, os.WriteFile(filepath.Join(backing, "one", "A"), []byte("PAYLOAD-A"), 0644))
	require.NoError(t, os.Link(filepath.Join(backing, "one", "A"), filepath.Join(backing, "one", "B")))
	require.NoError(t, os.WriteFile(filepath.Join(backing, "two", "C"), []byte("PAYLOAD-C"), 0644))
	require.NoError(t, os.Link(filepath.Join(backing, "two", "C"), filepath.Join(backing, "two", "D")))
	require.NoError(t, os.WriteFile(filepath.Join(backing, "three", "E"), []byte("PAYLOAD-E"), 0644))
	require.NoError(t, os.Link(filepath.Join(backing, "three", "E"), filepath.Join(backing, "three", "F")))

	mnt, _ := mountVFSWithBacking(t, backing)

	// --- direction 1: cache A first, unlink A (cached), B survives ---
	dirOne := filepath.Join(mnt, "one")
	a := filepath.Join(dirOne, "A")
	b := filepath.Join(dirOne, "B")
	fiA, err := os.Stat(a)
	require.NoError(t, err)
	fiB, err := os.Stat(b)
	require.NoError(t, err)
	require.True(t, os.SameFile(fiA, fiB), "A and B must be the same inode through the mount")

	// Open a handle on the surviving alias B BEFORE the cached A is unlinked.
	bFh, err := os.OpenFile(b, os.O_RDWR, 0)
	require.NoError(t, err)

	require.NoError(t, os.Remove(a), "unlink the cached (first-looked-up) alias A")

	// Surviving alias B must still resolve by name...
	data, err := os.ReadFile(b)
	require.NoError(t, err, "surviving alias B must resolve after cached A is unlinked")
	assert.Equal(t, "PAYLOAD-A", string(data))

	// ...and through the pre-existing handle (inode identity kept honest).
	_, err = bFh.Seek(0, 0)
	require.NoError(t, err)
	hbuf := make([]byte, len("PAYLOAD-A"))
	_, err = bFh.Read(hbuf)
	require.NoError(t, err, "read through handle opened on B before A was unlinked")
	assert.Equal(t, "PAYLOAD-A", string(hbuf))

	// Write through the surviving name and confirm host write-through.
	require.NoError(t, os.WriteFile(b, []byte("PAYLOAD-B2"), 0644))
	hostData, err := os.ReadFile(filepath.Join(backing, "one", "B"))
	require.NoError(t, err)
	assert.Equal(t, "PAYLOAD-B2", string(hostData))
	// Write through the pre-unlink handle too; host sees it on the surviving name.
	_, err = bFh.WriteAt([]byte("PAYLOAD-B3"), 0)
	require.NoError(t, err)
	require.NoError(t, bFh.Sync())
	hostData, err = os.ReadFile(filepath.Join(backing, "one", "B"))
	require.NoError(t, err)
	assert.Equal(t, "PAYLOAD-B3", string(hostData))
	require.NoError(t, bFh.Close())

	// --- direction 2: cache D first, unlink D (cached), C survives ---
	dirTwo := filepath.Join(mnt, "two")
	c := filepath.Join(dirTwo, "C")
	d := filepath.Join(dirTwo, "D")
	// Look D up FIRST so D becomes the shared node's cached path.
	fiD, err := os.Stat(d)
	require.NoError(t, err)
	fiC, err := os.Stat(c)
	require.NoError(t, err)
	require.True(t, os.SameFile(fiC, fiD), "C and D must be the same inode through the mount")

	require.NoError(t, os.Remove(d), "unlink the cached (first-looked-up) alias D")

	data, err = os.ReadFile(c)
	require.NoError(t, err, "surviving alias C must resolve after cached D is unlinked")
	assert.Equal(t, "PAYLOAD-C", string(data))
	require.NoError(t, os.WriteFile(c, []byte("PAYLOAD-C2"), 0644))
	hostData, err = os.ReadFile(filepath.Join(backing, "two", "C"))
	require.NoError(t, err)
	assert.Equal(t, "PAYLOAD-C2", string(hostData))

	// --- direction 3: unlink the cached alias BEFORE the surviving alias is
	// ever looked up, with an open handle keeping the shared inode alive so the
	// kernel still coalesces the later Lookup onto the surviving node. The
	// node's cached path was dropped with no alternate known at unlink time;
	// discovering F afterwards must promote F to the cached path. ---
	dirThree := filepath.Join(mnt, "three")
	e := filepath.Join(dirThree, "E")
	f := filepath.Join(dirThree, "F")
	// Look E up only (E becomes the cached path). Keep a handle open so the
	// kernel inode (and the coalesced node) stays alive across the unlink.
	eFh, err := os.OpenFile(e, os.O_RDWR, 0)
	require.NoError(t, err)
	eStat, err := eFh.Stat()
	require.NoError(t, err)
	eIno := inoOf(t, eStat)
	require.NoError(t, os.Remove(e), "unlink the cached alias E while F has never been looked up")
	// FIRST discovery of the surviving alias F, after its sibling was removed.
	fiF, err := os.Stat(f)
	require.NoError(t, err)
	require.Equal(t, eIno, inoOf(t, fiF), "F must be the same inode as the still-open E")
	data, err = os.ReadFile(f)
	require.NoError(t, err, "surviving alias F discovered after cached E was unlinked must resolve")
	assert.Equal(t, "PAYLOAD-E", string(data))
	_, err = eFh.WriteAt([]byte("PAYLOAD-F2"), 0)
	require.NoError(t, err)
	require.NoError(t, eFh.Sync())
	hostData, err = os.ReadFile(filepath.Join(backing, "three", "F"))
	require.NoError(t, err)
	assert.Equal(t, "PAYLOAD-F2", string(hostData))
	require.NoError(t, eFh.Close())
}

// TestFuseMount_PreExistingHostHardLinks_CrossDirectory proves the same
// survival property when the two pre-existing host hard links live in DIFFERENT
// directories (the second alias is discovered through a different parent
// inode). Unlinking the cached first alias must keep the cross-directory
// surviving alias readable/writable.
func TestFuseMount_PreExistingHostHardLinks_CrossDirectory(t *testing.T) {
	backing := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(backing, "d1"), 0755))
	require.NoError(t, os.MkdirAll(filepath.Join(backing, "d2"), 0755))
	require.NoError(t, os.MkdirAll(filepath.Join(backing, "d3"), 0755))
	require.NoError(t, os.MkdirAll(filepath.Join(backing, "d4"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(backing, "d1", "A"), []byte("XDIR-PAYLOAD"), 0644))
	require.NoError(t, os.Link(filepath.Join(backing, "d1", "A"), filepath.Join(backing, "d2", "B")))
	require.NoError(t, os.WriteFile(filepath.Join(backing, "d3", "C"), []byte("XDIR-PAYLOAD-C"), 0644))
	require.NoError(t, os.Link(filepath.Join(backing, "d3", "C"), filepath.Join(backing, "d4", "D")))

	mnt, backing2 := mountVFSWithBacking(t, backing)
	assert.Equal(t, backing, backing2)

	// Direction 1: cache d1/A, unlink it; surviving alias d2/B must resolve.
	fa := filepath.Join(mnt, "d1", "A")
	fb := filepath.Join(mnt, "d2", "B")
	fiA, err := os.Stat(fa)
	require.NoError(t, err)
	fiB, err := os.Stat(fb)
	require.NoError(t, err)
	require.True(t, os.SameFile(fiA, fiB), "cross-directory A and B must be the same inode")
	require.NoError(t, os.Remove(fa))
	data, err := os.ReadFile(fb)
	require.NoError(t, err, "cross-directory surviving alias B must resolve after cached A unlinked")
	assert.Equal(t, "XDIR-PAYLOAD", string(data))
	require.NoError(t, os.WriteFile(fb, []byte("XDIR-PAYLOAD-B2"), 0644))
	hostData, err := os.ReadFile(filepath.Join(backing, "d2", "B"))
	require.NoError(t, err)
	assert.Equal(t, "XDIR-PAYLOAD-B2", string(hostData))

	// Direction 2: cache d4/D, unlink it; surviving alias d3/C must resolve.
	fd := filepath.Join(mnt, "d4", "D")
	fc := filepath.Join(mnt, "d3", "C")
	fiD, err := os.Stat(fd)
	require.NoError(t, err)
	fiC, err := os.Stat(fc)
	require.NoError(t, err)
	require.True(t, os.SameFile(fiC, fiD), "cross-directory C and D must be the same inode")
	require.NoError(t, os.Remove(fd))
	data, err = os.ReadFile(fc)
	require.NoError(t, err, "cross-directory surviving alias C must resolve after cached D unlinked")
	assert.Equal(t, "XDIR-PAYLOAD-C", string(data))
}

// TestFuseMount_PreExistingHostHardLink_RenameSameInodeNoop covers rename(A,B)
// when A and B are already hard links to the same inode. POSIX says the rename
// is a no-op that keeps BOTH names; whatever the kernel does with the rename
// (short-circuit or forward to FUSE), both aliases must stay resolvable and the
// file must keep one inode identity through subsequent unlinks in either order.
func TestFuseMount_PreExistingHostHardLink_RenameSameInodeNoop(t *testing.T) {
	backing := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(backing, "pair"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(backing, "pair", "A"), []byte("SAME-INODE-PAYLOAD"), 0644))
	require.NoError(t, os.Link(filepath.Join(backing, "pair", "A"), filepath.Join(backing, "pair", "B")))

	mnt, _ := mountVFSWithBacking(t, backing)
	dir := filepath.Join(mnt, "pair")
	a := filepath.Join(dir, "A")
	b := filepath.Join(dir, "B")

	fiA, err := os.Stat(a)
	require.NoError(t, err)
	fiB, err := os.Stat(b)
	require.NoError(t, err)
	require.True(t, os.SameFile(fiA, fiB))
	inoBefore := inoOf(t, fiA)

	// rename A->B where both are the same inode: POSIX no-op keeps both names.
	require.NoError(t, os.Rename(a, b))

	// BOTH names must still be present and readable with the same identity.
	fiA2, err := os.Stat(a)
	require.NoError(t, err, "A must still exist after same-inode rename A->B")
	fiB2, err := os.Stat(b)
	require.NoError(t, err, "B must still exist after same-inode rename A->B")
	require.True(t, os.SameFile(fiA2, fiB2))
	require.Equal(t, inoBefore, inoOf(t, fiA2), "inode identity must not change across the no-op rename")

	data, err := os.ReadFile(a)
	require.NoError(t, err)
	assert.Equal(t, "SAME-INODE-PAYLOAD", string(data))
	data, err = os.ReadFile(b)
	require.NoError(t, err)
	assert.Equal(t, "SAME-INODE-PAYLOAD", string(data))

	// Unlink the name that is most likely the cached path (B was the rename
	// target; the surviving A must resolve afterwards).
	require.NoError(t, os.Remove(b))
	data, err = os.ReadFile(a)
	require.NoError(t, err, "A must resolve after B is unlinked following the no-op rename")
	assert.Equal(t, "SAME-INODE-PAYLOAD", string(data))
	require.NoError(t, os.WriteFile(a, []byte("SAME-INODE-PAYLOAD-2"), 0644))
	hostData, err := os.ReadFile(filepath.Join(backing, "pair", "A"))
	require.NoError(t, err)
	assert.Equal(t, "SAME-INODE-PAYLOAD-2", string(hostData))
}

// TestFuseMount_PreExistingHostHardLink_RenameOverwriteSurvivingAlias locks the
// rename-overwrite case where the DESTINATION name is one alias of a host
// hard-linked inode that has a SECOND alias. rename(A,B) removes destination B
// (an alias of inode I2) and moves A over it; the surviving alias D of I2 must
// keep resolving to I2's content and inode identity — never to the moved file's
// content through the stale B path.
func TestFuseMount_PreExistingHostHardLink_RenameOverwriteSurvivingAlias(t *testing.T) {
	backing := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(backing, "ov"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(backing, "ov", "A"), []byte("AAA-CONTENT"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(backing, "ov", "B"), []byte("DDD-CONTENT"), 0644))
	require.NoError(t, os.Link(filepath.Join(backing, "ov", "B"), filepath.Join(backing, "ov", "D")))

	mnt, _ := mountVFSWithBacking(t, backing)
	dir := filepath.Join(mnt, "ov")
	a := filepath.Join(dir, "A")
	b := filepath.Join(dir, "B")
	d := filepath.Join(dir, "D")

	// Populate the kernel/go-fuse tree: A is its own inode; B is looked up
	// FIRST so I2's shared node caches B, then D is discovered as I2's alias.
	fiA, err := os.Stat(a)
	require.NoError(t, err)
	fiB, err := os.Stat(b)
	require.NoError(t, err)
	fiD, err := os.Stat(d)
	require.NoError(t, err)
	require.True(t, os.SameFile(fiB, fiD), "B and D must be aliases of the same inode")
	require.False(t, os.SameFile(fiA, fiB), "A must be a different inode")
	inoI2 := inoOf(t, fiB)
	inoI1 := inoOf(t, fiA)

	// Handle on the surviving alias D, opened before the overwrite rename.
	dFh, err := os.OpenFile(d, os.O_RDWR, 0)
	require.NoError(t, err)
	defer dFh.Close()

	// rename A -> B overwrites destination B (an alias of I2).
	require.NoError(t, os.Rename(a, b))

	// The moved file is now at B...
	fiB2, err := os.Stat(b)
	require.NoError(t, err)
	require.Equal(t, inoI1, inoOf(t, fiB2), "B must now be the moved A inode")
	data, err := os.ReadFile(b)
	require.NoError(t, err)
	assert.Equal(t, "AAA-CONTENT", string(data), "B must carry the moved file's content")

	// ...while the surviving alias D still resolves to I2 (content + identity).
	fiD2, err := os.Stat(d)
	require.NoError(t, err)
	require.Equal(t, inoI2, inoOf(t, fiD2), "D must keep I2's inode identity after the overwrite rename")
	data, err = os.ReadFile(d)
	require.NoError(t, err, "surviving alias D must still resolve by name")
	assert.Equal(t, "DDD-CONTENT", string(data), "D must keep I2's content, never the moved file's")

	// Through the pre-existing handle, I2 is still readable/writable.
	_, err = dFh.Seek(0, 0)
	require.NoError(t, err)
	hbuf := make([]byte, len("DDD-CONTENT"))
	_, err = dFh.Read(hbuf)
	require.NoError(t, err)
	assert.Equal(t, "DDD-CONTENT", string(hbuf), "handle opened before the rename must keep I2's data")
	_, err = dFh.WriteAt([]byte("DDD-CONTENT-2"), 0)
	require.NoError(t, err)
	require.NoError(t, dFh.Sync())
	hostData, err := os.ReadFile(filepath.Join(backing, "ov", "D"))
	require.NoError(t, err)
	assert.Equal(t, "DDD-CONTENT-2", string(hostData), "host must see the write on the surviving alias D")
}

// inoOf returns the host-visible inode number of a file seen through the mount.
func inoOf(t *testing.T, fi os.FileInfo) uint64 {
	t.Helper()
	st, ok := fi.Sys().(*syscall.Stat_t)
	require.True(t, ok, "expected *syscall.Stat_t from os.Stat")
	return st.Ino
}
