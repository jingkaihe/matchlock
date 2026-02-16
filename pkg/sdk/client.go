// Package sdk provides a client for interacting with Matchlock sandboxes via JSON-RPC.
//
// Use the builder API for a fluent experience:
//
//	client, err := sdk.NewClient(sdk.DefaultConfig())
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer client.Close(0)
//
//	sandbox := sdk.New("python:3.12-alpine").
//	    WithCPUs(2).
//	    WithMemory(1024).
//	    AllowHost("dl-cdn.alpinelinux.org", "api.openai.com").
//	    AddSecret("API_KEY", os.Getenv("API_KEY"), "api.openai.com")
//
//	vmID, err := client.Launch(sandbox)
//
//	result, err := client.Exec(ctx, "echo hello")
//	fmt.Println(result.Stdout)
package sdk

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/api"
)

// Client is a Matchlock JSON-RPC client.
// All methods are safe for concurrent use.
type Client struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    *bufio.Reader
	stderr    io.ReadCloser
	requestID atomic.Uint64
	vmID      string
	mu        sync.Mutex // legacy — kept for Close()
	closed    bool

	// Concurrent request handling
	writeMu    sync.Mutex                 // serializes writes to stdin
	pendingMu  sync.Mutex                 // protects pending map
	pending    map[uint64]*pendingRequest // in-flight requests by ID
	readerOnce sync.Once                  // ensures reader goroutine starts once

	vfsHookMu      sync.RWMutex
	vfsHooks       []compiledVFSHook
	vfsMutateHooks []compiledVFSMutateHook
	vfsActionHooks []compiledVFSActionHook
	vfsHookActive  atomic.Bool
}

// Config holds client configuration
type Config struct {
	// BinaryPath is the path to the matchlock binary
	BinaryPath string
	// UseSudo runs matchlock with sudo (required for TAP devices)
	UseSudo bool
}

// DefaultConfig returns the default client configuration
func DefaultConfig() Config {
	path := os.Getenv("MATCHLOCK_BIN")
	if path == "" {
		path = "matchlock"
	}
	return Config{
		BinaryPath: path,
	}
}

