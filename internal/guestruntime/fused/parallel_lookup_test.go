//go:build linux

package guestfused

// Parallel first-Lookup regression for pre-existing HOST hard links (real host
// FUSE, zero-model).
//
// The registry race this suite targets is the one the owner-election fix in
// main.go closes: when several aliases of a never-before-seen host inode are
// first looked up at the same time, exactly one canonical VFSNode/go-fuse inode
// may be created for the inode, and every concurrent Lookup must be handed that
// SAME initialized inode with its alias path registered on it. The deterministic
// interleavings are forced in owner_election_test.go (barrier tests that park
// the elected creator mid-initialization); these tests hammer the same contract
// through a real FUSE mount where go-fuse serves many in-flight LOOKUPs from
// several goroutines, then prove that whichever alias is unlinked first the
// survivor keeps resolving (name + inode identity + content + host write-through)
// and that a rename-overwrite onto one alias never redirects the surviving
// alias to the moved file.
//
// All pairs are created on the HOST before the mount (pre-existing hard links
// discovered by Lookup, exactly the not-yet-proven forensics case) and every
// inode is first looked up by the parallel storm below, so no lookups are ever
// served from the go-fuse tree.

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFuseMount_ParallelFirstLookupsAcrossDirs races the very first lookups of
// many pre-existing host hard-link pairs whose aliases live in DIFFERENT
// directories, then exercises both unlink directions, reads/writes (name and
// host write-through) and a rename-overwrite with a surviving alias.
func TestFuseMount_ParallelFirstLookupsAcrossDirs(t *testing.T) {
	const (
		stormPairs  = 48 // aliases A_i (d1) <-> B_i (d2), all first-looked-up in parallel
		renamePairs = 8  // aliases R-B_i (d2r) <-> R-D_i (d2r) + solo R-A_i (d1r)
	)
	backing := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(backing, "d1"), 0755))
	require.NoError(t, os.MkdirAll(filepath.Join(backing, "d2"), 0755))
	require.NoError(t, os.MkdirAll(filepath.Join(backing, "d1r"), 0755))
	require.NoError(t, os.MkdirAll(filepath.Join(backing, "d2r"), 0755))

	// --- host-side fixtures, all BEFORE the mount (pre-existing hard links) ---
	content := make([]string, stormPairs)
	for i := 0; i < stormPairs; i++ {
		content[i] = fmt.Sprintf("PAR-%d-PAYLOAD", i)
		a := filepath.Join(backing, "d1", fmt.Sprintf("A_%d", i))
		b := filepath.Join(backing, "d2", fmt.Sprintf("B_%d", i))
		require.NoError(t, os.WriteFile(a, []byte(content[i]), 0644))
		require.NoError(t, os.Link(a, b))
	}
	for i := 0; i < renamePairs; i++ {
		// B_i and D_i are aliases of one inode (destination); A_i is a solo
		// inode used to overwrite B_i later.
		require.NoError(t, os.WriteFile(filepath.Join(backing, "d1r", fmt.Sprintf("A_%d", i)), []byte(fmt.Sprintf("RN-A-%d", i)), 0644))
		rb := filepath.Join(backing, "d2r", fmt.Sprintf("B_%d", i))
		require.NoError(t, os.WriteFile(rb, []byte(fmt.Sprintf("RN-D-%d", i)), 0644))
		require.NoError(t, os.Link(rb, filepath.Join(backing, "d2r", fmt.Sprintf("D_%d", i))))
	}

	mnt, backing2 := mountVFSWithBacking(t, backing)
	assert.Equal(t, backing, backing2)

	// --- Phase 1: parallel FIRST lookups of every storm pair. ---
	// Each pair's aliases live in different directories, so the kernel issues
	// independent LOOKUPs that go-fuse serves concurrently; every inode is
	// never-before-seen, so each pair is a genuine first-lookup election race.
	type openRes struct {
		fa, fb os.FileInfo
		ea, eb error
	}
	results := make([]openRes, stormPairs)
	var wg sync.WaitGroup
	for i := 0; i < stormPairs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a := filepath.Join(mnt, "d1", fmt.Sprintf("A_%d", i))
			b := filepath.Join(mnt, "d2", fmt.Sprintf("B_%d", i))
			fa, ea := os.Stat(a)
			fb, eb := os.Stat(b)
			results[i] = openRes{fa: fa, fb: fb, ea: ea, eb: eb}
		}(i)
	}
	wg.Wait()
	for i := 0; i < stormPairs; i++ {
		r := results[i]
		require.NoError(t, r.ea, "parallel first lookup d1/A_%d", i)
		require.NoError(t, r.eb, "parallel first lookup d2/B_%d", i)
		require.True(t, os.SameFile(r.fa, r.fb), "parallel first lookups of A_%d/B_%d must expose one inode", i)
	}

	// --- Phase 2: each unlink direction + read/write through the survivor. ---
	for i := 0; i < stormPairs; i++ {
		a := filepath.Join(mnt, "d1", fmt.Sprintf("A_%d", i))
		b := filepath.Join(mnt, "d2", fmt.Sprintf("B_%d", i))
		var survivor, removed string
		if i%2 == 0 {
			removed, survivor = a, b
		} else {
			removed, survivor = b, a
		}
		require.NoError(t, os.Remove(removed), "unlink one alias of pair %d", i)

		data, err := os.ReadFile(survivor)
		require.NoError(t, err, "surviving alias of pair %d must resolve after the other alias was unlinked (direction %s)", i, removed)
		assert.Equal(t, content[i], string(data), "surviving alias of pair %d must expose the pair's content", i)

		// Write through the survivor and verify host write-through on the same
		// host name (the survivor is the ONLY remaining name for the inode).
		want := fmt.Sprintf("PAR-%d-V2", i)
		require.NoError(t, os.WriteFile(survivor, []byte(want), 0644))
		hostSurvivor := filepath.Join(backing, filepath.Base(filepath.Dir(survivor)), filepath.Base(survivor))
		hostData, err := os.ReadFile(hostSurvivor)
		require.NoError(t, err, "host backing must expose the survivor write for pair %d", i)
		assert.Equal(t, want, string(hostData))
	}

	// --- Phase 3: rename-overwrite after parallel FIRST lookups. ---
	// For each rename pair: first look up the destination aliases B_i/D_i (d2r)
	// and the solo A_i (d1r) in parallel, then rename A_i over B_i. The
	// overwritten destination name B_i is an alias of the destination inode whose
	// surviving alias D_i must keep identity/content (never the moved file's).
	type rnRes struct {
		fb, fd, fa os.FileInfo
		eb, ed, ea error
	}
	rn := make([]rnRes, renamePairs)
	for i := 0; i < renamePairs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b := filepath.Join(mnt, "d2r", fmt.Sprintf("B_%d", i))
			d := filepath.Join(mnt, "d2r", fmt.Sprintf("D_%d", i))
			a := filepath.Join(mnt, "d1r", fmt.Sprintf("A_%d", i))
			fb, eb := os.Stat(b)
			fd, ed := os.Stat(d)
			fa, ea := os.Stat(a)
			rn[i] = rnRes{fb: fb, fd: fd, fa: fa, eb: eb, ed: ed, ea: ea}
		}(i)
	}
	wg.Wait()
	for i := 0; i < renamePairs; i++ {
		r := rn[i]
		require.NoError(t, r.eb, "parallel first lookup d2r/B_%d", i)
		require.NoError(t, r.ed, "parallel first lookup d2r/D_%d", i)
		require.NoError(t, r.ea, "parallel first lookup d1r/A_%d", i)
		require.True(t, os.SameFile(r.fb, r.fd), "B_%d and D_%d must alias one inode", i)
		require.False(t, os.SameFile(r.fa, r.fb), "A_%d must be a distinct inode", i)
	}
	for i := 0; i < renamePairs; i++ {
		a := filepath.Join(mnt, "d1r", fmt.Sprintf("A_%d", i))
		b := filepath.Join(mnt, "d2r", fmt.Sprintf("B_%d", i))
		d := filepath.Join(mnt, "d2r", fmt.Sprintf("D_%d", i))
		inoDest := inoOf(t, rn[i].fd)
		require.NoError(t, os.Rename(a, b), "rename A_%d over alias B_%d", i)

		fiB, err := os.Stat(b)
		require.NoError(t, err)
		data, err := os.ReadFile(b)
		require.NoError(t, err)
		assert.Equal(t, fmt.Sprintf("RN-A-%d", i), string(data), "B_%d must carry the moved file after overwrite", i)
		assert.NotEqual(t, inoDest, inoOf(t, fiB), "B_%d must now be the moved inode", i)

		fiD, err := os.Stat(d)
		require.NoError(t, err)
		require.Equal(t, inoDest, inoOf(t, fiD), "surviving alias D_%d must keep its inode identity", i)
		data, err = os.ReadFile(d)
		require.NoError(t, err, "surviving alias D_%d must keep resolving by name", i)
		assert.Equal(t, fmt.Sprintf("RN-D-%d", i), string(data), "D_%d must keep its own content, never the moved file's", i)
		// Host write-through on the surviving alias.
		require.NoError(t, os.WriteFile(d, []byte(fmt.Sprintf("RN-D-%d-V2", i)), 0644))
		hostData, err := os.ReadFile(filepath.Join(backing, "d2r", fmt.Sprintf("D_%d", i)))
		require.NoError(t, err)
		assert.Equal(t, fmt.Sprintf("RN-D-%d-V2", i), string(hostData))
	}
}
