package policy

import (
	"encoding/json"
	"log/slog"
	"strings"
)

// HostFilterConfig is the typed config for the host_filter plugin.
// It mirrors the relevant flat fields from api.NetworkConfig.
type HostFilterConfig struct {
	AllowedHosts        []string `json:"allowed_hosts,omitempty"`
	BlockPrivateIPs     bool     `json:"block_private_ips,omitempty"`
	AllowedPrivateHosts []string `json:"allowed_private_hosts,omitempty"`
}

// hostFilterPlugin implements GatePlugin.
type hostFilterPlugin struct {
	config HostFilterConfig
	logger *slog.Logger
}

// NewHostFilterPlugin creates a host_filter plugin from typed config.
// Called during flat-field compilation in NewEngine.
func NewHostFilterPlugin(allowedHosts []string, blockPrivateIPs bool, allowedPrivateHosts []string, logger *slog.Logger) *hostFilterPlugin {
	if logger == nil {
		logger = slog.Default()
	}
	return &hostFilterPlugin{
		config: HostFilterConfig{
			AllowedHosts:        allowedHosts,
			BlockPrivateIPs:     blockPrivateIPs,
			AllowedPrivateHosts: allowedPrivateHosts,
		},
		logger: logger,
	}
}

// NewHostFilterPluginFromConfig creates a host_filter plugin from JSON config.
// Called by the plugin registry factory.
func NewHostFilterPluginFromConfig(raw json.RawMessage, logger *slog.Logger) (Plugin, error) {
	if logger == nil {
		logger = slog.Default()
	}
	var cfg HostFilterConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	return &hostFilterPlugin{config: cfg, logger: logger}, nil
}

func (p *hostFilterPlugin) Name() string {
	return "host_filter"
}

// Gate implements GatePlugin.
// Logic is extracted from Engine.IsHostAllowed.
func (p *hostFilterPlugin) Gate(host string) *GateVerdict {
	host = strings.Split(host, ":")[0]

	if p.config.BlockPrivateIPs {
		if isPrivateIP(host) {
			if !p.isPrivateHostAllowed(host) {
				return &GateVerdict{Allowed: false, Reason: "private IP blocked"}
			}
			p.logger.Debug("private IP allowed via exception", "host", host)
		}
	}

	if len(p.config.AllowedHosts) == 0 {
		return &GateVerdict{Allowed: true}
	}

	for _, pattern := range p.config.AllowedHosts {
		if matchGlob(pattern, host) {
			p.logger.Debug("matched allowlist pattern", "host", host, "pattern", pattern)
			return &GateVerdict{Allowed: true}
		}
	}

	return &GateVerdict{Allowed: false, Reason: "host not in allowlist"}
}

func (p *hostFilterPlugin) isPrivateHostAllowed(host string) bool {
	for _, pattern := range p.config.AllowedPrivateHosts {
		if matchGlob(pattern, host) {
			return true
		}
	}
	return false
}
