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

// AddAllowedHosts appends hosts to the allow-list. Returns the list of newly added hosts.
func (p *hostFilterPlugin) AddAllowedHosts(hosts ...string) []string {
	var added []string
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		found := false
		for _, existing := range p.config.AllowedHosts {
			if existing == h {
				found = true
				break
			}
		}
		if !found {
			// Check if already in the "added" batch to avoid duplicates
			alreadyAdded := false
			for _, a := range added {
				if a == h {
					alreadyAdded = true
					break
				}
			}
			if !alreadyAdded {
				p.config.AllowedHosts = append(p.config.AllowedHosts, h)
				added = append(added, h)
			}
		}
	}
	return added
}

// RemoveAllowedHosts removes hosts from the allow-list. Returns the list of actually removed hosts (unique).
func (p *hostFilterPlugin) RemoveAllowedHosts(hosts ...string) []string {
	remove := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		remove[strings.TrimSpace(h)] = true
	}
	removedSet := make(map[string]bool)
	filtered := p.config.AllowedHosts[:0]
	for _, h := range p.config.AllowedHosts {
		if remove[h] {
			removedSet[h] = true
		} else {
			filtered = append(filtered, h)
		}
	}
	p.config.AllowedHosts = filtered
	var removed []string
	for h := range removedSet {
		removed = append(removed, h)
	}
	return removed
}

// AllowedHosts returns a copy of the current allow-list.
func (p *hostFilterPlugin) AllowedHosts() []string {
	result := make([]string, len(p.config.AllowedHosts))
	copy(result, p.config.AllowedHosts)
	return result
}
