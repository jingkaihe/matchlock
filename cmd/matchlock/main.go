package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/google/uuid"
	"github.com/jingkaihe/matchlock/pkg/api"
	sandboxnet "github.com/jingkaihe/matchlock/pkg/net"
	"github.com/jingkaihe/matchlock/pkg/policy"
	"github.com/jingkaihe/matchlock/pkg/rpc"
	"github.com/jingkaihe/matchlock/pkg/state"
	"github.com/jingkaihe/matchlock/pkg/vfs"
	"github.com/jingkaihe/matchlock/pkg/vm"
	"github.com/jingkaihe/matchlock/pkg/vm/linux"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "run":
		cmdRun(os.Args[2:])
	case "list":
		cmdList(os.Args[2:])
	case "get":
		cmdGet(os.Args[2:])
	case "kill":
		cmdKill(os.Args[2:])
	case "rm":
		cmdRemove(os.Args[2:])
	case "prune":
		cmdPrune(os.Args[2:])
	case "--rpc":
		cmdRPC(os.Args[2:])
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`Usage: matchlock <command> [options]

Commands:
  run <command>     Run a command in a new sandbox
  list              List all sandboxes
  get <id>          Get details of a sandbox
  kill <id>         Kill a running sandbox
  rm <id>           Remove a stopped sandbox
  prune             Remove all stopped sandboxes
  --rpc             Run in RPC mode (for programmatic access)

Options:
  -it                    Interactive mode with TTY (like docker -it)
  --image <name>         Image variant (minimal, standard, full)
  --allow-host <host>    Add host to allowlist (can be repeated, supports wildcards)
  -v <host:guest[:ro]>   Mount host directory into sandbox (can be repeated)
  --cpus <n>             Number of CPUs
  --memory <mb>          Memory in MB
  --timeout <s>          Timeout in seconds

Volume Mounts (-v):
  All mounts appear under /workspace in the guest:
  ./mycode:/workspace              Mount to /workspace root
  ./data:/data                     Mounts to /workspace/data
  /host/path:/workspace/subdir:ro  Read-only mount

Wildcard Patterns for --allow-host:
  *                      Allow all hosts
  *.example.com          Allow all subdomains (api.example.com, a.b.example.com)
  api-*.example.com      Allow pattern match (api-v1.example.com, api-prod.example.com)

Examples:
  matchlock run python script.py
  matchlock run -it python3                              # Interactive Python
  matchlock run -it sh                                   # Interactive shell
  matchlock run -v ./mycode:/workspace python /workspace/script.py
  matchlock run -v ./data:/data ls /workspace/data       # /data -> /workspace/data
  matchlock run --allow-host "*.openai.com" python agent.py
  matchlock list
  matchlock kill vm-abc123`)
}

func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	image := fs.String("image", "standard", "Image variant")
	cpus := fs.Int("cpus", 1, "Number of CPUs")
	memory := fs.Int("memory", 512, "Memory in MB")
	timeout := fs.Int("timeout", 300, "Timeout in seconds")
	interactive := fs.Bool("it", false, "Interactive mode with TTY")
	var allowHosts stringSlice
	fs.Var(&allowHosts, "allow-host", "Allowed hosts")
	var volumes stringSlice
	fs.Var(&volumes, "v", "Volume mount (host:guest or host:guest:ro)")

	fs.Parse(args)

	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "Error: command required")
		os.Exit(1)
	}

	command := fs.Args()[0]
	if len(fs.Args()) > 1 {
		for _, arg := range fs.Args()[1:] {
			command += " " + arg
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeout)*time.Second)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	// Parse volume mounts
	var vfsConfig *api.VFSConfig
	if len(volumes) > 0 {
		mounts := make(map[string]api.MountConfig)
		for _, vol := range volumes {
			hostPath, guestPath, readonly, err := parseVolumeMount(vol)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: invalid volume mount %q: %v\n", vol, err)
				os.Exit(1)
			}
			mounts[guestPath] = api.MountConfig{
				Type:     "real_fs",
				HostPath: hostPath,
				Readonly: readonly,
			}
		}
		vfsConfig = &api.VFSConfig{Mounts: mounts}
	}

	config := &api.Config{
		Image: *image,
		Resources: &api.Resources{
			CPUs:           *cpus,
			MemoryMB:       *memory,
			TimeoutSeconds: *timeout,
		},
		Network: &api.NetworkConfig{
			AllowedHosts:    allowHosts,
			BlockPrivateIPs: true,
		},
		VFS: vfsConfig,
	}

	vm, err := createVM(ctx, config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating VM: %v\n", err)
		os.Exit(1)
	}

	if err := vm.Start(ctx); err != nil {
		vm.Close()
		fmt.Fprintf(os.Stderr, "Error starting VM: %v\n", err)
		os.Exit(1)
	}

	if *interactive {
		exitCode := runInteractive(ctx, vm, command)
		vm.Close()
		os.Exit(exitCode)
	}

	result, err := vm.Exec(ctx, command, nil)
	if err != nil {
		vm.Close()
		fmt.Fprintf(os.Stderr, "Error executing command: %v\n", err)
		os.Exit(1)
	}

	os.Stdout.Write(result.Stdout)
	os.Stderr.Write(result.Stderr)
	
	// Close VM before exit (os.Exit doesn't run deferred functions)
	vm.Close()
	os.Exit(result.ExitCode)
}

