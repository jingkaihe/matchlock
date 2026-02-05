package policy

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"strings"

	"github.com/jingkaihe/matchlock/pkg/api"
)

type Engine struct {
	config       *api.NetworkConfig
	placeholders map[string]string
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
			return false
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

	if req.Body != nil && req.ContentLength > 0 && req.ContentLength < 10*1024*1024 {
		body := make([]byte, req.ContentLength)
		req.Body.Read(body)
		req.Body.Close()

		if bytes.Contains(body, []byte(placeholder)) {
			body = bytes.ReplaceAll(body, []byte(placeholder), []byte(value))
			req.ContentLength = int64(len(body))
		}
		req.Body = &readCloser{bytes.NewReader(body)}
	}
}

type readCloser struct {
	*bytes.Reader
}

func (r *readCloser) Close() error { return nil }

func matchGlob(pattern, str string) bool {
	if pattern == "*" {
		return true
	}

	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:]
		return strings.HasSuffix(str, suffix)
	}

	if strings.HasSuffix(pattern, ".*") {
		prefix := pattern[:len(pattern)-2]
		return strings.HasPrefix(str, prefix+".")
	}

	return pattern == str
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
