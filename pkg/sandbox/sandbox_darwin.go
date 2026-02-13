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

	"github.com/google/uuid"
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
	id          string
	config      *api.Config
	machine     vm.Machine
	netStack    *sandboxnet.NetworkStack
	policy      *policy.Engine
	vfsRoot     *vfs.MountRouter
	vfsServer   *vfs.VFSServer
	vfsStopFunc func()
	events      chan api.Event
	stateMgr    *state.Manager
	caPool      *sandboxnet.CAPool
	subnetInfo  *state.SubnetInfo
	subnetAlloc *state.SubnetAllocator
	workspace   string
	lifecycle   *lifecycle.Store
}

type Options struct {
	KernelPath    string
	InitramfsPath string
	RootfsPath    string // Required: path to the rootfs image
}

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

	subnetAlloc := state.NewSubnetAllocator()
	subnetInfo, err := subnetAlloc.Allocate(id)
	if err != nil {
		stateMgr.Unregister(id)
		return nil, errx.Wrap(ErrAllocateSubnet, err)
	}
	_ = lifecycleStore.SetResource(func(r *lifecycle.Resources) {
		r.GatewayIP = subnetInfo.GatewayIP
		r.GuestIP = subnetInfo.GuestIP
		r.SubnetCIDR = subnetInfo.Subnet
	})

	backend := darwin.NewDarwinBackend()

	kernelPath := opts.KernelPath
	if kernelPath == "" {
		kernelPath = DefaultKernelPath()
	}
	initramfsPath := opts.InitramfsPath
	if initramfsPath == "" {
		initramfsPath = DefaultInitramfsPath()
	}
	rootfsPath := opts.RootfsPath

	// Determine if we need network interception (calculated before VM creation)
	needsInterception := config.Network != nil && (len(config.Network.AllowedHosts) > 0 || len(config.Network.Secrets) > 0)

	// Create CAPool early so we can inject the cert into rootfs before the VM sees the disk
	var caPool *sandboxnet.CAPool
	if needsInterception {
		caPool, err = sandboxnet.NewCAPool()
		if err != nil {
			subnetAlloc.Release(id)
			stateMgr.Unregister(id)
			return nil, errx.Wrap(ErrCreateCAPool, err)
		}
	}

	// Copy rootfs into the VM state directory, then inject components and resize
	// before backend.Create() so VZ sees the final image.
	prebuiltRootfs := filepath.Join(stateMgr.Dir(id), "rootfs.ext4")
	if err := copyRootfsDarwin(rootfsPath, prebuiltRootfs); err != nil {
		subnetAlloc.Release(id)
		stateMgr.Unregister(id)
		return nil, errx.Wrap(ErrCopyRootfs, err)
	}
	_ = lifecycleStore.SetResource(func(r *lifecycle.Resources) {
		r.RootfsPath = prebuiltRootfs
	})
	var diskSizeMB int64
	if config.Resources != nil {
		diskSizeMB = int64(config.Resources.DiskSizeMB)
	}
	if err := prepareRootfs(prebuiltRootfs, diskSizeMB); err != nil {
		os.Remove(prebuiltRootfs)
		subnetAlloc.Release(id)
		stateMgr.Unregister(id)
		return nil, errx.Wrap(ErrPrepareRootfs, err)
	}

	// Inject CA cert into rootfs before backend.Create() attaches the disk
	if caPool != nil {
		if err := injectConfigFileIntoRootfs(prebuiltRootfs, "/etc/ssl/certs/matchlock-ca.crt", caPool.CACertPEM()); err != nil {
			os.Remove(prebuiltRootfs)
			subnetAlloc.Release(id)
			stateMgr.Unregister(id)
			return nil, errx.Wrap(ErrInjectCACert, err)
		}
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
		ID:              id,
		KernelPath:      kernelPath,
		InitramfsPath:   initramfsPath,
		RootfsPath:      prebuiltRootfs,
		CPUs:            config.Resources.CPUs,
		MemoryMB:        config.Resources.MemoryMB,
		SocketPath:      stateMgr.SocketPath(id) + ".sock",
		LogPath:         stateMgr.LogPath(id),
		GatewayIP:       subnetInfo.GatewayIP,
		GuestIP:         subnetInfo.GuestIP,
		SubnetCIDR:      subnetInfo.GatewayIP + "/24",
		Workspace:       workspace,
		UseInterception: needsInterception,
		Privileged:      config.Privileged,
		PrebuiltRootfs:  prebuiltRootfs,
		ExtraDisks:      extraDisks,
		DNSServers:      config.Network.GetDNSServers(),
	}
	_ = lifecycleStore.SetResource(func(r *lifecycle.Resources) {
		r.VsockPath = stateMgr.Dir(id) + "/vsock.sock"
	})

	machine, err := backend.Create(ctx, vmConfig)
	if err != nil {
		if prebuiltRootfs != "" {
			os.Remove(prebuiltRootfs)
		}
		subnetAlloc.Release(id)
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

	policyEngine := policy.NewEngine(config.Network)
	events := make(chan api.Event, 100)

	var netStack *sandboxnet.NetworkStack

	if needsInterception {
		networkFile := darwinMachine.NetworkFile()
		if networkFile == nil {
			machine.Close(ctx)
			subnetAlloc.Release(id)
			stateMgr.Unregister(id)
			return nil, ErrNetworkFile
		}

		netStack, err = sandboxnet.NewNetworkStack(&sandboxnet.Config{
			File:       networkFile,
			GatewayIP:  subnetInfo.GatewayIP,
			GuestIP:    subnetInfo.GuestIP,
			MTU:        1500,
			Policy:     policyEngine,
			Events:     events,
			CAPool:     caPool,
			DNSServers: config.Network.GetDNSServers(),
		})
		if err != nil {
			machine.Close(ctx)
			subnetAlloc.Release(id)
			stateMgr.Unregister(id)
			return nil, errx.Wrap(ErrNetworkStack, err)
		}
	}

	vfsProviders := buildVFSProviders(config, workspace)
	vfsRoot := vfs.NewMountRouter(vfsProviders)

	vfsServer := vfs.NewVFSServer(vfsRoot)

	vfsListener, err := darwinMachine.SetupVFSListener()
	if err != nil {
		if netStack != nil {
			netStack.Close()
		}
		machine.Close(ctx)
		subnetAlloc.Release(id)
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
		id:          id,
		config:      config,
		machine:     machine,
		netStack:    netStack,
		policy:      policyEngine,
		vfsRoot:     vfsRoot,
		vfsServer:   vfsServer,
		vfsStopFunc: vfsStopFunc,
		events:      events,
		stateMgr:    stateMgr,
		caPool:      caPool,
		subnetInfo:  subnetInfo,
		subnetAlloc: subnetAlloc,
		workspace:   workspace,
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

func copyRootfsDarwin(srcPath, dstPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		_ = os.Remove(dstPath)
		return err
	}
	return nil
}
