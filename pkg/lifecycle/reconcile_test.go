package lifecycle

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/state"
	"github.com/stretchr/testify/require"
)

func TestReconcileVMStopped(t *testing.T) {
	vmDir := t.TempDir()
	subnetDir := filepath.Join(t.TempDir(), "subnets")

	stateMgr := state.NewManagerWithDir(vmDir)
	subnetAlloc := state.NewSubnetAllocatorWithDir(subnetDir)
	reconciler := NewReconcilerWithManagers(stateMgr, subnetAlloc)

	vmID := "vm-reconcile1"
	require.NoError(t, stateMgr.Register(vmID, map[string]string{"image": "alpine:latest"}))
	require.NoError(t, stateMgr.Unregister(vmID))
	_, err := subnetAlloc.Allocate(vmID)
	require.NoError(t, err)

	rootfsPath := filepath.Join(stateMgr.Dir(vmID), "upper.ext4")
	require.NoError(t, os.WriteFile(rootfsPath, []byte("dummy"), 0600))

	store := NewStore(stateMgr.Dir(vmID))
	require.NoError(t, store.Init(vmID, "firecracker", stateMgr.Dir(vmID)))
	require.NoError(t, store.SetResource(func(r *Resources) {
		r.RootfsPath = rootfsPath
		r.SubnetFile = subnetAlloc.AllocationPath(vmID)
	}))
	require.NoError(t, store.SetPhase(PhaseCreated))
	require.NoError(t, store.SetPhase(PhaseStopping))
	require.NoError(t, store.SetPhase(PhaseStopped))

	report, err := reconciler.ReconcileVM(vmID, false)
	require.NoError(t, err)
	require.NotEmpty(t, report.Cleaned)

	_, err = os.Stat(rootfsPath)
	require.True(t, os.IsNotExist(err))
	_, err = os.Stat(subnetAlloc.AllocationPath(vmID))
	require.True(t, os.IsNotExist(err))

	rec, err := store.Load()
	require.NoError(t, err)
	require.Equal(t, PhaseCleaned, rec.Phase)
}

func TestReconcileVMRunningSkipped(t *testing.T) {
	vmDir := t.TempDir()
	subnetDir := filepath.Join(t.TempDir(), "subnets")

	stateMgr := state.NewManagerWithDir(vmDir)
	subnetAlloc := state.NewSubnetAllocatorWithDir(subnetDir)
	reconciler := NewReconcilerWithManagers(stateMgr, subnetAlloc)

	vmID := "vm-runningx"
	require.NoError(t, stateMgr.Register(vmID, map[string]string{"image": "alpine:latest"}))

	report, err := reconciler.ReconcileVM(vmID, false)
	require.Error(t, err)
	require.Equal(t, ErrVMRunning.Error(), report.Skipped)
}