// NewClient creates a new Matchlock client and starts the RPC process
func NewClient(cfg Config) (*Client, error) {
	var cmd *exec.Cmd
	if cfg.UseSudo {
		cmd = exec.Command("sudo", cfg.BinaryPath, "rpc")
	} else {
		cmd = exec.Command(cfg.BinaryPath, "rpc")
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, errx.Wrap(ErrStdinPipe, err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, errx.Wrap(ErrStdoutPipe, err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, errx.Wrap(ErrStderrPipe, err)
	}

	if err := cmd.Start(); err != nil {
		return nil, errx.Wrap(ErrStartProc, err)
	}

	// Drain stderr in background to prevent blocking
	go io.Copy(io.Discard, stderr)

	return &Client{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  bufio.NewReader(stdout),
		stderr:  stderr,
		pending: make(map[uint64]*pendingRequest),
	}, nil
}

// VMID returns the ID of the current VM, or empty string if none created
func (c *Client) VMID() string {
	return c.vmID
}

// Close closes the sandbox and cleans up resources.
// The VM state directory is preserved so it appears in "matchlock list".
// Call Remove after Close to delete the state entirely.
//
// timeout controls how long to wait for the process to exit after sending the
// close request. A zero value uses a short grace period and then force-kills
// if needed. When a non-zero timeout expires, the process is forcefully killed.
func (c *Client) Close(timeout time.Duration) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()

	c.setVFSHooks(nil, nil, nil)

	effectiveTimeout := timeout
	if effectiveTimeout <= 0 {
		effectiveTimeout = 2 * time.Second
	}

	params := map[string]interface{}{
		"timeout_seconds": effectiveTimeout.Seconds(),
	}

	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()

	// Send close RPC with a bounded context so it doesn't block forever
	// (e.g. if the handler is draining in-flight cancelled requests).
	closeCtx, closeCancel := context.WithTimeout(context.Background(), effectiveTimeout+5*time.Second)
	c.sendRequestCtx(closeCtx, "close", params, nil)
	closeCancel()
	c.stdin.Close()

	select {
	case err := <-done:
		return err
	case <-time.After(effectiveTimeout):
		c.cmd.Process.Kill()
		<-done
		return errx.With(ErrCloseTimeout, " after %s", effectiveTimeout)
	}
}

// Remove deletes the stopped VM state directory.
// Must be called after Close. Uses the matchlock CLI binary
// that was configured in Config.BinaryPath.
func (c *Client) Remove() error {
	if c.vmID == "" {
		return nil
	}
	bin := c.cmd.Path
	out, err := exec.Command(bin, "rm", c.vmID).CombinedOutput()
	if err != nil {
		return errx.With(ErrRemoveVM, " %s: %s: %w", c.vmID, out, err)
	}
	return nil
}

// CreateOptions holds options for creating a sandbox
type CreateOptions struct {
	// Image is the container image reference (required, e.g., alpine:latest)
	Image string
	// Privileged skips in-guest security restrictions (seccomp, cap drop, no_new_privs)
	Privileged bool
	// CPUs is the number of vCPUs
	CPUs int
	// MemoryMB is the memory in megabytes
	MemoryMB int
	// DiskSizeMB is the disk size in megabytes (default: 5120)
	DiskSizeMB int
	// TimeoutSeconds is the maximum execution time
	TimeoutSeconds int
	// AllowedHosts is a list of allowed network hosts (supports wildcards)
	AllowedHosts []string
	// BlockPrivateIPs controls access to private IP ranges.
	// Use together with BlockPrivateIPsSet to express explicit true/false.
	BlockPrivateIPs bool
	// BlockPrivateIPsSet marks whether BlockPrivateIPs was explicitly set.
	// When false, the SDK preserves API defaults for private IP blocking.
	BlockPrivateIPsSet bool
	// Mounts defines VFS mount configurations
	Mounts map[string]MountConfig
	// Env defines non-secret environment variables for command execution.
	// These are visible in VM state and inspect/get outputs.
	Env map[string]string
	// Secrets defines secrets to inject (replaced in HTTP requests to allowed hosts)
	Secrets []Secret
	// Workspace is the mount point for VFS in the guest (default: /workspace)
	Workspace string
	// VFSInterception configures host-side VFS interception hooks/rules.
	VFSInterception *VFSInterceptionConfig
	// DNSServers overrides the default DNS servers (8.8.8.8, 8.8.4.4)
	DNSServers []string
	// NetworkMTU overrides the guest interface/network stack MTU (default: 1500).
	NetworkMTU int
	// PortForwards maps local host ports to remote sandbox ports.
	// These are applied after VM creation via the port_forward RPC.
	PortForwards []api.PortForward
	// PortForwardAddresses controls host bind addresses used when applying
	// PortForwards (default: 127.0.0.1).
	PortForwardAddresses []string
	// ImageConfig holds OCI image metadata (USER, ENTRYPOINT, CMD, WORKDIR, ENV)
	ImageConfig *ImageConfig
}

// ImageConfig holds OCI image metadata for user/entrypoint/cmd/workdir/env.
type ImageConfig struct {
	User       string            `json:"user,omitempty"`
	WorkingDir string            `json:"working_dir,omitempty"`
	Entrypoint []string          `json:"entrypoint,omitempty"`
	Cmd        []string          `json:"cmd,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
}

// Secret defines a secret that will be injected as a placeholder env var
// and replaced with the real value in HTTP requests to allowed hosts
type Secret struct {
	// Name is the environment variable name (e.g., "ANTHROPIC_API_KEY")
	Name string
	// Value is the actual secret value
	Value string
	// Hosts is a list of hosts where this secret can be used (supports wildcards)
	Hosts []string
}

// MountConfig defines a VFS mount
type MountConfig struct {
	Type     string `json:"type"` // memory, real_fs, overlay
	HostPath string `json:"host_path,omitempty"`
	Readonly bool   `json:"readonly,omitempty"`
}

// VFSInterceptionConfig configures host-side VFS interception rules.
type VFSInterceptionConfig struct {
	EmitEvents bool          `json:"emit_events,omitempty"`
	Rules      []VFSHookRule `json:"rules,omitempty"`
}

// VFS hook phases.
type VFSHookPhase = string

const (
	VFSHookPhaseBefore VFSHookPhase = "before"
	VFSHookPhaseAfter  VFSHookPhase = "after"
)

// VFS hook actions.
type VFSHookAction = string

const (
	VFSHookActionAllow VFSHookAction = "allow"
	VFSHookActionBlock VFSHookAction = "block"
)

// VFS hook operations.
type VFSHookOp = string

const (
	VFSHookOpStat      VFSHookOp = "stat"
	VFSHookOpReadDir   VFSHookOp = "readdir"
	VFSHookOpOpen      VFSHookOp = "open"
	VFSHookOpCreate    VFSHookOp = "create"
	VFSHookOpMkdir     VFSHookOp = "mkdir"
	VFSHookOpChmod     VFSHookOp = "chmod"
	VFSHookOpRemove    VFSHookOp = "remove"
	VFSHookOpRemoveAll VFSHookOp = "remove_all"
	VFSHookOpRename    VFSHookOp = "rename"
	VFSHookOpSymlink   VFSHookOp = "symlink"
	VFSHookOpReadlink  VFSHookOp = "readlink"
	VFSHookOpRead      VFSHookOp = "read"
	VFSHookOpWrite     VFSHookOp = "write"
	VFSHookOpClose     VFSHookOp = "close"
	VFSHookOpSync      VFSHookOp = "sync"
	VFSHookOpTruncate  VFSHookOp = "truncate"
)

// VFSHookRule describes a single interception rule.
type VFSHookRule struct {
	Name      string        `json:"name,omitempty"`
	Phase     VFSHookPhase  `json:"phase,omitempty"`  // before, after
	Ops       []VFSHookOp   `json:"ops,omitempty"`    // read, write, create, ...
	Path      string        `json:"path,omitempty"`   // filepath-style glob
	Action    VFSHookAction `json:"action,omitempty"` // allow, block
	TimeoutMS int           `json:"timeout_ms,omitempty"`
	// Hook is safe-by-default and does not expose client methods.
	Hook VFSHookFunc `json:"-"`
	// DangerousHook disables recursion suppression and may retrigger itself.
	// Use only when you intentionally want re-entrant callbacks.
	DangerousHook VFSDangerousHookFunc `json:"-"`
	MutateHook    VFSMutateHookFunc    `json:"-"`
	ActionHook    VFSActionHookFunc    `json:"-"`
}

// VFSHookEvent contains metadata about an intercepted file event.
type VFSHookEvent struct {
	Op   VFSHookOp
	Path string
	Size int64
	Mode uint32
	UID  int
	GID  int
}

// VFSHookFunc runs in the SDK process when a matching after-file-event is observed.
// Returning an error currently does not fail the triggering VFS operation.
type VFSHookFunc func(ctx context.Context, event VFSHookEvent) error

// VFSDangerousHookFunc runs with a client handle and can trigger re-entrant hook execution.
// Use this only when you intentionally need side effects that call back into the sandbox.
type VFSDangerousHookFunc func(ctx context.Context, client *Client, event VFSHookEvent) error

// VFSMutateRequest is passed to SDK-local mutate hooks before WriteFile.
type VFSMutateRequest struct {
	Path string
	Size int
	Mode uint32
	UID  int
	GID  int
}

// VFSMutateHookFunc computes replacement bytes for SDK WriteFile calls.
// This hook runs in the SDK process and currently applies only to write_file RPCs.
type VFSMutateHookFunc func(ctx context.Context, req VFSMutateRequest) ([]byte, error)

// VFSActionRequest is passed to SDK-local allow/block action hooks.
type VFSActionRequest struct {
	Op   VFSHookOp
	Path string
	Size int
	Mode uint32
	UID  int
	GID  int
}

// VFSActionHookFunc decides whether an operation should be allowed or blocked.
type VFSActionHookFunc func(ctx context.Context, req VFSActionRequest) VFSHookAction

type compiledVFSHook struct {
	name      string
	ops       map[string]struct{}
	path      string
	timeout   time.Duration
	dangerous bool
	callback  func(ctx context.Context, client *Client, event VFSHookEvent) error
}

type compiledVFSMutateHook struct {
	name     string
	ops      map[string]struct{}
	path     string
	callback VFSMutateHookFunc
}

type compiledVFSActionHook struct {
	name     string
	ops      map[string]struct{}
	path     string
	callback VFSActionHookFunc
}

// Create creates and starts a new sandbox VM.
// If post-create setup fails (for example, port-forward bind errors), it
// returns the created VM ID with a non-nil error so callers can clean up.
func (c *Client) Create(opts CreateOptions) (string, error) {
	if opts.Image == "" {
		return "", ErrImageRequired
	}
	if opts.CPUs == 0 {
		opts.CPUs = api.DefaultCPUs
	}
	if opts.MemoryMB == 0 {
		opts.MemoryMB = api.DefaultMemoryMB
	}
	if opts.DiskSizeMB == 0 {
		opts.DiskSizeMB = api.DefaultDiskSizeMB
	}
	if opts.TimeoutSeconds == 0 {
		opts.TimeoutSeconds = api.DefaultTimeoutSeconds
	}
	if opts.NetworkMTU < 0 {
		return "", ErrInvalidNetworkMTU
	}

	wireVFS, localHooks, localMutateHooks, localActionHooks, err := compileVFSHooks(opts.VFSInterception)
	if err != nil {
		return "", err
	}

	params := map[string]interface{}{
		"image": opts.Image,
		"resources": map[string]interface{}{
			"cpus":            opts.CPUs,
			"memory_mb":       opts.MemoryMB,
			"disk_size_mb":    opts.DiskSizeMB,
			"timeout_seconds": opts.TimeoutSeconds,
		},
	}

	if opts.Privileged {
		params["privileged"] = true
	}

	if network := buildCreateNetworkParams(opts); network != nil {
		params["network"] = network
	}

	if len(opts.Mounts) > 0 || opts.Workspace != "" || wireVFS != nil {
		vfs := make(map[string]interface{})
		if len(opts.Mounts) > 0 {
			vfs["mounts"] = opts.Mounts
		}
		if opts.Workspace != "" {
			vfs["workspace"] = opts.Workspace
		}
		if wireVFS != nil {
			vfs["interception"] = wireVFS
		}
		params["vfs"] = vfs
	}

	if len(opts.Env) > 0 {
		params["env"] = opts.Env
	}

	if opts.ImageConfig != nil {
		params["image_config"] = opts.ImageConfig
	}

	result, err := c.sendRequest("create", params)
	if err != nil {
		return "", err
	}

	var createResult struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(result, &createResult); err != nil {
		return "", errx.Wrap(ErrParseCreateResult, err)
	}

	c.vmID = createResult.ID
	c.setVFSHooks(localHooks, localMutateHooks, localActionHooks)

	if len(opts.PortForwards) > 0 {
		if _, err := c.portForwardMappings(context.Background(), opts.PortForwardAddresses, opts.PortForwards); err != nil {
			return c.vmID, err
		}
	}
	return c.vmID, nil
}

func buildCreateNetworkParams(opts CreateOptions) map[string]interface{} {
	hasAllowedHosts := len(opts.AllowedHosts) > 0
	hasSecrets := len(opts.Secrets) > 0
	hasDNSServers := len(opts.DNSServers) > 0
	hasMTU := opts.NetworkMTU > 0
	blockPrivateIPs, hasBlockPrivateIPsOverride := resolveCreateBlockPrivateIPs(opts)

	includeNetwork := hasAllowedHosts || hasSecrets || hasDNSServers || hasMTU || hasBlockPrivateIPsOverride
	if !includeNetwork {
		return nil
	}

	// Create config merges replace network defaults wholesale. Preserve default
	// private-IP blocking unless explicitly overridden.
	if !hasBlockPrivateIPsOverride {
		blockPrivateIPs = true
	}

	network := map[string]interface{}{
		"allowed_hosts":     opts.AllowedHosts,
		"block_private_ips": blockPrivateIPs,
	}
	if hasSecrets {
		secrets := make(map[string]interface{})
		for _, s := range opts.Secrets {
			secrets[s.Name] = map[string]interface{}{
				"value": s.Value,
				"hosts": s.Hosts,
			}
		}
		network["secrets"] = secrets
	}
	if hasDNSServers {
		network["dns_servers"] = opts.DNSServers
	}
	if hasMTU {
		network["mtu"] = opts.NetworkMTU
	}
	return network
}

func resolveCreateBlockPrivateIPs(opts CreateOptions) (value bool, hasOverride bool) {
	if opts.BlockPrivateIPsSet {
		return opts.BlockPrivateIPs, true
	}
	// Backward compatibility: old callers could only express explicit true.
	if opts.BlockPrivateIPs {
		return true, true
	}
	return false, false
}

func compileVFSHooks(cfg *VFSInterceptionConfig) (*VFSInterceptionConfig, []compiledVFSHook, []compiledVFSMutateHook, []compiledVFSActionHook, error) {
	if cfg == nil {
		return nil, nil, nil, nil, nil
	}

	wire := &VFSInterceptionConfig{
		EmitEvents: cfg.EmitEvents,
	}
	local := make([]compiledVFSHook, 0, len(cfg.Rules))
	localMutate := make([]compiledVFSMutateHook, 0, len(cfg.Rules))
	localAction := make([]compiledVFSActionHook, 0, len(cfg.Rules))
	wire.Rules = make([]VFSHookRule, 0, len(cfg.Rules))

	for _, rule := range cfg.Rules {
		callbackCount := 0
		if rule.Hook != nil {
			callbackCount++
		}
		if rule.DangerousHook != nil {
			callbackCount++
		}
		if rule.MutateHook != nil {
			callbackCount++
		}
		if rule.ActionHook != nil {
			callbackCount++
		}
		if callbackCount > 1 {
			return nil, nil, nil, nil, errx.With(ErrInvalidVFSHook, " %q cannot set more than one callback hook", rule.Name)
		}

		if rule.Hook == nil && rule.DangerousHook == nil && rule.MutateHook == nil && rule.ActionHook == nil {
			action := strings.ToLower(strings.TrimSpace(string(rule.Action)))
			switch action {
			case "mutate_write":
				return nil, nil, nil, nil, errx.With(ErrInvalidVFSHook, " %q mutate_write requires MutateHook callback", rule.Name)
			}
			wire.Rules = append(wire.Rules, rule)
			continue
		}

		if rule.Hook != nil {
			if action := strings.ToLower(strings.TrimSpace(string(rule.Action))); action != "" && action != string(VFSHookActionAllow) {
				return nil, nil, nil, nil, errx.With(ErrInvalidVFSHook, " %q callback hooks cannot set action=%q", rule.Name, rule.Action)
			}
			if !strings.EqualFold(rule.Phase, VFSHookPhaseAfter) {
				return nil, nil, nil, nil, errx.With(ErrInvalidVFSHook, " %q must use phase=after", rule.Name)
			}

			compiled := compiledVFSHook{
				name: rule.Name,
				path: rule.Path,
				callback: func(ctx context.Context, _ *Client, event VFSHookEvent) error {
					return rule.Hook(ctx, event)
				},
			}
			if rule.TimeoutMS > 0 {
				compiled.timeout = time.Duration(rule.TimeoutMS) * time.Millisecond
			}
			if len(rule.Ops) > 0 {
				compiled.ops = make(map[string]struct{}, len(rule.Ops))
				for _, op := range rule.Ops {
					if op == "" {
						continue
					}
					compiled.ops[strings.ToLower(op)] = struct{}{}
				}
			}
			local = append(local, compiled)
			continue
		}

		if rule.DangerousHook != nil {
			if action := strings.ToLower(strings.TrimSpace(string(rule.Action))); action != "" && action != string(VFSHookActionAllow) {
				return nil, nil, nil, nil, errx.With(ErrInvalidVFSHook, " %q dangerous hooks cannot set action=%q", rule.Name, rule.Action)
			}
			if !strings.EqualFold(rule.Phase, VFSHookPhaseAfter) {
				return nil, nil, nil, nil, errx.With(ErrInvalidVFSHook, " %q dangerous hooks must use phase=after", rule.Name)
			}

			compiled := compiledVFSHook{
				name:      rule.Name,
				path:      rule.Path,
				timeout:   0,
				dangerous: true,
				callback: func(ctx context.Context, client *Client, event VFSHookEvent) error {
					return rule.DangerousHook(ctx, client, event)
				},
			}
			if rule.TimeoutMS > 0 {
				compiled.timeout = time.Duration(rule.TimeoutMS) * time.Millisecond
			}
			if len(rule.Ops) > 0 {
				compiled.ops = make(map[string]struct{}, len(rule.Ops))
				for _, op := range rule.Ops {
					if op == "" {
						continue
					}
					compiled.ops[strings.ToLower(op)] = struct{}{}
				}
			}
			local = append(local, compiled)
			continue
		}

		if rule.ActionHook != nil {
			if action := strings.ToLower(strings.TrimSpace(string(rule.Action))); action != "" && action != string(VFSHookActionAllow) {
				return nil, nil, nil, nil, errx.With(ErrInvalidVFSHook, " %q action hooks cannot set action=%q", rule.Name, rule.Action)
			}
			if rule.Phase != "" && !strings.EqualFold(rule.Phase, VFSHookPhaseBefore) {
				return nil, nil, nil, nil, errx.With(ErrInvalidVFSHook, " %q action hook must use phase=before", rule.Name)
			}
			compiledAction := compiledVFSActionHook{
				name:     rule.Name,
				path:     rule.Path,
				callback: rule.ActionHook,
			}
			if len(rule.Ops) > 0 {
				compiledAction.ops = make(map[string]struct{}, len(rule.Ops))
				for _, op := range rule.Ops {
					if op == "" {
						continue
					}
					compiledAction.ops[strings.ToLower(op)] = struct{}{}
				}
			}
			localAction = append(localAction, compiledAction)
			continue
		}

		if action := strings.ToLower(strings.TrimSpace(string(rule.Action))); action != "" && action != string(VFSHookActionAllow) {
			return nil, nil, nil, nil, errx.With(ErrInvalidVFSHook, " %q mutate hooks cannot set action=%q", rule.Name, rule.Action)
		}
		if rule.Phase != "" && !strings.EqualFold(rule.Phase, VFSHookPhaseBefore) {
			return nil, nil, nil, nil, errx.With(ErrInvalidVFSHook, " %q mutate hook must use phase=before", rule.Name)
		}

		compiledMutate := compiledVFSMutateHook{
			name:     rule.Name,
			path:     rule.Path,
			callback: rule.MutateHook,
		}
		if len(rule.Ops) > 0 {
			compiledMutate.ops = make(map[string]struct{}, len(rule.Ops))
			for _, op := range rule.Ops {
				if op == "" {
					continue
				}
				compiledMutate.ops[strings.ToLower(op)] = struct{}{}
			}
		}
		localMutate = append(localMutate, compiledMutate)
	}

	if len(local) > 0 {
		wire.EmitEvents = true
	}

	if len(wire.Rules) == 0 && !wire.EmitEvents {
		wire = nil
	}

	return wire, local, localMutate, localAction, nil
}

func (c *Client) setVFSHooks(hooks []compiledVFSHook, mutateHooks []compiledVFSMutateHook, actionHooks []compiledVFSActionHook) {
	c.vfsHookMu.Lock()
	c.vfsHooks = hooks
	c.vfsMutateHooks = mutateHooks
	c.vfsActionHooks = actionHooks
	c.vfsHookActive.Store(false)
	c.vfsHookMu.Unlock()
}

func (c *Client) handleVFSFileEvent(op, path string, size int64, mode uint32, uid, gid int) {
	c.vfsHookMu.RLock()
	hooks := append([]compiledVFSHook(nil), c.vfsHooks...)
	c.vfsHookMu.RUnlock()

	if len(hooks) == 0 {
		return
	}
	event := VFSHookEvent{
		Op:   VFSHookOp(op),
		Path: path,
		Size: size,
		Mode: mode,
		UID:  uid,
		GID:  gid,
	}

	opLower := strings.ToLower(op)
	safeHooks := make([]compiledVFSHook, 0, len(hooks))
	for _, hook := range hooks {
		if !matchesVFSHook(hook, opLower, path) {
			continue
		}
		if hook.dangerous {
			go c.runSingleVFSHook(hook, event)
			continue
		}
		safeHooks = append(safeHooks, hook)
	}

	if len(safeHooks) == 0 {
		return
	}
	if c.vfsHookActive.Load() {
		return
	}

	go c.runVFSSafeHooksForEvent(safeHooks, event)
}

func (c *Client) runVFSSafeHooksForEvent(hooks []compiledVFSHook, event VFSHookEvent) {
	if !c.vfsHookActive.CompareAndSwap(false, true) {
		return
	}
	defer c.vfsHookActive.Store(false)

	for _, hook := range hooks {
		c.runSingleVFSHook(hook, event)
	}
}

func (c *Client) runSingleVFSHook(hook compiledVFSHook, event VFSHookEvent) {
	ctx := context.Background()
	cancel := func() {}
	if hook.timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, hook.timeout)
	}
	_ = hook.callback(ctx, c, event)
	cancel()
}

func matchesVFSHook(hook compiledVFSHook, op, path string) bool {
	if len(hook.ops) > 0 {
		if _, ok := hook.ops[strings.ToLower(op)]; !ok {
			return false
		}
	}
	if hook.path == "" {
		return true
	}
	matched, err := filepath.Match(hook.path, path)
	if err != nil {
		return false
	}
	return matched
}

func matchesVFSMutateHook(hook compiledVFSMutateHook, op, path string) bool {
	if len(hook.ops) > 0 {
		if _, ok := hook.ops[strings.ToLower(op)]; !ok {
			return false
		}
	}
	if hook.path == "" {
		return true
	}
	matched, err := filepath.Match(hook.path, path)
	if err != nil {
		return false
	}
	return matched
}

func matchesVFSActionHook(hook compiledVFSActionHook, op, path string) bool {
	if len(hook.ops) > 0 {
		if _, ok := hook.ops[strings.ToLower(op)]; !ok {
			return false
		}
	}
	if hook.path == "" {
		return true
	}
	matched, err := filepath.Match(hook.path, path)
	if err != nil {
		return false
	}
	return matched
}

func (c *Client) applyLocalActionHooks(ctx context.Context, op VFSHookOp, path string, size int, mode uint32) error {
	c.vfsHookMu.RLock()
	hooks := append([]compiledVFSActionHook(nil), c.vfsActionHooks...)
	c.vfsHookMu.RUnlock()

	if len(hooks) == 0 {
		return nil
	}

	req := VFSActionRequest{
		Op:   op,
		Path: path,
		Size: size,
		Mode: mode,
		UID:  os.Geteuid(),
		GID:  os.Getegid(),
	}

	for _, hook := range hooks {
		if !matchesVFSActionHook(hook, string(op), path) {
			continue
		}

		decision := VFSHookAction(strings.ToLower(strings.TrimSpace(string(hook.callback(ctx, req)))))
		switch decision {
		case "", VFSHookActionAllow:
			continue
		case VFSHookActionBlock:
			return errx.With(ErrVFSHookBlocked, " op=%s path=%s hook=%q", op, path, hook.name)
		default:
			return errx.With(ErrInvalidVFSHook, " %q returned invalid action decision %q", hook.name, decision)
		}
	}

	return nil
}

func (c *Client) applyLocalWriteMutations(ctx context.Context, path string, content []byte, mode uint32) ([]byte, error) {
	c.vfsHookMu.RLock()
	hooks := append([]compiledVFSMutateHook(nil), c.vfsMutateHooks...)
	c.vfsHookMu.RUnlock()

	if len(hooks) == 0 {
		return content, nil
	}

	current := content
	for _, hook := range hooks {
		if !matchesVFSMutateHook(hook, string(VFSHookOpWrite), path) {
			continue
		}
		req := VFSMutateRequest{
			Path: path,
			Size: len(current),
			Mode: mode,
			UID:  os.Geteuid(),
			GID:  os.Getegid(),
		}
		mutated, err := hook.callback(ctx, req)
		if err != nil {
			return nil, err
		}
		if mutated != nil {
			current = mutated
		}
	}

	return current, nil
}

// PortForward applies one or more [LOCAL_PORT:]REMOTE_PORT mappings with the
// default bind address (127.0.0.1).
func (c *Client) PortForward(ctx context.Context, specs ...string) ([]api.PortForwardBinding, error) {
	return c.PortForwardWithAddresses(ctx, nil, specs...)
}

// PortForwardWithAddresses applies one or more [LOCAL_PORT:]REMOTE_PORT
// mappings bound on the provided host addresses.
func (c *Client) PortForwardWithAddresses(ctx context.Context, addresses []string, specs ...string) ([]api.PortForwardBinding, error) {
	forwards, err := api.ParsePortForwards(specs)
	if err != nil {
		return nil, errx.Wrap(ErrParsePortForwards, err)
	}
	return c.portForwardMappings(ctx, addresses, forwards)
}

func (c *Client) portForwardMappings(ctx context.Context, addresses []string, forwards []api.PortForward) ([]api.PortForwardBinding, error) {
	if len(forwards) == 0 {
		return nil, nil
	}

	if len(addresses) == 0 {
		addresses = []string{"127.0.0.1"}
	}
	addrCopy := append([]string(nil), addresses...)

	params := map[string]interface{}{
		"forwards":  forwards,
		"addresses": addrCopy,
	}
	result, err := c.sendRequestCtx(ctx, "port_forward", params, nil)
	if err != nil {
		return nil, err
	}

	var parsed struct {
		Bindings []api.PortForwardBinding `json:"bindings"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		return nil, errx.Wrap(ErrParsePortBindings, err)
	}
	return parsed.Bindings, nil
}

