package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/rpc"
	"github.com/jingkaihe/matchlock/pkg/sandbox"
	"github.com/jingkaihe/matchlock/pkg/state"
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
  --workspace <path>     Guest VFS mount point (default: /workspace)
  --allow-host <host>    Add host to allowlist (can be repeated, supports wildcards)
  -v <host:guest[:ro]>   Mount host directory into sandbox (can be repeated)
  --cpus <n>             Number of CPUs
  --memory <mb>          Memory in MB
  --timeout <s>          Timeout in seconds

Volume Mounts (-v):
  Guest paths are relative to workspace (or use full workspace paths):
  ./mycode:code                    Mounts to <workspace>/code
  ./data:/workspace/data           Same as above (explicit)
  /host/path:subdir:ro             Read-only mount to <workspace>/subdir

Wildcard Patterns for --allow-host:
  *                      Allow all hosts
  *.example.com          Allow all subdomains (api.example.com, a.b.example.com)
  api-*.example.com      Allow pattern match (api-v1.example.com, api-prod.example.com)

Examples:
  matchlock run python script.py
  matchlock run -it python3                              # Interactive Python
  matchlock run -it sh                                   # Interactive shell
  matchlock run -v ./mycode:code python /workspace/code/script.py
  matchlock run --workspace /home/user -v ./code:code ls /home/user/code
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
	workspace := fs.String("workspace", api.DefaultWorkspace, "Guest mount point for VFS")
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
	vfsConfig := &api.VFSConfig{Workspace: *workspace}
	if len(volumes) > 0 {
		mounts := make(map[string]api.MountConfig)
		for _, vol := range volumes {
			hostPath, guestPath, readonly, err := api.ParseVolumeMount(vol, *workspace)
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
		vfsConfig.Mounts = mounts
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

	sb, err := sandbox.New(ctx, config, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating sandbox: %v\n", err)
		os.Exit(1)
	}

	if err := sb.Start(ctx); err != nil {
		sb.Close()
		fmt.Fprintf(os.Stderr, "Error starting sandbox: %v\n", err)
		os.Exit(1)
	}

	if *interactive {
		exitCode := runInteractive(ctx, sb, command)
		sb.Close()
		os.Exit(exitCode)
	}

	result, err := sb.Exec(ctx, command, nil)
	if err != nil {
		sb.Close()
		fmt.Fprintf(os.Stderr, "Error executing command: %v\n", err)
		os.Exit(1)
	}

	os.Stdout.Write(result.Stdout)
	os.Stderr.Write(result.Stderr)

	sb.Close()
	os.Exit(result.ExitCode)
}

func runInteractive(ctx context.Context, sb *sandbox.Sandbox, command string) int {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintln(os.Stderr, "Error: -it requires a TTY")
		return 1
	}

	cols, rows, err := term.GetSize(int(os.Stdin.Fd()))
	if err != nil {
		rows, cols = 24, 80
	}

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error setting raw mode: %v\n", err)
		return 1
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

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

	linuxMachine, ok := sb.Machine().(*linux.LinuxMachine)
	if !ok {
		fmt.Fprintln(os.Stderr, "Error: interactive mode requires Linux backend")
		return 1
	}

	opts := &api.ExecOptions{Env: make(map[string]string)}
	if sb.CAInjector() != nil {
		certPath := sb.Workspace() + "/.sandbox-ca.crt"
		opts.Env["SSL_CERT_FILE"] = certPath
		opts.Env["REQUESTS_CA_BUNDLE"] = certPath
		opts.Env["CURL_CA_BUNDLE"] = certPath
		opts.Env["NODE_EXTRA_CA_CERTS"] = certPath
	}
	if sb.Policy() != nil {
		for name, placeholder := range sb.Policy().GetPlaceholders() {
			opts.Env[name] = placeholder
		}
	}

	exitCode, err := linuxMachine.ExecInteractive(ctx, command, opts, uint16(rows), uint16(cols), os.Stdin, os.Stdout, resizeCh)
	if err != nil {
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
		return sandbox.New(ctx, config, nil)
	}

	if err := rpc.RunRPC(ctx, factory); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

type stringSlice []string

func (s *stringSlice) String() string  { return fmt.Sprintf("%v", *s) }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }
