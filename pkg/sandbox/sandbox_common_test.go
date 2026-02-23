package sandbox

import (
	"testing"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/policy"
	"github.com/jingkaihe/matchlock/pkg/vfs"
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

	providers := buildVFSProviders(config)
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

	providers := buildVFSProviders(config)
	router := vfs.NewMountRouter(providers)
	_, err := router.Stat("/workspace")
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

	providers := buildVFSProviders(config)
	require.Len(t, providers, 1)
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
