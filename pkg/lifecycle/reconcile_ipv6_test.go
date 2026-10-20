//go:build linux

package lifecycle

import (
	"path/filepath"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/state"
	"github.com/stretchr/testify/require"
)

// TestNFTTableCandidatesCoverBothFamiliesForEachTapCandidate pins the reconcile
// scope: for every TAP-name candidate the IPv4 table, the IPv6 table and the NAT
// table are in scope, plus whatever the record itself names. Nothing else is
// ever a candidate, so a reconcile can never touch the shared host tables.
func TestNFTTableCandidatesCoverBothFamiliesForEachTapCandidate(t *testing.T) {
	rec := &Record{
		VMID: "vm-abc12345",
		Resources: Resources{
			TAPName:         "fc-abc12345",
			FirewallTable:   "matchlock_fc-abc12345",
			FirewallTableV6: "matchlock6_fc-abc12345",
			NATTable:        "matchlock_nat_fc-abc12345",
		},
	}

	got := nftTableCandidates(rec)
	require.Contains(t, got, "matchlock_fc-abc12345")
	require.Contains(t, got, "matchlock6_fc-abc12345")
	require.Contains(t, got, "matchlock_nat_fc-abc12345")
	// Both families are derived for the vm-id TAP candidate as well.
	require.Contains(t, got, "matchlock6_fc-vm-abc12")

	seen := make(map[string]int, len(got))
	for _, name := range got {
		seen[name]++
		require.Contains(t, name, "matchlock", "only matchlock-owned tables are ever deleted")
	}
	for name, count := range seen {
		require.Equal(t, 1, count, "duplicate table candidate %q", name)
	}
	require.NotContains(t, got, "filter")
	require.NotContains(t, got, "nat")
	require.NotContains(t, got, "DOCKER-USER")
}

// TestNFTTableCandidatesTolerateRecordsWithoutIPv6 pins the legacy shapes: a
// record written before the ip6 table was recorded (or one that only names it)
// must still yield the per-TAP IPv6 table, so an orphaned ip6 table left by an
// interrupted create is always in scope. Deleting a table that does not exist is
// a no-op, so naming it unconditionally is safe.
func TestNFTTableCandidatesTolerateRecordsWithoutIPv6(t *testing.T) {
	t.Run("pre-IPv6 record", func(t *testing.T) {
		rec := &Record{VMID: "vm-old12345", Resources: Resources{TAPName: "fc-old12345", FirewallTable: "matchlock_fc-old12345"}}
		got := nftTableCandidates(rec)
		require.Contains(t, got, "matchlock_fc-old12345")
		require.Contains(t, got, "matchlock6_fc-old12345")
	})

	t.Run("record naming only the ip6 table", func(t *testing.T) {
		rec := &Record{VMID: "vm-v6only1", Resources: Resources{FirewallTableV6: "matchlock6_fc-dead"}}
		require.Contains(t, nftTableCandidates(rec), "matchlock6_fc-dead")
	})
}

// TestReconcileRemovesOrphanedIPv6Table drives a reconcile of a stopped VM and
// asserts the per-TAP ip6 table (matchlock6_<tap>) is deleted alongside the
// IPv4 one, using the deleteTable seam so the host ruleset is untouched.
func TestReconcileRemovesOrphanedIPv6Table(t *testing.T) {
	injectReconcileSeams(t)
	prev := deleteTable
	var deleted []string
	deleteTable = func(name string) error {
		deleted = append(deleted, name)
		return nil
	}
	t.Cleanup(func() { deleteTable = prev })

	vmDir := t.TempDir()
	stateMgr := state.NewManagerWithDir(vmDir)
	subnetAlloc := state.NewSubnetAllocatorWithDir(filepath.Join(t.TempDir(), "subnets"))
	reconciler := NewReconcilerWithManagers(stateMgr, subnetAlloc)

	vmID := "vm-recon123"
	require.NoError(t, stateMgr.Register(vmID, map[string]string{"image": "alpine:latest"}))
	require.NoError(t, stateMgr.Unregister(vmID))

	store := NewStore(stateMgr.Dir(vmID))
	require.NoError(t, store.Init(vmID, "firecracker", stateMgr.Dir(vmID)))
	require.NoError(t, store.SetResource(func(r *Resources) {
		r.TAPName = "fc-recon123"
		r.FirewallTable = "matchlock_fc-recon123"
		r.FirewallTableV6 = "matchlock6_fc-recon123"
		r.NATTable = "matchlock_nat_fc-recon123"
	}))
	require.NoError(t, store.SetPhase(PhaseCreated))
	require.NoError(t, store.SetPhase(PhaseStopping))
	require.NoError(t, store.SetPhase(PhaseStopped))

	report, err := reconciler.ReconcileVM(vmID, false)
	require.NoError(t, err)

	require.Contains(t, deleted, "matchlock6_fc-recon123", "the orphaned ip6 table must be removed")
	require.Contains(t, deleted, "matchlock_fc-recon123")
	require.Contains(t, report.Cleaned, "nft_table_delete:matchlock6_fc-recon123")

	// The ip6 table name survives the record round trip (this is what a later
	// reconcile of the same VM reads).
	rec, err := store.Load()
	require.NoError(t, err)
	require.Equal(t, "matchlock6_fc-recon123", rec.Resources.FirewallTableV6)
}
