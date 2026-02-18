package sandbox

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/stretchr/testify/require"
)

func TestPrepareOverlaySnapshotsCopiesDirectoryMount(t *testing.T) {
	stateDir := t.TempDir()
	hostDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(hostDir, "file.txt"), []byte("from-host"), 0644))

	cfg := &api.Config{
		VFS: &api.VFSConfig{
			Mounts: map[string]api.MountConfig{
				"/workspace/data": {Type: api.MountTypeOverlay, HostPath: hostDir},
			},
		},
	}

	snapshots, err := prepareOverlaySnapshots(cfg, stateDir)
	require.NoError(t, err)
	require.Len(t, snapshots, 1)

	mount := cfg.VFS.Mounts["/workspace/data"]
	require.Equal(t, api.MountTypeHostFS, mount.Type)
	require.Equal(t, snapshots[0], mount.HostPath)
	require.False(t, mount.Readonly)

	content, err := os.ReadFile(filepath.Join(mount.HostPath, "file.txt"))
	require.NoError(t, err)
	require.Equal(t, "from-host", string(content))

	// Snapshot must be isolated from host updates after preparation.
	require.NoError(t, os.WriteFile(filepath.Join(hostDir, "file.txt"), []byte("host-updated"), 0644))
	content, err = os.ReadFile(filepath.Join(mount.HostPath, "file.txt"))
	require.NoError(t, err)
	require.Equal(t, "from-host", string(content))
}

func TestPrepareOverlaySnapshotsCopiesSingleFileMount(t *testing.T) {
	stateDir := t.TempDir()
	hostDir := t.TempDir()
	hostFile := filepath.Join(hostDir, "single.txt")
	require.NoError(t, os.WriteFile(hostFile, []byte("single-file"), 0644))

	cfg := &api.Config{
		VFS: &api.VFSConfig{
			Mounts: map[string]api.MountConfig{
				"/workspace/single.txt": {Type: api.MountTypeOverlay, HostPath: hostFile},
			},
		},
	}

	snapshots, err := prepareOverlaySnapshots(cfg, stateDir)
	require.NoError(t, err)
	require.Len(t, snapshots, 1)

	mount := cfg.VFS.Mounts["/workspace/single.txt"]
	require.Equal(t, api.MountTypeHostFS, mount.Type)
	require.Equal(t, snapshots[0], mount.HostPath)

	content, err := os.ReadFile(mount.HostPath)
	require.NoError(t, err)
	require.Equal(t, "single-file", string(content))
}

func TestPrepareOverlaySnapshotsNoOverlayMounts(t *testing.T) {
	stateDir := t.TempDir()
	hostDir := t.TempDir()

	cfg := &api.Config{
		VFS: &api.VFSConfig{
			Mounts: map[string]api.MountConfig{
				"/workspace/data": {Type: api.MountTypeHostFS, HostPath: hostDir},
			},
		},
	}

	snapshots, err := prepareOverlaySnapshots(cfg, stateDir)
	require.NoError(t, err)
	require.Empty(t, snapshots)
}

func TestPrepareOverlaySnapshotsRejectsOverlayWithoutHostPath(t *testing.T) {
	stateDir := t.TempDir()
	cfg := &api.Config{
		VFS: &api.VFSConfig{
			Mounts: map[string]api.MountConfig{
				"/workspace/data": {Type: api.MountTypeOverlay},
			},
		},
	}

	_, err := prepareOverlaySnapshots(cfg, stateDir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "host_path is required")
}

func TestPrepareOverlaySnapshotsRejectsUpperLowerOverlayConfig(t *testing.T) {
	stateDir := t.TempDir()
	hostDir := t.TempDir()
	cfg := &api.Config{
		VFS: &api.VFSConfig{
			Mounts: map[string]api.MountConfig{
				"/workspace/data": {
					Type:     api.MountTypeOverlay,
					HostPath: hostDir,
					Upper:    &api.MountConfig{Type: api.MountTypeMemory},
					Lower:    &api.MountConfig{Type: api.MountTypeHostFS, HostPath: hostDir},
				},
			},
		},
	}

	_, err := prepareOverlaySnapshots(cfg, stateDir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "upper/lower overlay config is not supported")
}
