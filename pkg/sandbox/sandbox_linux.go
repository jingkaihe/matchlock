//go:build linux

// Package sandbox provides the core sandbox VM management functionality.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/lifecycle"
	sandboxnet "github.com/jingkaihe/matchlock/pkg/net"
	"github.com/jingkaihe/matchlock/pkg/policy"
	"github.com/jingkaihe/matchlock/pkg/state"
	"github.com/jingkaihe/matchlock/pkg/vfs"
	"github.com/jingkaihe/matchlock/pkg/vm"
	"github.com/jingkaihe/matchlock/pkg/vm/linux"
)

// FirewallRules is an interface for managing firewall rules.
type FirewallRules interface {
	Setup() error
	Cleanup() error
}

// Sandbox represents a running sandbox VM with all associated resources.
type Sandbox struct {
	id               string
	config           *api.Config
	machine          vm.Machine
	proxy            *sandboxnet.TransparentProxy
	fwRules          FirewallRules
	natRules         *sandboxnet.NFTablesNAT
	policy           *policy.Engine
	vfsRoot          vfs.Provider
	vfsHooks         *vfs.HookEngine
	vfsServer        *vfs.VFSServer
	vfsStopFunc      func()
	events           chan api.Event
	stateMgr         *state.Manager
	tapName          string
	caPool           *sandboxnet.CAPool
	subnetInfo       *state.SubnetInfo
	subnetAlloc      *state.SubnetAllocator
	workspace        string
	rootfsPath       string // Writable overlay upper disk
	bootstrapPath    string // Bootstrap root disk (vda)
	overlaySnapshots []string
	lifecycle        *lifecycle.Store
}

// Options configures sandbox creation.
type Options struct {
	// KernelPath overrides the default kernel path
	KernelPath string
	// RootfsPaths are immutable lower image paths in base->top order (required).
	RootfsPaths []string
	// RootfsFSTypes optionally declares filesystem type per lower image.
	RootfsFSTypes []string
}

