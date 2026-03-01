package api

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jingkaihe/matchlock/internal/errx"
)

// DefaultWorkspace is the conventional mount point for the VFS in the guest.
// VFS is now opt-in and this path is used only when explicitly configured.
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
	ID         string `json:"id,omitempty"`
	Image      string `json:"image,omitempty"`
	Privileged bool   `json:"privileged,omitempty"`
	// LaunchEntrypoint starts image ENTRYPOINT/CMD in detached mode during create.
	// This is used by SDK `launch` convenience helpers.
	LaunchEntrypoint bool              `json:"launch_entrypoint,omitempty"`
	Resources        *Resources        `json:"resources,omitempty"`
	Network          *NetworkConfig    `json:"network,omitempty"`
	VFS              *VFSConfig        `json:"vfs,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	ExtraDisks       []DiskMount       `json:"extra_disks,omitempty"`
	ImageCfg         *ImageConfig      `json:"image_config,omitempty"`
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
	CPUs           float64       `json:"cpus,omitempty"`
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
	AllowedHosts    []string                   `json:"allowed_hosts,omitempty"`
	AddHosts        []HostIPMapping            `json:"add_hosts,omitempty"`
	BlockPrivateIPs bool                       `json:"block_private_ips,omitempty"`
	NoNetwork       bool                       `json:"no_network,omitempty"`
	Intercept       bool                       `json:"intercept,omitempty"`
	Interception    *NetworkInterceptionConfig `json:"interception,omitempty"`
	Secrets         map[string]Secret          `json:"secrets,omitempty"`
	PolicyScript    string                     `json:"policy_script,omitempty"`
	DNSServers      []string                   `json:"dns_servers,omitempty"`
	Hostname        string                     `json:"hostname,omitempty"`
	MTU             int                        `json:"mtu,omitempty"`
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

// Validate checks network config invariants.
func (n *NetworkConfig) Validate() error {
	if n == nil || !n.NoNetwork {
		return nil
	}
	if len(n.AllowedHosts) > 0 {
		return errx.With(ErrInvalidConfig, ": network.no_network cannot be combined with network.allowed_hosts")
	}
	if len(n.Secrets) > 0 {
		return errx.With(ErrInvalidConfig, ": network.no_network cannot be combined with network.secrets")
	}
	if n.Intercept {
		return errx.With(ErrInvalidConfig, ": network.no_network cannot be combined with network.intercept")
	}
	if n.Interception != nil {
		return errx.With(ErrInvalidConfig, ": network.no_network cannot be combined with network.interception")
	}
	return nil
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

// GetWorkspace returns the configured workspace path, or empty when unset.
func (v *VFSConfig) GetWorkspace() string {
	if v != nil && v.Workspace != "" {
		return v.Workspace
	}
	return ""
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

// GetWorkspace returns the workspace path from config, or empty when unset.
func (c *Config) GetWorkspace() string {
	if c.VFS != nil {
		return c.VFS.GetWorkspace()
	}
	return ""
}

// HasVFSMounts reports whether VFS is enabled with at least one mount.
func (c *Config) HasVFSMounts() bool {
	return c != nil && c.VFS != nil && len(c.VFS.Mounts) > 0
}

// ValidateVFS checks VFS config invariants.
//
// Rules:
// - vfs.workspace requires at least one vfs.mounts entry
// - vfs.mounts requires a non-empty vfs.workspace
// - vfs.interception requires at least one vfs.mounts entry
// - vfs.workspace must be a safe absolute guest path
// - every vfs.mounts key must be under vfs.workspace
func (c *Config) ValidateVFS() error {
	if c == nil || c.VFS == nil {
		return nil
	}

	workspace := c.VFS.Workspace
	hasWorkspace := strings.TrimSpace(workspace) != ""
	hasMounts := len(c.VFS.Mounts) > 0
	hasInterception := c.VFS.Interception != nil

	if hasWorkspace && !hasMounts {
		return errx.With(ErrInvalidConfig, ": vfs.workspace requires at least one vfs.mounts entry")
	}
	if hasMounts && !hasWorkspace {
		return errx.With(ErrInvalidConfig, ": vfs.workspace is required when vfs.mounts is set")
	}
	if hasInterception && !hasMounts {
		return errx.With(ErrInvalidConfig, ": vfs.interception requires at least one vfs.mounts entry")
	}
	if !hasMounts {
		return nil
	}

	if err := ValidateGuestMount(workspace); err != nil {
		return errx.With(ErrInvalidConfig, ": vfs.workspace: %v", err)
	}
	if err := ValidateVFSMountsWithinWorkspace(c.VFS.Mounts, workspace); err != nil {
		return errx.With(ErrInvalidConfig, ": %v", err)
	}
	return nil
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
	if other.LaunchEntrypoint {
		result.LaunchEntrypoint = true
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
