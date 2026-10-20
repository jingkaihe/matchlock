package api

import (
	"encoding/json"
	"math"
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

func TestConfigMerge_KernelOverridesDefault(t *testing.T) {
	base := DefaultConfig()
	base.Kernel = &KernelConfig{Ref: "ghcr.io/jingkaihe/matchlock/kernel:6.1.137"}

	merged := base.Merge(&Config{Kernel: &KernelConfig{Ref: "file:///tmp/kernel"}})

	require.NotNil(t, merged.Kernel)
	assert.Equal(t, "file:///tmp/kernel", merged.Kernel.Ref)
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

func TestNetworkConfigValidateNoNetworkWithIntercept(t *testing.T) {
	cfg := &NetworkConfig{
		NoNetwork: true,
		Intercept: true,
	}

	err := cfg.Validate()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidConfig)
	assert.Contains(t, err.Error(), "network.no_network")
}

func TestNetworkConfigValidateNoNetworkWithInterceptionRules(t *testing.T) {
	cfg := &NetworkConfig{
		NoNetwork: true,
		Interception: &NetworkInterceptionConfig{
			Rules: []NetworkHookRule{
				{
					Phase:  "before",
					Action: "block",
					Hosts:  []string{"api.openai.com"},
				},
			},
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

func TestNetworkConfigValidateRejectsOverlappingSecretPlaceholders(t *testing.T) {
	cfg := &NetworkConfig{
		Secrets: map[string]Secret{
			"A": {
				Value:       "real_a",
				Placeholder: "foo",
				Hosts:       []string{"example.com"},
			},
			"B": {
				Value:       "real_b",
				Placeholder: "foobar",
				Hosts:       []string{"example.com"},
			},
		},
	}

	err := cfg.Validate()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidConfig)
	assert.Contains(t, err.Error(), "overlap")
	assert.Contains(t, err.Error(), `"A"`)
	assert.Contains(t, err.Error(), `"B"`)
}

func TestNetworkConfigValidateRejectsPlaceholderOverlapWithGeneratedFormat(t *testing.T) {
	cfg := &NetworkConfig{
		Secrets: map[string]Secret{
			"A": {
				Value:       "real_a",
				Placeholder: "SECRET",
				Hosts:       []string{"api.example.com"},
			},
			"B": {
				Value: "real_b",
				Hosts: []string{"api.example.com"},
			},
		},
	}

	err := cfg.Validate()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidConfig)
	assert.Contains(t, err.Error(), "overlap")
	assert.Contains(t, err.Error(), `"A"`)
	assert.Contains(t, err.Error(), `"B"`)
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

func TestValidateVFS_RejectsOwnerOverrideOnMemoryMount(t *testing.T) {
	uid := uint32(1000)
	cfg := &Config{
		VFS: &VFSConfig{
			Workspace: "/workspace",
			Mounts: map[string]MountConfig{
				"/workspace/data": {Type: MountTypeMemory, OwnerUID: &uid},
			},
		},
	}

	err := cfg.ValidateVFS()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidConfig)
	assert.Contains(t, err.Error(), "owner_uid/owner_gid are only supported for host_fs")
}

func TestValidateVFS_RejectsOwnerOverrideOnOverlayMount(t *testing.T) {
	gid := uint32(1000)
	cfg := &Config{
		VFS: &VFSConfig{
			Workspace: "/workspace",
			Mounts: map[string]MountConfig{
				"/workspace/data": {Type: MountTypeOverlay, HostPath: "/tmp/data", OwnerGID: &gid},
			},
		},
	}

	err := cfg.ValidateVFS()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidConfig)
	assert.Contains(t, err.Error(), "owner_uid/owner_gid are only supported for host_fs")
}

func TestValidateVFS_AllowsOwnerOverrideOnHostFSMount(t *testing.T) {
	uid := uint32(1000)
	gid := uint32(1000)
	cfg := &Config{
		VFS: &VFSConfig{
			Workspace: "/workspace",
			Mounts: map[string]MountConfig{
				"/workspace/data": {Type: MountTypeHostFS, HostPath: "/tmp/data", OwnerUID: &uid, OwnerGID: &gid},
			},
		},
	}

	require.NoError(t, cfg.ValidateVFS())
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

func TestMaxSwapMBConstant(t *testing.T) {
	assert.Equal(t, 65536, maxSwapMB)
}

func TestResourcesValidateSwapMB(t *testing.T) {
	tests := []struct {
		name    string
		swapMB  int
		wantErr bool
	}{
		{name: "zero is off", swapMB: 0, wantErr: false},
		{name: "positive below cap", swapMB: 512, wantErr: false},
		{name: "exactly at cap", swapMB: maxSwapMB, wantErr: false},
		{name: "negative rejected", swapMB: -1, wantErr: true},
		{name: "above cap rejected", swapMB: maxSwapMB + 1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Resources{SwapMB: tt.swapMB}
			err := r.Validate()
			if tt.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, ErrInvalidConfig)
				assert.Contains(t, err.Error(), "swap_mb")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestResourcesValidateNilSafe(t *testing.T) {
	var r *Resources
	require.NoError(t, r.Validate())
}

func TestConfigValidateSwapMB(t *testing.T) {
	require.NoError(t, (&Config{Resources: &Resources{SwapMB: 0}}).Validate())
	require.NoError(t, (&Config{Resources: &Resources{SwapMB: maxSwapMB}}).Validate())

	require.ErrorIs(t, (&Config{Resources: &Resources{SwapMB: -1}}).Validate(), ErrInvalidConfig)
	require.ErrorIs(t, (&Config{Resources: &Resources{SwapMB: maxSwapMB + 1}}).Validate(), ErrInvalidConfig)
}

func TestConfigValidateNilSafe(t *testing.T) {
	var c *Config
	require.NoError(t, c.Validate())
	// Empty config has nil Resources/Network/VFS; every section is nil-safe.
	require.NoError(t, (&Config{}).Validate())
}

func TestConfigValidatePropagatesNetworkErrors(t *testing.T) {
	cfg := &Config{
		Resources: &Resources{SwapMB: 1024},
		Network: &NetworkConfig{
			NoNetwork:    true,
			AllowedHosts: []string{"api.openai.com"},
		},
	}

	err := cfg.Validate()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidConfig)
	assert.Contains(t, err.Error(), "network.no_network")
}

func TestConfigMergeSwapMB(t *testing.T) {
	t.Run("positive other overrides base", func(t *testing.T) {
		base := &Config{Resources: &Resources{SwapMB: 1024}}
		merged := base.Merge(&Config{Resources: &Resources{SwapMB: 512}})
		require.NotNil(t, merged.Resources)
		assert.Equal(t, 512, merged.Resources.SwapMB)
	})

	t.Run("explicit zero leaves base unchanged", func(t *testing.T) {
		base := &Config{Resources: &Resources{SwapMB: 1024}}
		merged := base.Merge(&Config{Resources: &Resources{SwapMB: 0}})
		require.NotNil(t, merged.Resources)
		assert.Equal(t, 1024, merged.Resources.SwapMB)
	})

	t.Run("nil base resources initialized from other", func(t *testing.T) {
		base := &Config{}
		merged := base.Merge(&Config{Resources: &Resources{SwapMB: 256}})
		require.NotNil(t, merged.Resources)
		assert.Equal(t, 256, merged.Resources.SwapMB)
	})
}

func TestResourcesSwapMBJSONTag(t *testing.T) {
	data, err := json.Marshal(&Resources{SwapMB: 512})
	require.NoError(t, err)
	assert.Contains(t, string(data), `"swap_mb":512`)

	data, err = json.Marshal(&Resources{})
	require.NoError(t, err)
	assert.NotContains(t, string(data), "swap_mb")

	var r Resources
	require.NoError(t, json.Unmarshal([]byte(`{"swap_mb":1024}`), &r))
	assert.Equal(t, 1024, r.SwapMB)
}

// TestConfigMergeThenValidateRejectsNegativeSwapMB reproduces the RPC-create
// ordering exactly: api.DefaultConfig().Merge(&params) followed by Validate.
// Before the fix, Merge's positive-only guard discarded the negative swap_mb so
// the value was silently normalized to the default 0 (swap off) and Validate
// passed. It must now be rejected with the documented resources.swap_mb error.
func TestConfigMergeThenValidateRejectsNegativeSwapMB(t *testing.T) {
	params := &Config{Resources: &Resources{SwapMB: -1}}
	merged := DefaultConfig().Merge(params)

	err := merged.Validate()
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Contains(t, err.Error(), "swap_mb")
}

// TestConfigMergeThenValidateAcceptsValidSwapMB locks the "0 = off, positives
// override" contract for the RPC-shaped merge-then-validate path.
func TestConfigMergeThenValidateAcceptsValidSwapMB(t *testing.T) {
	tests := []struct {
		name   string
		swapMB int
	}{
		{name: "zero is off", swapMB: 0},
		{name: "positive below cap", swapMB: 512},
		{name: "exactly at cap", swapMB: maxSwapMB},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			merged := DefaultConfig().Merge(&Config{Resources: &Resources{SwapMB: tt.swapMB}})
			require.NotNil(t, merged.Resources)
			assert.Equal(t, tt.swapMB, merged.Resources.SwapMB)
			require.NoError(t, merged.Validate())
		})
	}
}

// TestConfigMergePreservesNonZeroOverrides proves Merge no longer filters out
// negative resource values: they must survive the merge so Validate can see
// them. (0 still means "leave base unchanged".)
func TestConfigMergePreservesNonZeroOverrides(t *testing.T) {
	base := &Config{Resources: &Resources{
		CPUs:           1,
		MemoryMB:       DefaultMemoryMB,
		DiskSizeMB:     DefaultDiskSizeMB,
		SwapMB:         0,
		TimeoutSeconds: DefaultTimeoutSeconds,
	}}
	merged := base.Merge(&Config{Resources: &Resources{
		CPUs:           -1,
		MemoryMB:       -2,
		DiskSizeMB:     -3,
		SwapMB:         -4,
		TimeoutSeconds: -5,
	}})

	require.NotNil(t, merged.Resources)
	assert.Equal(t, -1.0, merged.Resources.CPUs)
	assert.Equal(t, -2, merged.Resources.MemoryMB)
	assert.Equal(t, -3, merged.Resources.DiskSizeMB)
	assert.Equal(t, -4, merged.Resources.SwapMB)
	assert.Equal(t, -5, merged.Resources.TimeoutSeconds)
}

// TestConfigMergeThenValidateRejectsNegativeSiblingResources covers the same
// positive-only merge class for the other resource fields: negative values must
// reach Validate and be rejected instead of reverting to the defaults.
func TestConfigMergeThenValidateRejectsNegativeSiblingResources(t *testing.T) {
	tests := []struct {
		name    string
		other   *Resources
		wantMsg string
	}{
		{name: "negative memory", other: &Resources{MemoryMB: -1}, wantMsg: "memory_mb"},
		{name: "negative disk", other: &Resources{DiskSizeMB: -1}, wantMsg: "disk_size_mb"},
		{name: "negative timeout", other: &Resources{TimeoutSeconds: -1}, wantMsg: "timeout_seconds"},
		{name: "negative cpus", other: &Resources{CPUs: -0.5}, wantMsg: "cpus"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			merged := DefaultConfig().Merge(&Config{Resources: tt.other})
			err := merged.Validate()
			require.Error(t, err)
			require.ErrorIs(t, err, ErrInvalidConfig)
			require.Contains(t, err.Error(), tt.wantMsg)
		})
	}
}

