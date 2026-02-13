package state

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemoveNonExistentVM(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManagerWithDir(dir)

	err := mgr.Remove("vm-nonexistent")
	require.Error(t, err)
}

func TestRemoveStoppedVM(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManagerWithDir(dir)

	vmDir := filepath.Join(dir, "vm-test123")
	os.MkdirAll(vmDir, 0700)
	os.WriteFile(filepath.Join(vmDir, "status"), []byte("stopped"), 0600)

	err := mgr.Remove("vm-test123")
	require.NoError(t, err)

	_, err = os.Stat(vmDir)
	require.True(t, os.IsNotExist(err), "expected VM directory to be removed")
}

func TestRemoveRunningVM(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManagerWithDir(dir)

	vmDir := filepath.Join(dir, "vm-running1")
	os.MkdirAll(vmDir, 0700)
	os.WriteFile(filepath.Join(vmDir, "status"), []byte("running"), 0600)
	os.WriteFile(filepath.Join(vmDir, "pid"), []byte(strconv.Itoa(os.Getpid())), 0600)

	err := mgr.Remove("vm-running1")
	require.Error(t, err)
}

func TestRemoveRunningVMWithDeadProcess(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManagerWithDir(dir)

	vmDir := filepath.Join(dir, "vm-dead1")
	os.MkdirAll(vmDir, 0700)
	os.WriteFile(filepath.Join(vmDir, "status"), []byte("running"), 0600)
	os.WriteFile(filepath.Join(vmDir, "pid"), []byte("999999999"), 0600)

	err := mgr.Remove("vm-dead1")
	require.NoError(t, err)

	_, err = os.Stat(vmDir)
	require.True(t, os.IsNotExist(err), "expected VM directory to be removed")
}

// TestUnregisterThenRemove_RmFlag is a regression test for
// https://github.com/jingkaihe/matchlock/issues/12
// When --rm is set, the VM state directory must be fully removed after Close().
// Previously, Close() only called Unregister() (marking status as "stopped")
// without calling Remove(), leaving stale state behind.
func TestUnregisterThenRemove_RmFlag(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManagerWithDir(dir)

	id := "vm-rm-test"

	err := mgr.Register(id, map[string]string{"image": "alpine:latest"})
	require.NoError(t, err)

	states, err := mgr.List()
	require.NoError(t, err)
	found := false
	for _, s := range states {
		if s.ID == id {
			found = true
			assert.Contains(t, []string{"running", "crashed"}, s.Status, "expected status running or crashed")
		}
	}
	require.True(t, found, "expected VM to be listed after Register")

	// Simulate what sb.Close() does
	err = mgr.Unregister(id)
	require.NoError(t, err)

	// After Unregister, the VM should still exist in state (as "stopped")
	state, err := mgr.Get(id)
	require.NoError(t, err)
	require.Equal(t, "stopped", state.Status)

	// Simulate what --rm should do: call Remove() after Close()
	err = mgr.Remove(id)
	require.NoError(t, err)

	// After Remove, the VM should not be listed
	states, err = mgr.List()
	require.NoError(t, err)
	for _, s := range states {
		assert.NotEqual(t, id, s.ID, "VM should not appear in list after Remove")
	}

	// The state directory should be gone
	vmDir := filepath.Join(dir, id)
	_, err = os.Stat(vmDir)
	require.True(t, os.IsNotExist(err), "expected VM directory to be fully removed after Unregister + Remove")
}

// TestUnregisterWithoutRemove_NoRmFlag verifies that when --rm is NOT set,
// Unregister alone leaves the state directory intact so the VM remains visible.
func TestUnregisterWithoutRemove_NoRmFlag(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManagerWithDir(dir)

	id := "vm-no-rm-test"

	err := mgr.Register(id, map[string]string{"image": "alpine:latest"})
	require.NoError(t, err)

	err = mgr.Unregister(id)
	require.NoError(t, err)

	// Without Remove, VM should still be listed as stopped
	states, err := mgr.List()
	require.NoError(t, err)
	found := false
	for _, s := range states {
		if s.ID == id {
			found = true
			require.Equal(t, "stopped", s.Status)
		}
	}
	require.True(t, found, "VM should still be listed after Unregister without Remove")

	vmDir := filepath.Join(dir, id)
	_, err = os.Stat(vmDir)
	require.False(t, os.IsNotExist(err), "expected VM directory to persist after Unregister without Remove")
}
