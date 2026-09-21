//go:build linux

// Package sandbox provides the core sandbox VM management functionality.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/kvm"
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
	dnsForwarder     *sandboxnet.DNSForwarder
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
	swapPath         string // Ephemeral swap backing image ("" when swap is off)
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

// interceptionWiring is the per-VM addressing the sandbox hands to every
// consumer of the guest's network link: the backend VMConfig (the TAP addresses
// plus the guest's ip=/matchlock.ipv6= boot args), the proxy's IPv6 bind
// address, the DNS forwarder's IPv6 bind address and the ip6 nftables table.
// Deriving it in ONE place from the subnet lease is what keeps those consumers
// from drifting apart - the guest's boot arg, the address the proxy binds and
// the ip6 DNAT target all come from here.
//
// The IPv6 half is live only when host-side interception is active
// (interceptionEnabled): a plain NAT sandbox and a --no-network sandbox keep
// the IPv4-only shape, so their backend config is byte-identical to before.
type interceptionWiring struct {
	gatewayIPv4 string // host TAP IPv4 address (proxy/DNS bind, ip table gateway)
	guestIPv4   string // guest IPv4 address (boot arg)
	subnetCIDR  string // TAP address and prefix, e.g. 192.168.100.1/24
	gatewayIPv6 string // guest-visible IPv6 gateway; "" = IPv6 path not wired
	guestIPv6   string // guest IPv6 address (boot arg)
	subnet6CIDR string // network form (fd00:100::/64), never an interface address
	tapName     string // TAP the ip6 table is built for; "" until the VM exists
}

// newInterceptionWiring derives the wiring from the VM's subnet lease.
// intercept is the sandbox's needsProxy decision: only an intercepted sandbox
// gets IPv6 addressing at all, because the per-TAP ip6 table is what keeps guest
// IPv6 inside the policy path. A nil lease (--no-network) yields an empty,
// fully inert wiring.
func newInterceptionWiring(info *state.SubnetInfo, intercept bool) interceptionWiring {
	var w interceptionWiring
	if info == nil {
		return w
	}
	w.gatewayIPv4 = info.GatewayIP
	w.guestIPv4 = info.GuestIP
	w.subnetCIDR = info.GatewayIP + "/24"
	if intercept {
		w.gatewayIPv6 = info.GatewayIPv6
		w.guestIPv6 = info.GuestIPv6
		w.subnet6CIDR = info.Subnet6
	}
	return w
}

// interceptionEnabled reports whether the host-side interception stack runs for
// this sandbox. It is the single predicate behind the IPv6 wiring: the v6
// gateway is empty unless interception is active, so no consumer (backend,
// proxy, DNS forwarder, ip6 table) can be wired while another is not.
func (w interceptionWiring) interceptionEnabled() bool {
	return w.gatewayIPv4 != "" && w.gatewayIPv6 != ""
}

// applyToVMConfig copies the link into the backend config. The IPv6 fields stay
// empty unless interception is active, which is what keeps the generated kernel
// args and the TAP configuration unchanged for IPv4-only and --no-network
// sandboxes.
func (w interceptionWiring) applyToVMConfig(cfg *vm.VMConfig) {
	cfg.GatewayIP = w.gatewayIPv4
	cfg.GuestIP = w.guestIPv4
	cfg.SubnetCIDR = w.subnetCIDR
	cfg.GatewayIPv6 = w.gatewayIPv6
	cfg.GuestIPv6 = w.guestIPv6
	cfg.Subnet6CIDR = w.subnet6CIDR
}

// firewallTableV6 is the ip6 table the per-TAP rules install, or "" when the
// IPv6 path is not wired (or the VM has no TAP yet). It is recorded in the
// lifecycle resources so reconcile can remove an orphaned ip6 table.
func (w interceptionWiring) firewallTableV6() string {
	if w.tapName == "" || w.gatewayIPv6 == "" {
		return ""
	}
	return sandboxnet.FirewallTableV6Name(w.tapName)
}