// TestResourcesValidateSiblingFields documents the direct (CLI) validate path:
// zero is valid and means unset/use default, valid positives pass, and negative
// values (including a non-finite CPU count) are rejected.
func TestResourcesValidateSiblingFields(t *testing.T) {
	require.NoError(t, (&Resources{CPUs: 0, MemoryMB: 0, DiskSizeMB: 0, TimeoutSeconds: 0}).Validate())
	require.NoError(t, (&Resources{CPUs: 2.5, MemoryMB: 512, DiskSizeMB: 5120, TimeoutSeconds: 300}).Validate())

	require.ErrorIs(t, (&Resources{CPUs: -1}).Validate(), ErrInvalidConfig)
	require.ErrorIs(t, (&Resources{CPUs: math.NaN()}).Validate(), ErrInvalidConfig)
	require.ErrorIs(t, (&Resources{CPUs: math.Inf(1)}).Validate(), ErrInvalidConfig)
	require.ErrorIs(t, (&Resources{MemoryMB: -1}).Validate(), ErrInvalidConfig)
	require.ErrorIs(t, (&Resources{DiskSizeMB: -1}).Validate(), ErrInvalidConfig)
	require.ErrorIs(t, (&Resources{TimeoutSeconds: -1}).Validate(), ErrInvalidConfig)
}