// New creates a new sandbox VM with the given configuration.
func New(ctx context.Context, config *api.Config, opts *Options) (sb *Sandbox, retErr error) {
	if opts == nil {
		opts = &Options{}
	}
	if len(opts.RootfsPaths) == 0 {
		return nil, fmt.Errorf("RootfsPaths is required")
	}
	if err := config.ValidateVFS(); err != nil {
		return nil, err
	}
	rootfsFSTypes := normalizeOverlayLowerFSTypes(opts.RootfsPaths, opts.RootfsFSTypes)
	vfsEnabled := config.HasVFSMounts()

	id := config.GetID()
	hostname := config.GetHostname()
	workspace := config.GetWorkspace()
	noNetwork := config.Network != nil && config.Network.NoNetwork

	stateMgr := state.NewManager()
	if err := stateMgr.Register(id, config); err != nil {
		return nil, errx.Wrap(ErrRegisterState, err)
	}
	lifecycleStore := lifecycle.NewStore(stateMgr.Dir(id))
	if err := lifecycleStore.Init(id, "firecracker", stateMgr.Dir(id)); err != nil {
		stateMgr.Unregister(id)
		return nil, errx.Wrap(ErrLifecycleInit, err)
	}
	_ = lifecycleStore.SetResource(func(r *lifecycle.Resources) {
		r.StateDir = stateMgr.Dir(id)
		r.Workspace = workspace
	})
	defer func() {
		if retErr != nil {
			_ = lifecycleStore.SetLastError(retErr)
			_ = lifecycleStore.SetPhase(lifecycle.PhaseCreateFailed)
		}
	}()

	bootstrapRootfsPath := stateMgr.Dir(id) + "/bootstrap.ext4"
	upperRootfsPath := stateMgr.Dir(id) + "/upper.ext4"
	cleanupRootDisks := func() {
		_ = os.Remove(bootstrapRootfsPath)
		_ = os.Remove(upperRootfsPath)
	}
	defer func() {
		if retErr != nil {
			cleanupRootDisks()
		}
	}()

	if err := createBootstrapRootfs(bootstrapRootfsPath); err != nil {
		cleanupRootDisks()
		stateMgr.Unregister(id)
		return nil, errx.Wrap(ErrPrepareBootstrapRoot, err)
	}

	var diskSizeMB int64 = api.DefaultDiskSizeMB
	if config.Resources != nil && config.Resources.DiskSizeMB > 0 {
		diskSizeMB = int64(config.Resources.DiskSizeMB)
	}
	if err := createExt4Image(upperRootfsPath, diskSizeMB); err != nil {
		cleanupRootDisks()
		stateMgr.Unregister(id)
		return nil, errx.Wrap(ErrCreateRootfs, err)
	}
	if err := prepareOverlayUpperRootfs(upperRootfsPath); err != nil {
		cleanupRootDisks()
		stateMgr.Unregister(id)
		return nil, errx.Wrap(ErrPrepareRootfs, err)
	}
	_ = lifecycleStore.SetResource(func(r *lifecycle.Resources) {
		r.RootfsPath = upperRootfsPath
		r.VsockPath = stateMgr.Dir(id) + "/vsock.sock"
	})

	if config.Network != nil {
		if err := config.Network.Validate(); err != nil {
			stateMgr.Unregister(id)
			return nil, err
		}
	}

	// Create CAPool early and inject cert into writable upper before VM creation
	needsProxy := !noNetwork && config.Network != nil && (len(config.Network.AllowedHosts) > 0 || len(config.Network.Secrets) > 0)
	var caPool *sandboxnet.CAPool
	if needsProxy {
		var err error
		caPool, err = sandboxnet.NewCAPool()
		if err != nil {
			cleanupRootDisks()
			stateMgr.Unregister(id)
			return nil, errx.Wrap(ErrCreateCAPool, err)
		}
		if err := injectConfigFileIntoRootfs(upperRootfsPath, "/upper/etc/ssl/certs/matchlock-ca.crt", caPool.CACertPEM()); err != nil {
			cleanupRootDisks()
			stateMgr.Unregister(id)
			return nil, errx.Wrap(ErrInjectCACert, err)
		}
	}

	var (
		subnetAlloc *state.SubnetAllocator
		subnetInfo  *state.SubnetInfo
		err         error
	)
	releaseSubnet := func() {
		if subnetAlloc != nil {
			_ = subnetAlloc.Release(id)
		}
	}

	// Allocate unique subnet for this VM when networking is enabled.
	if !noNetwork {
		subnetAlloc = state.NewSubnetAllocator()
		subnetInfo, err = subnetAlloc.Allocate(id)
		if err != nil {
			cleanupRootDisks()
			stateMgr.Unregister(id)
			return nil, errx.Wrap(ErrAllocateSubnet, err)
		}
		_ = lifecycleStore.SetResource(func(r *lifecycle.Resources) {
			r.GatewayIP = subnetInfo.GatewayIP
			r.GuestIP = subnetInfo.GuestIP
			r.SubnetCIDR = subnetInfo.Subnet
		})
	}

	backend := linux.NewLinuxBackend()

	kernelPath := opts.KernelPath
	if kernelPath == "" {
		kernelPath = DefaultKernelPath()
	}

	var extraDisks []vm.DiskConfig
	for _, d := range config.ExtraDisks {
		if err := api.ValidateGuestMount(d.GuestMount); err != nil {
			releaseSubnet()
			stateMgr.Unregister(id)
			return nil, errx.Wrap(ErrInvalidDiskCfg, err)
		}
		extraDisks = append(extraDisks, vm.DiskConfig{
			HostPath:   d.HostPath,
			GuestMount: d.GuestMount,
			ReadOnly:   d.ReadOnly,
		})
	}
	if err := validateOverlayDiskLayout(len(opts.RootfsPaths), len(extraDisks)); err != nil {
		releaseSubnet()
		stateMgr.Unregister(id)
		return nil, err
	}

	gatewayIP := ""
	guestIP := ""
	subnetCIDR := ""
	if subnetInfo != nil {
		gatewayIP = subnetInfo.GatewayIP
		guestIP = subnetInfo.GuestIP
		subnetCIDR = subnetInfo.GatewayIP + "/24"
	}

	vmConfig := &vm.VMConfig{
		ID:                  id,
		KernelPath:          kernelPath,
		RootfsPath:          bootstrapRootfsPath,
		OverlayEnabled:      true,
		OverlayLowerPaths:   opts.RootfsPaths,
		OverlayLowerFSTypes: rootfsFSTypes,
		OverlayUpperPath:    upperRootfsPath,
		CPUs:                config.Resources.CPUs,
		MemoryMB:            config.Resources.MemoryMB,
		SocketPath:          stateMgr.SocketPath(id) + ".sock",
		LogPath:             stateMgr.LogPath(id),
		VsockCID:            3,
		VsockPath:           stateMgr.Dir(id) + "/vsock.sock",
		GatewayIP:           gatewayIP,
		GuestIP:             guestIP,
		SubnetCIDR:          subnetCIDR,
		Workspace:           workspace,
		Privileged:          config.Privileged,
		ExtraDisks:          extraDisks,
		DNSServers:          config.Network.GetDNSServers(),
		Hostname:            hostname,
		AddHosts:            config.Network.AddHosts,
		MTU:                 config.Network.GetMTU(),
		NoNetwork:           noNetwork,
	}

	machine, err := backend.Create(ctx, vmConfig)
	if err != nil {
		cleanupRootDisks()
		releaseSubnet()
		stateMgr.Unregister(id)
		return nil, errx.Wrap(ErrCreateVM, err)
	}

	linuxMachine := machine.(*linux.LinuxMachine)
	if tapName := linuxMachine.TapName(); tapName != "" {
		_ = lifecycleStore.SetResource(func(r *lifecycle.Resources) {
			r.TAPName = tapName
			r.FirewallTable = "matchlock_" + tapName
			r.NATTable = "matchlock_nat_" + tapName
		})
	}

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

	overlaySnapshots, err := prepareOverlaySnapshots(config, stateMgr.Dir(id))
	if err != nil {
		machine.Close(ctx)
		cleanupRootDisks()
		releaseSubnet()
		stateMgr.Unregister(id)
		return nil, err
	}

	// Create policy engine
	policyEngine := policy.NewEngine(config.Network)

	// Create event channel
	events := make(chan api.Event, 100)

	// Set up transparent proxy for HTTP/HTTPS interception
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
			releaseSubnet()
			stateMgr.Unregister(id)
			return nil, errx.Wrap(ErrCreateProxy, err)
		}

		proxy.Start()

		fwRules = sandboxnet.NewNFTablesRules(linuxMachine.TapName(), gatewayIP, proxy.HTTPPort(), proxy.HTTPSPort(), proxy.PassthroughPort(), config.Network.GetDNSServers())
		if err := fwRules.Setup(); err != nil {
			proxy.Close()
			machine.Close(ctx)
			releaseSubnet()
			stateMgr.Unregister(id)
			return nil, errx.Wrap(ErrFirewallSetup, err)
		}
	}

	// Set up basic NAT for guest network access using nftables
	var natRules *sandboxnet.NFTablesNAT
	if !noNetwork {
		natRules = sandboxnet.NewNFTablesNAT(linuxMachine.TapName())
		if err := natRules.Setup(); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to setup NAT: %v\n", err)
			natRules = nil
		}
	}

	var vfsRoot vfs.Provider
	var vfsHooks *vfs.HookEngine
	var vfsServer *vfs.VFSServer
	var vfsStopFunc func()
	if vfsEnabled {
		// Create VFS providers
		vfsProviders := buildVFSProviders(config)
		vfsRouter := vfs.NewMountRouter(vfsProviders)
		vfsRoot = vfsRouter
		vfsHooks = buildVFSHookEngine(config)
		if vfsHooks != nil {
			attachVFSFileEvents(vfsHooks, events)
			vfsRoot = vfs.NewInterceptProvider(vfsRoot, vfsHooks)
		}

		// Create VFS server for guest FUSE daemon connections
		vfsServer = vfs.NewVFSServer(vfsRoot)

		// Start VFS server on the vsock UDS path for VFS port
		vfsSocketPath := fmt.Sprintf("%s_%d", vmConfig.VsockPath, linux.VsockPortVFS)
		vfsStopFunc, err = vfsServer.ServeUDSBackground(vfsSocketPath)
		if err != nil {
			if proxy != nil {
				proxy.Close()
			}
			if fwRules != nil {
				fwRules.Cleanup()
			}
			machine.Close(ctx)
			releaseSubnet()
			stateMgr.Unregister(id)
			return nil, errx.Wrap(ErrVFSServer, err)
		}
	}

	sb = &Sandbox{
		id:               id,
		config:           config,
		machine:          machine,
		proxy:            proxy,
		fwRules:          fwRules,
		natRules:         natRules,
		policy:           policyEngine,
		vfsRoot:          vfsRoot,
		vfsHooks:         vfsHooks,
		vfsServer:        vfsServer,
		vfsStopFunc:      vfsStopFunc,
		events:           events,
		stateMgr:         stateMgr,
		tapName:          linuxMachine.TapName(),
		caPool:           caPool,
		subnetInfo:       subnetInfo,
		subnetAlloc:      subnetAlloc,
		workspace:        workspace,
		rootfsPath:       upperRootfsPath,
		bootstrapPath:    bootstrapRootfsPath,
		overlaySnapshots: overlaySnapshots,
		lifecycle:        lifecycleStore,
	}
	if err := lifecycleStore.SetPhase(lifecycle.PhaseCreated); err != nil {
		_ = sb.Close(ctx)
		return nil, errx.Wrap(ErrLifecycleUpdate, err)
	}
	if err := lifecycleStore.SetLastError(nil); err != nil {
		_ = sb.Close(ctx)
		return nil, errx.Wrap(ErrLifecycleUpdate, err)
	}
	return sb, nil
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
	if s.lifecycle != nil {
		if err := s.lifecycle.SetPhase(lifecycle.PhaseStarting); err != nil {
			return errx.Wrap(ErrLifecycleUpdate, err)
		}
	}
	if err := s.machine.Start(ctx); err != nil {
		if s.lifecycle != nil {
			_ = s.lifecycle.SetLastError(err)
			_ = s.lifecycle.SetPhase(lifecycle.PhaseStartFailed)
		}
		return err
	}
	if s.lifecycle != nil {
		if err := s.lifecycle.SetPhase(lifecycle.PhaseRunning); err != nil {
			return errx.Wrap(ErrLifecycleUpdate, err)
		}
		if err := s.lifecycle.SetLastError(nil); err != nil {
			return errx.Wrap(ErrLifecycleUpdate, err)
		}
	}
	return nil
}

