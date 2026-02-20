package api

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
)

// DefaultWorkspace is the default mount point for the VFS in the guest
const DefaultWorkspace = "/workspace"

const (
	DefaultCPUs                   = 1
	DefaultMemoryMB               = 512
	DefaultDiskSizeMB             = 5120
	DefaultTimeoutSeconds         = 300
	DefaultNetworkMTU             = 1500
	DefaultGracefulShutdownPeriod = 0
)

type ImageConfig struct {
	User       string            `json:"user,omitempty"`
	WorkingDir string            `json:"working_dir,omitempty"`
	Entrypoint []string          `json:"entrypoint,omitempty"`
	Cmd        []string          `json:"cmd,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
}

type Config struct {
	ID         string            `json:"id,omitempty"`
	Image      string            `json:"image,omitempty"`
	Privileged bool              `json:"privileged,omitempty"`
	Resources  *Resources        `json:"resources,omitempty"`
	Network    *NetworkConfig    `json:"network,omitempty"`
	VFS        *VFSConfig        `json:"vfs,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	ExtraDisks []DiskMount       `json:"extra_disks,omitempty"`
	ImageCfg   *ImageConfig      `json:"image_config,omitempty"`
}

// DiskMount describes a persistent ext4 disk image to attach as a block device.
type DiskMount struct {
	HostPath   string `json:"host_path"`
	GuestMount string `json:"guest_mount"`
	ReadOnly   bool   `json:"readonly,omitempty"`
}

var validGuestMountPath = regexp.MustCompile(`^/[a-zA-Z0-9/_.-]+$`)

// ValidateGuestMount checks that a guest mount path is safe for use in
// kernel cmdline args and shell scripts.
func ValidateGuestMount(path string) error {
	if !validGuestMountPath.MatchString(path) {
		return fmt.Errorf("invalid guest mount path %q: must be absolute and contain only alphanumeric, '/', '_', '.', '-'", path)
	}
	return nil
}

type Resources struct {
	CPUs           int           `json:"cpus,omitempty"`
	MemoryMB       int           `json:"memory_mb,omitempty"`
	DiskSizeMB     int           `json:"disk_size_mb,omitempty"`
	TimeoutSeconds int           `json:"timeout_seconds,omitempty"`
	Timeout        time.Duration `json:"-"`
}

// DefaultDNSServers are used when no custom DNS servers are configured.
var DefaultDNSServers = []string{"8.8.8.8", "8.8.4.4"}

type HostIPMapping struct {
	Host string `json:"host"`
	IP   string `json:"ip"`
}

type NetworkConfig struct {
	AllowedHosts        []string            `json:"allowed_hosts,omitempty"`
	AddHosts            []HostIPMapping     `json:"add_hosts,omitempty"`
	BlockPrivateIPs     bool                `json:"block_private_ips,omitempty"`
	AllowedPrivateHosts []string            `json:"allowed_private_hosts,omitempty"`
	Secrets             map[string]Secret   `json:"secrets,omitempty"`
	PolicyScript        string              `json:"policy_script,omitempty"`
	DNSServers          []string            `json:"dns_servers,omitempty"`
	Hostname            string              `json:"hostname,omitempty"`
	MTU                 int                 `json:"mtu,omitempty"`
	LocalModelRouting   []LocalModelRoute   `json:"local_model_routing,omitempty"`
}

// LocalModelRoute configures interception of LLM API requests from a
// specific source host and redirection to a local inference backend.
type LocalModelRoute struct {
	SourceHost  string                `json:"source_host"`
	Path        string                `json:"path,omitempty"`
	BackendHost string                `json:"backend_host,omitempty"`
	BackendPort int                   `json:"backend_port,omitempty"`
	Models      map[string]ModelRoute `json:"models"`
}

// GetPath returns the configured path or the default ("/api/v1/chat/completions").
func (r *LocalModelRoute) GetPath() string {
	if r != nil && r.Path != "" {
		return r.Path
	}
	return "/api/v1/chat/completions"
}

// GetBackendHost returns the configured backend host or the default ("127.0.0.1").
func (r *LocalModelRoute) GetBackendHost() string {
	if r != nil && r.BackendHost != "" {
		return r.BackendHost
	}
	return "127.0.0.1"
}

// GetBackendPort returns the configured backend port or the default (11434).
func (r *LocalModelRoute) GetBackendPort() int {
	if r != nil && r.BackendPort > 0 {
		return r.BackendPort
	}
	return 11434
}

// ModelRoute defines how a specific model is routed to a local backend.
type ModelRoute struct {
	Target      string `json:"target"`
	BackendHost string `json:"backend_host,omitempty"`
	BackendPort int    `json:"backend_port,omitempty"`
}

// EffectiveBackendHost returns the model-specific backend host,
// falling back to the route-level default.
func (m *ModelRoute) EffectiveBackendHost(routeDefault string) string {
	if m.BackendHost != "" {
		return m.BackendHost
	}
	return routeDefault
}

// EffectiveBackendPort returns the model-specific backend port,
// falling back to the route-level default.
func (m *ModelRoute) EffectiveBackendPort(routeDefault int) int {
	if m.BackendPort > 0 {
		return m.BackendPort
	}
	return routeDefault
}

