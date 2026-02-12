package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"golang.org/x/term"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/image"
	"github.com/jingkaihe/matchlock/pkg/sandbox"
	"github.com/jingkaihe/matchlock/pkg/state"
	"github.com/jingkaihe/matchlock/pkg/vm"
)

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

func init() {
	runCmd.Flags().String("image", "", "Container image (required)")
	runCmd.Flags().String("workspace", api.DefaultWorkspace, "Guest mount point for VFS")
	runCmd.Flags().StringSlice("allow-host", nil, "Allowed hosts (can be repeated)")
	runCmd.Flags().StringSliceP("volume", "v", nil, "Volume mount (host:guest or host:guest:ro)")
	runCmd.Flags().StringSlice("secret", nil, "Secret (NAME=VALUE@host1,host2 or NAME@host1,host2)")
	runCmd.Flags().StringSlice("dns-servers", nil, "DNS servers (default: 8.8.8.8,8.8.4.4)")
	runCmd.Flags().Int("cpus", api.DefaultCPUs, "Number of CPUs")
	runCmd.Flags().Int("memory", api.DefaultMemoryMB, "Memory in MB")
	runCmd.Flags().Int("timeout", api.DefaultTimeoutSeconds, "Timeout in seconds")
	runCmd.Flags().Int("disk-size", api.DefaultDiskSizeMB, "Disk size in MB")
	runCmd.Flags().BoolP("tty", "t", false, "Allocate a pseudo-TTY")
	runCmd.Flags().BoolP("interactive", "i", false, "Keep STDIN open")
	runCmd.Flags().Bool("pull", false, "Always pull image from registry (ignore cache)")
	runCmd.Flags().Bool("rm", true, "Remove sandbox after command exits (set --rm=false to keep running)")
	runCmd.Flags().Bool("privileged", false, "Skip in-guest security restrictions (seccomp, cap drop, no_new_privs)")
	runCmd.Flags().StringP("workdir", "w", "", "Working directory inside the sandbox (default: workspace path)")
	runCmd.Flags().StringP("user", "u", "", "Run as user (uid, uid:gid, or username; overrides image USER)")
	runCmd.Flags().String("entrypoint", "", "Override image ENTRYPOINT")
	runCmd.Flags().Duration("graceful-shutdown", api.DefaultGracefulShutdownPeriod, "Graceful shutdown timeout before force-stopping the VM ")
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

	rootCmd.AddCommand(runCmd)
}

