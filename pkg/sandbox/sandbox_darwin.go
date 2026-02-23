//go:build darwin

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/lifecycle"
	sandboxnet "github.com/jingkaihe/matchlock/pkg/net"
	"github.com/jingkaihe/matchlock/pkg/policy"
	"github.com/jingkaihe/matchlock/pkg/state"
	"github.com/jingkaihe/matchlock/pkg/vfs"
	"github.com/jingkaihe/matchlock/pkg/vm"
	"github.com/jingkaihe/matchlock/pkg/vm/darwin"
)

type Sandbox struct {
	id               string
	config           *api.Config
	machine          vm.Machine
	netStack         *sandboxnet.NetworkStack
	policy           *policy.Engine
	vfsRoot          vfs.Provider
	vfsHooks         *vfs.HookEngine
	vfsServer        *vfs.VFSServer
	vfsStopFunc      func()
	events           chan api.Event
	stateMgr         *state.Manager
	caPool           *sandboxnet.CAPool
	subnetInfo       *state.SubnetInfo
	subnetAlloc      *state.SubnetAllocator
	workspace        string
	rootfsPath       string // Writable overlay upper disk
	bootstrapPath    string // Bootstrap root disk (vda)
	overlaySnapshots []string
	lifecycle        *lifecycle.Store
}

type Options struct {
	KernelPath    string
	InitramfsPath string
	RootfsPaths   []string // Required: immutable lower image paths (base->top)
	RootfsFSTypes []string // Optional fs type per lower image (defaults to erofs).
}