// GetDNSServers returns the configured DNS servers or defaults.
func (n *NetworkConfig) GetDNSServers() []string {
	if n != nil && len(n.DNSServers) > 0 {
		return n.DNSServers
	}
	return DefaultDNSServers
}

// GetMTU returns the configured network MTU or the default.
func (n *NetworkConfig) GetMTU() int {
	if n != nil && n.MTU > 0 {
		return n.MTU
	}
	return DefaultNetworkMTU
}

type Secret struct {
	Value       string   `json:"value"`
	Placeholder string   `json:"placeholder,omitempty"`
	Hosts       []string `json:"hosts"`
}

type VFSConfig struct {
	Workspace    string                 `json:"workspace,omitempty"`
	DirectMounts map[string]DirectMount `json:"direct_mounts,omitempty"`
	Mounts       map[string]MountConfig `json:"mounts,omitempty"`
	Interception *VFSInterceptionConfig `json:"interception,omitempty"`
}

// GetWorkspace returns the configured workspace path or the default
func (v *VFSConfig) GetWorkspace() string {
	if v != nil && v.Workspace != "" {
		return v.Workspace
	}
	return DefaultWorkspace
}

type DirectMount struct {
	HostPath string `json:"host_path"`
	Readonly bool   `json:"readonly,omitempty"`
}

type MountConfig struct {
	Type     string       `json:"type"`
	HostPath string       `json:"host_path,omitempty"`
	Readonly bool         `json:"readonly,omitempty"`
	Upper    *MountConfig `json:"upper,omitempty"`
	Lower    *MountConfig `json:"lower,omitempty"`
}

const (
	MountTypeMemory  = "memory"
	MountTypeHostFS  = "host_fs"
	MountTypeOverlay = "overlay"

	MountOptionReadonlyShort = "ro"
	MountOptionReadonly      = "readonly"
)

// GetID returns the VM ID from config. Creates a new random ID if not set.
func (c *Config) GetID() string {
	if c.ID == "" {
		c.ID = "vm-" + uuid.New().String()[:8]
	}
	return c.ID
}

// GetHostname returns hostname from config or default to ID if not set
func (c *Config) GetHostname() string {
	if c.Network != nil && c.Network.Hostname != "" {
		return c.Network.Hostname
	}
	return c.GetID()
}

// GetWorkspace returns the workspace path from config, or default if not set
func (c *Config) GetWorkspace() string {
	if c.VFS != nil {
		return c.VFS.GetWorkspace()
	}
	return DefaultWorkspace
}

func DefaultConfig() *Config {
	return &Config{
		Resources: &Resources{
			CPUs:           DefaultCPUs,
			MemoryMB:       DefaultMemoryMB,
			DiskSizeMB:     DefaultDiskSizeMB,
			TimeoutSeconds: DefaultTimeoutSeconds,
		},
		Network: &NetworkConfig{
			BlockPrivateIPs: true,
			MTU:             DefaultNetworkMTU,
		},
		VFS: &VFSConfig{
			Mounts: map[string]MountConfig{
				DefaultWorkspace: {Type: MountTypeMemory},
			},
		},
	}
}

func (c *Config) Merge(other *Config) *Config {
	if other == nil {
		return c
	}

	result := *c
	if other.Image != "" {
		result.Image = other.Image
	}
	if other.Resources != nil {
		if result.Resources == nil {
			result.Resources = &Resources{}
		}
		if other.Resources.CPUs > 0 {
			result.Resources.CPUs = other.Resources.CPUs
		}
		if other.Resources.MemoryMB > 0 {
			result.Resources.MemoryMB = other.Resources.MemoryMB
		}
		if other.Resources.DiskSizeMB > 0 {
			result.Resources.DiskSizeMB = other.Resources.DiskSizeMB
		}
		if other.Resources.TimeoutSeconds > 0 {
			result.Resources.TimeoutSeconds = other.Resources.TimeoutSeconds
		}
	}
	if other.Network != nil {
		result.Network = other.Network
	}
	if other.VFS != nil {
		result.VFS = other.VFS
	}
	if other.Privileged {
		result.Privileged = true
	}
	if other.Env != nil {
		result.Env = other.Env
	}
	if len(other.ExtraDisks) > 0 {
		result.ExtraDisks = other.ExtraDisks
	}
	if other.ImageCfg != nil {
		result.ImageCfg = other.ImageCfg
	}
	return &result
}

// ComposeCommand builds a shell command from image ENTRYPOINT/CMD and user-provided args.
// Follows Docker semantics: if user provides args, they replace CMD; ENTRYPOINT is always prepended.
func (ic *ImageConfig) ComposeCommand(userArgs []string) []string {
	if ic == nil {
		return userArgs
	}
	result := make([]string, len(ic.Entrypoint))
	copy(result, ic.Entrypoint)
	if len(userArgs) > 0 {
		return append(result, userArgs...)
	}
	result = append(result, ic.Cmd...)
	if len(result) == 0 {
		return nil
	}
	return result
}

func ParseConfig(data []byte) (*Config, error) {
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
