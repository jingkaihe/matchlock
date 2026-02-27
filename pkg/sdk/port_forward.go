package sdk

import (
	"context"
	"encoding/json"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/api"
)

func (c *Client) PortForward(ctx context.Context, specs ...string) ([]api.PortForwardBinding, error) {
	return c.PortForwardWithAddresses(ctx, nil, specs...)
}

// PortForwardWithAddresses applies one or more [LOCAL_PORT:]REMOTE_PORT
// mappings bound on the provided host addresses.
func (c *Client) PortForwardWithAddresses(ctx context.Context, addresses []string, specs ...string) ([]api.PortForwardBinding, error) {
	forwards, err := api.ParsePortForwards(specs)
	if err != nil {
		return nil, errx.Wrap(ErrParsePortForwards, err)
	}
	return c.portForwardMappings(ctx, addresses, forwards)
}

func (c *Client) portForwardMappings(ctx context.Context, addresses []string, forwards []api.PortForward) ([]api.PortForwardBinding, error) {
	if len(forwards) == 0 {
		return nil, nil
	}

	if len(addresses) == 0 {
		addresses = []string{"127.0.0.1"}
	}
	addrCopy := append([]string(nil), addresses...)

	params := map[string]interface{}{
		"forwards":  forwards,
		"addresses": addrCopy,
	}
	result, err := c.sendRequestCtx(ctx, "port_forward", params, nil)
	if err != nil {
		return nil, err
	}

	var parsed struct {
		Bindings []api.PortForwardBinding `json:"bindings"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		return nil, errx.Wrap(ErrParsePortBindings, err)
	}
	return parsed.Bindings, nil
}