func runInteractive(ctx context.Context, vm *sandboxVM, command string) int {
	// Check if stdin is a terminal
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintln(os.Stderr, "Error: -it requires a TTY")
		return 1
	}

	// Get terminal size
	cols, rows, err := term.GetSize(int(os.Stdin.Fd()))
	if err != nil {
		rows, cols = 24, 80 // defaults
	}

	// Put terminal in raw mode
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error setting raw mode: %v\n", err)
		return 1
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	// Handle SIGWINCH for terminal resize
	resizeCh := make(chan [2]uint16, 1)
	winchCh := make(chan os.Signal, 1)
	signal.Notify(winchCh, syscall.SIGWINCH)
	go func() {
		for range winchCh {
			if c, r, err := term.GetSize(int(os.Stdin.Fd())); err == nil {
				select {
				case resizeCh <- [2]uint16{uint16(r), uint16(c)}:
				default:
				}
			}
		}
	}()
	defer signal.Stop(winchCh)
	defer close(resizeCh)

	// Get the linux machine for interactive exec
	linuxMachine, ok := vm.machine.(*linux.LinuxMachine)
	if !ok {
		fmt.Fprintln(os.Stderr, "Error: interactive mode requires Linux backend")
		return 1
	}

	// Build exec options with CA and secret injection if proxy is enabled
	opts := &api.ExecOptions{Env: make(map[string]string)}
	if vm.caInjector != nil {
		opts.Env["SSL_CERT_FILE"] = "/workspace/.sandbox-ca.crt"
		opts.Env["REQUESTS_CA_BUNDLE"] = "/workspace/.sandbox-ca.crt"
		opts.Env["CURL_CA_BUNDLE"] = "/workspace/.sandbox-ca.crt"
		opts.Env["NODE_EXTRA_CA_CERTS"] = "/workspace/.sandbox-ca.crt"
	}
	// Inject secret placeholders
	if vm.policy != nil {
		for name, placeholder := range vm.policy.GetPlaceholders() {
			opts.Env[name] = placeholder
		}
	}

	exitCode, err := linuxMachine.ExecInteractive(ctx, command, opts, uint16(rows), uint16(cols), os.Stdin, os.Stdout, resizeCh)
	if err != nil {
		// Restore terminal before printing error
		term.Restore(int(os.Stdin.Fd()), oldState)
		fmt.Fprintf(os.Stderr, "\nError: %v\n", err)
		return 1
	}

	return exitCode
}

