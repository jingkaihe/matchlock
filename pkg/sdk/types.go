package sdk

import (
	"context"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
)

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
	// AddHosts injects static host-to-IP mappings into guest /etc/hosts.
	AddHosts []api.HostIPMapping
	// BlockPrivateIPs controls access to private IP ranges.
	// Use together with BlockPrivateIPsSet to express explicit true/false.
	BlockPrivateIPs bool
	// BlockPrivateIPsSet marks whether BlockPrivateIPs was explicitly set.
	// When false, the SDK preserves API defaults for private IP blocking.
	BlockPrivateIPsSet bool
	// NoNetwork disables guest network egress entirely (no guest NIC).
	NoNetwork bool
	// AllowedPrivateHosts lists specific private IP addresses or patterns
	// that bypass block-private-ips when it is enabled.
	AllowedPrivateHosts []string
	// LocalModelRoutes configures local model routing rules.
	LocalModelRoutes []LocalModelRouteOption
	// Plugins configures explicit network policy plugins.
	Plugins []PluginConfig
	// ForceInterception forces network interception even when allow-list/secrets are empty.
	ForceInterception bool
	// NetworkInterception configures host-side network interception rules.
	NetworkInterception *NetworkInterceptionConfig
	// Mounts defines VFS mount configurations
	Mounts map[string]MountConfig
	// Env defines non-secret environment variables for command execution.
	// These are visible in VM state and inspect/get outputs.
	Env map[string]string
	// Secrets defines secrets to inject (replaced in HTTP requests to allowed hosts)
	Secrets []Secret
	// Workspace is the mount point for VFS in the guest.
	// This must be set when Mounts is non-empty.
	Workspace string
	// VFSInterception configures host-side VFS interception hooks/rules.
	VFSInterception *VFSInterceptionConfig
	// DNSServers overrides the default DNS servers (8.8.8.8, 8.8.4.4)
	DNSServers []string
	// Hostname overrides the default guest hostname (sandbox's ID)
	Hostname string
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
	// LaunchEntrypoint starts image ENTRYPOINT/CMD in detached mode during create.
	// Set by Client.Launch; low-level Create keeps this false unless requested.
	LaunchEntrypoint bool
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

// LocalModelRouteOption configures a local model routing rule for the SDK wire format.
type LocalModelRouteOption struct {
	SourceHost  string
	BackendHost string
	BackendPort int
	Models      map[string]ModelRouteOption
}

// ModelRouteOption configures a model routing target for the SDK wire format.
type ModelRouteOption struct {
	Target      string
	BackendHost string
	BackendPort int
}

// Network hook phases.
type NetworkHookPhase = string

const (
	NetworkHookPhaseBefore NetworkHookPhase = "before"
	NetworkHookPhaseAfter  NetworkHookPhase = "after"
)

// Network hook actions.
type NetworkHookAction = string

const (
	NetworkHookActionAllow  NetworkHookAction = "allow"
	NetworkHookActionBlock  NetworkHookAction = "block"
	NetworkHookActionMutate NetworkHookAction = "mutate"
)

// NetworkBodyTransform applies a literal response-body replacement.
// For SSE responses, replacements are applied to each `data:` line payload.
type NetworkBodyTransform struct {
	Find    string `json:"find"`
	Replace string `json:"replace,omitempty"`
}

// NetworkHookRequest is passed to an SDK-local network callback hook after
// static host/method/path prefiltering.
type NetworkHookRequest struct {
	Phase  NetworkHookPhase
	Host   string
	Method string
	Path   string

	Query          map[string]string
	RequestHeaders map[string][]string

	StatusCode      int
	ResponseHeaders map[string][]string
	IsSSE           bool
}

// NetworkHookResult describes dynamic mutations returned by an SDK-local
// network callback hook.
type NetworkHookResult struct {
	Action NetworkHookAction

	Request  *NetworkHookRequestMutation
	Response *NetworkHookResponseMutation
}

// NetworkHookFunc executes in the SDK process for matching network hook rules.
type NetworkHookFunc func(ctx context.Context, req NetworkHookRequest) (*NetworkHookResult, error)

// NetworkHookRequestMutation describes request-shaping changes returned by a
// network callback.
type NetworkHookRequestMutation struct {
	// Headers replaces the full outbound request header map when non-nil.
	Headers map[string][]string
	// Query replaces the full outbound query map when non-nil.
	Query map[string]string
	// Path rewrites the outbound request path when non-empty.
	Path string
}

// NetworkHookResponseMutation describes response-shaping changes returned by a
// network callback.
type NetworkHookResponseMutation struct {
	// Headers replaces the full inbound response header map when non-nil.
	Headers map[string][]string

	BodyReplacements []NetworkBodyTransform

	// SetBody replaces the entire response body when non-nil.
	SetBody []byte
}

// NetworkHookRule describes a network interception rule.
type NetworkHookRule struct {
	Name    string            `json:"name,omitempty"`
	Phase   NetworkHookPhase  `json:"phase,omitempty"`
	Hosts   []string          `json:"hosts,omitempty"`
	Methods []string          `json:"methods,omitempty"`
	Path    string            `json:"path,omitempty"`
	Action  NetworkHookAction `json:"action,omitempty"`

	SetHeaders    map[string]string `json:"set_headers,omitempty"`
	DeleteHeaders []string          `json:"delete_headers,omitempty"`
	SetQuery      map[string]string `json:"set_query,omitempty"`
	DeleteQuery   []string          `json:"delete_query,omitempty"`
	RewritePath   string            `json:"rewrite_path,omitempty"`

	SetResponseHeaders    map[string]string      `json:"set_response_headers,omitempty"`
	DeleteResponseHeaders []string               `json:"delete_response_headers,omitempty"`
	BodyReplacements      []NetworkBodyTransform `json:"body_replacements,omitempty"`
	TimeoutMS             int                    `json:"timeout_ms,omitempty"`

	// Hook runs in the SDK process and enables dynamic request/response mutation.
	Hook NetworkHookFunc `json:"-"`
}

// NetworkInterceptionConfig configures host-side network interception rules.
type NetworkInterceptionConfig struct {
	Rules []NetworkHookRule `json:"rules,omitempty"`
}

// MountConfig defines a VFS mount
type MountConfig struct {
	Type     string `json:"type"` // memory, host_fs, overlay
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

type compiledNetworkHook struct {
	id       string
	name     string
	phase    NetworkHookPhase
	timeout  time.Duration
	callback NetworkHookFunc
}

// Create creates and starts a new sandbox VM.
// If post-create setup fails (for example, port-forward bind errors), it
// returns the created VM ID with a non-nil error so callers can clean up.
