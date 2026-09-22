package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"golang.org/x/term"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/image"
	"github.com/jingkaihe/matchlock/pkg/sandbox"
	"github.com/jingkaihe/matchlock/pkg/state"
	"github.com/jingkaihe/matchlock/pkg/vm"
)

const (
	detachedVMPollInterval = 100 * time.Millisecond
	// detachedStartupTimeout bounds how long a detached run waits for its child
	// to register a running VM row, so a broken startup fails loudly instead of
	// hanging forever.
	detachedStartupTimeout = 5 * time.Minute
	// detachedReapGrace is how long the wait lingers for the reaper's verdict
	// once the child is observed gone.
	detachedReapGrace = time.Second
	// detachedStderrTailBytes caps the captured child stderr kept for an error.
	detachedStderrTailBytes = 4096
)

var runCmd = &cobra.Command{
	Use:   "run [flags] -- <command>",
	Short: "Run a command in a new sandbox",
	Long: `Run a command in a new sandbox.

Environment Variables (-e/--env, --env-file):
  Non-secret environment variables are visible inside the VM and in VM state
  output (for example 'matchlock get' and 'matchlock inspect').

  Formats:
    --env KEY=VALUE            Set inline value
    --env KEY                  Read from host environment ($KEY)
    --env-file /path/to/.env   Read KEY=VALUE or KEY entries per line

Secrets (--secret):
  Secrets are injected via MITM proxy - the real value never enters the VM.
  The VM sees a placeholder, which is replaced with the real value in HTTP headers.

	Formats:
	    NAME=VALUE@host1,host2     Inline secret value for specified hosts
	    NAME@host1,host2           Read secret from $NAME environment variable

	Custom placeholders:
	    --secret-placeholder NAME=value   Override the in-VM placeholder value
	    --secret-file /path/to/secrets.json  Load full secret definitions from JSON

	Note: When using sudo, env vars are not preserved. Use 'sudo -E' or pass inline.

Volume Mounts (-v):
  Requires --workspace. Guest paths are relative to workspace (or use full workspace paths):
  ./mycode:code                    Isolated snapshot mount to <workspace>/code (default)
  ./mycode:code:overlay            Same as above (explicit)
  ./data:/workspace/data           Same as above (explicit guest path)
  /host/path:subdir:host_fs        Read-write host mount to <workspace>/subdir
  /host/path:subdir:ro             Read-only host mount to <workspace>/subdir
  /host/path:subdir:host_fs,uid=1000,gid=1000
                                    Host mount whose files appear owned by 1000:1000

Raw Disk Mounts (--disk):
  Attach an ext4 disk image directly as a block device.
  Use @<name> to mount a named volume:
  /host/cache.ext4:/var/lib/buildkit
  @buildkit-cache:/var/lib/buildkit
  /host/data.ext4:/mnt/data:ro
  @pgdata:/var/lib/postgresql:uid=999,gid=999
  Ownership options chown the mount root only and require a writable disk.

Wildcard Patterns for --allow-host:
  *                      Allow all hosts
  *.example.com          Allow all subdomains (api.example.com, a.b.example.com)
  api-*.example.com      Allow pattern match (api-v1.example.com, api-prod.example.com)

Custom hosts with --add-host:
  --add-host api.internal:10.0.0.10
  --add-host db.internal:10.0.0.11`,
	Example: `  matchlock run --image alpine:latest -it sh
	  matchlock run --image python:3.12-alpine python3 -c 'print(42)'
	  matchlock run --image alpine:latest --rm=false   # keep VM alive after exit
	  matchlock run --image nginx:latest -d            # detached (same lifecycle as --rm=false)
	  matchlock exec <vm-id> echo hello                # exec into running VM

  # With non-secret env vars
  matchlock run --image alpine:latest -e FOO=bar -- sh -c 'echo $FOO'
  matchlock run --image alpine:latest --env-file .env -- printenv

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
	runCmd.Flags().String("workspace", "", "Guest mount point for VFS (required with --volume)")
	runCmd.Flags().String("kernel", "", "Guest kernel ref: file:///absolute/path or OCI image reference")
	runCmd.Flags().StringSlice("allow-host", nil, "Allowed hosts (can be repeated)")
	runCmd.Flags().StringSlice("add-host", nil, "Add a custom host-to-IP mapping (host:ip, can be repeated)")
	runCmd.Flags().StringArrayP("volume", "v", nil, fmt.Sprintf("Volume mount, repeatable (host:guest = overlay snapshot by default; use :%s for direct rw host mount, :%s for read-only host mount; host_fs supports uid/gid owner options)", api.MountTypeHostFS, api.MountOptionReadonlyShort))
	runCmd.Flags().StringArray("disk", nil, "Attach raw ext4 disk image (host_path:guest_mount[:option[,option...]] or @volume_name:guest_mount[:option[,option...]])")
	runCmd.Flags().StringArrayP("env", "e", nil, "Environment variable (KEY=VALUE or KEY; can be repeated)")
	runCmd.Flags().StringArray("env-file", nil, "Environment file (KEY=VALUE or KEY per line; can be repeated)")
	runCmd.Flags().StringArray("secret", nil, "Secret (NAME=VALUE@host1,host2 or NAME@host1,host2)")
	runCmd.Flags().StringArray("secret-placeholder", nil, "Secret placeholder override (NAME=PLACEHOLDER; can be repeated)")
	runCmd.Flags().String("secret-file", "", "JSON file with full secret definitions (name -> {value, placeholder, hosts})")
	runCmd.Flags().StringSlice("dns-servers", nil, "DNS servers (default: 8.8.8.8,8.8.4.4)")
	runCmd.Flags().String("hostname", "", "Guest hostname (default: sandbox ID)")
	runCmd.Flags().Int("mtu", api.DefaultNetworkMTU, "Network MTU for guest interface")
	runCmd.Flags().Bool("no-network", false, "Create sandbox with no network interfaces")
	runCmd.Flags().Bool("network-intercept", false, "Force network interception proxy/stack even when allow-list and secrets are empty")
	runCmd.Flags().StringArrayP("publish", "p", nil, "Publish a host port to a sandbox port ([LOCAL_PORT:]REMOTE_PORT)")
	runCmd.Flags().StringSlice("address", []string{"127.0.0.1"}, "Address to bind published ports on the host (can be repeated)")
	runCmd.Flags().Float64("cpus", float64(api.DefaultCPUs), "Number of CPUs (supports fractional values, e.g. 0.5)")
	runCmd.Flags().Int("memory", api.DefaultMemoryMB, "Memory in MB")
	runCmd.Flags().Int("timeout", api.DefaultTimeoutSeconds, "Timeout in seconds")
	runCmd.Flags().Int("disk-size", api.DefaultDiskSizeMB, "Disk size in MB")
	runCmd.Flags().BoolP("detach", "d", false, "Run sandbox in detached mode (implies --rm=false; incompatible with -t/-i)")
	runCmd.Flags().BoolP("tty", "t", false, "Allocate a pseudo-TTY")
	runCmd.Flags().BoolP("interactive", "i", false, "Keep STDIN open")
	runCmd.Flags().Bool("pull", false, "Always pull image from registry (ignore cache)")
	runCmd.Flags().Bool("rm", true, "Remove sandbox after command exits (set --rm=false to keep running)")
	runCmd.Flags().Bool("privileged", false, "Skip in-guest security restrictions (seccomp, cap drop, no_new_privs)")
	runCmd.Flags().StringP("workdir", "w", "", "Working directory inside the sandbox (default: image WORKDIR, then configured workspace path)")
	runCmd.Flags().StringP("user", "u", "", "Run as user (uid, uid:gid, or username; overrides image USER)")
	runCmd.Flags().String("entrypoint", "", "Override image ENTRYPOINT")
	runCmd.Flags().Duration("graceful-shutdown", api.DefaultGracefulShutdownPeriod, "Graceful shutdown timeout before force-stopping the VM ")
	runCmd.MarkFlagRequired("image")

	viper.BindPFlag("run.image", runCmd.Flags().Lookup("image"))
	viper.BindPFlag("run.workspace", runCmd.Flags().Lookup("workspace"))
	viper.BindPFlag("run.kernel", runCmd.Flags().Lookup("kernel"))
	viper.BindPFlag("run.allow-host", runCmd.Flags().Lookup("allow-host"))
	viper.BindPFlag("run.add-host", runCmd.Flags().Lookup("add-host"))
	viper.BindPFlag("run.volume", runCmd.Flags().Lookup("volume"))
	viper.BindPFlag("run.disk", runCmd.Flags().Lookup("disk"))
	viper.BindPFlag("run.env", runCmd.Flags().Lookup("env"))
	viper.BindPFlag("run.env-file", runCmd.Flags().Lookup("env-file"))
	viper.BindPFlag("run.secret", runCmd.Flags().Lookup("secret"))
	viper.BindPFlag("run.secret-placeholder", runCmd.Flags().Lookup("secret-placeholder"))
	viper.BindPFlag("run.secret-file", runCmd.Flags().Lookup("secret-file"))
	viper.BindPFlag("run.hostname", runCmd.Flags().Lookup("hostname"))
	viper.BindPFlag("run.mtu", runCmd.Flags().Lookup("mtu"))
	viper.BindPFlag("run.no-network", runCmd.Flags().Lookup("no-network"))
	viper.BindPFlag("run.network-intercept", runCmd.Flags().Lookup("network-intercept"))
	viper.BindPFlag("run.publish", runCmd.Flags().Lookup("publish"))
	viper.BindPFlag("run.address", runCmd.Flags().Lookup("address"))
	viper.BindPFlag("run.cpus", runCmd.Flags().Lookup("cpus"))
	viper.BindPFlag("run.memory", runCmd.Flags().Lookup("memory"))
	viper.BindPFlag("run.timeout", runCmd.Flags().Lookup("timeout"))
	viper.BindPFlag("run.disk-size", runCmd.Flags().Lookup("disk-size"))
	viper.BindPFlag("run.detach", runCmd.Flags().Lookup("detach"))
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
	cpus, _ := cmd.Flags().GetFloat64("cpus")
	memory, _ := cmd.Flags().GetInt("memory")
	diskSize, _ := cmd.Flags().GetInt("disk-size")
	timeout, _ := cmd.Flags().GetInt("timeout")

	// Exec options
	detach, _ := cmd.Flags().GetBool("detach")
	tty, _ := cmd.Flags().GetBool("tty")
	interactive, _ := cmd.Flags().GetBool("interactive")
	interactiveMode := tty && interactive
	if err := validateDetachFlags(detach, tty, interactive); err != nil {
		return err
	}
	if detach {
		rm = false
	}
	if detach {
		return startDetachedRun()
	}
	workspace, _ := cmd.Flags().GetString("workspace")
	workspaceSet := cmd.Flags().Changed("workspace")
	kernelRef, _ := cmd.Flags().GetString("kernel")
	workdir, _ := cmd.Flags().GetString("workdir")

	// Network & security
	allowHosts, _ := cmd.Flags().GetStringSlice("allow-host")
	addHostSpecs, _ := cmd.Flags().GetStringSlice("add-host")
	volumes, _ := cmd.Flags().GetStringArray("volume")
	diskMountSpecs, _ := cmd.Flags().GetStringArray("disk")
	envVars, _ := cmd.Flags().GetStringArray("env")
	envFiles, _ := cmd.Flags().GetStringArray("env-file")
	secrets, _ := cmd.Flags().GetStringArray("secret")
	secretPlaceholders, _ := cmd.Flags().GetStringArray("secret-placeholder")
	secretFile, _ := cmd.Flags().GetString("secret-file")
	dnsServers, _ := cmd.Flags().GetStringSlice("dns-servers")
	hostname, _ := cmd.Flags().GetString("hostname")
	networkMTU, _ := cmd.Flags().GetInt("mtu")
	noNetwork, _ := cmd.Flags().GetBool("no-network")
	networkIntercept, _ := cmd.Flags().GetBool("network-intercept")
	publishSpecs, _ := cmd.Flags().GetStringArray("publish")
	addresses, _ := cmd.Flags().GetStringSlice("address")

	if networkMTU <= 0 {
		return fmt.Errorf("--mtu must be > 0")
	}
	vcpuCount, ok := api.VCPUCount(cpus)
	if !ok {
		return fmt.Errorf("--cpus must be a finite number > 0")
	}
	hostCPUs := runtime.NumCPU()
	if vcpuCount > hostCPUs {
		return fmt.Errorf("--cpus must be <= host cpus (%d)", hostCPUs)
	}
	if noNetwork {
		if len(allowHosts) > 0 {
			return fmt.Errorf("--no-network cannot be combined with --allow-host")
		}
		if len(secrets) > 0 || secretFile != "" || len(secretPlaceholders) > 0 {
			return fmt.Errorf("--no-network cannot be combined with --secret")
		}
		if networkIntercept {
			return fmt.Errorf("--no-network cannot be combined with --network-intercept")
		}
	}

	// Shutdown
	gracefulShutdown, _ := cmd.Flags().GetDuration("graceful-shutdown")

	user, _ := cmd.Flags().GetString("user")
	entrypoint, _ := cmd.Flags().GetString("entrypoint")

	resolvedCommand := append([]string(nil), args...)

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
		return errx.Wrap(ErrBuildingRootfs, err)
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
			resolvedCommand = composed
		}
	}
	command := api.ShellQuoteArgs(resolvedCommand)

	if rm && command == "" && !interactiveMode {
		return fmt.Errorf("command required (or use --rm=false to start without a command)")
	}

	sandboxOpts := &sandbox.Options{
		RootfsPaths:   buildResult.LowerPaths,
		RootfsFSTypes: buildResult.LowerFSTypes,
	}

	var vfsConfig *api.VFSConfig
	if workspaceSet || len(volumes) > 0 {
		vfsConfig = &api.VFSConfig{Workspace: workspace}
	}
	if len(volumes) > 0 {
		if workspace == "" {
			return fmt.Errorf("--workspace is required when using --volume")
		}
		mounts := make(map[string]api.MountConfig)
		for _, vol := range volumes {
			spec, err := api.ParseVolumeMountSpec(vol, workspace)
			if err != nil {
				return errx.With(ErrInvalidVolume, " %q: %w", vol, err)
			}

			mount := api.MountConfig{
				Type:     spec.Type,
				HostPath: spec.HostPath,
				Readonly: spec.Readonly,
				OwnerUID: spec.OwnerUID,
				OwnerGID: spec.OwnerGID,
			}
			mounts[spec.GuestPath] = mount
		}
		vfsConfig.Mounts = mounts
	}

	hasVFSMounts := vfsConfig != nil && len(vfsConfig.Mounts) > 0
	extraDisks := make([]api.DiskMount, 0, len(diskMountSpecs))
	for _, spec := range diskMountSpecs {
		diskMount, err := parseDiskMountSpec(spec)
		if err != nil {
			return errx.With(ErrInvalidDiskMount, " %q: %w", spec, err)
		}
		if hasVFSMounts && diskMountShadowedByWorkspace(diskMount.GuestMount, vfsConfig.Workspace) {
			return errx.With(ErrInvalidDiskMount, " %q: guest mount %q is inside workspace %q and will be shadowed by the VFS mount", spec, diskMount.GuestMount, vfsConfig.Workspace)
		}
		extraDisks = append(extraDisks, diskMount)
	}

	parsedSecrets, err := parseRunSecrets(secrets, secretPlaceholders, secretFile)
	if err != nil {
		return err
	}

	parsedEnv, err := api.ParseEnvs(envVars, envFiles)
	if err != nil {
		return errx.Wrap(ErrInvalidEnv, err)
	}

	addHosts, err := api.ParseAddHosts(addHostSpecs)
	if err != nil {
		return errx.Wrap(ErrInvalidAddHost, err)
	}

	portForwards, err := api.ParsePortForwards(publishSpecs)
	if err != nil {
		return errx.Wrap(ErrInvalidPortForward, err)
	}
	if len(portForwards) > 0 {
		addresses, err = normalizePortForwardAddresses(addresses)
		if err != nil {
			return errx.Wrap(ErrInvalidPortForwardAddr, err)
		}
	}

	config := &api.Config{
		Image:      imageName,
		Privileged: privileged,
		Kernel:     &api.KernelConfig{Ref: kernelRef},
		Resources: &api.Resources{
			CPUs:           cpus,
			MemoryMB:       memory,
			DiskSizeMB:     diskSize,
			TimeoutSeconds: timeout,
		},
		Network: &api.NetworkConfig{
			AllowedHosts:    allowHosts,
			AddHosts:        addHosts,
			BlockPrivateIPs: true,
			NoNetwork:       noNetwork,
			Intercept:       networkIntercept,
			Secrets:         parsedSecrets,
			DNSServers:      dnsServers,
			Hostname:        hostname,
			MTU:             networkMTU,
		},
		VFS:        vfsConfig,
		Env:        parsedEnv,
		ExtraDisks: extraDisks,
		ImageCfg:   imageCfg,
	}
	if err := config.Network.Validate(); err != nil {
		return err
	}
	if err := config.ValidateVFS(); err != nil {
		return err
	}

	sb, err := sandbox.New(ctx, config, sandboxOpts)
	if err != nil {
		return errx.Wrap(ErrCreateSandbox, err)
	}

	if err := sb.Start(ctx); err != nil {
		closeErr := sb.Close(ctx)
		if closeErr != nil {
			return errors.Join(errx.Wrap(ErrStartSandbox, err), errx.Wrap(ErrCloseSandbox, closeErr))
		}
		return errx.Wrap(ErrStartSandbox, err)
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

	cleanupSandbox := func(remove bool) error {
		c, cancel := closeContext(gracefulShutdown)
		defer cancel()

		var errs []error
		if err := sb.Close(c); err != nil {
			errs = append(errs, errx.Wrap(ErrCloseSandbox, err))
		}
		if remove {
			if err := stateMgr.Remove(sb.ID()); err != nil {
				errs = append(errs, errx.Wrap(ErrRemoveSandbox, err))
			}
		}
		if len(errs) > 0 {
			return errors.Join(errs...)
		}
		return nil
	}

	var pfManager *sandbox.PortForwardManager
	if len(portForwards) > 0 {
		pfManager, err = sb.StartPortForwards(ctx, addresses, portForwards)
		if err != nil {
			pfErr := errx.Wrap(ErrPortForwardListen, err)
			if rm {
				if cleanupErr := cleanupSandbox(true); cleanupErr != nil {
					return errors.Join(pfErr, cleanupErr)
				}
			}
			return pfErr
		}
		defer pfManager.Close()
		for _, b := range pfManager.Bindings() {
			fmt.Fprintf(os.Stderr, "Forwarding %s:%d -> sandbox:%d\n", b.Address, b.LocalPort, b.RemotePort)
		}
	}

	if interactiveMode {
		exitCode := runInteractive(ctx, sb, command, workdir)
		if rm {
			if err := cleanupSandbox(true); err != nil {
				return err
			}
			return commandExit(exitCode)
		}
		// Keep sandbox alive for follow-up `matchlock exec` sessions.
		<-ctx.Done()
		return cleanupSandbox(false)
	}

	if command != "" {
		logWriter, err := openVMLogAppender(stateMgr.LogPath(sb.ID()))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to open VM log: %v\n", err)
		}
		if logWriter != nil {
			defer logWriter.Close()
		}

		stdout := io.Writer(os.Stdout)
		stderr := io.Writer(os.Stderr)
		if logWriter != nil {
			stdout = io.MultiWriter(os.Stdout, logWriter)
			stderr = io.MultiWriter(os.Stderr, logWriter)
		}

		opts := &api.ExecOptions{
			Stdout: stdout,
			Stderr: stderr,
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
				if cleanupErr := cleanupSandbox(true); cleanupErr != nil {
					return errors.Join(errx.Wrap(ErrExecCommand, err), cleanupErr)
				}
			}
			return errx.Wrap(ErrExecCommand, err)
		}

		if rm {
			if err := cleanupSandbox(true); err != nil {
				return err
			}
			return commandExit(result.ExitCode)
		}
	}

	if !rm {
		// Block until signal — keeps the sandbox alive for `matchlock exec`
		<-ctx.Done()
		return cleanupSandbox(false)
	}

	return nil
}

func parseRunSecrets(secretSpecs, placeholderSpecs []string, secretFile string) (map[string]api.Secret, error) {
	parsedSecrets := make(map[string]api.Secret)

	if secretFile != "" {
		secretsFromFile, err := loadSecretsFile(secretFile)
		if err != nil {
			return nil, errx.With(ErrInvalidSecret, " %q: %v", secretFile, err)
		}
		for name, secret := range secretsFromFile {
			parsedSecrets[name] = secret
		}
	}

	for _, s := range secretSpecs {
		name, secret, err := api.ParseSecret(s)
		if err != nil {
			return nil, errx.With(ErrInvalidSecret, " %q: %v", s, err)
		}
		parsedSecrets[name] = secret
	}

	for _, s := range placeholderSpecs {
		name, placeholder, err := api.ParseSecretPlaceholder(s)
		if err != nil {
			return nil, errx.With(ErrInvalidSecret, " %q: %v", s, err)
		}

		secret, ok := parsedSecrets[name]
		if !ok {
			return nil, errx.With(ErrInvalidSecret, " placeholder %q references unknown secret %q", s, name)
		}
		secret.Placeholder = placeholder
		parsedSecrets[name] = secret
	}

	if len(parsedSecrets) == 0 {
		return nil, nil
	}

	if err := api.ValidateSecretPlaceholders(parsedSecrets); err != nil {
		return nil, errx.With(ErrInvalidSecret, " %v", err)
	}

	return parsedSecrets, nil
}

func loadSecretsFile(path string) (map[string]api.Secret, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var secrets map[string]api.Secret
	if err := json.Unmarshal(data, &secrets); err != nil {
		return nil, err
	}

	for name, secret := range secrets {
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("secret name cannot be empty")
		}
		if len(secret.Hosts) == 0 {
			return nil, fmt.Errorf("secret %q must specify at least one host", name)
		}
		trimmedHosts := make([]string, 0, len(secret.Hosts))
		for _, host := range secret.Hosts {
			host = strings.TrimSpace(host)
			if host == "" {
				return nil, fmt.Errorf("secret %q has an empty host entry", name)
			}
			trimmedHosts = append(trimmedHosts, host)
		}
		if secret.Value == "" {
			return nil, fmt.Errorf("secret %q must specify a value", name)
		}
		secrets[name] = api.Secret{
			Value:       secret.Value,
			Placeholder: strings.TrimSpace(secret.Placeholder),
			Hosts:       trimmedHosts,
		}
	}

	return secrets, nil
}

func validateDetachFlags(detach, tty, interactive bool) error {
	if detach && (tty || interactive) {
		return fmt.Errorf("--detach cannot be combined with -t/--tty or -i/--interactive")
	}
	return nil
}

func startDetachedRun() error {
	exePath, err := os.Executable()
	if err != nil {
		return errx.Wrap(ErrResolveExecutable, err)
	}
	return startDetachedChild(exePath, detachedChildArgs(os.Args[1:]), detachedStartupTimeout)
}

// startDetachedChild forks the detached child, reaps it exactly once, and waits
// for it to register a running VM row. It never waits for the VM's whole
// lifetime: as soon as the row appears the child's pid is returned to the
// caller while the child keeps running (for --rm=false) under its own session.
func startDetachedChild(exePath string, childArgs []string, startupTimeout time.Duration) error {
	nullFile, err := os.OpenFile(os.DevNull, os.O_RDWR, 0600)
	if err != nil {
		return errx.Wrap(ErrPrepareDetachedIO, err)
	}
	defer nullFile.Close()

	// Capture stderr so an early exit can surface the child's own diagnostic
	// instead of discarding it to /dev/null. A regular file (not a pipe) is
	// required because the detached child outlives this process: a pipe reader
	// closing on parent exit would deliver SIGPIPE to the child as soon as it
	// wrote again. The file is unlinked on return; the child keeps its fd.
	stderrFile, err := os.CreateTemp("", "matchlock-detached-*.stderr")
	if err != nil {
		return errx.Wrap(ErrPrepareDetachedIO, err)
	}
	defer func() {
		_ = stderrFile.Close()
		_ = os.Remove(stderrFile.Name())
	}()

	child := exec.Command(exePath, childArgs...)
	child.Stdin = nullFile
	child.Stdout = nullFile
	child.Stderr = stderrFile
	child.Env = os.Environ()
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := child.Start(); err != nil {
		return errx.Wrap(ErrStartDetachedRun, err)
	}
	pid := child.Process.Pid

	// Reap the child exactly once. The detached child legitimately outlives
	// startup, so this goroutine only prevents a zombie and captures an early
	// exit; it is not awaited on the success path.
	childExited := make(chan detachedResult, 1)
	go func() {
		waitErr := child.Wait()
		childExited <- detachedResult{
			exitErr: waitErr,
			stderr:  readFileTail(stderrFile, detachedStderrTailBytes),
		}
	}()

	vmID, err := waitDetachedVMID(pid, childExited, startupTimeout)
	if err != nil {
		return err
	}
	fmt.Println(vmID)
	return nil
}

func detachedChildArgs(args []string) []string {
	splitAt := len(args)
	for i, arg := range args {
		if arg == "--" {
			splitAt = i
			break
		}
	}

	cliArgs := args[:splitAt]
	tail := args[splitAt:]

	out := make([]string, 0, len(cliArgs)+2+len(tail))
	out = append(out, cliArgs...)
	out = append(out, "--detach=false")
	out = append(out, "--rm=false")
	out = append(out, tail...)
	return out
}

// detachedResult is the single report published by the detached child's reaper.
type detachedResult struct {
	exitErr error
	stderr  string
}

// readFileTail returns at most the last max bytes of f. It must be called after
// the writer has exited so the file size is stable.
func readFileTail(f *os.File, max int) string {
	if f == nil || max <= 0 {
		return ""
	}
	fi, err := f.Stat()
	if err != nil || fi.Size() <= 0 {
		return ""
	}
	size := fi.Size()
	offset := int64(0)
	if size > int64(max) {
		offset = size - int64(max)
	}
	buf := make([]byte, size-offset)
	if _, err := f.ReadAt(buf, offset); err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	return string(buf)
}

func waitDetachedVMID(pid int, childExited <-chan detachedResult, timeout time.Duration) (string, error) {
	mgr := state.NewManager()
	ticker := time.NewTicker(detachedVMPollInterval)
	defer ticker.Stop()

	var timeoutCh <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		timeoutCh = timer.C
	}

	for {
		states, err := mgr.List()
		if err == nil {
			for _, vmState := range states {
				if vmState.PID == pid && vmState.Status == "running" {
					return vmState.ID, nil
				}
			}
		}

		// Prefer the child's own exit report: it carries the diagnostic that
		// explains why no VM row ever appeared. Reaping is guaranteed to
		// publish exactly one result, so this cannot be starved.
		select {
		case result := <-childExited:
			return "", detachedExitError(pid, result)
		case <-timeoutCh:
			killDetachedChild(pid)
			return "", errx.With(ErrDetachedStartupTimeout, " for detached process pid=%d after %s", pid, timeout)
		case <-ticker.C:
		}

		if !isProcessRunning(pid) {
			// The child is gone. Wait briefly for the reaper to publish its
			// captured status so the real error is surfaced instead of a bare
			// "process not found".
			if result, ok := awaitDetachedResult(childExited, detachedReapGrace); ok {
				return "", detachedExitError(pid, result)
			}
			return "", errx.With(ErrFindDetachedVM, " for detached process pid=%d", pid)
		}
	}
}

// awaitDetachedResult returns the reaper's verdict, waiting up to grace for it.
func awaitDetachedResult(childExited <-chan detachedResult, grace time.Duration) (detachedResult, bool) {
	if childExited == nil {
		return detachedResult{}, false
	}
	if grace <= 0 {
		select {
		case result := <-childExited:
			return result, true
		default:
			return detachedResult{}, false
		}
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case result := <-childExited:
		return result, true
	case <-timer.C:
		return detachedResult{}, false
	}
}

func detachedExitError(pid int, result detachedResult) error {
	stderr := strings.TrimSpace(result.stderr)
	switch {
	case result.exitErr != nil && stderr != "":
		return errx.With(ErrDetachedRunExited, " pid=%d: %v: %s", pid, result.exitErr, stderr)
	case result.exitErr != nil:
		return errx.With(ErrDetachedRunExited, " pid=%d: %v", pid, result.exitErr)
	case stderr != "":
		return errx.With(ErrDetachedRunExited, " pid=%d before registering a VM: %s", pid, stderr)
	default:
		return errx.With(ErrDetachedRunExited, " pid=%d before registering a VM", pid)
	}
}

// killDetachedChild makes a best-effort attempt to stop a child that overran
// the startup deadline so no orphan is left behind.
func killDetachedChild(pid int) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

func isProcessRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	if processIsZombie(pid) {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
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

	resizeCh, stopResize := watchTerminalResize(int(os.Stdin.Fd()))
	defer stopResize()

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

func openVMLogAppender(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
}

func parseDiskMountSpec(spec string) (api.DiskMount, error) {
	parts := strings.SplitN(spec, ":", 3)
	if len(parts) < 2 || len(parts) > 3 {
		return api.DiskMount{}, fmt.Errorf("expected format host_path:guest_mount[:option[,option...]] or @volume_name:guest_mount[:option[,option...]]")
	}

	hostPath := strings.TrimSpace(parts[0])
	guestMount := strings.TrimSpace(parts[1])
	if hostPath == "" || guestMount == "" {
		return api.DiskMount{}, fmt.Errorf("host_path and guest_mount are required")
	}

	if strings.HasPrefix(hostPath, "@") {
		volumeName := strings.TrimSpace(strings.TrimPrefix(hostPath, "@"))
		if volumeName == "" {
			return api.DiskMount{}, fmt.Errorf("volume name is required after '@'")
		}
		path, err := findNamedVolume(volumeName)
		if err != nil {
			return api.DiskMount{}, err
		}
		hostPath = path
	} else {
		if !filepath.IsAbs(hostPath) {
			abs, err := filepath.Abs(hostPath)
			if err != nil {
				return api.DiskMount{}, err
			}
			hostPath = abs
		}
		if _, err := os.Stat(hostPath); err != nil {
			return api.DiskMount{}, fmt.Errorf("host path does not exist: %s", hostPath)
		}
	}

	if err := api.ValidateGuestMount(guestMount); err != nil {
		return api.DiskMount{}, err
	}

	readonly := false
	var ownerUID *uint32
	var ownerGID *uint32
	if len(parts) == 3 {
		for _, rawOption := range strings.Split(parts[2], ",") {
			option := strings.TrimSpace(rawOption)
			key, value, hasValue := strings.Cut(option, "=")
			key = strings.ToLower(strings.TrimSpace(key))
			value = strings.TrimSpace(value)

			switch key {
			case "":
				continue
			case api.MountOptionReadonlyShort, api.MountOptionReadonly:
				if hasValue {
					return api.DiskMount{}, unknownDiskMountOption(option)
				}
				readonly = true
			case "rw":
				if hasValue {
					return api.DiskMount{}, unknownDiskMountOption(option)
				}
				readonly = false
			case "uid":
				if !hasValue {
					return api.DiskMount{}, unknownDiskMountOption(option)
				}
				uid, err := parseDiskOwnerID("uid", value)
				if err != nil {
					return api.DiskMount{}, err
				}
				ownerUID = &uid
			case "gid":
				if !hasValue {
					return api.DiskMount{}, unknownDiskMountOption(option)
				}
				gid, err := parseDiskOwnerID("gid", value)
				if err != nil {
					return api.DiskMount{}, err
				}
				ownerGID = &gid
			default:
				return api.DiskMount{}, unknownDiskMountOption(option)
			}
		}
	}
	if readonly && (ownerUID != nil || ownerGID != nil) {
		return api.DiskMount{}, fmt.Errorf("disk ownership options require a writable mount")
	}

	return api.DiskMount{
		HostPath:   hostPath,
		GuestMount: guestMount,
		ReadOnly:   readonly,
		OwnerUID:   ownerUID,
		OwnerGID:   ownerGID,
	}, nil
}

func unknownDiskMountOption(option string) error {
	return fmt.Errorf("unknown disk option %q (use 'ro', 'readonly', 'rw', 'uid=UID', or 'gid=GID')", option)
}

func parseDiskOwnerID(name string, value string) (uint32, error) {
	if value == "" {
		return 0, fmt.Errorf("invalid disk owner: %s cannot be empty", name)
	}
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid disk owner: %s %q must be an unsigned 32-bit integer: %w", name, value, err)
	}
	return uint32(parsed), nil
}

func diskMountShadowedByWorkspace(guestMount, workspace string) bool {
	if strings.TrimSpace(workspace) == "" {
		return false
	}
	return api.ValidateGuestPathWithinWorkspace(guestMount, workspace) == nil
}
