package sandbox

import (
	"path/filepath"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/vfs"
	"github.com/stretchr/testify/require"
)

func TestBuildVFSProvidersAddsWorkspaceRootForNestedMounts(t *testing.T) {
	workspace := "/workspace"
	config := &api.Config{
		VFS: &api.VFSConfig{
			Mounts: map[string]api.MountConfig{
				"/workspace/not_exist_folder": {Type: "memory"},
			},
		},
	}

	providers := buildVFSProviders(config, workspace)
	_, ok := providers[workspace]
	require.True(t, ok, "expected workspace mount %q to exist", workspace)
	_, ok = providers["/workspace/not_exist_folder"]
	require.True(t, ok, "expected nested mount to exist")

	router := vfs.NewMountRouter(providers)
	_, err := router.Stat(workspace)
	require.NoError(t, err, "expected workspace root to resolve")
}

func TestBuildVFSProvidersKeepsExplicitWorkspaceMount(t *testing.T) {
	workspace := "/workspace"
	config := &api.Config{
		VFS: &api.VFSConfig{
			Mounts: map[string]api.MountConfig{
				workspace: {Type: "memory"},
			},
		},
	}

	providers := buildVFSProviders(config, workspace)
	require.Len(t, providers, 1)
}

func TestBuildVFSProvidersDoesNotDuplicateCanonicalWorkspaceMount(t *testing.T) {
	workspace := "/workspace"
	config := &api.Config{
		VFS: &api.VFSConfig{
			Mounts: map[string]api.MountConfig{
				"/workspace/": {Type: "memory"},
			},
		},
	}

	providers := buildVFSProviders(config, workspace)

	var workspaceMounts int
	for path := range providers {
		if filepath.Clean(path) == workspace {
			workspaceMounts++
		}
	}

	require.Equal(t, 1, workspaceMounts, "expected exactly one canonical workspace mount (providers=%d)", len(providers))
}
