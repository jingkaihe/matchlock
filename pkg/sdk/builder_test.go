package sdk

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew(t *testing.T) {
	b := New("alpine:latest")
	opts := b.Options()
	require.Equal(t, "alpine:latest", opts.Image)
}

func TestBuilderResources(t *testing.T) {
	opts := New("alpine:latest").
		WithCPUs(4).
		WithMemory(2048).
		WithDiskSize(10240).
		WithTimeout(600).
		Options()

	require.Equal(t, 4, opts.CPUs)
	require.Equal(t, 2048, opts.MemoryMB)
	require.Equal(t, 10240, opts.DiskSizeMB)
	require.Equal(t, 600, opts.TimeoutSeconds)
}

func TestBuilderAllowHost(t *testing.T) {
	opts := New("alpine:latest").
		AllowHost("api.openai.com").
		AllowHost("dl-cdn.alpinelinux.org", "*.github.com").
		Options()

	expected := []string{"api.openai.com", "dl-cdn.alpinelinux.org", "*.github.com"}
	require.Equal(t, expected, opts.AllowedHosts)
}

func TestBuilderAddHost(t *testing.T) {
	opts := New("alpine:latest").
		AddHost("api.internal", "10.0.0.10").
		AddHost("db.internal", "10.0.0.11").
		Options()

	require.Equal(t, []api.HostIPMapping{{Host: "api.internal", IP: "10.0.0.10"}, {Host: "db.internal", IP: "10.0.0.11"}}, opts.AddHosts)
}

func TestBuilderAddSecret(t *testing.T) {
	opts := New("alpine:latest").
		AddSecret("API_KEY", "sk-123", "api.openai.com").
		AddSecret("TOKEN", "tok-456", "*.example.com", "api.example.com").
		Options()

	require.Len(t, opts.Secrets, 2)

	s := opts.Secrets[0]
	assert.Equal(t, "API_KEY", s.Name)
	assert.Equal(t, "sk-123", s.Value)
	require.Len(t, s.Hosts, 1)
	assert.Equal(t, "api.openai.com", s.Hosts[0])

	s = opts.Secrets[1]
	assert.Equal(t, "TOKEN", s.Name)
	assert.Equal(t, "tok-456", s.Value)
	require.Len(t, s.Hosts, 2)
}

func TestBuilderBlockPrivateIPs(t *testing.T) {
	opts := New("alpine:latest").BlockPrivateIPs().Options()
	require.True(t, opts.BlockPrivateIPs)
	require.True(t, opts.BlockPrivateIPsSet)
}

func TestBuilderAllowPrivateIPs(t *testing.T) {
	opts := New("alpine:latest").AllowPrivateIPs().Options()
	require.False(t, opts.BlockPrivateIPs)
	require.True(t, opts.BlockPrivateIPsSet)
}

func TestBuilderUnsetBlockPrivateIPs(t *testing.T) {
	opts := New("alpine:latest").
		BlockPrivateIPs().
		UnsetBlockPrivateIPs().
		Options()
	require.False(t, opts.BlockPrivateIPs)
	require.False(t, opts.BlockPrivateIPsSet)
}

func TestBuilderWorkspace(t *testing.T) {
	opts := New("alpine:latest").WithWorkspace("/home/user/code").Options()
	require.Equal(t, "/home/user/code", opts.Workspace)
}

func TestBuilderEnv(t *testing.T) {
	opts := New("alpine:latest").
		WithEnv("FOO", "bar").
		WithEnv("HELLO", "world").
		Options()

	require.Equal(t, map[string]string{
		"FOO":   "bar",
		"HELLO": "world",
	}, opts.Env)
}

func TestBuilderEnvMapMerge(t *testing.T) {
	opts := New("alpine:latest").
		WithEnv("FOO", "old").
		WithEnvMap(map[string]string{
			"FOO": "new",
			"BAR": "baz",
		}).
		Options()

	require.Equal(t, "new", opts.Env["FOO"])
	require.Equal(t, "baz", opts.Env["BAR"])
}

