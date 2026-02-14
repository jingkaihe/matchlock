package sandbox

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/api"
	sandboxnet "github.com/jingkaihe/matchlock/pkg/net"
)

func createUpstreamDialer(network *api.NetworkConfig, stateDir, vmID string) (sandboxnet.UpstreamDialer, error) {
	if network == nil || !network.IsTailscaleEnabled() {
		return sandboxnet.NewSystemDialer(), nil
	}

	authKeyEnv := network.GetTailscaleAuthKeyEnv()
	authKey := strings.TrimSpace(os.Getenv(authKeyEnv))
	if authKey == "" {
		return nil, errx.With(ErrMissingTailscaleAuthKey, " in $%s", authKeyEnv)
	}

	hostname := "matchlock-" + strings.TrimPrefix(vmID, "vm-")
	dialer, err := sandboxnet.NewTailscaleDialer(&sandboxnet.TailscaleDialerConfig{
		StateDir: filepath.Join(stateDir, "tailscale"),
		Hostname: hostname,
		AuthKey:  authKey,
	})
	if err != nil {
		return nil, errx.Wrap(ErrCreateTailscaleDialer, err)
	}

	return dialer, nil
}
