package policy

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
)

const networkHookIOTimeout = 2 * time.Second

type networkHookInvoker interface {
	Invoke(ctx context.Context, req *api.NetworkHookCallbackRequest) (*api.NetworkHookCallbackResponse, error)
}

type unixNetworkHookInvoker struct {
	socketPath string
}

func newNetworkHookInvoker(config *api.NetworkConfig) networkHookInvoker {
	if config == nil || config.Interception == nil {
		return nil
	}
	socketPath := strings.TrimSpace(config.Interception.CallbackSocket)
	if socketPath == "" {
		return nil
	}
	return &unixNetworkHookInvoker{socketPath: socketPath}
}

func (u *unixNetworkHookInvoker) Invoke(ctx context.Context, req *api.NetworkHookCallbackRequest) (*api.NetworkHookCallbackResponse, error) {
	if u == nil || strings.TrimSpace(u.socketPath) == "" {
		return nil, errors.New("network hook callback socket is not configured")
	}
	if req == nil {
		return nil, errors.New("network hook callback request is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	dialer := net.Dialer{Timeout: networkHookIOTimeout}
	conn, err := dialer.DialContext(ctx, "unix", u.socketPath)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	deadline := time.Now().Add(networkHookIOTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)

	enc := json.NewEncoder(conn)
	if err := enc.Encode(req); err != nil {
		return nil, err
	}

	var resp api.NetworkHookCallbackResponse
	dec := json.NewDecoder(conn)
	if err := dec.Decode(&resp); err != nil {
		return nil, err
	}
	if strings.TrimSpace(resp.Error) != "" {
		return nil, errors.New(resp.Error)
	}
	return &resp, nil
}