func runRun(cmd *cobra.Command, args []string) error {
	// Image & lifecycle
	imageName, _ := cmd.Flags().GetString("image")
	pull, _ := cmd.Flags().GetBool("pull")
	rm, _ := cmd.Flags().GetBool("rm")
	privileged, _ := cmd.Flags().GetBool("privileged")

	// Resources
	cpus, _ := cmd.Flags().GetInt("cpus")
	memory, _ := cmd.Flags().GetInt("memory")
	diskSize, _ := cmd.Flags().GetInt("disk-size")
	timeout, _ := cmd.Flags().GetInt("timeout")

	// Exec options
	tty, _ := cmd.Flags().GetBool("tty")
	interactive, _ := cmd.Flags().GetBool("interactive")
	interactiveMode := tty && interactive
	workspace, _ := cmd.Flags().GetString("workspace")
	workdir, _ := cmd.Flags().GetString("workdir")

	// Network & security
	allowHosts, _ := cmd.Flags().GetStringSlice("allow-host")
	volumes, _ := cmd.Flags().GetStringSlice("volume")
	secrets, _ := cmd.Flags().GetStringSlice("secret")
	dnsServers, _ := cmd.Flags().GetStringSlice("dns-servers")

	// Shutdown
	gracefulShutdown, _ := cmd.Flags().GetDuration("graceful-shutdown")

	user, _ := cmd.Flags().GetString("user")
	entrypoint, _ := cmd.Flags().GetString("entrypoint")

	command := api.ShellQuoteArgs(args)

	var ctx context.Context
	var cancel context.CancelFunc

	if cmd.Flags().Changed("timeout") {
		ctx, cancel = context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}
	defer cancel()
	ctx, cancel = contextWithSignal(ctx)
	defer cancel()

	builder := image.NewBuilder(&image.BuildOptions{
		ForcePull: pull,
	})

	buildResult, err := builder.Build(ctx, imageName)
	if err != nil {
		return fmt.Errorf("building rootfs: %w", err)
	}
	if !buildResult.Cached {
		fmt.Fprintf(os.Stderr, "Built rootfs from %s (%.1f MB)\n", imageName, float64(buildResult.Size)/(1024*1024))
	}

	var imageCfg *api.ImageConfig
	if buildResult.OCI != nil {
		imageCfg = &api.ImageConfig{
			User:       buildResult.OCI.User,
			WorkingDir: buildResult.OCI.WorkingDir,
			Entrypoint: buildResult.OCI.Entrypoint,
			Cmd:        buildResult.OCI.Cmd,
			Env:        buildResult.OCI.Env,
		}
	}

	// CLI --user overrides image USER
	if user != "" {
		if imageCfg == nil {
			imageCfg = &api.ImageConfig{}
		}
		imageCfg.User = user
	}

	// CLI --entrypoint overrides image ENTRYPOINT (single string, like Docker)
	if cmd.Flags().Changed("entrypoint") {
		if imageCfg == nil {
			imageCfg = &api.ImageConfig{}
		}
		if entrypoint == "" {
			imageCfg.Entrypoint = nil
		} else {
			imageCfg.Entrypoint = []string{entrypoint}
		}
	}

	// Compose command from image ENTRYPOINT/CMD and user args.
	// Always route through ComposeCommand so --entrypoint is applied even when
	// user provides args (args replace CMD but ENTRYPOINT is always prepended).
	if imageCfg != nil {
		composed := imageCfg.ComposeCommand(args)
		if len(composed) > 0 {
			command = api.ShellQuoteArgs(composed)
		}
	}

	if rm && command == "" && !interactiveMode {
		return fmt.Errorf("command required (or use --rm=false to start without a command)")
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
			name, secret, err := api.ParseSecret(s)
			if err != nil {
				return fmt.Errorf("invalid secret %q: %w", s, err)
			}
			parsedSecrets[name] = secret
		}
	}

	config := &api.Config{
		Image:      imageName,
		Privileged: privileged,
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
			DNSServers:      dnsServers,
		},
		VFS:      vfsConfig,
		ImageCfg: imageCfg,
	}

	sb, err := sandbox.New(ctx, config, sandboxOpts)
	if err != nil {
		return fmt.Errorf("creating sandbox: %w", err)
	}

	if err := sb.Start(ctx); err != nil {
		sb.Close(ctx)
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

	closeCtx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), gracefulShutdown)
	}

	if interactiveMode {
		exitCode := runInteractive(ctx, sb, command, workdir)
		if rm {
			c, cancel := closeCtx()
			sb.Close(c)
			cancel()
			stateMgr.Remove(sb.ID())
		}
		os.Exit(exitCode)
	}

	if command != "" {
		opts := &api.ExecOptions{
			Stdout: os.Stdout,
			Stderr: os.Stderr,
		}
		if interactive {
			opts.Stdin = os.Stdin
		}
		if workdir != "" {
			opts.WorkingDir = workdir
		}
		result, err := sb.Exec(ctx, command, opts)
		if err != nil {
			if rm {
				c, cancel := closeCtx()
				sb.Close(c)
				cancel()
				stateMgr.Remove(sb.ID())
			}
			return fmt.Errorf("executing command: %w", err)
		}

		if rm {
			c, cancel := closeCtx()
			sb.Close(c)
			cancel()
			stateMgr.Remove(sb.ID())
			os.Exit(result.ExitCode)
		}
	}

	if !rm {
		// Block until signal — keeps the sandbox alive for `matchlock exec`
		<-ctx.Done()
		c, cancel := closeCtx()
		sb.Close(c)
		cancel()
	}

	return nil
}

func runInteractive(ctx context.Context, sb *sandbox.Sandbox, command, workdir string) int {
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

	opts := sb.PrepareExecEnv()
	if workdir != "" {
		opts.WorkingDir = workdir
	}

	exitCode, err := interactiveMachine.ExecInteractive(ctx, command, opts, uint16(rows), uint16(cols), os.Stdin, os.Stdout, resizeCh)
	if err != nil {
		term.Restore(int(os.Stdin.Fd()), oldState)
		fmt.Fprintf(os.Stderr, "\nError: %v\n", err)
		return 1
	}

	return exitCode
}
