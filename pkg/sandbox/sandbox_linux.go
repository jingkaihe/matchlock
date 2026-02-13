//go:build linux

// Package sandbox provides the core sandbox VM management functionality.
package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/google/uuid"
	"github.com/jingkaihe/matchlock/pkg/api"
	sandboxnet "github.com/jingkaihe/matchlock/pkg/net"
	"github.com/jingkaihe/matchlock/pkg/policy"
	"github.com/jingkaihe/matchlock/pkg/state"
	"github.com/jingkaihe/matchlock/pkg/vfs"
	"github.com/jingkaihe/matchlock/pkg/vm"
	"github.com/jingkaihe/matchlock/pkg/vm/linux"
	"golang.org/x/sys/unix"
)

// FirewallRules is an interface for managing firewall rules.
type FirewallRules interface {
	Setup() error
	Cleanup() error
}

// Sandbox represents a running sandbox VM with all associated resources.
type Sandbox struct {
	id          string
	config      *api.Config
	machine     vm.Machine
	proxy       *sandboxnet.TransparentProxy
	fwRules     FirewallRules
	natRules    *sandboxnet.NFTablesNAT
	policy      *policy.Engine
	vfsRoot     *vfs.MountRouter
	vfsServer   *vfs.VFSServer
	vfsStopFunc func()
	events      chan api.Event
	stateMgr    *state.Manager
	tapName     string
	caPool      *sandboxnet.CAPool
	subnetInfo  *state.SubnetInfo
	subnetAlloc *state.SubnetAllocator
	workspace   string
	rootfsPath  string
}

// Options configures sandbox creation.
type Options struct {
	// KernelPath overrides the default kernel path
	KernelPath string
	// RootfsPath is the path to the rootfs image (required)
	RootfsPath string
}

