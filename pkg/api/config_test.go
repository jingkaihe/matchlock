package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestComposeCommand_NilImageConfig(t *testing.T) {
	var ic *ImageConfig
	got := ic.ComposeCommand([]string{"echo", "hello"})
	assert.Equal(t, []string{"echo", "hello"}, got)
}

func TestComposeCommand_EntrypointOnly(t *testing.T) {
	ic := &ImageConfig{Entrypoint: []string{"python3"}}
	got := ic.ComposeCommand(nil)
	assert.Equal(t, []string{"python3"}, got)
}

func TestComposeCommand_CmdOnly(t *testing.T) {
	ic := &ImageConfig{Cmd: []string{"sh"}}
	got := ic.ComposeCommand(nil)
	assert.Equal(t, []string{"sh"}, got)
}

func TestComposeCommand_EntrypointAndCmd(t *testing.T) {
	ic := &ImageConfig{
		Entrypoint: []string{"python3"},
		Cmd:        []string{"-c", "print('hi')"},
	}
	got := ic.ComposeCommand(nil)
	assert.Equal(t, []string{"python3", "-c", "print('hi')"}, got)
}

func TestComposeCommand_UserArgsReplaceCmd(t *testing.T) {
	ic := &ImageConfig{
		Entrypoint: []string{"python3"},
		Cmd:        []string{"-c", "print('hi')"},
	}
	got := ic.ComposeCommand([]string{"script.py"})
	assert.Equal(t, []string{"python3", "script.py"}, got)
}

func TestComposeCommand_UserArgsNoCmdNoEntrypoint(t *testing.T) {
	ic := &ImageConfig{}
	got := ic.ComposeCommand([]string{"echo", "hello"})
	assert.Equal(t, []string{"echo", "hello"}, got)
}

func TestComposeCommand_EmptyEntrypointAndCmd(t *testing.T) {
	ic := &ImageConfig{}
	got := ic.ComposeCommand(nil)
	assert.Nil(t, got)
}

func TestComposeCommand_NoMutation(t *testing.T) {
	ic := &ImageConfig{
		Entrypoint: []string{"python3"},
		Cmd:        []string{"-c", "print('hi')"},
	}

	_ = ic.ComposeCommand([]string{"script.py"})
	_ = ic.ComposeCommand([]string{"other.py"})

	assert.Equal(t, []string{"python3"}, ic.Entrypoint)
	assert.Equal(t, []string{"-c", "print('hi')"}, ic.Cmd)
}

func TestComposeCommand_RepeatedCallsConsistent(t *testing.T) {
	ic := &ImageConfig{
		Entrypoint: []string{"python3"},
		Cmd:        []string{"app.py"},
	}

	for i := 0; i < 10; i++ {
		got := ic.ComposeCommand(nil)
		assert.Equal(t, []string{"python3", "app.py"}, got)
	}
}

func TestGetHostname_UsesConfiguredNetworkHostname(t *testing.T) {
	cfg := &Config{
		Network: &NetworkConfig{Hostname: "override.internal"},
	}

	assert.Equal(t, "override.internal", cfg.GetHostname())
}

func TestGetHostname_NilNetworkFallsBackToGeneratedID(t *testing.T) {
	cfg := &Config{}

	hostname := cfg.GetHostname()
	assert.Regexp(t, `^vm-[0-9a-f]{8}$`, hostname)
	assert.Equal(t, hostname, cfg.ID)
}

func TestNetworkConfigValidateNoNetworkWithAllowedHosts(t *testing.T) {
	cfg := &NetworkConfig{
		NoNetwork:    true,
		AllowedHosts: []string{"api.openai.com"},
	}

	err := cfg.Validate()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidConfig)
	assert.Contains(t, err.Error(), "network.no_network")
}

func TestNetworkConfigValidateNoNetworkWithSecrets(t *testing.T) {
	cfg := &NetworkConfig{
		NoNetwork: true,
		Secrets: map[string]Secret{
			"API_KEY": {Value: "secret", Hosts: []string{"api.openai.com"}},
		},
	}

	err := cfg.Validate()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidConfig)
	assert.Contains(t, err.Error(), "network.no_network")
}

func TestNetworkConfigValidateNoNetworkOnly(t *testing.T) {
	cfg := &NetworkConfig{NoNetwork: true}
	require.NoError(t, cfg.Validate())
}

func TestDefaultConfig_VFSDisabledByDefault(t *testing.T) {
	cfg := DefaultConfig()
	require.Nil(t, cfg.VFS)
	assert.False(t, cfg.HasVFSMounts())
	assert.Equal(t, "", cfg.GetWorkspace())
}

func TestValidateVFS_RejectsWorkspaceWithoutMounts(t *testing.T) {
	cfg := &Config{
		VFS: &VFSConfig{
			Workspace: "/workspace",
		},
	}

	err := cfg.ValidateVFS()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidConfig)
	assert.Contains(t, err.Error(), "requires at least one")
}

func TestValidateVFS_RejectsMountsWithoutWorkspace(t *testing.T) {
	cfg := &Config{
		VFS: &VFSConfig{
			Mounts: map[string]MountConfig{
				"/workspace/data": {Type: MountTypeMemory},
			},
		},
	}

	err := cfg.ValidateVFS()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidConfig)
	assert.Contains(t, err.Error(), "vfs.workspace is required")
}

func TestValidateVFS_RejectsInterceptionWithoutMounts(t *testing.T) {
	cfg := &Config{
		VFS: &VFSConfig{
			Interception: &VFSInterceptionConfig{
				EmitEvents: true,
			},
		},
	}

	err := cfg.ValidateVFS()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidConfig)
	assert.Contains(t, err.Error(), "vfs.interception requires at least one")
}

func TestValidateVFS_RejectsMountOutsideWorkspace(t *testing.T) {
	cfg := &Config{
		VFS: &VFSConfig{
			Workspace: "/workspace/project",
			Mounts: map[string]MountConfig{
				"/workspace": {Type: MountTypeMemory},
			},
		},
	}

	err := cfg.ValidateVFS()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidConfig)
	assert.Contains(t, err.Error(), "must be within workspace")
}

func TestValidateVFS_AllowsValidWorkspaceMounts(t *testing.T) {
	cfg := &Config{
		VFS: &VFSConfig{
			Workspace: "/workspace/project",
			Mounts: map[string]MountConfig{
				"/workspace/project/data": {Type: MountTypeMemory},
			},
		},
	}

	require.NoError(t, cfg.ValidateVFS())
	assert.True(t, cfg.HasVFSMounts())
	assert.Equal(t, "/workspace/project", cfg.GetWorkspace())
}

func TestValidateVFS_AllowsInterceptionWithMounts(t *testing.T) {
	cfg := &Config{
		VFS: &VFSConfig{
			Workspace: "/workspace",
			Mounts: map[string]MountConfig{
				"/workspace/data": {Type: MountTypeMemory},
			},
			Interception: &VFSInterceptionConfig{
				EmitEvents: true,
			},
		},
	}

	require.NoError(t, cfg.ValidateVFS())
}
