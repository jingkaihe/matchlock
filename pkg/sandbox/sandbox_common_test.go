package sandbox

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/policy"
	"github.com/jingkaihe/matchlock/pkg/vfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildVFSProvidersDoesNotAddWorkspaceRootForNestedMounts(t *testing.T) {
	config := &api.Config{
		VFS: &api.VFSConfig{
			Mounts: map[string]api.MountConfig{
				"/workspace/not_exist_folder": {Type: api.MountTypeMemory},
			},
		},
	}

	providers, err := buildVFSProviders(config)
	require.NoError(t, err)
	_, ok := providers["/workspace"]
	require.False(t, ok, "did not expect implicit workspace root mount")
	_, ok = providers["/workspace/not_exist_folder"]
	require.True(t, ok, "expected nested mount to exist")
}

func TestBuildVFSProvidersNestedMountStillExposesWorkspaceViaRouter(t *testing.T) {
	config := &api.Config{
		VFS: &api.VFSConfig{
			Mounts: map[string]api.MountConfig{
				"/workspace/not_exist_folder": {Type: api.MountTypeMemory},
			},
		},
	}

	providers, err := buildVFSProviders(config)
	require.NoError(t, err)
	router := vfs.NewMountRouter(providers)
	_, err = router.Stat("/workspace")
	require.NoError(t, err, "expected workspace root to resolve")
}

func TestBuildVFSProvidersKeepsExplicitWorkspaceMount(t *testing.T) {
	workspace := "/workspace"
	config := &api.Config{
		VFS: &api.VFSConfig{
			Mounts: map[string]api.MountConfig{
				workspace: {Type: api.MountTypeMemory},
			},
		},
	}

	providers, err := buildVFSProviders(config)
	require.NoError(t, err)
	require.Len(t, providers, 1)
}

func TestBuildVFSProvidersHostFSOwnerOverrideFlowsToProvider(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi"), 0644))

	uid := uint32(1000)
	gid := uint32(2000)
	config := &api.Config{
		VFS: &api.VFSConfig{
			Mounts: map[string]api.MountConfig{
				"/workspace": {Type: api.MountTypeHostFS, HostPath: dir, OwnerUID: &uid, OwnerGID: &gid},
			},
		},
	}

	providers, err := buildVFSProviders(config)
	require.NoError(t, err)

	fi, err := providers["/workspace"].Stat("hello.txt")
	require.NoError(t, err)
	assert.Equal(t, uint32(1000), fi.UID())
	assert.Equal(t, uint32(2000), fi.GID())
}

func TestBuildVFSProvidersRejectsOwnerOnMemoryMount(t *testing.T) {
	uid := uint32(1000)
	config := &api.Config{
		VFS: &api.VFSConfig{
			Mounts: map[string]api.MountConfig{
				"/workspace": {Type: api.MountTypeMemory, OwnerUID: &uid},
			},
		},
	}

	_, err := buildVFSProviders(config)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidMountConfig)
}

func TestBuildVFSProvidersRejectsOwnerOnOverlayMount(t *testing.T) {
	gid := uint32(2000)
	config := &api.Config{
		VFS: &api.VFSConfig{
			Mounts: map[string]api.MountConfig{
				"/workspace": {Type: api.MountTypeOverlay, OwnerGID: &gid},
			},
		},
	}

	_, err := buildVFSProviders(config)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidMountConfig)
}

// TestBuildVFSProvidersExactDestinationRoutesToProvider proves that an
// exact-destination config (an absolute guest path outside the workspace, e.g.
// /opt/project) is routed to its provider by the host MountRouter. This is the
// host-side half of the reachability story; the guest-side FUSE/bind mount is
// separately gated on real-VM acceptance.
func TestBuildVFSProvidersExactDestinationRoutesToProvider(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi"), 0644))

	config := &api.Config{
		VFS: &api.VFSConfig{
			ExactDestinations: true,
			Mounts: map[string]api.MountConfig{
				"/opt/project": {Type: api.MountTypeHostFS, HostPath: dir},
			},
		},
	}

	providers, err := buildVFSProviders(config)
	require.NoError(t, err)
	require.Contains(t, providers, "/opt/project")

	router := vfs.NewMountRouter(providers)
	fi, err := router.Stat("/opt/project")
	require.NoError(t, err, "exact destination /opt/project must resolve through the router")
	assert.True(t, fi.IsDir())

	h, err := router.Open("/opt/project/hello.txt", os.O_RDONLY, 0)
	require.NoError(t, err)
	defer h.Close()
	var b [16]byte
	n, _ := h.Read(b[:])
	assert.Equal(t, "hi", string(b[:n]))
}

func TestBuildExtraDiskConfigsCopiesOwner(t *testing.T) {
	uid := uint32(999)
	gid := uint32(999)

	disks, err := buildExtraDiskConfigs([]api.DiskMount{{
		HostPath:   "/tmp/pgdata.ext4",
		GuestMount: "/var/lib/postgresql",
		OwnerUID:   &uid,
		OwnerGID:   &gid,
	}})
	require.NoError(t, err)
	require.Len(t, disks, 1)
	require.NotNil(t, disks[0].OwnerUID)
	require.NotNil(t, disks[0].OwnerGID)
	assert.Equal(t, uid, *disks[0].OwnerUID)
	assert.Equal(t, gid, *disks[0].OwnerGID)
}

