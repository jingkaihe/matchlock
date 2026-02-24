package policy

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/logging"
)

// SecretInjectorConfig is the typed config for the secret_injector plugin.
type SecretInjectorConfig struct {
	Secrets map[string]api.Secret `json:"secrets"`
}

// secretInjectorPlugin implements RequestPlugin and PlaceholderProvider.
type secretInjectorPlugin struct {
	secrets      map[string]api.Secret
	placeholders map[string]string
	logger       *slog.Logger
	emitter      *logging.Emitter // nil means no event logging
}

// NewSecretInjectorPlugin creates a secret_injector plugin from a secrets map.
// Called during flat-field compilation in NewEngine.
// Generates placeholders for secrets that don't already have one.
func NewSecretInjectorPlugin(secrets map[string]api.Secret, logger *slog.Logger, emitter *logging.Emitter) *secretInjectorPlugin {
	if logger == nil {
		logger = slog.Default()
	}
	p := &secretInjectorPlugin{
		secrets:      make(map[string]api.Secret),
		placeholders: make(map[string]string),
		logger:       logger,
		emitter:      emitter,
	}
	for name, secret := range secrets {
		if secret.Placeholder == "" {
			secret.Placeholder = generatePlaceholder()
		}
		p.secrets[name] = secret
		p.placeholders[name] = secret.Placeholder
	}
	return p
}

// NewSecretInjectorPluginFromConfig creates a secret_injector plugin from JSON config.
// Called by the plugin registry factory.
func NewSecretInjectorPluginFromConfig(raw json.RawMessage, logger *slog.Logger, emitter *logging.Emitter) (Plugin, error) {
	if logger == nil {
		logger = slog.Default()
	}
	var cfg SecretInjectorConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	return NewSecretInjectorPlugin(cfg.Secrets, logger, emitter), nil
}

func (p *secretInjectorPlugin) Name() string {
	return "secret_injector"
}

// GetPlaceholders implements PlaceholderProvider.
func (p *secretInjectorPlugin) GetPlaceholders() map[string]string {
	result := make(map[string]string, len(p.placeholders))
	for k, v := range p.placeholders {
		result[k] = v
	}
	return result
}

// TransformRequest implements RequestPlugin.
// Logic extracted from Engine.OnRequest.
func (p *secretInjectorPlugin) TransformRequest(req *http.Request, host string) (*http.Request, error) {
	host = strings.Split(host, ":")[0]

	for name, secret := range p.secrets {
		if !p.isSecretAllowedForHost(name, host) {
			if p.requestContainsPlaceholder(req, secret.Placeholder) {
				p.logger.Debug("secret leak detected", "name", name, "host", host)
				if p.emitter != nil {
					_ = p.emitter.Emit(logging.EventKeyInjection,
						fmt.Sprintf("secret %q leak blocked for %s", name, host),
						"secret_injector",
						nil,
						&logging.KeyInjectionData{
							SecretName: name,
							Host:       host,
							Action:     "leak_blocked",
						})
				}
				return nil, api.ErrSecretLeak
			}
			p.logger.Debug("secret skipped for host", "name", name, "host", host)
			if p.emitter != nil {
				_ = p.emitter.Emit(logging.EventKeyInjection,
					fmt.Sprintf("secret %q skipped for %s", name, host),
					"secret_injector",
					nil,
					&logging.KeyInjectionData{
						SecretName: name,
						Host:       host,
						Action:     "skipped",
					})
			}
			continue
		}
		p.replaceInRequest(req, secret.Placeholder, secret.Value)
		p.logger.Debug("secret injected", "name", name, "host", host)
		if p.emitter != nil {
			_ = p.emitter.Emit(logging.EventKeyInjection,
				fmt.Sprintf("secret %q injected for %s", name, host),
				"secret_injector",
				nil,
				&logging.KeyInjectionData{
					SecretName: name,
					Host:       host,
					Action:     "injected",
				})
		}
	}

	return req, nil
}

func (p *secretInjectorPlugin) isSecretAllowedForHost(secretName, host string) bool {
	secret, ok := p.secrets[secretName]
	if !ok {
		return false
	}
	if len(secret.Hosts) == 0 {
		return true
	}
	for _, pattern := range secret.Hosts {
		if matchGlob(pattern, host) {
			return true
		}
	}
	return false
}

func (p *secretInjectorPlugin) requestContainsPlaceholder(req *http.Request, placeholder string) bool {
	for _, values := range req.Header {
		for _, v := range values {
			if strings.Contains(v, placeholder) {
				return true
			}
		}
	}
	if req.URL != nil {
		if strings.Contains(req.URL.String(), placeholder) {
			return true
		}
	}
	return false
}

// replaceInRequest substitutes the placeholder with the real secret in headers
// and URL query params only. Body is intentionally skipped.
func (p *secretInjectorPlugin) replaceInRequest(req *http.Request, placeholder, value string) {
	for key, values := range req.Header {
		for i, v := range values {
			if strings.Contains(v, placeholder) {
				req.Header[key][i] = strings.ReplaceAll(v, placeholder, value)
			}
		}
	}
	if req.URL != nil {
		if strings.Contains(req.URL.RawQuery, placeholder) {
			req.URL.RawQuery = strings.ReplaceAll(req.URL.RawQuery, placeholder, value)
		}
	}
}
