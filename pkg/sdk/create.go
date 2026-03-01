package sdk

import (
	"context"
	"encoding/json"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/api"
)

func (c *Client) Create(opts CreateOptions) (string, error) {
	if opts.Image == "" {
		return "", ErrImageRequired
	}
	if opts.CPUs == 0 {
		opts.CPUs = api.DefaultCPUs
	}
	if opts.CPUs <= 0 {
		return "", ErrInvalidCPUCount
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
	if opts.NoNetwork && (len(opts.AllowedHosts) > 0 || len(opts.Secrets) > 0 || opts.ForceInterception || opts.NetworkInterception != nil) {
		return "", ErrNoNetworkConflict
	}
	for _, mapping := range opts.AddHosts {
		if err := api.ValidateAddHost(mapping); err != nil {
			return "", errx.Wrap(ErrInvalidAddHost, err)
		}
	}

	wireVFS, localHooks, localMutateHooks, localActionHooks, err := compileVFSHooks(opts.VFSInterception)
	if err != nil {
		return "", err
	}
	wireNetwork, localNetworkHooks, err := compileNetworkHooks(opts.NetworkInterception)
	if err != nil {
		return "", err
	}

	c.stopNetworkHookServer()
	startedNetworkHookServer := false
	if len(localNetworkHooks) > 0 {
		socketPath, listener, tempDir, err := startNetworkHookServer(localNetworkHooks)
		if err != nil {
			return "", errx.With(ErrInvalidNetworkHook, ": start callback server: %v", err)
		}
		if wireNetwork == nil {
			wireNetwork = &api.NetworkInterceptionConfig{}
		}
		wireNetwork.CallbackSocket = socketPath
		c.networkHookMu.Lock()
		c.networkHooks = localNetworkHooks
		c.networkHookSocket = socketPath
		c.networkHookListener = listener
		c.networkHookTempDir = tempDir
		c.networkHookMu.Unlock()
		startedNetworkHookServer = true
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

	if network := buildCreateNetworkParams(opts, wireNetwork); network != nil {
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
	if opts.LaunchEntrypoint {
		params["launch_entrypoint"] = true
	}

	result, err := c.sendRequest("create", params)
	if err != nil {
		if startedNetworkHookServer {
			c.stopNetworkHookServer()
		}
		return "", err
	}

	var createResult struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(result, &createResult); err != nil {
		if startedNetworkHookServer {
			c.stopNetworkHookServer()
		}
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

func buildCreateNetworkParams(opts CreateOptions, wireInterception *api.NetworkInterceptionConfig) map[string]interface{} {
	hasAllowedHosts := len(opts.AllowedHosts) > 0
	hasAddHosts := len(opts.AddHosts) > 0
	hasSecrets := len(opts.Secrets) > 0
	hasDNSServers := len(opts.DNSServers) > 0
	hasHostname := len(opts.Hostname) > 0
	hasMTU := opts.NetworkMTU > 0
	hasNoNetwork := opts.NoNetwork
	hasForceInterception := opts.ForceInterception
	hasNetworkInterception := wireInterception != nil
	blockPrivateIPs, hasBlockPrivateIPsOverride := resolveCreateBlockPrivateIPs(opts)

	includeNetwork := hasAllowedHosts || hasAddHosts || hasSecrets || hasDNSServers || hasHostname || hasMTU || hasNoNetwork || hasBlockPrivateIPsOverride || hasForceInterception || hasNetworkInterception
	if !includeNetwork {
		return nil
	}

	if hasNoNetwork {
		network := map[string]interface{}{
			"no_network": true,
		}
		if hasAddHosts {
			network["add_hosts"] = opts.AddHosts
		}
		if hasDNSServers {
			network["dns_servers"] = opts.DNSServers
		}
		if hasHostname {
			network["hostname"] = opts.Hostname
		}
		return network
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
	if hasForceInterception || hasNetworkInterception {
		network["intercept"] = true
	}
	if hasNetworkInterception {
		network["interception"] = wireInterception
	}
	if hasAddHosts {
		network["add_hosts"] = opts.AddHosts
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
	if hasHostname {
		network["hostname"] = opts.Hostname
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
