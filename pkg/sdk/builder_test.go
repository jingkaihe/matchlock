package sdk

import (
	"testing"

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
}

func TestBuilderWorkspace(t *testing.T) {
	opts := New("alpine:latest").WithWorkspace("/home/user/code").Options()
	require.Equal(t, "/home/user/code", opts.Workspace)
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

func TestBuilderMounts(t *testing.T) {
	opts := New("alpine:latest").
		MountHostDir("/data", "/host/data").
		MountHostDirReadonly("/config", "/host/config").
		MountMemory("/tmp/scratch").
		MountOverlay("/workspace", "/host/workspace").
		Options()

	require.Len(t, opts.Mounts, 4)

	m := opts.Mounts["/data"]
	assert.Equal(t, "real_fs", m.Type)
	assert.Equal(t, "/host/data", m.HostPath)
	assert.False(t, m.Readonly)

	m = opts.Mounts["/config"]
	assert.Equal(t, "real_fs", m.Type)
	assert.Equal(t, "/host/config", m.HostPath)
	assert.True(t, m.Readonly)

	m = opts.Mounts["/tmp/scratch"]
	assert.Equal(t, "memory", m.Type)

	m = opts.Mounts["/workspace"]
	assert.Equal(t, "overlay", m.Type)
	assert.Equal(t, "/host/workspace", m.HostPath)
}

func TestBuilderFullChain(t *testing.T) {
	opts := New("python:3.12-alpine").
		WithCPUs(2).
		WithMemory(1024).
		AllowHost("dl-cdn.alpinelinux.org", "api.anthropic.com").
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
	require.Len(t, opts.Secrets, 1)
	require.True(t, opts.BlockPrivateIPs)
	require.Equal(t, "/code", opts.Workspace)
	require.Len(t, opts.Mounts, 1)
	require.Equal(t, 120, opts.TimeoutSeconds)
}
