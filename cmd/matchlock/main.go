package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"golang.org/x/term"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/image"
	"github.com/jingkaihe/matchlock/pkg/rpc"
	"github.com/jingkaihe/matchlock/pkg/sandbox"
	"github.com/jingkaihe/matchlock/pkg/state"
	"github.com/jingkaihe/matchlock/pkg/version"
	"github.com/jingkaihe/matchlock/pkg/vm"
)

func shellQuoteArgs(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		if strings.ContainsAny(arg, " \t\n\"'`$\\!*?[]{}();<>&|") {
			quoted[i] = "'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'"
		} else {
			quoted[i] = arg
		}
	}
	return strings.Join(quoted, " ")
}

var rootCmd = &cobra.Command{
	Use:     "matchlock",
	Short:   "A lightweight micro-VM sandbox for running llm agent securely",
	Long:    "Matchlock is a lightweight micro-VM sandbox for running llm agent\nsecurely with network interception and secret protection.",
	Version: version.Version,

	SilenceUsage:  true,
	SilenceErrors: true,
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("matchlock %s (commit: %s, built: %s)\n", version.Version, version.GitCommit, version.BuildTime)
	},
}

var runCmd = &cobra.Command{
	Use:   "run [flags] -- <command>",
	Short: "Run a command in a new sandbox",
	Long: `Run a command in a new sandbox.

Secrets (--secret):
  Secrets are injected via MITM proxy - the real value never enters the VM.
  The VM sees a placeholder, which is replaced with the real value in HTTP headers.

  Formats:
    NAME=VALUE@host1,host2     Inline secret value for specified hosts
    NAME@host1,host2           Read secret from $NAME environment variable

  Note: When using sudo, env vars are not preserved. Use 'sudo -E' or pass inline.

Volume Mounts (-v):
  Guest paths are relative to workspace (or use full workspace paths):
  ./mycode:code                    Mounts to <workspace>/code
  ./data:/workspace/data           Same as above (explicit)
  /host/path:subdir:ro             Read-only mount to <workspace>/subdir

Wildcard Patterns for --allow-host:
  *                      Allow all hosts
  *.example.com          Allow all subdomains (api.example.com, a.b.example.com)
  api-*.example.com      Allow pattern match (api-v1.example.com, api-prod.example.com)`,
	Example: `  matchlock run --image alpine:latest -it sh
  matchlock run --image python:3.12-alpine python3 -c 'print(42)'
  matchlock run --image alpine:latest --rm=false   # keep VM alive after exit
  matchlock exec <vm-id> echo hello                # exec into running VM

  # With secrets (MITM replaces placeholder in HTTP requests)
  export ANTHROPIC_API_KEY=sk-xxx
  matchlock run --image python:3.12-alpine \
    --secret ANTHROPIC_API_KEY@api.anthropic.com \
    python call_api.py`,
	Args: cobra.ArbitraryArgs,
	RunE: runRun,
}

var buildCmd = &cobra.Command{
	Use:     "build <image>",
	Short:   "Build rootfs from container image",
	Example: `  matchlock build alpine:latest`,
	Args:    cobra.ExactArgs(1),
	RunE:    runBuild,
}

var listCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List all sandboxes",
	RunE:    runList,
}

var getCmd = &cobra.Command{
	Use:   "get <id>",
	Short: "Get details of a sandbox",
	Args:  cobra.ExactArgs(1),
	RunE:  runGet,
}

var killCmd = &cobra.Command{
	Use:   "kill <id>",
	Short: "Kill a running sandbox",
	RunE:  runKill,
}

var execCmd = &cobra.Command{
	Use:   "exec [flags] <id> -- <command>",
	Short: "Execute a command in a running sandbox",
	Long: `Execute a command in a running sandbox.

The sandbox must have been started with --rm=false to remain running.`,
	Example: `  matchlock exec vm-abc123 echo hello
  matchlock exec vm-abc123 -it sh`,
	Args: cobra.MinimumNArgs(1),
	RunE: runExec,
}

var rmCmd = &cobra.Command{
	Use:     "rm <id>",
	Aliases: []string{"remove"},
	Short:   "Remove a stopped sandbox",
	RunE:    runRemove,
}

var pruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Remove all stopped sandboxes",
	RunE:  runPrune,
}

var rpcCmd = &cobra.Command{
	Use:   "rpc",
	Short: "Run in RPC mode (for programmatic access)",
	RunE:  runRPC,
}

func init() {
	runCmd.Flags().String("image", "", "Container image (required)")
	runCmd.Flags().String("workspace", api.DefaultWorkspace, "Guest mount point for VFS")
	runCmd.Flags().StringSlice("allow-host", nil, "Allowed hosts (can be repeated)")
	runCmd.Flags().StringSliceP("volume", "v", nil, "Volume mount (host:guest or host:guest:ro)")
	runCmd.Flags().StringSlice("secret", nil, "Secret (NAME=VALUE@host1,host2 or NAME@host1,host2)")
	runCmd.Flags().Int("cpus", 1, "Number of CPUs")
	runCmd.Flags().Int("memory", 512, "Memory in MB")
	runCmd.Flags().Int("timeout", 300, "Timeout in seconds")
	runCmd.Flags().Int("disk-size", 5120, "Disk size in MB")
	runCmd.Flags().BoolP("tty", "t", false, "Allocate a pseudo-TTY")
	runCmd.Flags().BoolP("interactive", "i", false, "Keep STDIN open")
	runCmd.Flags().Bool("pull", false, "Always pull image from registry (ignore cache)")
	runCmd.Flags().Bool("rm", true, "Remove sandbox after command exits (set --rm=false to keep running)")
	runCmd.MarkFlagRequired("image")

	viper.BindPFlag("run.image", runCmd.Flags().Lookup("image"))
	viper.BindPFlag("run.workspace", runCmd.Flags().Lookup("workspace"))
	viper.BindPFlag("run.allow-host", runCmd.Flags().Lookup("allow-host"))
	viper.BindPFlag("run.volume", runCmd.Flags().Lookup("volume"))
	viper.BindPFlag("run.secret", runCmd.Flags().Lookup("secret"))
	viper.BindPFlag("run.cpus", runCmd.Flags().Lookup("cpus"))
	viper.BindPFlag("run.memory", runCmd.Flags().Lookup("memory"))
	viper.BindPFlag("run.timeout", runCmd.Flags().Lookup("timeout"))
	viper.BindPFlag("run.disk-size", runCmd.Flags().Lookup("disk-size"))
	viper.BindPFlag("run.tty", runCmd.Flags().Lookup("tty"))
	viper.BindPFlag("run.interactive", runCmd.Flags().Lookup("interactive"))
	viper.BindPFlag("run.pull", runCmd.Flags().Lookup("pull"))

	viper.BindPFlag("run.rm", runCmd.Flags().Lookup("rm"))

	execCmd.Flags().BoolP("tty", "t", false, "Allocate a pseudo-TTY")
	execCmd.Flags().BoolP("interactive", "i", false, "Keep STDIN open")

	buildCmd.Flags().Bool("pull", false, "Always pull image from registry (ignore cache)")

	listCmd.Flags().Bool("running", false, "Show only running VMs")
	viper.BindPFlag("list.running", listCmd.Flags().Lookup("running"))

	killCmd.Flags().Bool("all", false, "Kill all running VMs")
	viper.BindPFlag("kill.all", killCmd.Flags().Lookup("all"))

	rmCmd.Flags().Bool("stopped", false, "Remove all stopped VMs")
	viper.BindPFlag("rm.stopped", rmCmd.Flags().Lookup("stopped"))

	rootCmd.AddCommand(runCmd)
	rootCmd.AddCommand(execCmd)
	rootCmd.AddCommand(buildCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(getCmd)
	rootCmd.AddCommand(killCmd)
	rootCmd.AddCommand(rmCmd)
	rootCmd.AddCommand(pruneCmd)
	rootCmd.AddCommand(rpcCmd)
	rootCmd.AddCommand(versionCmd)

	viper.SetEnvPrefix("MATCHLOCK")
	viper.AutomaticEnv()
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_", ".", "_"))
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runRun(cmd *cobra.Command, args []string) error {
	imageName, _ := cmd.Flags().GetString("image")
	cpus, _ := cmd.Flags().GetInt("cpus")
	memory, _ := cmd.Flags().GetInt("memory")
	timeout, _ := cmd.Flags().GetInt("timeout")
	tty, _ := cmd.Flags().GetBool("tty")
	interactive, _ := cmd.Flags().GetBool("interactive")
	workspace, _ := cmd.Flags().GetString("workspace")
	allowHosts, _ := cmd.Flags().GetStringSlice("allow-host")
	volumes, _ := cmd.Flags().GetStringSlice("volume")
	secrets, _ := cmd.Flags().GetStringSlice("secret")
	rm, _ := cmd.Flags().GetBool("rm")

	interactiveMode := tty && interactive
	pull, _ := cmd.Flags().GetBool("pull")
	diskSize, _ := cmd.Flags().GetInt("disk-size")

	command := shellQuoteArgs(args)

	if rm && len(args) == 0 && !interactiveMode {
		return fmt.Errorf("command required (or use --rm=false to start without a command)")
	}

	var ctx context.Context
	var cancel context.CancelFunc

	if cmd.Flags().Changed("timeout") {
		ctx, cancel = context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	builder := image.NewBuilder(&image.BuildOptions{
		ForcePull: pull,
	})

	buildResult, err := builder.Build(ctx, imageName)
	if err != nil {
		return fmt.Errorf("building rootfs: %w", err)
	}
	if buildResult.Cached {
		fmt.Printf("Using cached image %s\n", imageName)
	} else {
		fmt.Printf("Built rootfs from %s (%.1f MB)\n", imageName, float64(buildResult.Size)/(1024*1024))
	}
	sandboxOpts := &sandbox.Options{RootfsPath: buildResult.RootfsPath}

	vfsConfig := &api.VFSConfig{Workspace: workspace}
	if len(volumes) > 0 {
		mounts := make(map[string]api.MountConfig)
		for _, vol := range volumes {
			hostPath, guestPath, readonly, err := api.ParseVolumeMount(vol, workspace)
			if err != nil {
				return fmt.Errorf("invalid volume mount %q: %w", vol, err)
			}
			mounts[guestPath] = api.MountConfig{
				Type:     "real_fs",
				HostPath: hostPath,
				Readonly: readonly,
			}
		}
		vfsConfig.Mounts = mounts
	}

	var parsedSecrets map[string]api.Secret
	if len(secrets) > 0 {
		parsedSecrets = make(map[string]api.Secret)
		for _, s := range secrets {
			name, secret, err := parseSecret(s)
			if err != nil {
				return fmt.Errorf("invalid secret %q: %w", s, err)
			}
			parsedSecrets[name] = secret
		}
	}

	config := &api.Config{
		Image: imageName,
		Resources: &api.Resources{
			CPUs:           cpus,
			MemoryMB:       memory,
			DiskSizeMB:     diskSize,
			TimeoutSeconds: timeout,
		},
		Network: &api.NetworkConfig{
			AllowedHosts:    allowHosts,
			BlockPrivateIPs: true,
			Secrets:         parsedSecrets,
		},
		VFS: vfsConfig,
	}

	sb, err := sandbox.New(ctx, config, sandboxOpts)
	if err != nil {
		return fmt.Errorf("creating sandbox: %w", err)
	}

	if err := sb.Start(ctx); err != nil {
		sb.Close()
		return fmt.Errorf("starting sandbox: %w", err)
	}

	// Start exec relay server so `matchlock exec` can connect from another process
	execRelay := sandbox.NewExecRelay(sb)
	stateMgr := state.NewManager()
	execSocketPath := stateMgr.ExecSocketPath(sb.ID())
	if err := execRelay.Start(execSocketPath); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to start exec relay: %v\n", err)
	}
	defer execRelay.Stop()

	if !rm {
		fmt.Fprintf(os.Stderr, "Sandbox %s is running\n", sb.ID())
		fmt.Fprintf(os.Stderr, "  Connect: matchlock exec %s -it bash\n", sb.ID())
		fmt.Fprintf(os.Stderr, "  Stop:    matchlock kill %s\n", sb.ID())
	}

	if interactiveMode {
		exitCode := runInteractive(ctx, sb, command)
		if rm {
			sb.Close()
		}
		os.Exit(exitCode)
	}

	if len(args) > 0 {
		result, err := sb.Exec(ctx, command, nil)
		if err != nil {
			if rm {
				sb.Close()
			}
			return fmt.Errorf("executing command: %w", err)
		}

		os.Stdout.Write(result.Stdout)
		os.Stderr.Write(result.Stderr)

		if rm {
			sb.Close()
			os.Exit(result.ExitCode)
		}
	}

	if !rm {
		// Block until signal — keeps the sandbox alive for `matchlock exec`
		<-ctx.Done()
		sb.Close()
	}

	return nil
}

func runExec(cmd *cobra.Command, args []string) error {
	vmID := args[0]
	cmdArgs := args[1:]

	tty, _ := cmd.Flags().GetBool("tty")
	interactive, _ := cmd.Flags().GetBool("interactive")
	interactiveMode := tty && interactive

	if len(cmdArgs) == 0 && !interactiveMode {
		return fmt.Errorf("command required (or use -it for interactive mode)")
	}

	mgr := state.NewManager()
	vmState, err := mgr.Get(vmID)
	if err != nil {
		return fmt.Errorf("VM %s not found: %w", vmID, err)
	}
	if vmState.Status != "running" {
		return fmt.Errorf("VM %s is not running (status: %s)", vmID, vmState.Status)
	}

	execSocketPath := mgr.ExecSocketPath(vmID)
	if _, err := os.Stat(execSocketPath); err != nil {
		return fmt.Errorf("exec socket not found for %s (was it started with --rm=false?)", vmID)
	}

	command := shellQuoteArgs(cmdArgs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	if interactiveMode {
		return runExecInteractive(ctx, execSocketPath, command)
	}

	result, err := sandbox.ExecViaRelay(ctx, execSocketPath, command)
	if err != nil {
		return fmt.Errorf("exec failed: %w", err)
	}

	os.Stdout.Write(result.Stdout)
	os.Stderr.Write(result.Stderr)
	os.Exit(result.ExitCode)
	return nil
}

func runExecInteractive(ctx context.Context, execSocketPath, command string) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("-it requires a TTY")
	}

	cols, rows, err := term.GetSize(int(os.Stdin.Fd()))
	if err != nil {
		rows, cols = 24, 80
	}

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("setting raw mode: %w", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	exitCode, err := sandbox.ExecInteractiveViaRelay(ctx, execSocketPath, command, uint16(rows), uint16(cols), os.Stdin, os.Stdout)
	if err != nil {
		term.Restore(int(os.Stdin.Fd()), oldState)
		return fmt.Errorf("interactive exec failed: %w", err)
	}

	term.Restore(int(os.Stdin.Fd()), oldState)
	os.Exit(exitCode)
	return nil
}

func runBuild(cmd *cobra.Command, args []string) error {
	imageRef := args[0]
	pull, _ := cmd.Flags().GetBool("pull")

	builder := image.NewBuilder(&image.BuildOptions{
		ForcePull: pull,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	fmt.Printf("Building rootfs from %s...\n", imageRef)
	result, err := builder.Build(ctx, imageRef)
	if err != nil {
		return err
	}

	fmt.Printf("Built: %s\n", result.RootfsPath)
	fmt.Printf("Digest: %s\n", result.Digest)
	fmt.Printf("Size: %.1f MB\n", float64(result.Size)/(1024*1024))
	return nil
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

	interactiveMachine, ok := sb.Machine().(vm.InteractiveMachine)
	if !ok {
		fmt.Fprintln(os.Stderr, "Error: interactive mode not supported on this backend")
		return 1
	}

	opts := &api.ExecOptions{Env: make(map[string]string)}
	if sb.CAPool() != nil {
		certPath := "/etc/ssl/certs/matchlock-ca.crt"
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

	exitCode, err := interactiveMachine.ExecInteractive(ctx, command, opts, uint16(rows), uint16(cols), os.Stdin, os.Stdout, resizeCh)
	if err != nil {
		term.Restore(int(os.Stdin.Fd()), oldState)
		fmt.Fprintf(os.Stderr, "\nError: %v\n", err)
		return 1
	}

	return exitCode
}

func runList(cmd *cobra.Command, args []string) error {
	running, _ := cmd.Flags().GetBool("running")

	mgr := state.NewManager()
	states, err := mgr.List()
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATUS\tIMAGE\tCREATED\tPID")

	for _, s := range states {
		if running && s.Status != "running" {
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
	return nil
}

func runGet(cmd *cobra.Command, args []string) error {
	mgr := state.NewManager()
	s, err := mgr.Get(args[0])
	if err != nil {
		return err
	}

	output, _ := json.MarshalIndent(s, "", "  ")
	fmt.Println(string(output))
	return nil
}

func runKill(cmd *cobra.Command, args []string) error {
	all, _ := cmd.Flags().GetBool("all")
	mgr := state.NewManager()

	if all {
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
		return nil
	}

	if len(args) == 0 {
		return fmt.Errorf("VM ID required (or use --all)")
	}

	if err := mgr.Kill(args[0]); err != nil {
		return err
	}
	fmt.Printf("Killed %s\n", args[0])
	return nil
}

func runRemove(cmd *cobra.Command, args []string) error {
	stopped, _ := cmd.Flags().GetBool("stopped")
	mgr := state.NewManager()

	if stopped {
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
		return nil
	}

	if len(args) == 0 {
		return fmt.Errorf("VM ID required (or use --stopped)")
	}

	if err := mgr.Remove(args[0]); err != nil {
		return err
	}
	fmt.Printf("Removed %s\n", args[0])
	return nil
}

func runPrune(cmd *cobra.Command, args []string) error {
	mgr := state.NewManager()
	pruned, err := mgr.Prune()
	if err != nil {
		return err
	}

	for _, id := range pruned {
		fmt.Printf("Pruned %s\n", id)
	}
	fmt.Printf("Pruned %d VMs\n", len(pruned))
	return nil
}

func runRPC(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	factory := func(ctx context.Context, config *api.Config) (rpc.VM, error) {
		if config.Image == "" {
			return nil, fmt.Errorf("image is required")
		}

		builder := image.NewBuilder(&image.BuildOptions{})

		result, err := builder.Build(ctx, config.Image)
		if err != nil {
			return nil, fmt.Errorf("failed to build rootfs: %w", err)
		}

		return sandbox.New(ctx, config, &sandbox.Options{RootfsPath: result.RootfsPath})
	}

	return rpc.RunRPC(ctx, factory)
}

func parseSecret(s string) (string, api.Secret, error) {
	atIdx := strings.LastIndex(s, "@")
	if atIdx == -1 {
		return "", api.Secret{}, fmt.Errorf("missing @hosts (format: NAME=VALUE@host1,host2 or NAME@host1,host2)")
	}

	hostsStr := s[atIdx+1:]
	if hostsStr == "" {
		return "", api.Secret{}, fmt.Errorf("no hosts specified after @")
	}
	hosts := strings.Split(hostsStr, ",")
	for i := range hosts {
		hosts[i] = strings.TrimSpace(hosts[i])
	}

	nameValue := s[:atIdx]
	eqIdx := strings.Index(nameValue, "=")

	var name, value string
	if eqIdx == -1 {
		name = nameValue
		value = os.Getenv(name)
		if value == "" {
			return "", api.Secret{}, fmt.Errorf("environment variable $%s is not set (hint: use 'sudo -E' to preserve env vars, or pass inline: %s=VALUE@%s)", name, name, hostsStr)
		}
	} else {
		name = nameValue[:eqIdx]
		value = nameValue[eqIdx+1:]
	}

	if name == "" {
		return "", api.Secret{}, fmt.Errorf("secret name cannot be empty")
	}

	return name, api.Secret{
		Value: value,
		Hosts: hosts,
	}, nil
}
