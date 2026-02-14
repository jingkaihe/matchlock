package sandbox

import (
	"github.com/jingkaihe/matchlock/pkg/api"
	sandboxnet "github.com/jingkaihe/matchlock/pkg/net"
)

func tailscaleSearchDomains(network *api.NetworkConfig) []string {
	if network == nil || !network.IsTailscaleEnabled() {
		return nil
	}

	domains, err := sandboxnet.HostTailscaleSearchDomains()
	if err != nil {
		return nil
	}
	return domains
}
