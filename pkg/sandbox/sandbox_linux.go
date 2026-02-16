//go:build linux

// Package sandbox provides the core sandbox VM management functionality.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/google/uuid"
	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/lifecycle"
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
	vfsRoot     vfs.Provider
	vfsHooks    *vfs.HookEngine
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
	lifecycle   *lifecycle.Store
}

// Options configures sandbox creation.
type Options struct {
	// KernelPath overrides the default kernel path
	KernelPath string
	// RootfsPath is the path to the rootfs image (required)
	RootfsPath string
}

// New creates a new sandbox VM with the given configuration.
func New(ctx context.Context, config *api.Config, opts *Options) (sb *Sandbox, retErr error) {
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

	// Create a copy of the rootfs for this VM (copy-on-write if supported)
	vmRootfsPath := stateMgr.Dir(id) + "/rootfs.ext4"
	if err := copyRootfs(opts.RootfsPath, vmRootfsPath); err != nil {
		stateMgr.Unregister(id)
		return nil, errx.Wrap(ErrCopyRootfs, err)
	}
	_ = lifecycleStore.SetResource(func(r *lifecycle.Resources) {
		r.RootfsPath = vmRootfsPath
		r.VsockPath = stateMgr.Dir(id) + "/vsock.sock"
	})

	// Inject guest runtime components and resize rootfs
	var diskSizeMB int64
	if config.Resources != nil {
		diskSizeMB = int64(config.Resources.DiskSizeMB)
	}
	if err := prepareRootfs(vmRootfsPath, diskSizeMB); err != nil {
		os.Remove(vmRootfsPath)
		stateMgr.Unregister(id)
		return nil, errx.Wrap(ErrPrepareRootfs, err)
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
			return nil, errx.Wrap(ErrCreateCAPool, err)
		}
		if err := injectConfigFileIntoRootfs(vmRootfsPath, "/etc/ssl/certs/matchlock-ca.crt", caPool.CACertPEM()); err != nil {
			os.Remove(vmRootfsPath)
			stateMgr.Unregister(id)
			return nil, errx.Wrap(ErrInjectCACert, err)
		}
	}

	// Allocate unique subnet for this VM
	subnetAlloc := state.NewSubnetAllocator()
	subnetInfo, err := subnetAlloc.Allocate(id)
	if err != nil {
		os.Remove(vmRootfsPath)
		stateMgr.Unregister(id)
		return nil, errx.Wrap(ErrAllocateSubnet, err)
	}
	_ = lifecycleStore.SetResource(func(r *lifecycle.Resources) {
		r.GatewayIP = subnetInfo.GatewayIP
		r.GuestIP = subnetInfo.GuestIP
		r.SubnetCIDR = subnetInfo.Subnet
	})

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
			return nil, errx.Wrap(ErrInvalidDiskCfg, err)
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
		MTU:        config.Network.GetMTU(),
	}

	machine, err := backend.Create(ctx, vmConfig)
	if err != nil {
		subnetAlloc.Release(id)
		stateMgr.Unregister(id)
		return nil, errx.Wrap(ErrCreateVM, err)
	}

	linuxMachine := machine.(*linux.LinuxMachine)
	_ = lifecycleStore.SetResource(func(r *lifecycle.Resources) {
		r.TAPName = linuxMachine.TapName()
		r.FirewallTable = "matchlock_" + linuxMachine.TapName()
		r.NATTable = "matchlock_nat_" + linuxMachine.TapName()
	})

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
			return nil, errx.Wrap(ErrCreateProxy, err)
		}

		proxy.Start()

		fwRules = sandboxnet.NewNFTablesRules(linuxMachine.TapName(), gatewayIP, proxy.HTTPPort(), proxy.HTTPSPort(), proxy.PassthroughPort(), config.Network.GetDNSServers())
		if err := fwRules.Setup(); err != nil {
			proxy.Close()
			machine.Close(ctx)
			subnetAlloc.Release(id)
			stateMgr.Unregister(id)
			return nil, errx.Wrap(ErrFirewallSetup, err)
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
	vfsRouter := vfs.NewMountRouter(vfsProviders)
	var vfsRoot vfs.Provider = vfsRouter
	vfsHooks := buildVFSHookEngine(config)
	if vfsHooks != nil {
		attachVFSFileEvents(vfsHooks, events)
		vfsRoot = vfs.NewInterceptProvider(vfsRoot, vfsHooks)
	}

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
		return nil, errx.Wrap(ErrVFSServer, err)
	}

	sb = &Sandbox{
		id:          id,
		config:      config,
		machine:     machine,
		proxy:       proxy,
		fwRules:     fwRules,
		natRules:    natRules,
		policy:      policyEngine,
		vfsRoot:     vfsRoot,
		vfsHooks:    vfsHooks,
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
		lifecycle:   lifecycleStore,
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

	// Remove rootfs copy to save disk space
	rootfsCopy := s.stateMgr.Dir(s.id) + "/rootfs.ext4"
	if err := os.Remove(rootfsCopy); err != nil && !os.IsNotExist(err) {
		errs = append(errs, errx.Wrap(ErrRemoveRootfs, err))
		markCleanup("rootfs_remove", err)
	} else {
		markCleanup("rootfs_remove", nil)
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
		return errx.Wrap(ErrOpenSource, err)
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	if err != nil {
		return errx.Wrap(ErrCreateDest, err)
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
		return errx.Wrap(ErrCopy, err)
	}

	return nil
}
