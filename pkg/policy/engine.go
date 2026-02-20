package policy

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/jingkaihe/matchlock/pkg/api"
)

type Engine struct {
	config       *api.NetworkConfig
	placeholders map[string]string
}

// RouteDirective tells the HTTP interceptor to send a request to an
// alternative backend instead of the original destination.
// A nil *RouteDirective means "use the original destination."
type RouteDirective struct {
	Host   string // Target host, e.g., "127.0.0.1"
	Port   int    // Target port, e.g., 11434
	UseTLS bool   // Whether to use TLS for the upstream connection
}

func NewEngine(config *api.NetworkConfig) *Engine {
	e := &Engine{
		config:       config,
		placeholders: make(map[string]string),
	}

	for name, secret := range config.Secrets {
		if secret.Placeholder == "" {
			placeholder := generatePlaceholder()
			config.Secrets[name] = api.Secret{
				Value:       secret.Value,
				Placeholder: placeholder,
				Hosts:       secret.Hosts,
			}
		}
		e.placeholders[name] = config.Secrets[name].Placeholder
	}

	return e
}

func generatePlaceholder() string {
	b := make([]byte, 16)
	rand.Read(b)
	return "SANDBOX_SECRET_" + hex.EncodeToString(b)
}

func (e *Engine) GetPlaceholder(name string) string {
	return e.placeholders[name]
}

func (e *Engine) GetPlaceholders() map[string]string {
	result := make(map[string]string)
	for k, v := range e.placeholders {
		result[k] = v
	}
	return result
}

func (e *Engine) IsHostAllowed(host string) bool {
	host = strings.Split(host, ":")[0]

	if e.config.BlockPrivateIPs {
		if isPrivateIP(host) {
			if !e.isPrivateHostAllowed(host) {
				return false
			}
		}
	}

	if len(e.config.AllowedHosts) == 0 {
		return true
	}

	for _, pattern := range e.config.AllowedHosts {
		if matchGlob(pattern, host) {
			return true
		}
	}

	return false
}

func (e *Engine) isPrivateHostAllowed(host string) bool {
	for _, pattern := range e.config.AllowedPrivateHosts {
		if matchGlob(pattern, host) {
			return true
		}
	}
	return false
}

func (e *Engine) OnRequest(req *http.Request, host string) (*http.Request, error) {
	host = strings.Split(host, ":")[0]

	for name, secret := range e.config.Secrets {
		if !e.isSecretAllowedForHost(name, host) {
			if e.requestContainsPlaceholder(req, secret.Placeholder) {
				return nil, api.ErrSecretLeak
			}
			continue
		}
		e.replaceInRequest(req, secret.Placeholder, secret.Value)
	}

	return req, nil
}

func (e *Engine) OnResponse(resp *http.Response, req *http.Request, host string) (*http.Response, error) {
	return resp, nil
}

// RouteRequest inspects a request and returns a RouteDirective if the
// request should be sent to an alternative backend.
func (e *Engine) RouteRequest(req *http.Request, host string) (*RouteDirective, error) {
	if len(e.config.LocalModelRouting) == 0 {
		return nil, nil
	}

	host = strings.Split(host, ":")[0]

	for _, route := range e.config.LocalModelRouting {
		if route.SourceHost != host {
			continue
		}

		if req.Method != "POST" || req.URL.Path != route.GetPath() {
			return nil, nil
		}

		if req.Body == nil {
			return nil, nil
		}
		bodyBytes, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, nil
		}
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))

		var payload struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(bodyBytes, &payload); err != nil {
			return nil, nil
		}

		modelRoute, ok := route.Models[payload.Model]
		if !ok {
			return nil, nil
		}

		backendHost := modelRoute.EffectiveBackendHost(route.GetBackendHost())
		backendPort := modelRoute.EffectiveBackendPort(route.GetBackendPort())

		e.rewriteRequestForLocal(req, bodyBytes, payload.Model, modelRoute.Target, backendHost, backendPort)

		return &RouteDirective{
			Host:   backendHost,
			Port:   backendPort,
			UseTLS: false,
		}, nil
	}

	return nil, nil
}

func (e *Engine) rewriteRequestForLocal(req *http.Request, bodyBytes []byte, originalModel, targetModel, backendHost string, backendPort int) {
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

func (e *Engine) isSecretAllowedForHost(secretName, host string) bool {
	secret, ok := e.config.Secrets[secretName]
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

func (e *Engine) requestContainsPlaceholder(req *http.Request, placeholder string) bool {
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
// and URL query params only. We intentionally skip the request body because the
// body is processed by the remote server's application layer, which may log or
// echo it back in responses — leaking the real secret into the VM.
func (e *Engine) replaceInRequest(req *http.Request, placeholder, value string) {
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

func matchGlob(pattern, str string) bool {
	if pattern == "*" {
		return true
	}

	// Simple prefix wildcard: *.example.com
	if strings.HasPrefix(pattern, "*.") && !strings.Contains(pattern[2:], "*") {
		suffix := pattern[1:]
		return strings.HasSuffix(str, suffix)
	}

	// Simple suffix wildcard: example.*
	if strings.HasSuffix(pattern, ".*") && !strings.Contains(pattern[:len(pattern)-2], "*") {
		prefix := pattern[:len(pattern)-2]
		return strings.HasPrefix(str, prefix+".")
	}

	// General glob matching with * as wildcard
	if strings.Contains(pattern, "*") {
		return matchWildcard(pattern, str)
	}

	return pattern == str
}

// matchWildcard handles patterns with * wildcards anywhere
func matchWildcard(pattern, str string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == str
	}

	// Check prefix (before first *)
	if parts[0] != "" && !strings.HasPrefix(str, parts[0]) {
		return false
	}
	str = str[len(parts[0]):]

	// Check suffix (after last *)
	lastPart := parts[len(parts)-1]
	if lastPart != "" && !strings.HasSuffix(str, lastPart) {
		return false
	}
	if lastPart != "" {
		str = str[:len(str)-len(lastPart)]
	}

	// Check middle parts in order
	for i := 1; i < len(parts)-1; i++ {
		if parts[i] == "" {
			continue
		}
		idx := strings.Index(str, parts[i])
		if idx < 0 {
			return false
		}
		str = str[idx+len(parts[i]):]
	}

	return true
}

func isPrivateIP(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		ips, err := net.LookupIP(host)
		if err != nil || len(ips) == 0 {
			return false
		}
		ip = ips[0]
	}

	privateRanges := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"127.0.0.0/8",
		"169.254.0.0/16",
		"::1/128",
		"fc00::/7",
		"fe80::/10",
	}

	for _, cidr := range privateRanges {
		_, network, _ := net.ParseCIDR(cidr)
		if network.Contains(ip) {
			return true
		}
	}

	return false
}
