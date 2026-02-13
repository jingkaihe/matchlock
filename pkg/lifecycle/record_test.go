package lifecycle

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	require.NoError(t, store.Init("vm-abc12345", "firecracker", dir))
	require.NoError(t, store.SetResource(func(r *Resources) {
		r.RootfsPath = filepath.Join(dir, "rootfs.ext4")
		r.TAPName = "fc-abc12345"
	}))
	require.NoError(t, store.SetPhase(PhaseCreated))
	require.NoError(t, store.SetPhase(PhaseStarting))
	require.NoError(t, store.SetPhase(PhaseRunning))
	require.NoError(t, store.MarkCleanup("tap_delete", errors.New("busy")))
	require.NoError(t, store.MarkCleanup("subnet_release", nil))

	rec, err := store.Load()
	require.NoError(t, err)
	require.Equal(t, "vm-abc12345", rec.VMID)
	require.Equal(t, "firecracker", rec.Backend)
	require.Equal(t, PhaseRunning, rec.Phase)
	require.Equal(t, "fc-abc12345", rec.Resources.TAPName)
	require.Equal(t, "error", rec.Cleanup["tap_delete"].Status)
	require.Equal(t, "ok", rec.Cleanup["subnet_release"].Status)

	require.NoError(t, store.SetLastError(errors.New("cleanup failed")))
	rec, err = store.Load()
	require.NoError(t, err)
	require.Equal(t, "cleanup failed", rec.LastError)

	require.NoError(t, store.SetLastError(nil))
	rec, err = store.Load()
	require.NoError(t, err)
	require.Equal(t, "", rec.LastError)
}

func TestStoreAppendOnlyVersions(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	require.NoError(t, store.Init("vm-append1", "firecracker", dir))
	require.NoError(t, store.SetPhase(PhaseCreated))
	require.NoError(t, store.SetPhase(PhaseStarting))
	require.NoError(t, store.SetPhase(PhaseRunning))
	require.NoError(t, store.MarkCleanup("tap_delete", errors.New("busy")))

	db, err := sql.Open("sqlite", store.Path())
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = db.Close()
	})

	rows, err := db.Query(`SELECT version FROM vm_lifecycle WHERE vm_id = ? ORDER BY version`, "vm-append1")
	require.NoError(t, err)
	defer rows.Close()

	var versions []int
	for rows.Next() {
		var version int
		require.NoError(t, rows.Scan(&version))
		versions = append(versions, version)
	}
	require.NoError(t, rows.Err())
	require.Len(t, versions, 5)
	for i, version := range versions {
		require.Equal(t, i+1, version)
	}

	rec, err := store.Load()
	require.NoError(t, err)
	require.Equal(t, 5, rec.Version)
	require.Equal(t, "error", rec.Cleanup["tap_delete"].Status)
}
