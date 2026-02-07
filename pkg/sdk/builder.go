package sdk

// SandboxBuilder provides a fluent API for configuring and creating sandboxes.
//
// Usage:
//
//	sandbox := sdk.New("python:3.12-alpine").
//		WithCPUs(2).
//		WithMemory(1024).
//		AllowHost("api.openai.com").
//		AddSecret("API_KEY", os.Getenv("API_KEY"), "api.openai.com").
//		BlockPrivateIPs()
//
//	vmID, err := client.Launch(sandbox)
type SandboxBuilder struct {
	opts CreateOptions
}

// New creates a SandboxBuilder for the given container image.
func New(image string) *SandboxBuilder {
	return &SandboxBuilder{
		opts: CreateOptions{Image: image},
	}
}

// WithPrivileged enables privileged mode, skipping in-guest security restrictions.
func (b *SandboxBuilder) WithPrivileged() *SandboxBuilder {
	b.opts.Privileged = true
	return b
}

// WithCPUs sets the number of vCPUs.
func (b *SandboxBuilder) WithCPUs(cpus int) *SandboxBuilder {
	b.opts.CPUs = cpus
	return b
}

// WithMemory sets memory in megabytes.
func (b *SandboxBuilder) WithMemory(mb int) *SandboxBuilder {
	b.opts.MemoryMB = mb
	return b
}

// WithDiskSize sets disk size in megabytes.
func (b *SandboxBuilder) WithDiskSize(mb int) *SandboxBuilder {
	b.opts.DiskSizeMB = mb
	return b
}

// WithTimeout sets the maximum execution time in seconds.
func (b *SandboxBuilder) WithTimeout(seconds int) *SandboxBuilder {
	b.opts.TimeoutSeconds = seconds
	return b
}

// WithWorkspace sets the VFS mount point in the guest.
func (b *SandboxBuilder) WithWorkspace(path string) *SandboxBuilder {
	b.opts.Workspace = path
	return b
}

// AllowHost adds one or more hosts to the network allowlist (supports glob patterns).
func (b *SandboxBuilder) AllowHost(hosts ...string) *SandboxBuilder {
	b.opts.AllowedHosts = append(b.opts.AllowedHosts, hosts...)
	return b
}

// BlockPrivateIPs blocks access to private IP ranges (10.x, 172.16.x, 192.168.x).
func (b *SandboxBuilder) BlockPrivateIPs() *SandboxBuilder {
	b.opts.BlockPrivateIPs = true
	return b
}

// AddSecret registers a secret for MITM injection. The secret is exposed as a
// placeholder environment variable inside the VM, and the real value is injected
// into HTTP requests to the specified hosts.
func (b *SandboxBuilder) AddSecret(name, value string, hosts ...string) *SandboxBuilder {
	b.opts.Secrets = append(b.opts.Secrets, Secret{
		Name:  name,
		Value: value,
		Hosts: hosts,
	})
	return b
}

// Mount adds a VFS mount at the given guest path.
func (b *SandboxBuilder) Mount(guestPath string, cfg MountConfig) *SandboxBuilder {
	if b.opts.Mounts == nil {
		b.opts.Mounts = make(map[string]MountConfig)
	}
	b.opts.Mounts[guestPath] = cfg
	return b
}

// MountHostDir is a convenience for mounting a host directory into the guest.
func (b *SandboxBuilder) MountHostDir(guestPath, hostPath string) *SandboxBuilder {
	return b.Mount(guestPath, MountConfig{Type: "real_fs", HostPath: hostPath})
}

// MountHostDirReadonly mounts a host directory into the guest as read-only.
func (b *SandboxBuilder) MountHostDirReadonly(guestPath, hostPath string) *SandboxBuilder {
	return b.Mount(guestPath, MountConfig{Type: "real_fs", HostPath: hostPath, Readonly: true})
}

// MountMemory creates an in-memory filesystem at the given guest path.
func (b *SandboxBuilder) MountMemory(guestPath string) *SandboxBuilder {
	return b.Mount(guestPath, MountConfig{Type: "memory"})
}

// MountOverlay creates a copy-on-write overlay at the given guest path.
func (b *SandboxBuilder) MountOverlay(guestPath, hostPath string) *SandboxBuilder {
	return b.Mount(guestPath, MountConfig{Type: "overlay", HostPath: hostPath})
}

// Options returns the underlying CreateOptions. Useful when you need to pass
// the options to Client.Create directly.
func (b *SandboxBuilder) Options() CreateOptions {
	return b.opts
}

// Launch creates and starts the sandbox using the given client.
// This is a convenience that calls client.Create(b.Options()).
func (c *Client) Launch(b *SandboxBuilder) (string, error) {
	return c.Create(b.Options())
}
