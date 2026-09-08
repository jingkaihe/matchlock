package guestfused

// Deterministic concurrency tests for the canonical owner-election/publication
// primitive (VFSClient.lookupOrReuse) that backs the Lookup-discovered
// hard-link registry.
//
// These are lower-level barrier tests, not sleep-based hopes: the create
// callback (the only place a canonical node can come into existence) is
// instrumented and parked on explicit channels, so the interleavings that used
// to race are forced deterministically:
//
//   - several first Lookups of aliases of a never-before-seen host inode all
//     arrive before any of them finishes -> exactly ONE node is created and
//     every caller returns the SAME fully-published go-fuse inode;
//   - a kernel forget (VFSNode.OnForget -> clearHardLinkOwner) landing between
//     lookups re-elects a fresh node for the next alias;
//   - a forget racing the election of the first node cannot clear or corrupt a
//     node the kernel has not even seen yet.
//
// The production Lookup methods drive the same primitive with a create
// callback that calls parent.NewInode; the tests replace that callback with an
// instrumented one, which lets the node creation itself be the barrier. The
// registry-level invariants asserted here are the exact properties the real
// go-fuse bridge relies on when it later coalesces every alias onto one
// stableAttr node (fs/bridge.go addNewChild), and which the previous
// implementation violated under concurrent first lookups.

import (
	"sync"
	"testing"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testOwnerNode builds an owner-candidate VFSNode whose embedded go-fuse inode
// is the one lookupOrReuse hands back. In production the embed is initialized
// by parent.NewInode (fs/bridge.go newInodeUnlocked) before the node is
// published; identity/registry tests do not need a live bridge.
func testOwnerNode(client *VFSClient, path string) *VFSNode {
	n := &VFSNode{client: client, path: path, isDir: false}
	return n
}

// sameEmbedder is a helper create that returns node n and its own embed,
// mirroring what parent.NewInode does in production (initInode is run on the
// node's embedded Inode and the same embed is returned to every alias).
func sameEmbedder(n *VFSNode) func() (*VFSNode, *fs.Inode) {
	return func() (*VFSNode, *fs.Inode) { return n, &n.Inode }
}

// TestLookupOrReuse_ConcurrentFirstLookups_ElectsOneInitializedOwner forces the
// exact interleaving that used to diverge: two (then many) Lookups of aliases
// of the same never-before-seen host inode all reach the registry before the
// first one has created and initialized the node. The winner parks inside
// create (holding the per-inode election lock) until every contender has
// arrived, then publishes; every contender must then observe the SAME
// initialized embed and register its own alias path on the canonical node.
//
// Old behavior: each Lookup created its own VFSNode when the owner map was
// still empty, so with the winner parked mid-creation the loser would already
// have constructed a second node (create would be called twice) and the two
// goroutines would hand go-fuse two different embeds for one host inode.
func TestLookupOrReuse_ConcurrentFirstLookups_ElectsOneInitializedOwner(t *testing.T) {
	const ino = uint64(0xC0FFEE)
	client := &VFSClient{}

	var (
		createMu      sync.Mutex
		createCalls   int
		createEntered = make(chan struct{})
		releaseCreate = make(chan struct{})
		createOnce    sync.Once
	)
	create := func() (*VFSNode, *fs.Inode) {
		createMu.Lock()
		createCalls++
		createMu.Unlock()
		// Park the winner inside create (it holds the per-inode slot lock), so
		// every contender deterministically arrives while the canonical node is
		// neither created nor published.
		createOnce.Do(func() { close(createEntered) })
		<-releaseCreate
		n := testOwnerNode(client, "/ws/d1/A")
		return n, &n.Inode
	}

	aliasPaths := []string{"/ws/d1/A", "/ws/d1/B", "/ws/d2/C", "/ws/d2/D", "/ws/d3/E"}

	results := make([]*fs.Inode, len(aliasPaths))
	started := make(chan struct{}, len(aliasPaths))
	var wg sync.WaitGroup
	for i, p := range aliasPaths {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			started <- struct{}{}
			results[i] = client.lookupOrReuse(ino, p, create)
		}(i, p)
	}

	// Wait until the winner is inside create (holding the slot lock) and every
	// contender has been launched.
	<-createEntered
	for i := 0; i < len(aliasPaths); i++ {
		<-started
	}
	// While the winner is parked mid-creation no other goroutine may have been
	// allowed to construct a competing node. We cannot observe callers blocked
	// on the slot lock directly, so release the winner and assert afterwards
	// that create ran exactly once and every caller got the same embed.
	close(releaseCreate)
	wg.Wait()

	createMu.Lock()
	require.Equal(t, 1, createCalls, "exactly one node must be created for one host inode")
	createMu.Unlock()

	// Every concurrent alias got the SAME initialized embed (the canonical
	// node's), so go-fuse's later addNewChild coalescing cannot split them.
	require.NotNil(t, results[0])
	for i := 1; i < len(results); i++ {
		require.NotNil(t, results[i], "alias %d must resolve", i)
		assert.Same(t, results[0], results[i], "all concurrent aliases must share one go-fuse inode")
	}

	// And every looked-up alias path is registered on the canonical node, so
	// whichever alias is unlinked first, a surviving alias keeps resolving.
	slot := client.ownerSlotFor(ino)
	slot.mu.Lock()
	canonical := slot.node
	slot.mu.Unlock()
	require.NotNil(t, canonical)
	assert.Same(t, results[0], &canonical.Inode, "registry owner must be the returned inode's ops")

	known := map[string]bool{canonical.currentPath(): true}
	for _, a := range canonical.altPaths {
		known[a] = true
	}
	for _, p := range aliasPaths {
		assert.True(t, known[p], "alias path %s must be registered on the canonical node", p)
	}
}

