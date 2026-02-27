package policy

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/logging"
)

// Engine orchestrates network policy plugins.
// Its public API is unchanged from the pre-plugin version.
type Engine struct {
	gates     []GatePlugin
	routers   []RoutePlugin
	requests  []RequestPlugin
	responses []ResponsePlugin

	placeholders map[string]string
	logger       *slog.Logger
	emitter      *logging.Emitter // nil means no event logging

	// Upstream network hook infrastructure — compiled interception rules
	// and an optional SDK-local callback invoker.
	networkRules []compiledNetworkRule
	networkHook  networkHookInvoker
}

// NewEngine creates a policy engine from a NetworkConfig.
// It compiles flat config fields into built-in plugins and processes
// any explicit plugin entries from config.Plugins.
func NewEngine(config *api.NetworkConfig, logger *slog.Logger, emitter *logging.Emitter) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	e := &Engine{
		placeholders: make(map[string]string),
		logger:       logger.With("component", "policy"),
		emitter:      emitter,
	}

	// --- Step 1: Compile flat config fields into built-in plugins ---

	// Track which built-in types were created from flat fields.
	// Used for conflict detection in step 2.
	flatTypes := make(map[string]bool)

	if len(config.AllowedHosts) > 0 || config.BlockPrivateIPs {
		pluginLogger := e.logger.With("plugin", "host_filter")
		e.addPlugin(NewHostFilterPlugin(
			config.AllowedHosts,
			config.BlockPrivateIPs,
			config.AllowedPrivateHosts,
			pluginLogger,
		))
		flatTypes["host_filter"] = true
		e.logger.Debug("plugin registered from flat config", "plugin", "host_filter")
	}

	if len(config.Secrets) > 0 {
		pluginLogger := e.logger.With("plugin", "secret_injector")
		p := NewSecretInjectorPlugin(config.Secrets, pluginLogger)
		e.addPlugin(p)
		flatTypes["secret_injector"] = true
		e.logger.Debug("plugin registered from flat config", "plugin", "secret_injector")

		// Back-populate placeholders into the original config.Secrets
		// so that callers who read config.Secrets[name].Placeholder
		// (e.g., sandbox_common.go) see the generated values.
		for name, placeholder := range p.GetPlaceholders() {
			if secret, ok := config.Secrets[name]; ok {
				secret.Placeholder = placeholder
				config.Secrets[name] = secret
			}
		}
	}

	if len(config.LocalModelRouting) > 0 {
		pluginLogger := e.logger.With("plugin", "local_model_router")
		e.addPlugin(NewLocalModelRouterPlugin(config.LocalModelRouting, pluginLogger))
		flatTypes["local_model_router"] = true
		e.logger.Debug("plugin registered from flat config", "plugin", "local_model_router")
	}

	var usageLogger *usageLoggerPlugin

	if config.UsageLogPath != "" {
		pluginLogger := e.logger.With("plugin", "usage_logger")
		usageLogger = NewUsageLoggerPlugin(config.UsageLogPath, pluginLogger)
		e.addPlugin(usageLogger)
		flatTypes["usage_logger"] = true
		e.logger.Debug("plugin registered from flat config", "plugin", "usage_logger")
	}

	if config.BudgetLimitUSD > 0 {
		if usageLogger == nil {
			e.logger.Error("budget_limit_usd requires usage_log_path to be set; budget enforcement disabled")
		} else {
			pluginLogger := e.logger.With("plugin", "budget_gate")
			e.addPlugin(NewBudgetGatePlugin(config.BudgetLimitUSD, usageLogger, pluginLogger))
			flatTypes["budget_gate"] = true
			e.logger.Debug("plugin registered from flat config", "plugin", "budget_gate")
		}
	}

	// --- Step 2: Add explicitly configured plugins from network.plugins ---

	for _, pluginCfg := range config.Plugins {
		if !pluginCfg.IsEnabled() {
			continue
		}

		// Conflict detection: merge, but warn
		if flatTypes[pluginCfg.Type] {
			e.logger.Warn("duplicate plugin type in flat fields and plugins array",
				"type", pluginCfg.Type)
		}

		factory, ok := LookupFactory(pluginCfg.Type)
		if !ok {
			e.logger.Warn("unknown plugin type, skipping", "type", pluginCfg.Type)
			continue
		}

		pluginLogger := e.logger.With("plugin", pluginCfg.Type)
		p, err := factory(pluginCfg.Config, pluginLogger)
		if err != nil {
			e.logger.Warn("plugin creation failed, skipping",
				"type", pluginCfg.Type, "error", err)
			continue
		}

		e.addPlugin(p)
		e.logger.Debug("plugin registered from config array", "plugin", pluginCfg.Type)
	}

	// --- Step 3: Collect placeholders from all PlaceholderProvider plugins ---

	e.collectPlaceholders()

	// --- Step 4: Compile upstream network interception rules ---

	if config.Interception != nil {
		e.networkRules = compileNetworkRules(config.Interception)
		e.networkHook = newNetworkHookInvoker(config)
	}

	e.logger.Info("engine ready",
		"gates", len(e.gates),
		"routers", len(e.routers),
		"requests", len(e.requests),
		"responses", len(e.responses),
		"networkRules", len(e.networkRules),
	)

	return e
}

// SetNetworkHookInvoker replaces the SDK-local network hook invoker.
// Used by the SDK to inject a callback invoker after engine creation.
func (e *Engine) SetNetworkHookInvoker(invoker networkHookInvoker) {
	e.networkHook = invoker
}

// addPlugin sorts a plugin into the correct phase slices based on
// which interfaces it implements. A single plugin can appear in
// multiple slices (e.g., local_model_router appears in both routers
// and requests).
func (e *Engine) addPlugin(p Plugin) {
	if gp, ok := p.(GatePlugin); ok {
		e.gates = append(e.gates, gp)
	}
	if rp, ok := p.(RoutePlugin); ok {
		e.routers = append(e.routers, rp)
	}
	if rqp, ok := p.(RequestPlugin); ok {
		e.requests = append(e.requests, rqp)
	}
	if rsp, ok := p.(ResponsePlugin); ok {
		e.responses = append(e.responses, rsp)
	}
}

// collectPlaceholders gathers placeholders from all registered plugins
// that implement PlaceholderProvider.
func (e *Engine) collectPlaceholders() {
	collect := func(p Plugin) {
		if pp, ok := p.(PlaceholderProvider); ok {
			for k, v := range pp.GetPlaceholders() {
				e.placeholders[k] = v
			}
		}
	}
	for _, p := range e.gates {
		collect(p)
	}
	for _, p := range e.routers {
		collect(p)
	}
	for _, p := range e.requests {
		collect(p)
	}
	for _, p := range e.responses {
		collect(p)
	}
}

// --- Public API (signatures unchanged) ---

// IsHostAllowed checks whether the given host is permitted by gate plugins.
// Returns nil if the host is allowed (all gates passed or no gates registered).
// Returns a non-nil *GateVerdict if a gate blocked the host.
func (e *Engine) IsHostAllowed(host string) *GateVerdict {
	if len(e.gates) == 0 {
		return nil
	}
	for _, g := range e.gates {
		verdict := g.Gate(host)
		if !verdict.Allowed {
			e.logger.Warn("gate blocked",
				"plugin", g.Name(),
				"host", host,
				"reason", verdict.Reason,
			)
			if e.emitter != nil {
				_ = e.emitter.Emit(logging.EventGateDecision,
					fmt.Sprintf("gate blocked %s by %s: %s", host, g.Name(), verdict.Reason),
					g.Name(),
					nil,
					&logging.GateDecisionData{
						Host:    host,
						Allowed: false,
						Reason:  verdict.Reason,
					})
			}
			return verdict
		}
		e.logger.Debug("gate allowed", "plugin", g.Name(), "host", host)
		if e.emitter != nil {
			_ = e.emitter.Emit(logging.EventGateDecision,
				fmt.Sprintf("gate allowed %s by %s", host, g.Name()),
				g.Name(),
				nil,
				&logging.GateDecisionData{
					Host:    host,
					Allowed: true,
				})
		}
	}
	return nil
}

// RouteRequest inspects a request and returns a RouteDirective if a router
// plugin wants to redirect it. First non-nil directive wins.
// Emits a route_decision event for each router plugin evaluated.
func (e *Engine) RouteRequest(req *http.Request, host string) (*RouteDirective, error) {
	for _, r := range e.routers {
		decision, err := r.Route(req, host)
		if err != nil {
			e.logger.Warn("route error", "plugin", r.Name(), "host", host, "error", err)
			return nil, err
		}

		if e.emitter != nil {
			action := "passthrough"
			routedTo := ""
			if decision.Directive != nil {
				action = "redirected"
				routedTo = fmt.Sprintf("%s:%d", decision.Directive.Host, decision.Directive.Port)
			}
			summary := fmt.Sprintf("route %s %s by %s", action, host, r.Name())
			if routedTo != "" {
				summary = fmt.Sprintf("route %s %s -> %s by %s", action, host, routedTo, r.Name())
			}
			_ = e.emitter.Emit(logging.EventRouteDecision,
				summary,
				r.Name(),
				nil,
				&logging.RouteDecisionData{
					Host:     host,
					Action:   action,
					RoutedTo: routedTo,
					Reason:   decision.Reason,
				})
		}

		if decision.Directive != nil {
			e.logger.Info(
				fmt.Sprintf("local model redirect: %s request to %s%s redirected to -> %s:%d (local-backend)",
					req.Method, host, req.URL.Path, decision.Directive.Host, decision.Directive.Port),
				"plugin", r.Name(),
			)
			return decision.Directive, nil
		}
	}
	e.logger.Debug("route passthrough",
		"host", host,
		"method", req.Method,
		"path", req.URL.Path,
	)
	return nil, nil
}