// ExecResult holds the result of command execution
type ExecResult struct {
	// ExitCode is the command's exit code
	ExitCode int
	// Stdout is the standard output
	Stdout string
	// Stderr is the standard error
	Stderr string
	// DurationMS is the execution time in milliseconds
	DurationMS int64
}

// Exec executes a command in the sandbox and returns the buffered result.
// The context controls the lifetime of the request — if cancelled, a cancel
// RPC is sent to abort the in-flight execution.
func (c *Client) Exec(ctx context.Context, command string) (*ExecResult, error) {
	return c.ExecWithDir(ctx, command, "")
}

// ExecWithDir executes a command in the sandbox with a working directory.
func (c *Client) ExecWithDir(ctx context.Context, command, workingDir string) (*ExecResult, error) {
	params := map[string]string{
		"command": command,
	}
	if workingDir != "" {
		params["working_dir"] = workingDir
	}

	result, err := c.sendRequestCtx(ctx, "exec", params, nil)
	if err != nil {
		return nil, err
	}

	var execResult struct {
		ExitCode   int    `json:"exit_code"`
		Stdout     string `json:"stdout"`
		Stderr     string `json:"stderr"`
		DurationMS int64  `json:"duration_ms"`
	}
	if err := json.Unmarshal(result, &execResult); err != nil {
		return nil, errx.Wrap(ErrParseExecResult, err)
	}

	stdout, _ := base64.StdEncoding.DecodeString(execResult.Stdout)
	stderr, _ := base64.StdEncoding.DecodeString(execResult.Stderr)

	return &ExecResult{
		ExitCode:   execResult.ExitCode,
		Stdout:     string(stdout),
		Stderr:     string(stderr),
		DurationMS: execResult.DurationMS,
	}, nil
}

