package sdk

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/api"
)

func buildWireNetworkInterception(cfg *NetworkInterceptionConfig) *api.NetworkInterceptionConfig {
	if cfg == nil {
		return nil
	}

	wire := &api.NetworkInterceptionConfig{
		Rules: make([]api.NetworkHookRule, 0, len(cfg.Rules)),
	}
	for _, rule := range cfg.Rules {
		wireRule := api.NetworkHookRule{
			Name:                  rule.Name,
			Phase:                 string(rule.Phase),
			Hosts:                 append([]string(nil), rule.Hosts...),
			Methods:               append([]string(nil), rule.Methods...),
			Path:                  rule.Path,
			Action:                string(rule.Action),
			TimeoutMS:             rule.TimeoutMS,
			SetHeaders:            cloneStringMap(rule.SetHeaders),
			DeleteHeaders:         append([]string(nil), rule.DeleteHeaders...),
			SetQuery:              cloneStringMap(rule.SetQuery),
			DeleteQuery:           append([]string(nil), rule.DeleteQuery...),
			RewritePath:           rule.RewritePath,
			SetResponseHeaders:    cloneStringMap(rule.SetResponseHeaders),
			DeleteResponseHeaders: append([]string(nil), rule.DeleteResponseHeaders...),
		}
		if len(rule.BodyReplacements) > 0 {
			wireRule.BodyReplacements = make([]api.NetworkBodyTransform, 0, len(rule.BodyReplacements))
			for _, item := range rule.BodyReplacements {
				wireRule.BodyReplacements = append(wireRule.BodyReplacements, api.NetworkBodyTransform{
					Find:    item.Find,
					Replace: item.Replace,
				})
			}
		}
		wire.Rules = append(wire.Rules, wireRule)
	}
	if len(wire.Rules) == 0 {
		return nil
	}
	return wire
}

func compileNetworkHooks(cfg *NetworkInterceptionConfig) (*api.NetworkInterceptionConfig, map[string]compiledNetworkHook, error) {
	if cfg == nil {
		return nil, nil, nil
	}

	wire := buildWireNetworkInterception(cfg)
	local := make(map[string]compiledNetworkHook)
	if wire == nil {
		return nil, nil, nil
	}

	for i, rule := range cfg.Rules {
		if rule.Hook == nil {
			continue
		}
		if action := strings.ToLower(strings.TrimSpace(string(rule.Action))); action != "" && action != string(NetworkHookActionAllow) {
			return nil, nil, errx.With(ErrInvalidNetworkHook, " %q callback hooks cannot set action=%q", rule.Name, rule.Action)
		}

		callbackID := fmt.Sprintf("network_hook_%d", i+1)
		timeout := time.Duration(0)
		if rule.TimeoutMS > 0 {
			timeout = time.Duration(rule.TimeoutMS) * time.Millisecond
		}

		local[callbackID] = compiledNetworkHook{
			id:       callbackID,
			name:     rule.Name,
			phase:    NetworkHookPhase(strings.ToLower(strings.TrimSpace(string(rule.Phase)))),
			timeout:  timeout,
			callback: rule.Hook,
		}
		wire.Rules[i].CallbackID = callbackID
	}

	if len(local) == 0 {
		return wire, nil, nil
	}
	return wire, local, nil
}

func startNetworkHookServer(hooks map[string]compiledNetworkHook) (string, net.Listener, string, error) {
	tempDir, err := os.MkdirTemp("", "matchlock-network-hook-*")
	if err != nil {
		return "", nil, "", err
	}
	socketPath := filepath.Join(tempDir, "hook.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		_ = os.RemoveAll(tempDir)
		return "", nil, "", err
	}

	hooksCopy := make(map[string]compiledNetworkHook, len(hooks))
	for k, v := range hooks {
		hooksCopy[k] = v
	}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				if strings.Contains(strings.ToLower(err.Error()), "closed") {
					return
				}
				continue
			}
			go serveNetworkHookConn(conn, hooksCopy)
		}
	}()

	return socketPath, listener, tempDir, nil
}

