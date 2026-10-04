package api

import (
	"encoding/json"
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

func TestParseConfigRejectsLegacyVFS(t *testing.T) {
	for _, data := range []string{
		`{"vfs":{}}`,
		`{"vfs":{"workspace":"/workspace"}}`,
		`{"vfs":{"mounts":{"/workspace":{"type":"host_fs","host_path":"/tmp"}}}}`,
		`{"vfs":{"interception":{"emit_events":true}}}`,
		`{"VFS":{}}`,
	} {
		t.Run(data, func(t *testing.T) {
			_, err := ParseConfig([]byte(data))
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidConfig)
			assert.Contains(t, err.Error(), "vfs is no longer supported")
		})
	}
}

func TestParseConfigAllowsAbsentOrNullVFS(t *testing.T) {
	for _, data := range []string{
		`{"image":"alpine:latest"}`,
		`{"image":"alpine:latest","vfs": null }`,
	} {
		t.Run(data, func(t *testing.T) {
			cfg, err := ParseConfig([]byte(data))
			require.NoError(t, err)
			assert.Equal(t, "alpine:latest", cfg.Image)
			encoded, err := json.Marshal(cfg)
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), `"vfs"`)
		})
	}
}

func TestConfigJSONRoundTripPreservesDisksAndImageConfig(t *testing.T) {
	uid := uint32(1000)
	cfg := DefaultConfig()
	cfg.Image = "alpine:latest"
	cfg.Env = map[string]string{"FOO": "bar"}
	cfg.ImageCfg = &ImageConfig{WorkingDir: "/app", User: "1000"}
	cfg.ExtraDisks = []DiskMount{{HostPath: "/tmp/data.ext4", GuestMount: "/data", OwnerUID: &uid}}
	encoded, err := json.Marshal(cfg)
	require.NoError(t, err)

	var decoded Config
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	assert.Equal(t, cfg, &decoded)
}
