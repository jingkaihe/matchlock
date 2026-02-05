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
	"syscall"
	"text/tabwriter"
	"time"

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
	fmt.Println(`Usage: sandbox <command> [options]

Commands:
  run <command>     Run a command in a new sandbox
  list              List all sandboxes
  get <id>          Get details of a sandbox
  kill <id>         Kill a running sandbox
  rm <id>           Remove a stopped sandbox
  prune             Remove all stopped sandboxes
  --rpc             Run in RPC mode (for programmatic access)

Options:
  --image <name>       Image variant (minimal, standard, full)
  --allow-host <host>  Add host to allowlist (can be repeated)
  --cpus <n>           Number of CPUs
  --memory <mb>        Memory in MB
  --timeout <s>        Timeout in seconds

Examples:
  sandbox run python script.py
  sandbox run --allow-host "api.openai.com" python agent.py
  sandbox list
  sandbox kill vm-abc123`)
}

func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	image := fs.String("image", "standard", "Image variant")
	cpus := fs.Int("cpus", 1, "Number of CPUs")
	memory := fs.Int("memory", 512, "Memory in MB")
	timeout := fs.Int("timeout", 300, "Timeout in seconds")
	var allowHosts stringSlice
	fs.Var(&allowHosts, "allow-host", "Allowed hosts")

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
	}

	vm, err := createVM(ctx, config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating VM: %v\n", err)
		os.Exit(1)
	}
	defer vm.Close()

	if err := vm.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error starting VM: %v\n", err)
		os.Exit(1)
	}

	result, err := vm.Exec(ctx, command, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error executing command: %v\n", err)
		os.Exit(1)
	}

	os.Stdout.Write(result.Stdout)
	os.Stderr.Write(result.Stderr)
	os.Exit(result.ExitCode)
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

type sandboxVM struct {
	id          string
	config      *api.Config
	machine     vm.Machine
	proxy       *sandboxnet.TransparentProxy
	iptRules    *sandboxnet.IPTablesRules
	policy      *policy.Engine
	vfsRoot     *vfs.MountRouter
	vfsServer   *vfs.VFSServer
	vfsStopFunc func()
	events      chan api.Event
	stateMgr    *state.Manager
}

func createVM(ctx context.Context, config *api.Config) (*sandboxVM, error) {
	id := "vm-" + uuid.New().String()[:8]

	stateMgr := state.NewManager()
	if err := stateMgr.Register(id, config); err != nil {
		return nil, fmt.Errorf("failed to register VM state: %w", err)
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
	}

	machine, err := backend.Create(ctx, vmConfig)
	if err != nil {
		stateMgr.Unregister(id)
		return nil, fmt.Errorf("failed to create VM: %w", err)
	}

	linuxMachine := machine.(*linux.LinuxMachine)

	// Create policy engine
	policyEngine := policy.NewEngine(config.Network)

	// Create event channel
	events := make(chan api.Event, 100)

	// Set up transparent proxy for HTTP/HTTPS interception
	const gatewayIP = "192.168.100.1"
	const proxyBindAddr = "0.0.0.0" // Bind to all interfaces
	const httpPort = 18080
	const httpsPort = 18443

	var proxy *sandboxnet.TransparentProxy
	var iptRules *sandboxnet.IPTablesRules

	// Only set up proxy if we have network policy (allowlist configured)
	if config.Network != nil && len(config.Network.AllowedHosts) > 0 {
		proxy, err = sandboxnet.NewTransparentProxy(&sandboxnet.ProxyConfig{
			BindAddr:  proxyBindAddr,
			HTTPPort:  httpPort,
			HTTPSPort: httpsPort,
			Policy:    policyEngine,
			Events:    events,
		})
		if err != nil {
			machine.Close()
			stateMgr.Unregister(id)
			return nil, fmt.Errorf("failed to create transparent proxy: %w", err)
		}

		proxy.Start()

		// Set up iptables rules to redirect traffic to the proxy
		iptRules = sandboxnet.NewIPTablesRules(linuxMachine.TapName(), gatewayIP, httpPort, httpsPort)
		if err := iptRules.Setup(); err != nil {
			proxy.Close()
			machine.Close()
			stateMgr.Unregister(id)
			return nil, fmt.Errorf("failed to setup iptables rules: %w", err)
		}
	}

	// Set up basic NAT for guest network access (for non-HTTP traffic)
	if err := setupNAT(linuxMachine); err != nil {
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
		stateMgr.Unregister(id)
		return nil, fmt.Errorf("failed to start VFS server: %w", err)
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
	return v.machine.Exec(ctx, command, opts)
}

func (v *sandboxVM) WriteFile(ctx context.Context, path string, content []byte, mode uint32) error {
	mp, ok := v.getMemoryProvider(path)
	if ok {
		return mp.WriteFile(path, content, os.FileMode(mode))
	}
	return fmt.Errorf("cannot write to path: %s", path)
}

func (v *sandboxVM) ReadFile(ctx context.Context, path string) ([]byte, error) {
	mp, ok := v.getMemoryProvider(path)
	if ok {
		return mp.ReadFile(path)
	}
	return nil, fmt.Errorf("cannot read from path: %s", path)
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
	if v.vfsStopFunc != nil {
		v.vfsStopFunc()
	}
	if v.iptRules != nil {
		v.iptRules.Cleanup()
	}
	if v.proxy != nil {
		v.proxy.Close()
	}
	close(v.events)
	v.stateMgr.Unregister(v.id)
	return v.machine.Close()
}

func (v *sandboxVM) getMemoryProvider(path string) (*vfs.MemoryProvider, bool) {
	for _, m := range []string{"/workspace", "/data", "/output"} {
		if len(path) >= len(m) && path[:len(m)] == m {
			return vfs.NewMemoryProvider(), true
		}
	}
	return nil, false
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
	paths := []string{
		os.Getenv("MATCHLOCK_KERNEL"),
		filepath.Join(home, ".cache/matchlock/kernel"),
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
	paths := []string{
		os.Getenv("MATCHLOCK_ROOTFS"),
		filepath.Join(home, ".cache/matchlock", fmt.Sprintf("rootfs-%s.ext4", image)),
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
func setupNAT(machine *linux.LinuxMachine) error {
	// Enable IP forwarding
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644); err != nil {
		return fmt.Errorf("failed to enable IP forwarding: %w", err)
	}

	// Get the TAP interface name from the machine
	tapName := machine.TapName()
	if tapName == "" {
		return fmt.Errorf("no TAP interface configured")
	}

	// Add NAT masquerade rule for guest subnet
	if err := sandboxnet.SetupNATMasquerade("192.168.100.0/24"); err != nil {
		return fmt.Errorf("failed to setup NAT masquerade: %w", err)
	}

	// Insert forwarding rules at beginning of FORWARD chain (before DROP policies)
	exec.Command("iptables", "-I", "FORWARD", "1", "-i", tapName, "-j", "ACCEPT").Run()
	exec.Command("iptables", "-I", "FORWARD", "2", "-o", tapName, "-j", "ACCEPT").Run()

	return nil
}
