package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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

	id := "vm-test123"
	require.NoError(t, mgr.Register(id, map[string]string{"image": "alpine:latest"}))
	require.NoError(t, mgr.Unregister(id))
	require.NoError(t, mgr.Remove(id))

	_, err := mgr.Get(id)
	require.Error(t, err)
	_, err = os.Stat(filepath.Join(dir, id))
	require.True(t, os.IsNotExist(err))
}

func TestRemoveRunningVM(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManagerWithDir(dir)

	id := "vm-running1"
	require.NoError(t, mgr.Register(id, map[string]string{"image": "alpine:latest"}))

	err := mgr.Remove(id)
	require.Error(t, err)
}

func TestRemoveRunningVMWithDeadProcess(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManagerWithDir(dir)

	id := "vm-dead1"
	require.NoError(t, mgr.Register(id, map[string]string{"image": "alpine:latest"}))

	db, err := openStateDB(dir)
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`UPDATE vms SET status = ?, pid = ?, updated_at = ? WHERE id = ?`, "running", 999999999, time.Now().UTC().Format(time.RFC3339Nano), id)
	require.NoError(t, err)

	err = mgr.Remove(id)
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, id))
	require.True(t, os.IsNotExist(err), "expected VM directory to be removed")
}

func TestListMarksDeadRunningVMWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManagerWithDir(dir)

	id := "vm-list-dead1"
	require.NoError(t, mgr.Register(id, map[string]string{"image": "alpine:latest"}))

	db, err := openStateDB(dir)
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`UPDATE vms SET status = ?, pid = ?, updated_at = ? WHERE id = ?`, "running", 999999999, time.Now().UTC().Format(time.RFC3339Nano), id)
	require.NoError(t, err)

	done := make(chan struct{})
	var states []VMState
	var listErr error
	go func() {
		states, listErr = mgr.List()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("List() timed out while handling stale running VM")
	}

	require.NoError(t, listErr)
	found := false
	for _, s := range states {
		if s.ID == id {
			found = true
			assert.Equal(t, "crashed", s.Status)
		}
	}
	require.True(t, found)

	state, err := mgr.Get(id)
	require.NoError(t, err)
	assert.Equal(t, "crashed", state.Status)
}

// TestUnregisterThenRemove_RmFlag is a regression test for
// https://github.com/jingkaihe/matchlock/issues/12
// When --rm is set, the VM state directory must be fully removed after Close().
func TestUnregisterThenRemove_RmFlag(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManagerWithDir(dir)

	id := "vm-rm-test"
	require.NoError(t, mgr.Register(id, map[string]string{"image": "alpine:latest"}))

	states, err := mgr.List()
	require.NoError(t, err)
	found := false
	for _, s := range states {
		if s.ID == id {
			found = true
			assert.Equal(t, "running", s.Status)
		}
	}
	require.True(t, found)

	require.NoError(t, mgr.Unregister(id))
	state, err := mgr.Get(id)
	require.NoError(t, err)
	require.Equal(t, "stopped", state.Status)

	require.NoError(t, mgr.Remove(id))

	states, err = mgr.List()
	require.NoError(t, err)
	for _, s := range states {
		assert.NotEqual(t, id, s.ID)
	}
	_, err = os.Stat(filepath.Join(dir, id))
	require.True(t, os.IsNotExist(err))
}

// TestUnregisterWithoutRemove_NoRmFlag verifies that when --rm is NOT set,
// Unregister alone leaves the state directory intact so the VM remains visible.
func TestUnregisterWithoutRemove_NoRmFlag(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManagerWithDir(dir)

	id := "vm-no-rm-test"
	require.NoError(t, mgr.Register(id, map[string]string{"image": "alpine:latest"}))
	require.NoError(t, mgr.Unregister(id))

	states, err := mgr.List()
	require.NoError(t, err)
	found := false
	for _, s := range states {
		if s.ID == id {
			found = true
			require.Equal(t, "stopped", s.Status)
		}
	}
	require.True(t, found)

	_, err = os.Stat(filepath.Join(dir, id))
	require.False(t, os.IsNotExist(err))
}
