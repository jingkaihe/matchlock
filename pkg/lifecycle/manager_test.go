package lifecycle

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/state"
	"github.com/stretchr/testify/require"
)

func TestVMManagerPrune(t *testing.T) {
	vmDir := t.TempDir()
	subnetDir := filepath.Join(t.TempDir(), "subnets")

	stateMgr := state.NewManagerWithDir(vmDir)
	subnetAlloc := state.NewSubnetAllocatorWithDir(subnetDir)
	mgr := NewVMManagerWithDeps(stateMgr, subnetAlloc)
	// Stub the nftables seam so the reconciler's DOCKER-USER sweep does not
	// depend on a real netlink connection in this environment.
	injectReconcileSeams(t)

	stoppedID := "vm-stopped1"
	require.NoError(t, stateMgr.Register(stoppedID, map[string]string{"image": "alpine:latest"}))
	require.NoError(t, stateMgr.Unregister(stoppedID))

	runningID := "vm-running1"
	require.NoError(t, stateMgr.Register(runningID, map[string]string{"image": "alpine:latest"}))

	pruned, err := mgr.Prune()
	require.NoError(t, err)
	require.Contains(t, pruned, stoppedID)
	require.NotContains(t, pruned, runningID)

	_, err = stateMgr.Get(stoppedID)
	require.Error(t, err)
	_, err = stateMgr.Get(runningID)
	require.NoError(t, err)
}

func TestVMManagerRemove_ReconcileFailureKeepsState(t *testing.T) {
	vmDir := t.TempDir()
	subnetDir := filepath.Join(t.TempDir(), "subnets")

	stateMgr := state.NewManagerWithDir(vmDir)
	subnetAlloc := state.NewSubnetAllocatorWithDir(subnetDir)
	mgr := NewVMManagerWithDeps(stateMgr, subnetAlloc)
	injectReconcileSeams(t)

	vmID := "vm-stopped2"
	require.NoError(t, stateMgr.Register(vmID, map[string]string{"image": "alpine:latest"}))
	require.NoError(t, stateMgr.Unregister(vmID))

	badRootfsDir := filepath.Join(stateMgr.Dir(vmID), "bad-rootfs")
	require.NoError(t, os.MkdirAll(badRootfsDir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(badRootfsDir, "nested"), []byte("x"), 0600))

	store := NewStore(stateMgr.Dir(vmID))
	require.NoError(t, store.Init(vmID, "firecracker", stateMgr.Dir(vmID)))
	require.NoError(t, store.SetResource(func(r *Resources) {
		r.RootfsPath = badRootfsDir
	}))
	require.NoError(t, store.SetPhase(PhaseCreated))
	require.NoError(t, store.SetPhase(PhaseStopping))
	require.NoError(t, store.SetPhase(PhaseStopped))

	err := mgr.Remove(vmID)
	require.Error(t, err)

	_, err = stateMgr.Get(vmID)
	require.NoError(t, err)
}

func TestVMManagerPrune_ReconcileFailureDoesNotRemove(t *testing.T) {
	vmDir := t.TempDir()
	subnetDir := filepath.Join(t.TempDir(), "subnets")

	stateMgr := state.NewManagerWithDir(vmDir)
	subnetAlloc := state.NewSubnetAllocatorWithDir(subnetDir)
	mgr := NewVMManagerWithDeps(stateMgr, subnetAlloc)
	injectReconcileSeams(t)

	vmID := "vm-stopped3"
	require.NoError(t, stateMgr.Register(vmID, map[string]string{"image": "alpine:latest"}))
	require.NoError(t, stateMgr.Unregister(vmID))

	badRootfsDir := filepath.Join(stateMgr.Dir(vmID), "bad-rootfs")
	require.NoError(t, os.MkdirAll(badRootfsDir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(badRootfsDir, "nested"), []byte("x"), 0600))

	store := NewStore(stateMgr.Dir(vmID))
	require.NoError(t, store.Init(vmID, "firecracker", stateMgr.Dir(vmID)))
	require.NoError(t, store.SetResource(func(r *Resources) {
		r.RootfsPath = badRootfsDir
	}))
	require.NoError(t, store.SetPhase(PhaseCreated))
	require.NoError(t, store.SetPhase(PhaseStopping))
	require.NoError(t, store.SetPhase(PhaseStopped))

	pruned, err := mgr.Prune()
	require.Error(t, err)
	require.Empty(t, pruned)

	_, err = stateMgr.Get(vmID)
	require.NoError(t, err)
}