// interceptionDeps groups the constructors of the interception stack. They are
// fields rather than direct calls so the IPv6 wiring (bind addresses, the ip6
// redirect target, the ports the ip6 rules carry) is unit-testable without a
// VM, a TAP or root privileges.
type interceptionDeps struct {
	newProxy func(*sandboxnet.ProxyConfig) (*sandboxnet.TransparentProxy, error)
	newDNS   func(bindAddrV4, bindAddrV6 string, dnsServers []string) (*sandboxnet.DNSForwarder, error)
}

// defaultInterceptionDeps wires the production constructors.
func defaultInterceptionDeps() interceptionDeps {
	return interceptionDeps{
		newProxy: sandboxnet.NewTransparentProxy,
		newDNS:   sandboxnet.NewDualStackDNSForwarder,
	}
}

// interceptionInputs are the non-addressing inputs of the interception stack.
type interceptionInputs struct {
	policy     *policy.Engine
	events     chan api.Event
	caPool     *sandboxnet.CAPool
	dnsServers []string
}

// provisionInterception builds and starts the interception stack for one VM:
// the proxy (IPv4 listeners, plus IPv6 listeners on the guest's gateway when the
// v6 path is wired), the dual-stack DNS forwarder and the per-TAP nftables
// rules (the IPv4 table and the ip6 table pointed at the gateway, whose catches
// are redirected to the very ports the two listeners reported).
//
// The rules are NOT installed here - the caller owns Setup/Cleanup so the
// rollback ordering stays in one place. Every failure after the listeners exist
// closes them again: a failed DNS forwarder (which itself closes a partially
// bound IPv6 socket) closes the proxy, including its IPv6 listeners, so the
// caller only has to close the machine and release the subnet and state entry.
func provisionInterception(deps interceptionDeps, w interceptionWiring, in interceptionInputs) (proxy *sandboxnet.TransparentProxy, dnsForwarder *sandboxnet.DNSForwarder, rules *sandboxnet.NFTablesRules, err error) {
	proxy, err = deps.newProxy(&sandboxnet.ProxyConfig{
		BindAddr:   w.gatewayIPv4,
		BindAddrV6: w.gatewayIPv6,
		Policy:     in.policy,
		Events:     in.events,
		CAPool:     in.caPool,
	})
	if err != nil {
		return nil, nil, nil, errx.Wrap(ErrCreateProxy, err)
	}
	proxy.Start()

	dnsForwarder, err = deps.newDNS(w.gatewayIPv4, w.gatewayIPv6, in.dnsServers)
	if err != nil {
		proxy.Close()
		return nil, nil, nil, errx.Wrap(ErrCreateProxy, err)
	}

	rules = sandboxnet.NewNFTablesRules(w.tapName, w.gatewayIPv4, proxy.HTTPPort(), proxy.HTTPSPort(), proxy.PassthroughPort(), in.dnsServers)
	// One redirect target per service: the IPv4 and the ip6 DNS DNAT rules both
	// point at this forwarder port.
	rules.SetDNSForwarderPort(dnsForwarder.Port())
	// An empty gateway leaves the ip6 table fail-closed: no redirect at all,
	// every guest IPv6 packet dropped.
	rules.SetGatewayIPv6(w.gatewayIPv6)

	return proxy, dnsForwarder, rules, nil
}