// ExecStreamResult holds the final result of a streaming exec (no stdout/stderr
// since those were delivered via the callback).
type ExecStreamResult struct {
	ExitCode   int
	DurationMS int64
}

// ExecStream executes a command and streams stdout/stderr to the provided writers
// in real-time. If stdout or stderr is nil, that stream is discarded.
// The final ExecStreamResult contains only the exit code and duration.
func (c *Client) ExecStream(ctx context.Context, command string, stdout, stderr io.Writer) (*ExecStreamResult, error) {
	return c.ExecStreamWithDir(ctx, command, "", stdout, stderr)
}

// ExecStreamWithDir executes a command with a working directory and streams
// stdout/stderr to the provided writers in real-time.
func (c *Client) ExecStreamWithDir(ctx context.Context, command, workingDir string, stdout, stderr io.Writer) (*ExecStreamResult, error) {
	params := map[string]string{
		"command": command,
	}
	if workingDir != "" {
		params["working_dir"] = workingDir
	}

	onNotification := func(method string, params json.RawMessage) {
		var chunk struct {
			Data string `json:"data"`
		}
		if err := json.Unmarshal(params, &chunk); err != nil {
			return
		}
		decoded, err := base64.StdEncoding.DecodeString(chunk.Data)
		if err != nil {
			return
		}
		switch method {
		case "exec_stream.stdout":
			if stdout != nil {
				stdout.Write(decoded)
			}
		case "exec_stream.stderr":
			if stderr != nil {
				stderr.Write(decoded)
			}
		}
	}

	result, err := c.sendRequestCtx(ctx, "exec_stream", params, onNotification)
	if err != nil {
		return nil, err
	}

	var streamResult struct {
		ExitCode   int   `json:"exit_code"`
		DurationMS int64 `json:"duration_ms"`
	}
	if err := json.Unmarshal(result, &streamResult); err != nil {
		return nil, errx.Wrap(ErrParseExecStreamResult, err)
	}

	return &ExecStreamResult{
		ExitCode:   streamResult.ExitCode,
		DurationMS: streamResult.DurationMS,
	}, nil
}