// Stop stops the sandbox VM.
func (s *Sandbox) Stop(ctx context.Context) error {
	if s.lifecycle != nil {
		if err := s.lifecycle.SetPhase(lifecycle.PhaseStopping); err != nil {
			return errx.Wrap(ErrLifecycleUpdate, err)
		}
	}
	if err := s.machine.Stop(ctx); err != nil {
		if s.lifecycle != nil {
			_ = s.lifecycle.SetLastError(err)
			_ = s.lifecycle.SetPhase(lifecycle.PhaseStopFailed)
		}
		return err
	}
	if s.lifecycle != nil {
		if err := s.lifecycle.SetPhase(lifecycle.PhaseStopped); err != nil {
			return errx.Wrap(ErrLifecycleUpdate, err)
		}
		if err := s.lifecycle.SetLastError(nil); err != nil {
			return errx.Wrap(ErrLifecycleUpdate, err)
		}
	}
	return nil
}

func (s *Sandbox) PrepareExecEnv() *api.ExecOptions {
	return prepareExecEnv(s.config, s.caPool, s.policy)
}

func (s *Sandbox) Exec(ctx context.Context, command string, opts *api.ExecOptions) (*api.ExecResult, error) {
	return execCommand(ctx, s.machine, s.config, s.caPool, s.policy, command, opts)
}

