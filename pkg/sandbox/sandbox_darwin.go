//go:build darwin

package sandbox

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"

	"github.com/google/uuid"
	"github.com/jingkaihe/matchlock/pkg/api"
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
}

type Options struct {
	KernelPath    string
	InitramfsPath string
	RootfsPath    string // Required: path to the rootfs image
}

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

	subnetAlloc := state.NewSubnetAllocator()
	subnetInfo, err := subnetAlloc.Allocate(id)
	if err != nil {
		stateMgr.Unregister(id)
		return nil, fmt.Errorf("failed to allocate subnet: %w", err)
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
			return nil, fmt.Errorf("failed to create CA pool: %w", err)
		}
	}

	// Copy rootfs, inject matchlock components, and resize before backend.Create()
	// so the VZ disk attachment sees the final size with all components in place
	prebuiltRootfs, err := darwin.CopyRootfsToTemp(rootfsPath)
	if err != nil {
		subnetAlloc.Release(id)
		stateMgr.Unregister(id)
		return nil, fmt.Errorf("failed to copy rootfs: %w", err)
	}
	var diskSizeMB int64
	if config.Resources != nil {
		diskSizeMB = int64(config.Resources.DiskSizeMB)
	}
	if err := prepareRootfs(prebuiltRootfs, diskSizeMB); err != nil {
		os.Remove(prebuiltRootfs)
		subnetAlloc.Release(id)
		stateMgr.Unregister(id)
		return nil, fmt.Errorf("failed to prepare rootfs: %w", err)
	}

	// Inject CA cert into rootfs before backend.Create() attaches the disk
	if caPool != nil {
		if err := injectConfigFileIntoRootfs(prebuiltRootfs, "/etc/ssl/certs/matchlock-ca.crt", caPool.CACertPEM()); err != nil {
			os.Remove(prebuiltRootfs)
			subnetAlloc.Release(id)
			stateMgr.Unregister(id)
			return nil, fmt.Errorf("failed to inject CA cert into rootfs: %w", err)
		}
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
		ID:              id,
		KernelPath:      kernelPath,
		InitramfsPath:   initramfsPath,
		RootfsPath:      rootfsPath,
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

	machine, err := backend.Create(ctx, vmConfig)
	if err != nil {
		if prebuiltRootfs != "" {
			os.Remove(prebuiltRootfs)
		}
		subnetAlloc.Release(id)
		stateMgr.Unregister(id)
		return nil, fmt.Errorf("failed to create VM: %w", err)
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
			return nil, fmt.Errorf("failed to get network file")
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
			return nil, fmt.Errorf("failed to create network stack: %w", err)
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
		return nil, fmt.Errorf("failed to setup VFS listener: %w", err)
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

	return &Sandbox{
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
	}, nil
}

func (s *Sandbox) ID() string                 { return s.id }
func (s *Sandbox) Config() *api.Config        { return s.config }
func (s *Sandbox) Workspace() string          { return s.workspace }
func (s *Sandbox) Machine() vm.Machine        { return s.machine }
func (s *Sandbox) Policy() *policy.Engine     { return s.policy }
func (s *Sandbox) CAPool() *sandboxnet.CAPool { return s.caPool }

func (s *Sandbox) Start(ctx context.Context) error {
	return s.machine.Start(ctx)
}

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

func (s *Sandbox) Events() <-chan api.Event {
	return s.events
}

func (s *Sandbox) Close(ctx context.Context) error {
	var errs []error

	if s.vfsStopFunc != nil {
		s.vfsStopFunc()
	}
	if s.netStack != nil {
		s.netStack.Close()
	}

	if s.subnetAlloc != nil {
		s.subnetAlloc.Release(s.id)
	}

	close(s.events)
	s.stateMgr.Unregister(s.id)
	if err := s.machine.Close(ctx); err != nil {
		errs = append(errs, fmt.Errorf("machine close: %w", err))
	}

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