// TestLookupOrReuse_ForgetBetweenLookups_Reelects proves the re-election path:
// once the kernel forgets the canonical node (VFSNode.OnForget ->
// clearHardLinkOwner), the next Lookup of an alias of the same host inode must
// elect a FRESH node (the forgotten embed is gone from go-fuse's stableAttrs,
// so handing it out again would be wrong), and the new alias's path must be
// registered on the fresh node.
func TestLookupOrReuse_ForgetBetweenLookups_Reelects(t *testing.T) {
	const ino = uint64(0xBADF00D)
	client := &VFSClient{}

	first := testOwnerNode(client, "/ws/dir/A")
	embed1 := client.lookupOrReuse(ino, "/ws/dir/A", sameEmbedder(first))
	require.Same(t, &first.Inode, embed1)

	// The kernel forgets the node: OnForget clears the slot (only when it still
	// points at the forgotten node).
	client.clearHardLinkOwner(ino, first)

	// A later Lookup of a second pre-existing alias must re-elect a fresh node.
	second := testOwnerNode(client, "/ws/dir/B")
	embed2 := client.lookupOrReuse(ino, "/ws/dir/B", sameEmbedder(second))
	require.NotSame(t, embed1, embed2, "after a forget the next alias must get a fresh node")
	require.Same(t, &second.Inode, embed2)

	slot := client.ownerSlotFor(ino)
	slot.mu.Lock()
	require.Same(t, second, slot.node, "registry must point at the freshly elected node")
	slot.mu.Unlock()
	assert.Equal(t, "/ws/dir/B", second.currentPath())
}

// TestLookupOrReuse_ForgetOfOlderOwnerDoesNotClearNewerOne locks the guard in
// clearHardLinkOwner: a late OnForget from a superseded node incarnation must
// never clear a registry entry that already points at a newer elected node.
func TestLookupOrReuse_ForgetOfOlderOwnerDoesNotClearNewerOne(t *testing.T) {
	const ino = uint64(0xFACE)
	client := &VFSClient{}

	old := testOwnerNode(client, "/ws/old")
	client.lookupOrReuse(ino, "/ws/old", sameEmbedder(old))
	client.clearHardLinkOwner(ino, old) // first forget

	cur := testOwnerNode(client, "/ws/cur")
	client.lookupOrReuse(ino, "/ws/cur", sameEmbedder(cur))

	// A stale/late clear for the OLD node arrives after the re-election.
	client.clearHardLinkOwner(ino, old)

	slot := client.ownerSlotFor(ino)
	slot.mu.Lock()
	require.Same(t, cur, slot.node, "late forget of the old owner must not clear the new owner")
	slot.mu.Unlock()
}

// TestLookupOrReuse_ConcurrentLosersAfterPublish_RegisterDistinctPaths runs a
// thundering herd of alias lookups AFTER the canonical node is published and
// asserts every path is registered exactly once on the shared node and every
// caller still receives the single canonical embed. Under -race this also
// shakes out data races between registration (pathMu) and the shared registry.
func TestLookupOrReuse_ConcurrentLosersAfterPublish_RegisterDistinctPaths(t *testing.T) {
	const ino = uint64(0x1234)
	client := &VFSClient{}

	first := testOwnerNode(client, "/ws/root/A")
	embed := client.lookupOrReuse(ino, "/ws/root/A", sameEmbedder(first))
	require.Same(t, &first.Inode, embed)

	const n = 32
	paths := make([]string, n)
	for i := range paths {
		paths[i] = "/ws/root/link" + string(rune('a'+i))
	}
	var wg sync.WaitGroup
	got := make([]*fs.Inode, n)
	for i, p := range paths {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			got[i] = client.lookupOrReuse(ino, p, func() (*VFSNode, *fs.Inode) {
				t.Error("owner already exists; create must not run")
				return nil, nil
			})
		}(i, p)
	}
	wg.Wait()

	for i := range got {
		assert.Same(t, embed, got[i], "every alias must reuse the canonical embed")
	}
	slot := client.ownerSlotFor(ino)
	slot.mu.Lock()
	canonical := slot.node
	slot.mu.Unlock()
	require.Same(t, first, canonical)

	known := map[string]int{canonical.currentPath(): 1}
	for _, a := range canonical.altPaths {
		known[a]++
	}
	for _, p := range paths {
		assert.Equal(t, 1, known[p], "path %s must be registered exactly once", p)
	}
}