// OnRequest runs request transform plugins in chain order, then applies
// upstream network interception rules.
// Emits a request_transform event for each plugin.
func (e *Engine) OnRequest(req *http.Request, host string) (*http.Request, error) {
	// Apply upstream network hook rules (before phase)
	if len(e.networkRules) > 0 {
		if err := e.applyBeforeNetworkRules(req, host); err != nil {
			return nil, err
		}
	}

	for _, p := range e.requests {
		decision, err := p.TransformRequest(req, host)
		if err != nil {
			e.logger.Warn("request transform failed",
				"plugin", p.Name(), "host", host, "error", err)
			return nil, err
		}

		if e.emitter != nil {
			_ = e.emitter.Emit(logging.EventRequestTransform,
				fmt.Sprintf("%s: %s for %s", p.Name(), decision.Action, host),
				p.Name(),
				nil,
				&logging.RequestTransformData{
					Host:   host,
					Action: decision.Action,
					Reason: decision.Reason,
				})
		}

		req = decision.Request
	}
	return req, nil
}

// OnResponse runs response transform plugins in chain order, then applies
// upstream network interception rules (after phase).
// Emits a response_transform event for each plugin.
func (e *Engine) OnResponse(resp *http.Response, req *http.Request, host string) (*http.Response, error) {
	// Apply upstream network hook rules (after phase)
	if len(e.networkRules) > 0 {
		var err error
		resp, err = e.applyAfterNetworkRules(resp, req, host)
		if err != nil {
			return nil, err
		}
	}

	for _, p := range e.responses {
		decision, err := p.TransformResponse(resp, req, host)
		if err != nil {
			e.logger.Warn("response transform failed",
				"plugin", p.Name(), "host", host, "error", err)
			return nil, err
		}

		if e.emitter != nil {
			_ = e.emitter.Emit(logging.EventResponseTransform,
				fmt.Sprintf("%s: %s for %s", p.Name(), decision.Action, host),
				p.Name(),
				nil,
				&logging.ResponseTransformData{
					Host:   host,
					Action: decision.Action,
					Reason: decision.Reason,
				})
		}

		resp = decision.Response
	}
	return resp, nil
}

// GetPlaceholder returns the placeholder for a named secret.
func (e *Engine) GetPlaceholder(name string) string {
	return e.placeholders[name]
}

// GetPlaceholders returns a copy of all secret placeholders.
func (e *Engine) GetPlaceholders() map[string]string {
	result := make(map[string]string, len(e.placeholders))
	for k, v := range e.placeholders {
		result[k] = v
	}
	return result
}

// AddAllowedHosts adds hosts to the first hostFilterPlugin's allow-list.
// Returns the current allow-list after modification.
func (e *Engine) AddAllowedHosts(hosts ...string) []string {
	for _, g := range e.gates {
		if hf, ok := g.(*hostFilterPlugin); ok {
			return hf.AddAllowedHosts(hosts...)
		}
	}
	return nil
}

// RemoveAllowedHosts removes hosts from the first hostFilterPlugin's allow-list.
// Returns the current allow-list after modification.
func (e *Engine) RemoveAllowedHosts(hosts ...string) []string {
	for _, g := range e.gates {
		if hf, ok := g.(*hostFilterPlugin); ok {
			return hf.RemoveAllowedHosts(hosts...)
		}
	}
	return nil
}

// AllowedHosts returns the current allow-list from the first hostFilterPlugin.
func (e *Engine) AllowedHosts() []string {
	for _, g := range e.gates {
		if hf, ok := g.(*hostFilterPlugin); ok {
			return hf.AllowedHosts()
		}
	}
	return nil
}

// Emitter returns the engine's event emitter. May be nil.
func (e *Engine) Emitter() *logging.Emitter {
	return e.emitter
}