func TestBuilderDNSServers(t *testing.T) {
	opts := New("alpine:latest").
		WithDNSServers("1.1.1.1", "1.0.0.1").
		Options()

	require.Equal(t, []string{"1.1.1.1", "1.0.0.1"}, opts.DNSServers)
}

func TestBuilderDNSServersChained(t *testing.T) {
	opts := New("alpine:latest").
		WithDNSServers("1.1.1.1").
		WithDNSServers("8.8.8.8").
		Options()

	require.Equal(t, []string{"1.1.1.1", "8.8.8.8"}, opts.DNSServers)
}

func TestBuilderDNSServersDefaultEmpty(t *testing.T) {
	opts := New("alpine:latest").Options()
	require.Empty(t, opts.DNSServers)
}

func TestBuilderHostname(t *testing.T) {
	opts := New("alpine:latest").
		WithHostname("override.internal").
		Options()

	require.Equal(t, "override.internal", opts.Hostname)
}

func TestBuilderNetworkMTU(t *testing.T) {
	opts := New("alpine:latest").
		WithNetworkMTU(1200).
		Options()

	require.Equal(t, 1200, opts.NetworkMTU)
}

func TestBuilderNoNetwork(t *testing.T) {
	opts := New("alpine:latest").
		WithNoNetwork().
		Options()

	require.True(t, opts.NoNetwork)
}

func TestBuilderPortForwards(t *testing.T) {
	opts := New("alpine:latest").
		WithPortForward(18080, 8080).
		WithPortForward(18443, 8443).
		WithPortForwardAddresses("127.0.0.1", "0.0.0.0").
		Options()

	require.Equal(t, []api.PortForward{
		{LocalPort: 18080, RemotePort: 8080},
		{LocalPort: 18443, RemotePort: 8443},
	}, opts.PortForwards)
	require.Equal(t, []string{"127.0.0.1", "0.0.0.0"}, opts.PortForwardAddresses)
}

func TestBuilderMounts(t *testing.T) {
	opts := New("alpine:latest").
		MountHostDir("/data", "/host/data").
		MountHostDirReadonly("/config", "/host/config").
		MountMemory("/tmp/scratch").
		MountOverlay("/workspace", "/host/workspace").
		Options()

	require.Len(t, opts.Mounts, 4)

	m := opts.Mounts["/data"]
	assert.Equal(t, api.MountTypeHostFS, m.Type)
	assert.Equal(t, "/host/data", m.HostPath)
	assert.False(t, m.Readonly)

	m = opts.Mounts["/config"]
	assert.Equal(t, api.MountTypeHostFS, m.Type)
	assert.Equal(t, "/host/config", m.HostPath)
	assert.True(t, m.Readonly)

	m = opts.Mounts["/tmp/scratch"]
	assert.Equal(t, api.MountTypeMemory, m.Type)

	m = opts.Mounts["/workspace"]
	assert.Equal(t, api.MountTypeOverlay, m.Type)
	assert.Equal(t, "/host/workspace", m.HostPath)
}

func TestBuilderFullChain(t *testing.T) {
	opts := New("python:3.12-alpine").
		WithCPUs(2).
		WithMemory(1024).
		WithEnv("PLAIN_TOKEN", "abc123").
		AllowHost("dl-cdn.alpinelinux.org", "api.anthropic.com").
		AddHost("api.internal", "10.0.0.10").
		AddSecret("ANTHROPIC_API_KEY", "sk-ant-xxx", "api.anthropic.com").
		BlockPrivateIPs().
		WithWorkspace("/code").
		MountHostDirReadonly("/data", "/host/data").
		WithTimeout(120).
		Options()

	require.Equal(t, "python:3.12-alpine", opts.Image)
	require.Equal(t, 2, opts.CPUs)
	require.Equal(t, 1024, opts.MemoryMB)
	require.Len(t, opts.AllowedHosts, 2)
	require.Len(t, opts.AddHosts, 1)
	require.Len(t, opts.Secrets, 1)
	require.Equal(t, "abc123", opts.Env["PLAIN_TOKEN"])
	require.True(t, opts.BlockPrivateIPs)
	require.True(t, opts.BlockPrivateIPsSet)
	require.Equal(t, "/code", opts.Workspace)
	require.Len(t, opts.Mounts, 1)
	require.Equal(t, 120, opts.TimeoutSeconds)
}