func cmdList(args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	running := fs.Bool("running", false, "Show only running VMs")
	fs.Parse(args)

	mgr := state.NewManager()
	states, err := mgr.List()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATUS\tIMAGE\tCREATED\tPID")

	for _, s := range states {
		if *running && s.Status != "running" {
			continue
		}
		created := s.CreatedAt.Format("2006-01-02 15:04")
		pid := "-"
		if s.PID > 0 {
			pid = fmt.Sprintf("%d", s.PID)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", s.ID, s.Status, s.Image, created, pid)
	}
	w.Flush()
}

func cmdGet(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Error: VM ID required")
		os.Exit(1)
	}

	mgr := state.NewManager()
	s, err := mgr.Get(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	output, _ := json.MarshalIndent(s, "", "  ")
	fmt.Println(string(output))
}

func cmdKill(args []string) {
	fs := flag.NewFlagSet("kill", flag.ExitOnError)
	all := fs.Bool("all", false, "Kill all running VMs")
	fs.Parse(args)

	mgr := state.NewManager()

	if *all {
		states, _ := mgr.List()
		for _, s := range states {
			if s.Status == "running" {
				if err := mgr.Kill(s.ID); err != nil {
					fmt.Fprintf(os.Stderr, "Failed to kill %s: %v\n", s.ID, err)
				} else {
					fmt.Printf("Killed %s\n", s.ID)
				}
			}
		}
		return
	}

	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "Error: VM ID required")
		os.Exit(1)
	}

	if err := mgr.Kill(fs.Arg(0)); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Killed %s\n", fs.Arg(0))
}

func cmdRemove(args []string) {
	fs := flag.NewFlagSet("rm", flag.ExitOnError)
	stopped := fs.Bool("stopped", false, "Remove all stopped VMs")
	fs.Parse(args)

	mgr := state.NewManager()

	if *stopped {
		states, _ := mgr.List()
		for _, s := range states {
			if s.Status != "running" {
				if err := mgr.Remove(s.ID); err != nil {
					fmt.Fprintf(os.Stderr, "Failed to remove %s: %v\n", s.ID, err)
				} else {
					fmt.Printf("Removed %s\n", s.ID)
				}
			}
		}
		return
	}

	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "Error: VM ID required")
		os.Exit(1)
	}

	if err := mgr.Remove(fs.Arg(0)); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Removed %s\n", fs.Arg(0))
}

func cmdPrune(args []string) {
	mgr := state.NewManager()
	pruned, err := mgr.Prune()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	for _, id := range pruned {
		fmt.Printf("Pruned %s\n", id)
	}
	fmt.Printf("Pruned %d VMs\n", len(pruned))
}

func cmdRPC(args []string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	factory := func(ctx context.Context, config *api.Config) (rpc.VM, error) {
		return createVM(ctx, config)
	}

	if err := rpc.RunRPC(ctx, factory); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

type stringSlice []string

func (s *stringSlice) String() string  { return fmt.Sprintf("%v", *s) }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }

// parseVolumeMount parses a volume mount string in format "host:guest" or "host:guest:ro"
// Guest paths are automatically prefixed with /workspace if not already
func parseVolumeMount(vol string) (hostPath, guestPath string, readonly bool, err error) {
	parts := strings.Split(vol, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return "", "", false, fmt.Errorf("expected format host:guest or host:guest:ro")
	}

	hostPath = parts[0]
	guestPath = parts[1]

	// Resolve to absolute path
	if !filepath.IsAbs(hostPath) {
		hostPath, err = filepath.Abs(hostPath)
		if err != nil {
			return "", "", false, fmt.Errorf("failed to resolve path: %w", err)
		}
	}

	// Verify host path exists
	if _, err := os.Stat(hostPath); err != nil {
		return "", "", false, fmt.Errorf("host path does not exist: %s", hostPath)
	}

	// Check for readonly flag
	if len(parts) == 3 {
		if parts[2] == "ro" || parts[2] == "readonly" {
			readonly = true
		} else {
			return "", "", false, fmt.Errorf("unknown option %q (use 'ro' for readonly)", parts[2])
		}
	}

	// Guest path must be absolute
	if !filepath.IsAbs(guestPath) {
		guestPath = "/" + guestPath
	}

	// All mounts go through /workspace - remap if needed
	// The guest FUSE daemon mounts at /workspace and prefixes all paths with /workspace
	if !strings.HasPrefix(guestPath, "/workspace") {
		guestPath = "/workspace" + guestPath
	}

	return hostPath, guestPath, readonly, nil
}

