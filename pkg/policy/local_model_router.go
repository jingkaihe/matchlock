package policy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jingkaihe/matchlock/pkg/api"
)

// LocalModelRouterConfig is the typed config for the local_model_router plugin.
type LocalModelRouterConfig struct {
	Routes []api.LocalModelRoute `json:"routes"`
}

// localModelRouterPlugin implements RoutePlugin and RequestPlugin.
type localModelRouterPlugin struct {
	routes []api.LocalModelRoute
	logger *slog.Logger
}

// NewLocalModelRouterPlugin creates a local_model_router plugin from route config.
// Called during flat-field compilation in NewEngine.
func NewLocalModelRouterPlugin(routes []api.LocalModelRoute, logger *slog.Logger) *localModelRouterPlugin {
	if logger == nil {
		logger = slog.Default()
	}
	return &localModelRouterPlugin{
		routes: routes,
		logger: logger,
	}
}

// NewLocalModelRouterPluginFromConfig creates from JSON config.
// Called by the plugin registry factory.
func NewLocalModelRouterPluginFromConfig(raw json.RawMessage, logger *slog.Logger) (Plugin, error) {
	if logger == nil {
		logger = slog.Default()
	}
	var cfg LocalModelRouterConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	return NewLocalModelRouterPlugin(cfg.Routes, logger), nil
}

func (p *localModelRouterPlugin) Name() string {
	return "local_model_router"
}

// Route implements RoutePlugin.
// Logic extracted from Engine.RouteRequest.
//
// This method also performs request rewriting as a side effect
// (same as the current engine). The Route() call both determines the routing
// directive AND rewrites the request body/headers.
func (p *localModelRouterPlugin) Route(req *http.Request, host string) (*RouteDecision, error) {
	if len(p.routes) == 0 {
		return &RouteDecision{Reason: "no routes configured"}, nil
	}

	host = strings.Split(host, ":")[0]

	for _, route := range p.routes {
		if route.SourceHost != host {
			continue
		}

		if req.Method != "POST" || req.URL.Path != route.GetPath() {
			return &RouteDecision{Reason: fmt.Sprintf("passthrough: method=%s path=%s", req.Method, req.URL.Path)}, nil
		}

		if req.Body == nil {
			return &RouteDecision{Reason: "passthrough: no request body"}, nil
		}
		bodyBytes, err := io.ReadAll(req.Body)
		if err != nil {
			return &RouteDecision{Reason: "passthrough: failed to read body"}, nil
		}
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))

		var payload struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(bodyBytes, &payload); err != nil {
			return &RouteDecision{Reason: "passthrough: invalid JSON body"}, nil
		}

		modelRoute, ok := route.Models[payload.Model]
		if !ok {
			p.logger.Debug("model not in route table, passing through",
				"model", payload.Model, "source_host", host)
			return &RouteDecision{
				Reason: fmt.Sprintf("no matching route for %s", payload.Model),
			}, nil
		}

		backendHost := modelRoute.EffectiveBackendHost(route.GetBackendHost())
		backendPort := modelRoute.EffectiveBackendPort(route.GetBackendPort())

		p.logger.Debug("model matched, rewriting request",
			"model", payload.Model,
			"target", modelRoute.Target,
			"backend", fmt.Sprintf("%s:%d", backendHost, backendPort),
		)

		// Perform request rewriting inline (same as current engine behavior)
		rewriteRequestForLocal(req, bodyBytes, payload.Model, modelRoute.Target, backendHost, backendPort)

		return &RouteDecision{
			Directive: &RouteDirective{
				Host:   backendHost,
				Port:   backendPort,
				UseTLS: false,
			},
			Reason: fmt.Sprintf("matched model %s -> %s at %s:%d",
				payload.Model, modelRoute.Target, backendHost, backendPort),
		}, nil
	}

	return &RouteDecision{Reason: fmt.Sprintf("no route entry for host %s", host)}, nil
}

// TransformRequest implements RequestPlugin.
// For the local_model_router, the rewriting is already done in Route().
// This is a no-op pass-through to satisfy the interface.
func (p *localModelRouterPlugin) TransformRequest(req *http.Request, host string) (*RequestDecision, error) {
	return &RequestDecision{
		Request: req,
		Action:  "no_op",
		Reason:  "request transform is handled in Route()",
	}, nil
}

// rewriteRequestForLocal is extracted from Engine.rewriteRequestForLocal.
// It is a package-level function since it does not need plugin state.
func rewriteRequestForLocal(req *http.Request, bodyBytes []byte, originalModel, targetModel, backendHost string, backendPort int) {
	req.URL.Path = "/v1/chat/completions"

	req.Header.Del("Authorization")
	req.Header.Del("Http-Referer")
	req.Header.Del("X-Title")

	var bodyMap map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &bodyMap); err != nil {
		newBody := bytes.Replace(bodyBytes, []byte(`"`+originalModel+`"`), []byte(`"`+targetModel+`"`), 1)
		req.Body = io.NopCloser(bytes.NewReader(newBody))
		req.ContentLength = int64(len(newBody))
		return
	}

	bodyMap["model"] = targetModel
	delete(bodyMap, "route")
	delete(bodyMap, "transforms")
	delete(bodyMap, "provider")

	newBody, err := json.Marshal(bodyMap)
	if err != nil {
		newBody = bytes.Replace(bodyBytes, []byte(`"`+originalModel+`"`), []byte(`"`+targetModel+`"`), 1)
	}

	req.Body = io.NopCloser(bytes.NewReader(newBody))
	req.ContentLength = int64(len(newBody))
	req.Header.Set("Content-Length", fmt.Sprintf("%d", len(newBody)))

	req.Host = fmt.Sprintf("%s:%d", backendHost, backendPort)
}