func serveNetworkHookConn(conn net.Conn, hooks map[string]compiledNetworkHook) {
	defer conn.Close()
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)

	var req api.NetworkHookCallbackRequest
	if err := dec.Decode(&req); err != nil {
		_ = enc.Encode(api.NetworkHookCallbackResponse{Error: err.Error()})
		return
	}

	hook, ok := hooks[req.CallbackID]
	if !ok || hook.callback == nil {
		_ = enc.Encode(api.NetworkHookCallbackResponse{Error: "network hook callback not found"})
		return
	}
	if hook.phase != "" && !strings.EqualFold(string(hook.phase), req.Phase) {
		_ = enc.Encode(api.NetworkHookCallbackResponse{Error: "network hook phase mismatch"})
		return
	}

	ctx := context.Background()
	cancel := func() {}
	if hook.timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, hook.timeout)
	}
	defer cancel()

	result, err := hook.callback(ctx, NetworkHookRequest{
		Phase:           NetworkHookPhase(req.Phase),
		Host:            req.Host,
		Method:          req.Method,
		Path:            req.Path,
		Query:           cloneStringMap(req.Query),
		RequestHeaders:  cloneStringSliceMap(req.RequestHeaders),
		StatusCode:      req.StatusCode,
		ResponseHeaders: cloneStringSliceMap(req.ResponseHeaders),
		IsSSE:           req.IsSSE,
	})
	if err != nil {
		_ = enc.Encode(api.NetworkHookCallbackResponse{Error: err.Error()})
		return
	}

	resp := api.NetworkHookCallbackResponse{}
	if result != nil {
		resp.Action = string(result.Action)
		if result.Request != nil {
			resp.Request = &api.NetworkHookRequestMutation{
				Headers: cloneStringSliceMapPreserveEmpty(result.Request.Headers),
				Query:   cloneStringMapPreserveEmpty(result.Request.Query),
				Path:    result.Request.Path,
			}
		}
		if result.Response != nil {
			resp.Response = &api.NetworkHookResponseMutation{
				Headers: cloneStringSliceMapPreserveEmpty(result.Response.Headers),
			}
			if len(result.Response.BodyReplacements) > 0 {
				resp.Response.BodyReplacements = make([]api.NetworkBodyTransform, 0, len(result.Response.BodyReplacements))
				for _, item := range result.Response.BodyReplacements {
					resp.Response.BodyReplacements = append(resp.Response.BodyReplacements, api.NetworkBodyTransform{
						Find:    item.Find,
						Replace: item.Replace,
					})
				}
			}
			if result.Response.SetBody != nil {
				resp.Response.SetBodyBase64 = base64.StdEncoding.EncodeToString(result.Response.SetBody)
			}
		}
	}

	_ = enc.Encode(resp)
}

func cloneStringSliceMap(in map[string][]string) map[string][]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]string, len(in))
	for k, values := range in {
		out[k] = append([]string(nil), values...)
	}
	return out
}

func cloneStringSliceMapPreserveEmpty(in map[string][]string) map[string][]string {
	if in == nil {
		return nil
	}
	out := make(map[string][]string, len(in))
	for k, values := range in {
		out[k] = append([]string(nil), values...)
	}
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneStringMapPreserveEmpty(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (c *Client) stopNetworkHookServer() {
	c.networkHookMu.Lock()
	listener := c.networkHookListener
	tempDir := c.networkHookTempDir
	c.networkHooks = nil
	c.networkHookSocket = ""
	c.networkHookListener = nil
	c.networkHookTempDir = ""
	c.networkHookMu.Unlock()

	if listener != nil {
		_ = listener.Close()
	}
	if tempDir != "" {
		_ = os.RemoveAll(tempDir)
	}
}