func TestBuildExtraDiskConfigsRejectsReadonlyOwner(t *testing.T) {
	uid := uint32(999)

	_, err := buildExtraDiskConfigs([]api.DiskMount{{
		HostPath:   "/tmp/pgdata.ext4",
		GuestMount: "/var/lib/postgresql",
		ReadOnly:   true,
		OwnerUID:   &uid,
	}})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidDiskCfg)
	assert.Contains(t, err.Error(), "writable disk")
}

func TestPrepareExecEnv_ConfigEnvOverridesImageEnv(t *testing.T) {
	config := &api.Config{
		VFS: &api.VFSConfig{Workspace: "/workspace"},
		ImageCfg: &api.ImageConfig{
			Env: map[string]string{
				"FOO": "from-image",
				"BAR": "from-image",
			},
		},
		Env: map[string]string{
			"FOO": "from-config",
		},
	}

	opts := prepareExecEnv(config, nil, nil)
	require.Equal(t, "from-config", opts.Env["FOO"])
	require.Equal(t, "from-image", opts.Env["BAR"])
}

func TestPrepareExecEnv_DefaultWorkingDirUsesImageWorkdir(t *testing.T) {
	config := &api.Config{
		VFS: &api.VFSConfig{Workspace: "/workspace/project"},
		ImageCfg: &api.ImageConfig{
			WorkingDir: "/app",
		},
	}

	opts := prepareExecEnv(config, nil, nil)

	require.Equal(t, "/app", opts.WorkingDir)
}

func TestPrepareExecEnv_DefaultWorkingDirFallsBackToWorkspace(t *testing.T) {
	config := &api.Config{
		VFS: &api.VFSConfig{Workspace: "/workspace/project"},
		ImageCfg: &api.ImageConfig{
			WorkingDir: "",
		},
	}

	opts := prepareExecEnv(config, nil, nil)

	require.Equal(t, "/workspace/project", opts.WorkingDir)
}

func TestPrepareExecEnv_DefaultWorkingDirEmptyWithoutImageOrWorkspace(t *testing.T) {
	config := &api.Config{}
	opts := prepareExecEnv(config, nil, nil)
	require.Equal(t, "", opts.WorkingDir)
}

func TestPrepareExecEnv_SecretPlaceholderOverridesConfigEnv(t *testing.T) {
	config := &api.Config{
		VFS: &api.VFSConfig{Workspace: "/workspace"},
		Env: map[string]string{
			"API_KEY": "not-secret",
		},
	}
	pol := policy.NewEngine(&api.NetworkConfig{
		Secrets: map[string]api.Secret{
			"API_KEY": {Value: "real-secret"},
		},
	})

	opts := prepareExecEnv(config, nil, pol)

	require.NotEmpty(t, opts.Env["API_KEY"])
	require.NotEqual(t, "not-secret", opts.Env["API_KEY"])
	require.Contains(t, opts.Env["API_KEY"], "SANDBOX_SECRET_")
}

func TestBuildSwapDiskConfigIsUnmountedSwapDevice(t *testing.T) {
	disk := buildSwapDiskConfig("/var/lib/matchlock/vms/vm-1/swap.raw")

	assert.Equal(t, "/var/lib/matchlock/vms/vm-1/swap.raw", disk.HostPath)
	assert.True(t, disk.Swap)
	assert.Empty(t, disk.GuestMount, "swap is attached but never mounted")
	assert.False(t, disk.ReadOnly, "swap must be attached read-write")
	assert.Nil(t, disk.OwnerUID)
	assert.Nil(t, disk.OwnerGID)
}

func TestProvisionSwapDiskCreatesImageAndReturnsSwapDisk(t *testing.T) {
	dir := t.TempDir()
	swapPath := filepath.Join(dir, "swap.raw")

	disk, err := provisionSwapDisk(swapPath, 2)
	require.NoError(t, err)

	assert.Equal(t, swapPath, disk.HostPath)
	assert.True(t, disk.Swap)
	assert.Empty(t, disk.GuestMount)

	fi, err := os.Stat(swapPath)
	require.NoError(t, err)
	assert.EqualValues(t, 2*1024*1024, fi.Size())
}

func TestProvisionSwapDiskWrapsCreateFailureAsCreateVM(t *testing.T) {
	// A path inside a non-existent directory cannot be created, so provisioning
	// fails. The error must be classified as a VM-create failure so the caller
	// can run its root-disk/state cleanup.
	swapPath := filepath.Join(t.TempDir(), "missing", "swap.raw")

	_, err := provisionSwapDisk(swapPath, 1)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCreateVM)
	_, statErr := os.Stat(swapPath)
	assert.True(t, os.IsNotExist(statErr), "no partial swap image may remain")
}
