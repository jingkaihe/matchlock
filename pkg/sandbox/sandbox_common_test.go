package sandbox

import (
	"testing"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/policy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
		ImageCfg: &api.ImageConfig{
			WorkingDir: "/app",
		},
	}

	opts := prepareExecEnv(config, nil, nil)

	require.Equal(t, "/app", opts.WorkingDir)
}

func TestPrepareExecEnv_DefaultWorkingDirEmptyWithoutImageWorkdir(t *testing.T) {
	config := &api.Config{
		ImageCfg: &api.ImageConfig{
			WorkingDir: "",
		},
	}

	opts := prepareExecEnv(config, nil, nil)

	require.Empty(t, opts.WorkingDir)
}

func TestPrepareExecEnv_DefaultWorkingDirEmptyWithoutImage(t *testing.T) {
	config := &api.Config{}
	opts := prepareExecEnv(config, nil, nil)
	require.Equal(t, "", opts.WorkingDir)
}

func TestPrepareExecEnv_SecretPlaceholderOverridesConfigEnv(t *testing.T) {
	config := &api.Config{
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