// WriteFile writes content to a file in the sandbox.
func (c *Client) WriteFile(ctx context.Context, path string, content []byte) error {
	return c.WriteFileMode(ctx, path, content, 0644)
}

// WriteFileMode writes content to a file with specific permissions.
func (c *Client) WriteFileMode(ctx context.Context, path string, content []byte, mode uint32) error {
	if err := c.applyLocalActionHooks(ctx, VFSHookOpWrite, path, len(content), mode); err != nil {
		return err
	}

	mutated, err := c.applyLocalWriteMutations(ctx, path, content, mode)
	if err != nil {
		return err
	}

	params := map[string]interface{}{
		"path":    path,
		"content": base64.StdEncoding.EncodeToString(mutated),
		"mode":    mode,
	}

	_, err = c.sendRequestCtx(ctx, "write_file", params, nil)
	return err
}

// ReadFile reads a file from the sandbox.
func (c *Client) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if err := c.applyLocalActionHooks(ctx, VFSHookOpRead, path, 0, 0); err != nil {
		return nil, err
	}

	params := map[string]string{
		"path": path,
	}

	result, err := c.sendRequestCtx(ctx, "read_file", params, nil)
	if err != nil {
		return nil, err
	}

	var readResult struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(result, &readResult); err != nil {
		return nil, errx.Wrap(ErrParseReadResult, err)
	}

	return base64.StdEncoding.DecodeString(readResult.Content)
}

// FileInfo holds file metadata
type FileInfo struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	Mode  uint32 `json:"mode"`
	IsDir bool   `json:"is_dir"`
}

// ListFiles lists files in a directory.
func (c *Client) ListFiles(ctx context.Context, path string) ([]FileInfo, error) {
	if err := c.applyLocalActionHooks(ctx, VFSHookOpReadDir, path, 0, 0); err != nil {
		return nil, err
	}

	params := map[string]string{
		"path": path,
	}

	result, err := c.sendRequestCtx(ctx, "list_files", params, nil)
	if err != nil {
		return nil, err
	}

	var listResult struct {
		Files []FileInfo `json:"files"`
	}
	if err := json.Unmarshal(result, &listResult); err != nil {
		return nil, errx.Wrap(ErrParseListResult, err)
	}

	return listResult.Files, nil
}