type sandboxVM struct {
	id             string
	config         *api.Config
	machine        vm.Machine
	proxy          *sandboxnet.TransparentProxy
	iptRules       *sandboxnet.IPTablesRules
	policy         *policy.Engine
	vfsRoot        *vfs.MountRouter
	vfsServer      *vfs.VFSServer
	vfsStopFunc    func()
	events         chan api.Event
	stateMgr       *state.Manager
	tapName        string
	caInjector     *sandboxnet.CAInjector
	subnetInfo     *state.SubnetInfo
	subnetAlloc    *state.SubnetAllocator
}

func createVM(ctx context.Context, config *api.Config) (*sandboxVM, error) {
	id := "vm-" + uuid.New().String()[:8]

	stateMgr := state.NewManager()
	if err := stateMgr.Register(id, config); err != nil {
		return nil, fmt.Errorf("failed to register VM state: %w", err)
	}

	// Allocate unique subnet for this VM
	subnetAlloc := state.NewSubnetAllocator()
	subnetInfo, err := subnetAlloc.Allocate(id)
	if err != nil {
		stateMgr.Unregister(id)
		return nil, fmt.Errorf("failed to allocate subnet: %w", err)
	}

	backend := linux.NewLinuxBackend()

	vmConfig := &vm.VMConfig{
		ID:         id,
		KernelPath: getKernelPath(),
		RootfsPath: getRootfsPath(config.Image),
		CPUs:       config.Resources.CPUs,
		MemoryMB:   config.Resources.MemoryMB,
		SocketPath: stateMgr.SocketPath(id) + ".sock",
		LogPath:    stateMgr.LogPath(id),
		VsockCID:   3,
		VsockPath:  stateMgr.Dir(id) + "/vsock.sock",
		GatewayIP:  subnetInfo.GatewayIP,
		GuestIP:    subnetInfo.GuestIP,
		SubnetCIDR: subnetInfo.GatewayIP + "/24",
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
	const proxyBindAddr = "0.0.0.0" // Bind to all interfaces
	const httpPort = 18080
	const httpsPort = 18443

	var proxy *sandboxnet.TransparentProxy
	var iptRules *sandboxnet.IPTablesRules

	// Set up proxy if we have network policy (allowlist or secrets configured)
	needsProxy := config.Network != nil && (len(config.Network.AllowedHosts) > 0 || len(config.Network.Secrets) > 0)
	if needsProxy {
		proxy, err = sandboxnet.NewTransparentProxy(&sandboxnet.ProxyConfig{
			BindAddr:  proxyBindAddr,
			HTTPPort:  httpPort,
			HTTPSPort: httpsPort,
			Policy:    policyEngine,
			Events:    events,
		})
		if err != nil {
			machine.Close()
			subnetAlloc.Release(id)
			stateMgr.Unregister(id)
			return nil, fmt.Errorf("failed to create transparent proxy: %w", err)
		}

		proxy.Start()

		// Set up iptables rules to redirect traffic to the proxy
		iptRules = sandboxnet.NewIPTablesRules(linuxMachine.TapName(), gatewayIP, httpPort, httpsPort)
		if err := iptRules.Setup(); err != nil {
			proxy.Close()
			machine.Close()
			subnetAlloc.Release(id)
			stateMgr.Unregister(id)
			return nil, fmt.Errorf("failed to setup iptables rules: %w", err)
		}
	}

	// Set up basic NAT for guest network access (for non-HTTP traffic)
	if err := setupNAT(linuxMachine, subnetInfo.Subnet); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to setup NAT: %v\n", err)
	}

	// Create VFS providers
	vfsProviders := make(map[string]vfs.Provider)
	if config.VFS != nil && config.VFS.Mounts != nil {
		for path, mount := range config.VFS.Mounts {
			provider := createProvider(mount)
			if provider != nil {
				vfsProviders[path] = provider
			}
		}
	}
	if len(vfsProviders) == 0 {
		vfsProviders["/workspace"] = vfs.NewMemoryProvider()
	}
	vfsRoot := vfs.NewMountRouter(vfsProviders)

	// Create VFS server for guest FUSE daemon connections
	vfsServer := vfs.NewVFSServer(vfsRoot)

	// Start VFS server on the vsock UDS path for VFS port
	// Firecracker exposes vsock as {uds_path}_{port}
	vfsSocketPath := fmt.Sprintf("%s_%d", vmConfig.VsockPath, linux.VsockPortVFS)
	vfsStopFunc, err := vfsServer.ServeUDSBackground(vfsSocketPath)
	if err != nil {
		if proxy != nil {
			proxy.Close()
		}
		if iptRules != nil {
			iptRules.Cleanup()
		}
		machine.Close()
		subnetAlloc.Release(id)
		stateMgr.Unregister(id)
		return nil, fmt.Errorf("failed to start VFS server: %w", err)
	}

	// Set up CA injector if proxy is enabled
	var caInjector *sandboxnet.CAInjector
	if proxy != nil {
		caInjector = sandboxnet.NewCAInjector(proxy.CAPool())
		// Write CA cert to workspace for guest access
		if mp, ok := vfsProviders["/workspace"].(*vfs.MemoryProvider); ok {
			mp.WriteFile("/.sandbox-ca.crt", caInjector.CACertPEM(), 0644)
		}
	}

	return &sandboxVM{
		id:          id,
		config:      config,
		machine:     machine,
		proxy:       proxy,
		iptRules:    iptRules,
		policy:      policyEngine,
		vfsRoot:     vfsRoot,
		vfsServer:   vfsServer,
		vfsStopFunc: vfsStopFunc,
		events:      events,
		stateMgr:    stateMgr,
		tapName:     linuxMachine.TapName(),
		caInjector:  caInjector,
		subnetInfo:  subnetInfo,
		subnetAlloc: subnetAlloc,
	}, nil
}

