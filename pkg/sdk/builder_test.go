package sdk

import (
	"testing"
)

func TestNew(t *testing.T) {
	b := New("alpine:latest")
	opts := b.Options()
	if opts.Image != "alpine:latest" {
		t.Fatalf("expected image alpine:latest, got %s", opts.Image)
	}
}

func TestBuilderResources(t *testing.T) {
	opts := New("alpine:latest").
		WithCPUs(4).
		WithMemory(2048).
		WithDiskSize(10240).
		WithTimeout(600).
		Options()

	if opts.CPUs != 4 {
		t.Fatalf("expected 4 CPUs, got %d", opts.CPUs)
	}
	if opts.MemoryMB != 2048 {
		t.Fatalf("expected 2048 MB memory, got %d", opts.MemoryMB)
	}
	if opts.DiskSizeMB != 10240 {
		t.Fatalf("expected 10240 MB disk, got %d", opts.DiskSizeMB)
	}
	if opts.TimeoutSeconds != 600 {
		t.Fatalf("expected 600s timeout, got %d", opts.TimeoutSeconds)
	}
}

func TestBuilderAllowHost(t *testing.T) {
	opts := New("alpine:latest").
		AllowHost("api.openai.com").
		AllowHost("dl-cdn.alpinelinux.org", "*.github.com").
		Options()

	expected := []string{"api.openai.com", "dl-cdn.alpinelinux.org", "*.github.com"}
	if len(opts.AllowedHosts) != len(expected) {
		t.Fatalf("expected %d hosts, got %d", len(expected), len(opts.AllowedHosts))
	}
	for i, h := range expected {
		if opts.AllowedHosts[i] != h {
			t.Fatalf("expected host %s at index %d, got %s", h, i, opts.AllowedHosts[i])
		}
	}
}

func TestBuilderAddSecret(t *testing.T) {
	opts := New("alpine:latest").
		AddSecret("API_KEY", "sk-123", "api.openai.com").
		AddSecret("TOKEN", "tok-456", "*.example.com", "api.example.com").
		Options()

	if len(opts.Secrets) != 2 {
		t.Fatalf("expected 2 secrets, got %d", len(opts.Secrets))
	}

	s := opts.Secrets[0]
	if s.Name != "API_KEY" || s.Value != "sk-123" || len(s.Hosts) != 1 || s.Hosts[0] != "api.openai.com" {
		t.Fatalf("unexpected first secret: %+v", s)
	}

	s = opts.Secrets[1]
	if s.Name != "TOKEN" || s.Value != "tok-456" || len(s.Hosts) != 2 {
		t.Fatalf("unexpected second secret: %+v", s)
	}
}

func TestBuilderBlockPrivateIPs(t *testing.T) {
	opts := New("alpine:latest").BlockPrivateIPs().Options()
	if !opts.BlockPrivateIPs {
		t.Fatal("expected BlockPrivateIPs to be true")
	}
}

func TestBuilderWorkspace(t *testing.T) {
	opts := New("alpine:latest").WithWorkspace("/home/user/code").Options()
	if opts.Workspace != "/home/user/code" {
		t.Fatalf("expected workspace /home/user/code, got %s", opts.Workspace)
	}
}

func TestBuilderMounts(t *testing.T) {
	opts := New("alpine:latest").
		MountHostDir("/data", "/host/data").
		MountHostDirReadonly("/config", "/host/config").
		MountMemory("/tmp/scratch").
		MountOverlay("/workspace", "/host/workspace").
		Options()

	if len(opts.Mounts) != 4 {
		t.Fatalf("expected 4 mounts, got %d", len(opts.Mounts))
	}

	m := opts.Mounts["/data"]
	if m.Type != "real_fs" || m.HostPath != "/host/data" || m.Readonly {
		t.Fatalf("unexpected /data mount: %+v", m)
	}

	m = opts.Mounts["/config"]
	if m.Type != "real_fs" || m.HostPath != "/host/config" || !m.Readonly {
		t.Fatalf("unexpected /config mount: %+v", m)
	}

	m = opts.Mounts["/tmp/scratch"]
	if m.Type != "memory" {
		t.Fatalf("unexpected /tmp/scratch mount: %+v", m)
	}

	m = opts.Mounts["/workspace"]
	if m.Type != "overlay" || m.HostPath != "/host/workspace" {
		t.Fatalf("unexpected /workspace mount: %+v", m)
	}
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

	if opts.Image != "python:3.12-alpine" {
		t.Fatalf("wrong image: %s", opts.Image)
	}
	if opts.CPUs != 2 {
		t.Fatal("wrong cpus")
	}
	if opts.MemoryMB != 1024 {
		t.Fatal("wrong memory")
	}
	if len(opts.AllowedHosts) != 2 {
		t.Fatal("wrong hosts count")
	}
	if len(opts.Secrets) != 1 {
		t.Fatal("wrong secrets count")
	}
	if !opts.BlockPrivateIPs {
		t.Fatal("block private IPs not set")
	}
	if opts.Workspace != "/code" {
		t.Fatal("wrong workspace")
	}
	if len(opts.Mounts) != 1 {
		t.Fatal("wrong mounts count")
	}
	if opts.TimeoutSeconds != 120 {
		t.Fatal("wrong timeout")
	}
}