func (s *Sandbox) WriteFile(ctx context.Context, path string, content []byte, mode uint32) error {
	if s.vfsRoot == nil {
		return errx.With(ErrVFSDisabled, ": write_file %q", path)
	}
	return writeFile(s.vfsRoot, path, content, mode)
}

func (s *Sandbox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if s.vfsRoot == nil {
		return nil, errx.With(ErrVFSDisabled, ": read_file %q", path)
	}
	return readFile(s.vfsRoot, path)
}

func (s *Sandbox) ReadFileTo(ctx context.Context, path string, w io.Writer) (int64, error) {
	if s.vfsRoot == nil {
		return 0, errx.With(ErrVFSDisabled, ": read_file %q", path)
	}
	return readFileTo(s.vfsRoot, path, w)
}

func (s *Sandbox) ListFiles(ctx context.Context, path string) ([]api.FileInfo, error) {
	if s.vfsRoot == nil {
		return nil, errx.With(ErrVFSDisabled, ": list_files %q", path)
	}
	return listFiles(s.vfsRoot, path)
}

// Events returns a channel for receiving sandbox events.
func (s *Sandbox) Events() <-chan api.Event {
	return s.events
}

// Close shuts down the sandbox and releases all resources.
func (s *Sandbox) Close(ctx context.Context) error {
	var errs []error
	markCleanup := func(name string, opErr error) {
		if s.lifecycle == nil {
			return
		}
		if err := s.lifecycle.MarkCleanup(name, opErr); err != nil {
			errs = append(errs, errx.Wrap(ErrLifecycleUpdate, err))
		}
	}
	if s.lifecycle != nil {
		if err := s.lifecycle.SetPhase(lifecycle.PhaseStopping); err != nil {
			errs = append(errs, errx.Wrap(ErrLifecycleUpdate, err))
		}
		if err := s.lifecycle.SetPhase(lifecycle.PhaseCleaning); err != nil {
			errs = append(errs, errx.Wrap(ErrLifecycleUpdate, err))
		}
	}

	if s.vfsStopFunc != nil {
		s.vfsStopFunc()
		markCleanup("vfs_stop", nil)
	} else {
		markCleanup("vfs_stop", nil)
	}
	if s.vfsHooks != nil {
		s.vfsHooks.Close()
		markCleanup("vfs_hooks", nil)
	} else {
		markCleanup("vfs_hooks", nil)
	}
	if s.fwRules != nil {
		if err := s.fwRules.Cleanup(); err != nil {
			errs = append(errs, errx.Wrap(ErrFirewallCleanup, err))
			markCleanup("firewall_cleanup", err)
		} else {
			markCleanup("firewall_cleanup", nil)
		}
	} else {
		markCleanup("firewall_cleanup", nil)
	}
	if s.natRules != nil {
		if err := s.natRules.Cleanup(); err != nil {
			errs = append(errs, errx.Wrap(ErrNATCleanup, err))
			markCleanup("nat_cleanup", err)
		} else {
			markCleanup("nat_cleanup", nil)
		}
	} else {
		markCleanup("nat_cleanup", nil)
	}
	if s.proxy != nil {
		if err := s.proxy.Close(); err != nil {
			errs = append(errs, errx.Wrap(ErrProxyClose, err))
			markCleanup("proxy_close", err)
		} else {
			markCleanup("proxy_close", nil)
		}
	} else {
		markCleanup("proxy_close", nil)
	}

	// Release subnet allocation
	if s.subnetAlloc != nil {
		if err := s.subnetAlloc.Release(s.id); err != nil {
			errs = append(errs, errx.Wrap(ErrReleaseSubnet, err))
			markCleanup("subnet_release", err)
		} else {
			markCleanup("subnet_release", nil)
		}
	} else {
		markCleanup("subnet_release", nil)
	}

	close(s.events)
	markCleanup("events_close", nil)

	flushGuestDisks(s.machine)
	markCleanup("guest_sync", nil)

	if err := s.stateMgr.Unregister(s.id); err != nil {
		errs = append(errs, errx.Wrap(ErrUnregisterState, err))
		markCleanup("state_unregister", err)
	} else {
		markCleanup("state_unregister", nil)
	}
	if err := s.machine.Close(ctx); err != nil {
		errs = append(errs, errx.Wrap(ErrMachineClose, err))
		markCleanup("machine_close", err)
	} else {
		markCleanup("machine_close", nil)
	}

	var overlayCleanupErr error
	for _, snapshotPath := range s.overlaySnapshots {
		if err := os.RemoveAll(snapshotPath); err != nil {
			errs = append(errs, errx.With(ErrRemoveOverlaySnapshot, " %s: %v", snapshotPath, err))
			overlayCleanupErr = err
		}
	}
	markCleanup("overlay_snapshot_remove", overlayCleanupErr)

	// Remove writable upper disk
	if err := os.Remove(s.rootfsPath); err != nil && !os.IsNotExist(err) {
		errs = append(errs, errx.Wrap(ErrRemoveRootfs, err))
		markCleanup("rootfs_remove", err)
	} else {
		markCleanup("rootfs_remove", nil)
	}
	// Remove bootstrap disk
	if err := os.Remove(s.bootstrapPath); err != nil && !os.IsNotExist(err) {
		errs = append(errs, errx.Wrap(ErrRemoveRootfs, err))
		markCleanup("bootstrap_remove", err)
	} else {
		markCleanup("bootstrap_remove", nil)
	}

	if len(errs) > 0 {
		joined := errors.Join(errs...)
		if s.lifecycle != nil {
			_ = s.lifecycle.SetLastError(joined)
			_ = s.lifecycle.SetPhase(lifecycle.PhaseCleanupFailed)
		}
		return joined
	}
	if s.lifecycle != nil {
		if err := s.lifecycle.SetPhase(lifecycle.PhaseCleaned); err != nil {
			return errx.Wrap(ErrLifecycleUpdate, err)
		}
		if err := s.lifecycle.SetLastError(nil); err != nil {
			return errx.Wrap(ErrLifecycleUpdate, err)
		}
	}
	return nil
}

func createProvider(mount api.MountConfig) vfs.Provider {
	switch mount.Type {
	case api.MountTypeMemory:
		return vfs.NewMemoryProvider()
	case api.MountTypeHostFS:
		p := vfs.NewRealFSProvider(mount.HostPath)
		if mount.Readonly {
			return vfs.NewReadonlyProvider(p)
		}
		return p
	default:
		return vfs.NewMemoryProvider()
	}
}