// New creates a new sandbox VM with the given configuration.
func New(ctx context.Context, config *api.Config, opts *Options) (*Sandbox, error) {
	if opts == nil {
		opts = &Options{}
	}
	if opts.RootfsPath == "" {
		return nil, fmt.Errorf("RootfsPath is required")
	}

	id := "vm-" + uuid.New().String()[:8]
	workspace := config.GetWorkspace()

	stateMgr := state.NewManager()
	if err := stateMgr.Register(id, config); err != nil {
		return nil, fmt.Errorf("failed to register VM state: %w", err)
	}

	// Create a copy of the rootfs for this VM (copy-on-write if supported)
	vmRootfsPath := stateMgr.Dir(id) + "/rootfs.ext4"
	if err := copyRootfs(opts.RootfsPath, vmRootfsPath); err != nil {
		stateMgr.Unregister(id)
		return nil, fmt.Errorf("failed to copy rootfs: %w", err)
	}

	// Inject matchlock components (guest-agent, guest-fused, init, DNS) and resize
	var diskSizeMB int64
	if config.Resources != nil {
		diskSizeMB = int64(config.Resources.DiskSizeMB)
	}
	if err := prepareRootfs(vmRootfsPath, diskSizeMB); err != nil {
		os.Remove(vmRootfsPath)
		stateMgr.Unregister(id)
		return nil, fmt.Errorf("failed to prepare rootfs: %w", err)
	}

	// Create CAPool early and inject cert into rootfs before VM creation
	needsProxy := config.Network != nil && (len(config.Network.AllowedHosts) > 0 || len(config.Network.Secrets) > 0)
	var caPool *sandboxnet.CAPool
	if needsProxy {
		var err error
		caPool, err = sandboxnet.NewCAPool()
		if err != nil {
			os.Remove(vmRootfsPath)
			stateMgr.Unregister(id)
			return nil, fmt.Errorf("failed to create CA pool: %w", err)
		}
		if err := injectConfigFileIntoRootfs(vmRootfsPath, "/etc/ssl/certs/matchlock-ca.crt", caPool.CACertPEM()); err != nil {
			os.Remove(vmRootfsPath)
			stateMgr.Unregister(id)
			return nil, fmt.Errorf("failed to inject CA cert into rootfs: %w", err)
		}
	}

	// Allocate unique subnet for this VM
	subnetAlloc := state.NewSubnetAllocator()
	subnetInfo, err := subnetAlloc.Allocate(id)
	if err != nil {
		os.Remove(vmRootfsPath)
		stateMgr.Unregister(id)
		return nil, fmt.Errorf("failed to allocate subnet: %w", err)
	}

	backend := linux.NewLinuxBackend()

	kernelPath := opts.KernelPath
	if kernelPath == "" {
		kernelPath = DefaultKernelPath()
	}

	var extraDisks []vm.DiskConfig
	for _, d := range config.ExtraDisks {
		if err := api.ValidateGuestMount(d.GuestMount); err != nil {
			subnetAlloc.Release(id)
			stateMgr.Unregister(id)
			return nil, fmt.Errorf("invalid extra disk config: %w", err)
		}
		extraDisks = append(extraDisks, vm.DiskConfig{
			HostPath:   d.HostPath,
			GuestMount: d.GuestMount,
			ReadOnly:   d.ReadOnly,
		})
	}

	vmConfig := &vm.VMConfig{
		ID:         id,
		KernelPath: kernelPath,
		RootfsPath: vmRootfsPath,
		CPUs:       config.Resources.CPUs,
		MemoryMB:   config.Resources.MemoryMB,
		SocketPath: stateMgr.SocketPath(id) + ".sock",
		LogPath:    stateMgr.LogPath(id),
		VsockCID:   3,
		VsockPath:  stateMgr.Dir(id) + "/vsock.sock",
		GatewayIP:  subnetInfo.GatewayIP,
		GuestIP:    subnetInfo.GuestIP,
		SubnetCIDR: subnetInfo.GatewayIP + "/24",
		Workspace:  workspace,
		Privileged: config.Privileged,
		ExtraDisks: extraDisks,
		DNSServers: config.Network.GetDNSServers(),
	}

	machine, err := backend.Create(ctx, vmConfig)
	if err != nil {
		subnetAlloc.Release(id)
		stateMgr.Unregister(id)
		return nil, fmt.Errorf("failed to create VM: %w", err)
	}

	linuxMachine := machine.(*linux.LinuxMachine)

	// Auto-add secret hosts to allowed hosts if secrets are defined
	if config.Network != nil && len(config.Network.Secrets) > 0 {
		hostSet := make(map[string]bool)
		for _, h := range config.Network.AllowedHosts {
			hostSet[h] = true
		}
		for _, secret := range config.Network.Secrets {
			for _, h := range secret.Hosts {
				if !hostSet[h] {
					config.Network.AllowedHosts = append(config.Network.AllowedHosts, h)
					hostSet[h] = true
				}
			}
		}
	}

	// Create policy engine
	policyEngine := policy.NewEngine(config.Network)

	// Create event channel
	events := make(chan api.Event, 100)

	// Set up transparent proxy for HTTP/HTTPS interception
	gatewayIP := subnetInfo.GatewayIP
	const proxyBindAddr = "0.0.0.0"

	var proxy *sandboxnet.TransparentProxy
	var fwRules FirewallRules

	if needsProxy {
		proxy, err = sandboxnet.NewTransparentProxy(&sandboxnet.ProxyConfig{
			BindAddr:        proxyBindAddr,
			HTTPPort:        0,
			HTTPSPort:       0,
			PassthroughPort: 0,
			Policy:          policyEngine,
			Events:          events,
			CAPool:          caPool,
		})
		if err != nil {
			machine.Close(ctx)
			subnetAlloc.Release(id)
			stateMgr.Unregister(id)
			return nil, fmt.Errorf("failed to create transparent proxy: %w", err)
		}

		proxy.Start()

		fwRules = sandboxnet.NewNFTablesRules(linuxMachine.TapName(), gatewayIP, proxy.HTTPPort(), proxy.HTTPSPort(), proxy.PassthroughPort(), config.Network.GetDNSServers())
		if err := fwRules.Setup(); err != nil {
			proxy.Close()
			machine.Close(ctx)
			subnetAlloc.Release(id)
			stateMgr.Unregister(id)
			return nil, fmt.Errorf("failed to setup firewall rules: %w", err)
		}
	}

	// Set up basic NAT for guest network access using nftables
	natRules := sandboxnet.NewNFTablesNAT(linuxMachine.TapName())
	if err := natRules.Setup(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to setup NAT: %v\n", err)
		natRules = nil
	}

	// Create VFS providers
	vfsProviders := buildVFSProviders(config, workspace)
	vfsRoot := vfs.NewMountRouter(vfsProviders)

	// Create VFS server for guest FUSE daemon connections
	vfsServer := vfs.NewVFSServer(vfsRoot)

	// Start VFS server on the vsock UDS path for VFS port
	vfsSocketPath := fmt.Sprintf("%s_%d", vmConfig.VsockPath, linux.VsockPortVFS)
	vfsStopFunc, err := vfsServer.ServeUDSBackground(vfsSocketPath)
	if err != nil {
		if proxy != nil {
			proxy.Close()
		}
		if fwRules != nil {
			fwRules.Cleanup()
		}
		machine.Close(ctx)
		subnetAlloc.Release(id)
		stateMgr.Unregister(id)
		return nil, fmt.Errorf("failed to start VFS server: %w", err)
	}

	return &Sandbox{
		id:          id,
		config:      config,
		machine:     machine,
		proxy:       proxy,
		fwRules:     fwRules,
		natRules:    natRules,
		policy:      policyEngine,
		vfsRoot:     vfsRoot,
		vfsServer:   vfsServer,
		vfsStopFunc: vfsStopFunc,
		events:      events,
		stateMgr:    stateMgr,
		tapName:     linuxMachine.TapName(),
		caPool:      caPool,
		subnetInfo:  subnetInfo,
		subnetAlloc: subnetAlloc,
		workspace:   workspace,
		rootfsPath:  vmRootfsPath,
	}, nil
}