func TestBuilderVFSInterception(t *testing.T) {
	cfg := &VFSInterceptionConfig{
		Rules: []VFSHookRule{
			{
				Phase:  VFSHookPhaseBefore,
				Ops:    []VFSHookOp{VFSHookOpCreate},
				Path:   "/workspace/blocked.txt",
				Action: VFSHookActionBlock,
			},
		},
	}

	opts := New("alpine:latest").WithVFSInterception(cfg).Options()
	require.NotNil(t, opts.VFSInterception)
	require.Len(t, opts.VFSInterception.Rules, 1)
	assert.Equal(t, "block", opts.VFSInterception.Rules[0].Action)
}

func TestBuilderVFSInterceptionCallback(t *testing.T) {
	cfg := &VFSInterceptionConfig{
		Rules: []VFSHookRule{
			{
				Phase: VFSHookPhaseAfter,
				Ops:   []VFSHookOp{VFSHookOpWrite},
				Path:  "/workspace/*",
				Hook: func(ctx context.Context, event VFSHookEvent) error {
					return nil
				},
			},
		},
	}

	opts := New("alpine:latest").WithVFSInterception(cfg).Options()
	require.NotNil(t, opts.VFSInterception)
	require.Len(t, opts.VFSInterception.Rules, 1)
	assert.NotNil(t, opts.VFSInterception.Rules[0].Hook)
}

func TestBuilderWithPlugin(t *testing.T) {
	opts := New("alpine:latest").
		WithPlugin(PluginConfig{
			Type:   "host_filter",
			Config: json.RawMessage(`{"allowed_hosts":["example.com"]}`),
		}).
		Options()

	require.Len(t, opts.Plugins, 1)
	assert.Equal(t, "host_filter", opts.Plugins[0].Type)
	assert.Nil(t, opts.Plugins[0].Enabled)
	assert.JSONEq(t, `{"allowed_hosts":["example.com"]}`, string(opts.Plugins[0].Config))
}

func TestBuilderWithPluginMultiple(t *testing.T) {
	enabled := false
	opts := New("alpine:latest").
		WithPlugin(PluginConfig{
			Type:   "host_filter",
			Config: json.RawMessage(`{"allowed_hosts":["a.com"]}`),
		}).
		WithPlugin(PluginConfig{
			Type:    "local_model_router",
			Enabled: &enabled,
			Config:  json.RawMessage(`{"routes":[]}`),
		}).
		Options()

	require.Len(t, opts.Plugins, 2)
	assert.Equal(t, "host_filter", opts.Plugins[0].Type)
	assert.Equal(t, "local_model_router", opts.Plugins[1].Type)
	assert.NotNil(t, opts.Plugins[1].Enabled)
	assert.False(t, *opts.Plugins[1].Enabled)
}

func TestBuilderWithPluginSerialization(t *testing.T) {
	opts := CreateOptions{
		Image: "alpine:latest",
		Plugins: []PluginConfig{
			{
				Type:   "host_filter",
				Config: json.RawMessage(`{"allowed_hosts":["example.com"]}`),
			},
		},
	}

	network := buildCreateNetworkParams(opts)
	require.NotNil(t, network, "Network params should be included when plugins are present")

	plugins, ok := network["plugins"]
	require.True(t, ok, "Network params should contain plugins key")

	pluginSlice, ok := plugins.([]PluginConfig)
	require.True(t, ok)
	require.Len(t, pluginSlice, 1)
	assert.Equal(t, "host_filter", pluginSlice[0].Type)
}