// TestLookupOrReuse_ForgetDuringElection_CannotClearUnpublishedNode forces a
// forget/clear to arrive while the first creation for the inode is still in
// flight (kernel cannot actually know the node yet, but a racing clear for it
// must be a no-op and must not disturb the election).
//
// The clear is serialized behind the in-flight election by the per-inode slot
// lock (lookupOrReuse holds the slot lock across create so no clear can
// interleave between the "no canonical node yet" decision and the publish of
// the freshly initialized node). The test therefore starts the clear in a
// goroutine while the election is parked inside create, then releases the
// election: the clear must complete only AFTER the publish, and must leave the
// freshly elected node in place (a clear of a node that was never published —
// or of any stale node — is a no-op).
func TestLookupOrReuse_ForgetDuringElection_CannotClearUnpublishedNode(t *testing.T) {
	const ino = uint64(0x99)
	client := &VFSClient{}

	entered := make(chan struct{})
	release := make(chan struct{})
	create := func() (*VFSNode, *fs.Inode) {
		close(entered)
		<-release
		n := testOwnerNode(client, "/ws/X")
		return n, &n.Inode
	}

	type result struct {
		embed *fs.Inode
	}
	resCh := make(chan result, 1)
	go func() {
		resCh <- result{client.lookupOrReuse(ino, "/ws/X", create)}
	}()

	// The winner is parked inside create holding the per-inode slot lock; no
	// other goroutine can observe or modify the slot until it publishes.
	<-entered

	clearDone := make(chan struct{})
	go func() {
		// A clear for a node that was never published (or any stale node) must
		// be a no-op and must not corrupt the in-flight election. It is
		// serialized behind the election's slot lock, so it cannot complete
		// until the election publishes and releases the lock.
		client.clearHardLinkOwner(ino, testOwnerNode(client, "/ws/stale"))
		close(clearDone)
	}()

	// Release the election: it publishes the freshly created node, returns its
	// embed, and only then lets the blocked clear proceed.
	close(release)
	res := <-resCh
	<-clearDone

	slot := client.ownerSlotFor(ino)
	slot.mu.Lock()
	canonical := slot.node
	slot.mu.Unlock()
	require.NotNil(t, canonical)
	require.Same(t, &canonical.Inode, res.embed, "a stale clear during the election must not displace the elected node")
}

// TestLookupOrReuse_DroppedPathPromotionSurvivesReuse keeps the
// unlink-before-lookup discipline: when the canonical alias is removed while no
// other name is known (dropped), a later Lookup of a surviving alias promotes
// the new path even through the shared lookupOrReuse path.
func TestLookupOrReuse_DroppedPathPromotionSurvivesReuse(t *testing.T) {
	const ino = uint64(0x777)
	client := &VFSClient{}

	first := testOwnerNode(client, "/ws/E")
	client.lookupOrReuse(ino, "/ws/E", sameEmbedder(first))

	// The only known alias is unlinked on the host while the inode stays alive
	// through an open handle; no alternate is known yet.
	first.dropLinkPath("/ws/E")
	assert.True(t, first.dropped)

	// First discovery of the surviving alias F promotes F to the cached path.
	client.lookupOrReuse(ino, "/ws/F", func() (*VFSNode, *fs.Inode) {
		t.Fatal("dropped-but-alive canonical node must be reused, not re-created")
		return nil, nil
	})
	assert.Equal(t, "/ws/F", first.currentPath())
	assert.False(t, first.dropped)
}

// TestLookupOrReuse_ZeroInodeOrNilClient_FailsClosed
func TestLookupOrReuse_ZeroInodeOrNilClient_FailsClosed(t *testing.T) {
	client := &VFSClient{}
	create := func() (*VFSNode, *fs.Inode) {
		t.Fatal("create must not run for a degenerate lookup")
		return nil, nil
	}
	assert.Nil(t, client.lookupOrReuse(0, "/ws/A", create))
	var nilClient *VFSClient
	assert.Nil(t, nilClient.lookupOrReuse(5, "/ws/A", create))
}