func (v *sandboxVM) ID() string          { return v.id }
func (v *sandboxVM) Config() *api.Config { return v.config }

func (v *sandboxVM) Start(ctx context.Context) error {
	return v.machine.Start(ctx)
}

func (v *sandboxVM) Stop(ctx context.Context) error {
	return v.machine.Stop(ctx)
}

func (v *sandboxVM) Exec(ctx context.Context, command string, opts *api.ExecOptions) (*api.ExecResult, error) {
	if opts == nil {
		opts = &api.ExecOptions{}
	}
	if opts.Env == nil {
		opts.Env = make(map[string]string)
	}

	// Inject CA certificate environment variables if proxy is enabled
	if v.caInjector != nil {
		certPath := "/workspace/.sandbox-ca.crt"
		opts.Env["SSL_CERT_FILE"] = certPath
		opts.Env["REQUESTS_CA_BUNDLE"] = certPath
		opts.Env["CURL_CA_BUNDLE"] = certPath
		opts.Env["NODE_EXTRA_CA_CERTS"] = certPath
	}

	// Inject secret placeholders as environment variables
	// The actual secret values will be substituted by the MITM proxy
	if v.policy != nil {
		for name, placeholder := range v.policy.GetPlaceholders() {
			opts.Env[name] = placeholder
		}
	}

	return v.machine.Exec(ctx, command, opts)
}

func (v *sandboxVM) WriteFile(ctx context.Context, path string, content []byte, mode uint32) error {
	if mode == 0 {
		mode = 0644
	}
	h, err := v.vfsRoot.Create(path, os.FileMode(mode))
	if err != nil {
		return err
	}
	defer h.Close()
	_, err = h.Write(content)
	return err
}

