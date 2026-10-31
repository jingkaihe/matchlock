package sdk

import (
	"context"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
)

type CreateOptions struct {
	// Image is the container image reference (required, e.g., alpine:latest)
	Image string
	// KernelRef selects the guest kernel to boot.
	// Supports empty (default), file:///absolute/path, or OCI image refs.
	KernelRef string
	// Privileged skips in-guest security restrictions (seccomp, cap drop, no_new_privs)
	Privileged bool
	// CPUs is the number of vCPUs
	CPUs float64
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
	// ForceInterception forces network interception even when allow-list/secrets are empty.
	ForceInterception bool
	// NetworkInterception configures host-side network interception rules.
	NetworkInterception *NetworkInterceptionConfig
	// Env defines non-secret environment variables for command execution.
	// These are visible in VM state and inspect/get outputs.
	Env map[string]string
	// Secrets defines secrets to inject (replaced in HTTP requests to allowed hosts)
	Secrets []Secret
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
	// Placeholder overrides the generated in-VM placeholder value when set.
	Placeholder string
	// Hosts is a list of hosts where this secret can be used (supports wildcards)
	Hosts []string
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
