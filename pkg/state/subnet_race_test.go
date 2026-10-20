package state

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// allocBaseDir returns a test-scoped allocator base directory. stateDBPath puts
// the DB at the PARENT of baseDir (see db.go), so passing an inner subdir keeps
// the DB at <tmp>/state.db — inside the auto-cleaned temp dir — rather than
// leaking to its parent and being reused across runs.
func allocBaseDir(t *testing.T) string {
	return filepath.Join(t.TempDir(), "subnets")
}

// TestAllocateRetriesOnSameBaseDir exercises the cross-process subnet-allocation
// model: several goroutines, each with its OWN allocator (own process-local mutex
// and own *sql.DB handle) but a SHARED state.db, allocate distinct VMIDs
// concurrently. The only cross-goroutine synchronization is the SQLite UNIQUE
// index on octet, so a genuine race is induced; the retry-on-UNIQUE-violation
// logic must salvage every allocation into a distinct, valid octet.
func TestAllocateRetriesOnSameBaseDir(t *testing.T) {
	dir := allocBaseDir(t)
	const n = 6

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results = make([]*SubnetInfo, n)
		errs    = make([]error, n)
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			a := NewSubnetAllocatorWithDir(dir) // same state.db as siblings
			if err := a.ready(); err != nil {
				errs[idx] = err
				return
			}
			defer a.db.Close()
			info, err := a.Allocate(fmt.Sprintf("vm-%d-%d", n, idx))
			if err != nil {
				errs[idx] = err
				return
			}
			mu.Lock()
			results[idx] = info
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "alloc %d", i)
		require.NotNil(t, results[i])
	}
	seen := map[int]string{}
	for _, info := range results {
		require.GreaterOrEqual(t, info.Octet, 100)
		require.LessOrEqual(t, info.Octet, 254)
		if prior, ok := seen[info.Octet]; ok {
			t.Fatalf("octet %d assigned to both %s and %s — collision not salvaged", info.Octet, prior, info.VMID)
		}
		seen[info.Octet] = info.VMID
	}
	require.Len(t, seen, n)
}

// TestAllocateDeterministicUniqueCollision PROVES the retry branch executes. A's
// allocate hook, after A's scan selects the first free octet (100), synchronously
// runs B's plain Allocate (no seam). B's scan sees 100 still free (A hasn't
// inserted yet), so B inserts 100. Then A resumes and INSERTs 100, which MUST hit
// the UNIQUE index and be salvaged by the retry to a DIFFERENT octet. If the retry
// logic were absent, B (or A) would surface the UNIQUE error and the test fails.
func TestAllocateDeterministicUniqueCollision(t *testing.T) {
	dir := allocBaseDir(t)
	a := NewSubnetAllocatorWithDir(dir)
	require.NoError(t, a.ready())
	defer a.db.Close()
	b := NewSubnetAllocatorWithDir(dir)
	require.NoError(t, b.ready())
	defer b.db.Close()

	var bInfo *SubnetInfo
	var hookRan bool
	hookA := func(_ int) {
		if hookRan {
			return // ignore A's retry attempt; B already grabbed the candidate
		}
		hookRan = true
		// B's plain Allocate scans, sees the SAME first-free octet A picked
		// (A hasn't inserted yet), and inserts it — a genuine UNIQUE collision
		// for A's upcoming INSERT.
		bi, err := b.Allocate("vm-deterministic-b")
		require.NoError(t, err, "B must allocate cleanly")
		bInfo = bi
	}

	// Synchronous: A's allocate scans, hook runs B inline (B takes the candidate),
	// then A tries to INSERT the same octet -> UNIQUE -> retry salvages a different one.
	ra, err := a.allocate("vm-deterministic-a", hookA)
	require.NoError(t, err, "A must be salvaged by retry, not surface UNIQUE")
	require.NotNil(t, ra)
	require.NotNil(t, bInfo)

	// A and B must have gotten DIFFERENT octets (the UNIQUE index forbids sharing).
	require.NotEqual(t, ra.Octet, bInfo.Octet,
		"A and B must not share an octet: A=%d B=%d", ra.Octet, bInfo.Octet)
	require.GreaterOrEqual(t, ra.Octet, 100)
	require.LessOrEqual(t, ra.Octet, 254)
	require.GreaterOrEqual(t, bInfo.Octet, 100)
	require.LessOrEqual(t, bInfo.Octet, 254)

	// The final table has exactly two live allocations.
	used, err := a.usedOctets()
	require.NoError(t, err)
	live := 0
	for o := 100; o <= 254; o++ {
		if used[o] {
			live++
		}
	}
	require.Equal(t, 2, live, "expected exactly two leased octets")
}