func (v *sandboxVM) ReadFile(ctx context.Context, path string) ([]byte, error) {
	h, err := v.vfsRoot.Open(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer h.Close()
	
	info, err := v.vfsRoot.Stat(path)
	if err != nil {
		return nil, err
	}
	
	content := make([]byte, info.Size())
	_, err = h.Read(content)
	if err != nil {
		return nil, err
	}
	return content, nil
}

func (v *sandboxVM) ListFiles(ctx context.Context, path string) ([]api.FileInfo, error) {
	entries, err := v.vfsRoot.ReadDir(path)
	if err != nil {
		return nil, err
	}

	result := make([]api.FileInfo, len(entries))
	for i, e := range entries {
		info, _ := e.Info()
		result[i] = api.FileInfo{
			Name:  e.Name(),
			Size:  info.Size(),
			Mode:  uint32(info.Mode()),
			IsDir: e.IsDir(),
		}
	}
	return result, nil
}

func (v *sandboxVM) Events() <-chan api.Event {
	return v.events
}

func (v *sandboxVM) Close() error {
	var errs []error
	
	if v.vfsStopFunc != nil {
		v.vfsStopFunc()
	}
	if v.iptRules != nil {
		if err := v.iptRules.Cleanup(); err != nil {
			errs = append(errs, fmt.Errorf("iptables cleanup: %w", err))
		}
	}
	if v.proxy != nil {
		v.proxy.Close()
	}
	
	// Clean up NAT forwarding rules
	cleanupNAT(v.tapName)
	
	// Release subnet allocation
	if v.subnetAlloc != nil {
		v.subnetAlloc.Release(v.id)
	}
	
	close(v.events)
	v.stateMgr.Unregister(v.id)
	if err := v.machine.Close(); err != nil {
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

func getKernelPath() string {
	home, _ := os.UserHomeDir()
	// Also check SUDO_USER's home if running as root
	sudoHome := ""
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" && os.Getuid() == 0 {
		sudoHome = filepath.Join("/home", sudoUser)
	}
	paths := []string{
		os.Getenv("MATCHLOCK_KERNEL"),
		filepath.Join(home, ".cache/matchlock/kernel"),
	}
	if sudoHome != "" {
		paths = append(paths, filepath.Join(sudoHome, ".cache/matchlock/kernel"))
	}
	for _, p := range paths {
		if p != "" {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return filepath.Join(home, ".cache/matchlock/kernel")
}

func getRootfsPath(image string) string {
	if image == "" {
		image = "standard"
	}
	home, _ := os.UserHomeDir()
	// Also check SUDO_USER's home if running as root
	sudoHome := ""
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" && os.Getuid() == 0 {
		sudoHome = filepath.Join("/home", sudoUser)
	}
	paths := []string{
		os.Getenv("MATCHLOCK_ROOTFS"),
		filepath.Join(home, ".cache/matchlock", fmt.Sprintf("rootfs-%s.ext4", image)),
	}
	if sudoHome != "" {
		paths = append(paths, filepath.Join(sudoHome, ".cache/matchlock", fmt.Sprintf("rootfs-%s.ext4", image)))
	}
	for _, p := range paths {
		if p != "" {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return filepath.Join(home, ".cache/matchlock", fmt.Sprintf("rootfs-%s.ext4", image))
}

// setupNAT configures iptables NAT rules for guest network access
// If running without root and rules are already set up (via setup-permissions.sh),
// this will fail silently which is okay.
func setupNAT(machine *linux.LinuxMachine, subnet string) error {
	// Enable IP forwarding (may fail without root, but might already be enabled)
	os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644)

	// Get the TAP interface name from the machine
	tapName := machine.TapName()
	if tapName == "" {
		return fmt.Errorf("no TAP interface configured")
	}

	// Try to add NAT masquerade rule for the specific subnet
	// If setup-permissions.sh was run with 192.168.0.0/16, this covers all VMs
	sandboxnet.SetupNATMasquerade(subnet)

	// Try to insert forwarding rules - these are now optional since
	// setup-permissions.sh sets up subnet-based rules
	exec.Command("iptables", "-I", "FORWARD", "1", "-i", tapName, "-j", "ACCEPT").Run()
	exec.Command("iptables", "-I", "FORWARD", "2", "-o", tapName, "-j", "ACCEPT").Run()

	return nil
}

// cleanupNAT removes iptables rules for the given TAP interface
func cleanupNAT(tapName string) {
	if tapName == "" {
		return
	}
	exec.Command("iptables", "-D", "FORWARD", "-i", tapName, "-j", "ACCEPT").Run()
	exec.Command("iptables", "-D", "FORWARD", "-o", tapName, "-j", "ACCEPT").Run()
}
