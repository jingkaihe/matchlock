package api

import (
	"encoding/json"
	"time"
)

// DefaultWorkspace is the default mount point for the VFS in the guest
const DefaultWorkspace = "/workspace"

const (
	DefaultCPUs           = 1
	DefaultMemoryMB       = 512
	DefaultDiskSizeMB     = 5120
	DefaultTimeoutSeconds = 300
)

type Config struct {
	Image      string            `json:"image,omitempty"`
	Privileged bool              `json:"privileged,omitempty"`
	Resources  *Resources        `json:"resources,omitempty"`
	Network    *NetworkConfig    `json:"network,omitempty"`
	VFS        *VFSConfig        `json:"vfs,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
}

type Resources struct {
	CPUs           int           `json:"cpus,omitempty"`
	MemoryMB       int           `json:"memory_mb,omitempty"`
	DiskSizeMB     int           `json:"disk_size_mb,omitempty"`
	TimeoutSeconds int           `json:"timeout_seconds,omitempty"`
	Timeout        time.Duration `json:"-"`
}

type NetworkConfig struct {
	AllowedHosts    []string          `json:"allowed_hosts,omitempty"`
	BlockPrivateIPs bool              `json:"block_private_ips,omitempty"`
	Secrets         map[string]Secret `json:"secrets,omitempty"`
	PolicyScript    string            `json:"policy_script,omitempty"`
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
		},
		VFS: &VFSConfig{
			Mounts: map[string]MountConfig{
				DefaultWorkspace: {Type: "memory"},
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
	return &result
}

func ParseConfig(data []byte) (*Config, error) {
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