// ID returns the sandbox identifier.
func (s *Sandbox) ID() string { return s.id }

// Config returns the sandbox configuration.
func (s *Sandbox) Config() *api.Config { return s.config }

// Workspace returns the VFS mount point path.
func (s *Sandbox) Workspace() string { return s.workspace }

// Machine returns the underlying VM machine for advanced operations.
func (s *Sandbox) Machine() vm.Machine { return s.machine }

// Policy returns the policy engine.
func (s *Sandbox) Policy() *policy.Engine { return s.policy }

func (s *Sandbox) CAPool() *sandboxnet.CAPool { return s.caPool }

// Start starts the sandbox VM.
func (s *Sandbox) Start(ctx context.Context) error {
	return s.machine.Start(ctx)
}

// Stop stops the sandbox VM.
func (s *Sandbox) Stop(ctx context.Context) error {
	return s.machine.Stop(ctx)
}

func (s *Sandbox) PrepareExecEnv() *api.ExecOptions {
	return prepareExecEnv(s.config, s.caPool, s.policy)
}

func (s *Sandbox) Exec(ctx context.Context, command string, opts *api.ExecOptions) (*api.ExecResult, error) {
	return execCommand(ctx, s.machine, s.config, s.caPool, s.policy, command, opts)
}

func (s *Sandbox) WriteFile(ctx context.Context, path string, content []byte, mode uint32) error {
	return writeFile(s.vfsRoot, path, content, mode)
}

func (s *Sandbox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	return readFile(s.vfsRoot, path)
}

func (s *Sandbox) ReadFileTo(ctx context.Context, path string, w io.Writer) (int64, error) {
	return readFileTo(s.vfsRoot, path, w)
}

func (s *Sandbox) ListFiles(ctx context.Context, path string) ([]api.FileInfo, error) {
	return listFiles(s.vfsRoot, path)
}

// Events returns a channel for receiving sandbox events.
func (s *Sandbox) Events() <-chan api.Event {
	return s.events
}

// Close shuts down the sandbox and releases all resources.
func (s *Sandbox) Close(ctx context.Context) error {
	var errs []error

	if s.vfsStopFunc != nil {
		s.vfsStopFunc()
	}
	if s.fwRules != nil {
		if err := s.fwRules.Cleanup(); err != nil {
			errs = append(errs, fmt.Errorf("firewall cleanup: %w", err))
		}
	}
	if s.natRules != nil {
		if err := s.natRules.Cleanup(); err != nil {
			errs = append(errs, fmt.Errorf("NAT cleanup: %w", err))
		}
	}
	if s.proxy != nil {
		s.proxy.Close()
	}

	// Release subnet allocation
	if s.subnetAlloc != nil {
		s.subnetAlloc.Release(s.id)
	}

	close(s.events)
	s.stateMgr.Unregister(s.id)
	if err := s.machine.Close(ctx); err != nil {
		errs = append(errs, fmt.Errorf("machine close: %w", err))
	}

	// Remove rootfs copy to save disk space
	rootfsCopy := s.stateMgr.Dir(s.id) + "/rootfs.ext4"
	os.Remove(rootfsCopy)

	if len(errs) > 0 {
		fmt.Fprintf(os.Stderr, "Warning: cleanup errors: %v\n", errs)
	}
	return nil
}

func createProvider(mount api.MountConfig) vfs.Provider {
	switch mount.Type {
	case "memory":
		return vfs.NewMemoryProvider()
	case "real_fs":
		p := vfs.NewRealFSProvider(mount.HostPath)
		if mount.Readonly {
			return vfs.NewReadonlyProvider(p)
		}
		return p
	case "overlay":
		var upper, lower vfs.Provider
		if mount.Upper != nil {
			upper = createProvider(*mount.Upper)
		} else {
			upper = vfs.NewMemoryProvider()
		}
		if mount.Lower != nil {
			lower = createProvider(*mount.Lower)
		}
		if upper != nil && lower != nil {
			return vfs.NewOverlayProvider(upper, lower)
		}
		return upper
	default:
		return vfs.NewMemoryProvider()
	}
}

func copyRootfs(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create dest: %w", err)
	}
	defer dstFile.Close()

	// Try copy-on-write clone first (FICLONE ioctl)
	// Works on btrfs, xfs (with reflink), bcachefs, etc.
	err = unix.IoctlFileClone(int(dstFile.Fd()), int(srcFile.Fd()))
	if err == nil {
		return nil
	}

	// Fall back to regular copy
	fmt.Fprintf(os.Stderr, "Note: copy-on-write not supported (%v), using regular copy\n", err)
	if _, err := io.Copy(dstFile, srcFile); err != nil {
		os.Remove(dst)
		return fmt.Errorf("copy: %w", err)
	}

	return nil
}