func New(ctx context.Context, config *api.Config, opts *Options) (sb *Sandbox, retErr error) {
	if opts == nil {
		opts = &Options{}
	}
	if len(opts.RootfsPaths) == 0 {
		return nil, fmt.Errorf("RootfsPaths is required")
	}

	id := config.GetID()
	hostname := config.GetHostname()
	workspace := config.GetWorkspace()
	noNetwork := config.Network != nil && config.Network.NoNetwork

	if config.Network != nil {
		if err := config.Network.Validate(); err != nil {
			return nil, err
		}
	}

	stateMgr := state.NewManager()
	if err := stateMgr.Register(id, config); err != nil {
		return nil, errx.Wrap(ErrRegisterState, err)
	}
	lifecycleStore := lifecycle.NewStore(stateMgr.Dir(id))
	if err := lifecycleStore.Init(id, "virtualization.framework", stateMgr.Dir(id)); err != nil {
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
	if !noNetwork {
		subnetAlloc = state.NewSubnetAllocator()
		subnetInfo, err = subnetAlloc.Allocate(id)
		if err != nil {
			stateMgr.Unregister(id)
			return nil, errx.Wrap(ErrAllocateSubnet, err)
		}
		_ = lifecycleStore.SetResource(func(r *lifecycle.Resources) {
			r.GatewayIP = subnetInfo.GatewayIP
			r.GuestIP = subnetInfo.GuestIP
			r.SubnetCIDR = subnetInfo.Subnet
		})
	}

	backend := darwin.NewDarwinBackend()

	kernelPath := opts.KernelPath
	if kernelPath == "" {
		kernelPath = DefaultKernelPath()
	}
	initramfsPath := opts.InitramfsPath
	if initramfsPath == "" {
		initramfsPath = DefaultInitramfsPath()
	}
	rootfsPaths := opts.RootfsPaths
	rootfsFSTypes := normalizeOverlayLowerFSTypes(rootfsPaths, opts.RootfsFSTypes)
	bootstrapRootfsPath := filepath.Join(stateMgr.Dir(id), "bootstrap.ext4")
	upperRootfsPath := filepath.Join(stateMgr.Dir(id), "upper.ext4")
	cleanupRootDisks := func() {
		_ = os.Remove(bootstrapRootfsPath)
		_ = os.Remove(upperRootfsPath)
	}
	defer func() {
		if retErr != nil {
			cleanupRootDisks()
		}
	}()

	// Determine if we need network interception (calculated before VM creation)
	needsInterception := !noNetwork && config.Network != nil && (len(config.Network.AllowedHosts) > 0 || len(config.Network.Secrets) > 0)

	// Create CAPool early so we can inject the cert into rootfs before the VM sees the disk
	var caPool *sandboxnet.CAPool
	if needsInterception {
		caPool, err = sandboxnet.NewCAPool()
		if err != nil {
			releaseSubnet()
			stateMgr.Unregister(id)
			return nil, errx.Wrap(ErrCreateCAPool, err)
		}
	}

	// Prepare bootstrap root disk (vda) and writable upper disk for overlay root.
	if err := createBootstrapRootfs(bootstrapRootfsPath); err != nil {
		releaseSubnet()
		stateMgr.Unregister(id)
		return nil, errx.Wrap(ErrPrepareBootstrapRoot, err)
	}
	var diskSizeMB int64 = api.DefaultDiskSizeMB
	if config.Resources != nil && config.Resources.DiskSizeMB > 0 {
		diskSizeMB = int64(config.Resources.DiskSizeMB)
	}
	if err := createExt4Image(upperRootfsPath, diskSizeMB); err != nil {
		releaseSubnet()
		stateMgr.Unregister(id)
		return nil, errx.Wrap(ErrCreateRootfs, err)
	}
	if err := prepareOverlayUpperRootfs(upperRootfsPath); err != nil {
		releaseSubnet()
		stateMgr.Unregister(id)
		return nil, errx.Wrap(ErrPrepareRootfs, err)
	}
	_ = lifecycleStore.SetResource(func(r *lifecycle.Resources) {
		r.RootfsPath = upperRootfsPath
	})

	// Inject CA cert into writable upper before backend.Create() attaches disks.
	if caPool != nil {
		if err := injectConfigFileIntoRootfs(upperRootfsPath, "/upper/etc/ssl/certs/matchlock-ca.crt", caPool.CACertPEM()); err != nil {
			releaseSubnet()
			stateMgr.Unregister(id)
			return nil, errx.Wrap(ErrInjectCACert, err)
		}
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
	if err := validateOverlayDiskLayout(len(rootfsPaths), len(extraDisks)); err != nil {
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
		InitramfsPath:       initramfsPath,
		RootfsPath:          bootstrapRootfsPath,
		OverlayEnabled:      true,
		OverlayLowerPaths:   rootfsPaths,
		OverlayLowerFSTypes: rootfsFSTypes,
		OverlayUpperPath:    upperRootfsPath,
		CPUs:                config.Resources.CPUs,
		MemoryMB:            config.Resources.MemoryMB,
		SocketPath:          stateMgr.SocketPath(id) + ".sock",
		LogPath:             stateMgr.LogPath(id),
		GatewayIP:           gatewayIP,
		GuestIP:             guestIP,
		SubnetCIDR:          subnetCIDR,
		Workspace:           workspace,
		UseInterception:     needsInterception,
		Privileged:          config.Privileged,
		PrebuiltRootfs:      bootstrapRootfsPath,
		ExtraDisks:          extraDisks,
		DNSServers:          config.Network.GetDNSServers(),
		Hostname:            hostname,
		AddHosts:            config.Network.AddHosts,
		MTU:                 config.Network.GetMTU(),
		NoNetwork:           noNetwork,
	}
	_ = lifecycleStore.SetResource(func(r *lifecycle.Resources) {
		r.VsockPath = stateMgr.Dir(id) + "/vsock.sock"
	})

	// Prepare overlay snapshots before creating the VZ VM process.
	// On macOS, large snapshot work between Create() and Start() can cause
	// startup instability in Virtualization.framework.
	overlaySnapshots, err := prepareOverlaySnapshots(config, stateMgr.Dir(id))
	if err != nil {
		releaseSubnet()
		stateMgr.Unregister(id)
		return nil, err
	}

	machine, err := backend.Create(ctx, vmConfig)
	if err != nil {
		releaseSubnet()
		stateMgr.Unregister(id)
		return nil, errx.Wrap(ErrCreateVM, err)
	}

	darwinMachine := machine.(*darwin.DarwinMachine)

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

	policyEngine := policy.NewEngine(config.Network, nil)
	events := make(chan api.Event, 100)

	var netStack *sandboxnet.NetworkStack

	if needsInterception {
		networkFile := darwinMachine.NetworkFile()
		if networkFile == nil {
			machine.Close(ctx)
			releaseSubnet()
			stateMgr.Unregister(id)
			return nil, ErrNetworkFile
		}

		netStack, err = sandboxnet.NewNetworkStack(&sandboxnet.Config{
			File:       networkFile,
			GatewayIP:  gatewayIP,
			GuestIP:    guestIP,
			MTU:        uint32(config.Network.GetMTU()),
			Policy:     policyEngine,
			Events:     events,
			CAPool:     caPool,
			DNSServers: config.Network.GetDNSServers(),
			Logger:     nil,
		})
		if err != nil {
			machine.Close(ctx)
			releaseSubnet()
			stateMgr.Unregister(id)
			return nil, errx.Wrap(ErrNetworkStack, err)
		}
	}

	vfsProviders := buildVFSProviders(config, workspace)
	vfsRouter := vfs.NewMountRouter(vfsProviders)
	var vfsRoot vfs.Provider = vfsRouter
	vfsHooks := buildVFSHookEngine(config)
	if vfsHooks != nil {
		attachVFSFileEvents(vfsHooks, events)
		vfsRoot = vfs.NewInterceptProvider(vfsRoot, vfsHooks)
	}

	vfsServer := vfs.NewVFSServer(vfsRoot)

	vfsListener, err := darwinMachine.SetupVFSListener()
	if err != nil {
		if netStack != nil {
			netStack.Close()
		}
		machine.Close(ctx)
		releaseSubnet()
		stateMgr.Unregister(id)
		return nil, errx.Wrap(ErrVFSListener, err)
	}

	vfsStopCh := make(chan struct{})
	vfsStopFunc := func() {
		close(vfsStopCh)
		vfsListener.Close()
	}

	go func() {
		for {
			select {
			case <-vfsStopCh:
				return
			default:
				conn, err := vfsListener.Accept()
				if err != nil {
					if err == net.ErrClosed {
						return
					}
					continue
				}
				go vfsServer.HandleConnection(conn)
			}
		}
	}()

	sb = &Sandbox{
		id:               id,
		config:           config,
		machine:          machine,
		netStack:         netStack,
		policy:           policyEngine,
		vfsRoot:          vfsRoot,
		vfsHooks:         vfsHooks,
		vfsServer:        vfsServer,
		vfsStopFunc:      vfsStopFunc,
		events:           events,
		stateMgr:         stateMgr,
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

func (s *Sandbox) ID() string                 { return s.id }
func (s *Sandbox) Config() *api.Config        { return s.config }
func (s *Sandbox) Workspace() string          { return s.workspace }
func (s *Sandbox) Machine() vm.Machine        { return s.machine }
func (s *Sandbox) Policy() *policy.Engine     { return s.policy }
func (s *Sandbox) CAPool() *sandboxnet.CAPool { return s.caPool }

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

func (s *Sandbox) Events() <-chan api.Event {
	return s.events
}

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
	if s.netStack != nil {
		if err := s.netStack.Close(); err != nil {
			errs = append(errs, errx.Wrap(ErrNetworkStack, err))
			markCleanup("netstack_close", err)
		} else {
			markCleanup("netstack_close", nil)
		}
	} else {
		markCleanup("netstack_close", nil)
	}

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

	var overlayCleanupErr error
	for _, snapshotPath := range s.overlaySnapshots {
		if err := os.RemoveAll(snapshotPath); err != nil {
			errs = append(errs, errx.With(ErrRemoveOverlaySnapshot, " %s: %v", snapshotPath, err))
			overlayCleanupErr = err
		}
	}
	markCleanup("overlay_snapshot_remove", overlayCleanupErr)

	if err := os.Remove(s.rootfsPath); err != nil && !os.IsNotExist(err) {
		errs = append(errs, errx.Wrap(ErrRemoveRootfs, err))
		markCleanup("rootfs_remove", err)
	} else {
		markCleanup("rootfs_remove", nil)
	}
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