// New creates a new sandbox VM with the given configuration.
func New(ctx context.Context, config *api.Config, opts *Options) (sb *Sandbox, retErr error) {
	if opts == nil {
		opts = &Options{}
	}
	if len(opts.RootfsPaths) == 0 {
		return nil, fmt.Errorf("RootfsPaths is required")
	}
	if err := config.Validate(); err != nil {
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

	// Choose the VM backend before initializing the lifecycle record so its
	// backend label is accurate. Prefer the KVM-accelerated Firecracker backend
	// and fall back to QEMU TCG only when KVM is definitively unavailable.
	backendKind, selectionErr := selectBackendKind(kvm.Check(), qemuAvailable)
	if selectionErr != nil {
		stateMgr.Unregister(id)
		return nil, errx.Wrap(ErrCreateVM, selectionErr)
	}
	// Validate backend constraints before provisioning networking or injecting
	// certificates so unsupported configurations fail without resource side effects.
	if err := validateBackendConstraints(backendKind, config); err != nil {
		stateMgr.Unregister(id)
		return nil, errx.Wrap(ErrCreateVM, err)
	}
	if err := lifecycleStore.Init(id, backendKind.String(), stateMgr.Dir(id)); err != nil {
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
	var swapPath string
	cleanupRootDisks := func() {
		_ = os.Remove(bootstrapRootfsPath)
		_ = os.Remove(upperRootfsPath)
		if swapPath != "" {
			_ = os.Remove(swapPath)
		}
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

	if config.Resources == nil {
		config.Resources = &api.Resources{CPUs: api.DefaultCPUs, MemoryMB: api.DefaultMemoryMB}
	}
	vcpus, ok := api.VCPUCount(config.Resources.CPUs)
	if !ok {
		stateMgr.Unregister(id)
		return nil, errx.With(ErrCreateVM, ": cpus must be a finite number > 0")
	}
	hostCPUs := runtime.NumCPU()
	if vcpus > hostCPUs {
		stateMgr.Unregister(id)
		return nil, errx.With(ErrCreateVM, ": cpus must be <= host cpus (%d)", hostCPUs)
	}

	// Create CAPool early and inject cert into writable upper before VM creation
	needsProxy := !noNetwork && config.Network != nil && (config.Network.Intercept || config.Network.Interception != nil || len(config.Network.AllowedHosts) > 0 || len(config.Network.Secrets) > 0)
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

	// The lease decides the guest's IPv4 AND IPv6 addressing for every consumer
	// (backend config, proxy, DNS forwarder, ip6 table). Interception is the
	// gate for the IPv6 half, so a plain NAT or --no-network sandbox is
	// unchanged.
	wiring := newInterceptionWiring(subnetInfo, needsProxy)

	backend := defaultVMBackendFactory(backendKind)

	kernelPath, err := resolveLinuxKernelForConfig(ctx, config, opts, lifecycleStore, backendKind)
	if err != nil {
		releaseSubnet()
		cleanupRootDisks()
		stateMgr.Unregister(id)
		return nil, errx.Wrap(ErrCreateVM, err)
	}

	extraDisks, err := buildExtraDiskConfigs(config.ExtraDisks)
	if err != nil {
		releaseSubnet()
		stateMgr.Unregister(id)
		return nil, err
	}
	if config.Resources != nil && config.Resources.SwapMB > 0 {
		swapPath = stateMgr.Dir(id) + "/swap.raw"
	}
	if err := validateOverlayDiskLayout(len(opts.RootfsPaths), len(extraDisks), swapPath != ""); err != nil {
		releaseSubnet()
		stateMgr.Unregister(id)
		return nil, err
	}
	if swapPath != "" {
		swapDisk, err := provisionSwapDisk(swapPath, config.Resources.SwapMB)
		if err != nil {
			cleanupRootDisks()
			releaseSubnet()
			stateMgr.Unregister(id)
			return nil, err
		}
		extraDisks = append(extraDisks, swapDisk)
	}

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
		Workspace:           workspace,
		ExactMounts:         exactFUSEMountpoints(config),
		Privileged:          config.Privileged,
		ExtraDisks:          extraDisks,
		DNSServers:          config.Network.GetDNSServers(),
		Hostname:            hostname,
		AddHosts:            config.Network.AddHosts,
		MTU:                 config.Network.GetMTU(),
		NoNetwork:           noNetwork,
	}
	// Addressing comes from the wiring: IPv4 always (when networking is on),
	// IPv6 only for an intercepted sandbox, and nothing at all for
	// --no-network.
	wiring.applyToVMConfig(vmConfig)

	machine, err := backend.Create(ctx, vmConfig)
	if err != nil {
		cleanupRootDisks()
		releaseSubnet()
		stateMgr.Unregister(id)
		return nil, errx.Wrap(ErrCreateVM, err)
	}

	// TapName is set only by the Firecracker backend; the QEMU backend never
	// configures TAP networking (it is a --no-network fallback). A type
	// assertion on *linux.LinuxMachine would panic for QEMU, so read it through
	// a small interface that both machines satisfy (LinuxMachine and
	// qemu.Machine implement TapName()).
	var tapName string
	if tm, ok := machine.(interface{ TapName() string }); ok {
		tapName = tm.TapName()
	}
	if tapName != "" {
		// Record the TAP and BOTH families' interception tables for this TAP,
		// so an interrupted create leaves nothing for reconcile to hunt for.
		wiring.tapName = tapName
		_ = lifecycleStore.SetResource(func(r *lifecycle.Resources) {
			r.TAPName = tapName
			r.FirewallTable = sandboxnet.FirewallTableName(tapName)
			r.FirewallTableV6 = wiring.firewallTableV6()
			r.NATTable = "matchlock_nat_" + tapName
		})
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

	var proxy *sandboxnet.TransparentProxy
	var dnsForwarder *sandboxnet.DNSForwarder
	var fwRules FirewallRules

	if needsProxy {
		if !wiring.interceptionEnabled() {
			machine.Close(ctx)
			releaseSubnet()
			stateMgr.Unregister(id)
			return nil, errx.With(ErrCreateProxy, ": missing gateway IP for proxy bind")
		}

		// One call builds the whole stack from the wiring: the proxy (IPv4 and
		// IPv6 listeners), the dual-stack DNS forwarder and the per-TAP rules
		// whose ip6 table redirects to those exact ports. A failure inside
		// closes whatever was already opened, so only the VM, the subnet and
		// the state entry are left for this path to release.
		var nfRules *sandboxnet.NFTablesRules
		proxy, dnsForwarder, nfRules, err = provisionInterception(defaultInterceptionDeps(), wiring, interceptionInputs{
			policy:     policyEngine,
			events:     events,
			caPool:     caPool,
			dnsServers: config.Network.GetDNSServers(),
		})
		if err != nil {
			machine.Close(ctx)
			releaseSubnet()
			stateMgr.Unregister(id)
			return nil, err
		}
		fwRules = nfRules
		if err := fwRules.Setup(); err != nil {
			dnsForwarder.Close()
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
		natRules = sandboxnet.NewNFTablesNAT(tapName)
		if err := natRules.Setup(); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to setup NAT: %v\n", err)
			natRules = nil
		}
	}

	cleanupVM := func() {
		if proxy != nil {
			proxy.Close()
		}
		if dnsForwarder != nil {
			dnsForwarder.Close()
		}
		if fwRules != nil {
			fwRules.Cleanup()
		}
		if natRules != nil {
			natRules.Cleanup()
		}
		machine.Close(ctx)
		releaseSubnet()
		stateMgr.Unregister(id)
	}

	var vfsRoot vfs.Provider
	var vfsHooks *vfs.HookEngine
	var vfsServer *vfs.VFSServer
	var vfsStopFunc func()
	if vfsEnabled {
		// Create VFS providers
		vfsProviders, err := buildVFSProviders(config)
		if err != nil {
			cleanupVM()
			return nil, err
		}
		vfsRouter := vfs.NewMountRouter(vfsProviders)
		vfsRoot = vfsRouter
		vfsHooks = buildVFSHookEngine(config)
		if vfsHooks != nil {
			attachVFSFileEvents(vfsHooks, events)
			vfsRoot = vfs.NewInterceptProvider(vfsRoot, vfsHooks)
		}

		// Create VFS server for guest FUSE daemon connections
		vfsServer = vfs.NewVFSServer(vfsRoot)

		// Start the VFS server. Firecracker/Darwin expose port 5001 as a UDS
		// (vmConfig.VsockPath_5001). The QEMU backend instead serves a per-sandbox
		// AF_VSOCK listener: guest-fused dials the kernel-assigned port passed via
		// matchlock.vfs_port. Both paths are isolated per-sandbox.
		if vfsListener, ok := machine.(interface{ VFSListener() (net.Listener, error) }); ok {
			ln, lErr := vfsListener.VFSListener()
			if lErr != nil {
				cleanupVM()
				return nil, errx.Wrap(ErrVFSServer, lErr)
			}
			vfsStopFunc = vfsServer.ServeListenerBackground(ln)
		} else {
			vfsSocketPath := fmt.Sprintf("%s_%d", vmConfig.VsockPath, linux.VsockPortVFS)
			vfsStopFunc, err = vfsServer.ServeUDSBackground(vfsSocketPath)
			if err != nil {
				cleanupVM()
				return nil, errx.Wrap(ErrVFSServer, err)
			}
		}
	}

	sb = &Sandbox{
		id:               id,
		config:           config,
		machine:          machine,
		proxy:            proxy,
		dnsForwarder:     dnsForwarder,
		fwRules:          fwRules,
		natRules:         natRules,
		policy:           policyEngine,
		vfsRoot:          vfsRoot,
		vfsHooks:         vfsHooks,
		vfsServer:        vfsServer,
		vfsStopFunc:      vfsStopFunc,
		events:           events,
		stateMgr:         stateMgr,
		tapName:          tapName,
		caPool:           caPool,
		subnetInfo:       subnetInfo,
		subnetAlloc:      subnetAlloc,
		workspace:        workspace,
		rootfsPath:       upperRootfsPath,
		bootstrapPath:    bootstrapRootfsPath,
		swapPath:         swapPath,
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

func (s *Sandbox) AddAllowedHosts(ctx context.Context, hosts []string) ([]string, error) {
	if s.proxy == nil {
		return nil, errx.With(ErrAllowListUnavailable, ": sandbox was started without network interception")
	}
	if len(hosts) == 0 {
		return nil, errx.With(ErrAllowListHosts, ": no hosts provided")
	}
	return s.policy.AddAllowedHosts(hosts...), nil
}

func (s *Sandbox) RemoveAllowedHosts(ctx context.Context, hosts []string) ([]string, error) {
	if s.proxy == nil {
		return nil, errx.With(ErrAllowListUnavailable, ": sandbox was started without network interception")
	}
	if len(hosts) == 0 {
		return nil, errx.With(ErrAllowListHosts, ": no hosts provided")
	}
	return s.policy.RemoveAllowedHosts(hosts...), nil
}

func (s *Sandbox) AllowedHosts(ctx context.Context) ([]string, error) {
	if s.proxy == nil {
		return nil, errx.With(ErrAllowListUnavailable, ": sandbox was started without network interception")
	}
	return s.policy.AllowedHosts(), nil
}

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

func (s *Sandbox) ExecInteractive(ctx context.Context, command string, opts *api.ExecOptions, rows, cols uint16, stdin io.Reader, stdout io.Writer, resizeCh <-chan [2]uint16) (int, error) {
	interactiveMachine, ok := s.machine.(vm.InteractiveMachine)
	if !ok {
		return 1, errx.With(ErrInteractiveUnsupported, ": VM backend does not support interactive exec")
	}

	opts = prepareExecOptions(s.config, s.caPool, s.policy, opts)
	return interactiveMachine.ExecInteractive(ctx, command, opts, rows, cols, stdin, stdout, resizeCh)
}

func (s *Sandbox) WriteFile(ctx context.Context, path string, content []byte, mode uint32) error {
	return s.machine.WriteFile(ctx, path, content, mode)
}

func (s *Sandbox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	return s.machine.ReadFile(ctx, path)
}

func (s *Sandbox) ReadFileTo(ctx context.Context, path string, w io.Writer) (int64, error) {
	return readFileTo(ctx, s.machine, path, w)
}

func (s *Sandbox) ListFiles(ctx context.Context, path string) ([]api.FileInfo, error) {
	return s.machine.ListFiles(ctx, path)
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
	if s.vfsRoot != nil {
		if err := vfs.CloseProvider(s.vfsRoot); err != nil {
			errs = append(errs, errx.Wrap(ErrVFSServer, err))
			markCleanup("vfs_root_close", err)
		} else {
			markCleanup("vfs_root_close", nil)
		}
	} else {
		markCleanup("vfs_root_close", nil)
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

	if s.dnsForwarder != nil {
		_ = s.dnsForwarder.Close()
		s.dnsForwarder = nil
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
	// Remove ephemeral swap image
	if s.swapPath != "" {
		if err := os.Remove(s.swapPath); err != nil && !os.IsNotExist(err) {
			errs = append(errs, errx.Wrap(ErrRemoveRootfs, err))
			markCleanup("swap_remove", err)
		} else {
			markCleanup("swap_remove", nil)
		}
	} else {
		markCleanup("swap_remove", nil)
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