func TestNetworkConfigAllowPrivateJSONUnmarshal(t *testing.T) {
	var cfg Config
	require.NoError(t, json.Unmarshal([]byte(`{"network":{"allow_private":["192.168.107.74:8888"]}}`), &cfg))

	require.NotNil(t, cfg.Network)
	assert.Equal(t, []string{"192.168.107.74:8888"}, cfg.Network.AllowPrivate)
}

func TestNetworkConfigAllowPrivateJSONTag(t *testing.T) {
	cfg := &NetworkConfig{AllowPrivate: []string{"10.0.0.1:443", "[200::1]:8443"}}

	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"allow_private"`)

	var decoded NetworkConfig
	require.NoError(t, json.Unmarshal(raw, &decoded))
	assert.Equal(t, cfg.AllowPrivate, decoded.AllowPrivate)

	// omitempty: an unset list must not appear in the wire form.
	empty, err := json.Marshal(&NetworkConfig{})
	require.NoError(t, err)
	assert.NotContains(t, string(empty), "allow_private")
}

func TestNetworkConfigValidateNoNetworkWithAllowPrivate(t *testing.T) {
	cfg := &NetworkConfig{
		NoNetwork:    true,
		AllowPrivate: []string{"192.168.1.1"},
	}

	err := cfg.Validate()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidConfig)
	assert.Contains(t, err.Error(), "network.no_network cannot be combined with network.allow_private")
}

func TestNetworkConfigValidateAllowPrivateWithoutNoNetwork(t *testing.T) {
	cfg := &NetworkConfig{
		BlockPrivateIPs: true,
		AllowPrivate:    []string{"192.168.107.74:8888"},
	}

	require.NoError(t, cfg.Validate())
}

// TestConfigMergeReplacesNetworkAllowPrivate documents Config.Merge's
// whole-Network replacement contract for the new allow_private field.
func TestConfigMergeReplacesNetworkAllowPrivate(t *testing.T) {
	base := &Config{Network: &NetworkConfig{
		BlockPrivateIPs: true,
		AllowPrivate:    []string{"10.0.0.1"},
	}}

	// A nil override Network preserves the base list.
	preserved := base.Merge(&Config{})
	require.NotNil(t, preserved.Network)
	assert.Equal(t, []string{"10.0.0.1"}, preserved.Network.AllowPrivate)

	// A non-nil override Network replaces the whole struct, list included.
	replaced := base.Merge(&Config{Network: &NetworkConfig{
		BlockPrivateIPs: true,
		AllowPrivate:    []string{"192.168.107.74:8888"},
	}})
	require.NotNil(t, replaced.Network)
	assert.Equal(t, []string{"192.168.107.74:8888"}, replaced.Network.AllowPrivate)
}
