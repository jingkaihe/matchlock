package policy

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The proxy hands OnRequest/OnResponse the guest's Host header verbatim, which
// for an IPv6 destination is the bracketed literal ("[fd00:1::5]:8080"). The
// engine must normalize it exactly like an IPv4 host:port pair, otherwise every
// host-scoped decision (network-hook rules, secret host lists) silently misses
// and a rule that should block lets the request through.

func TestEngine_OnRequest_NetworkHookBlockMatchesBracketedIPv6Host(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Interception: &api.NetworkInterceptionConfig{
			Rules: []api.NetworkHookRule{
				{
					Phase:  "before",
					Action: "block",
					Hosts:  []string{"fd00:1::5"},
				},
			},
		},
	})

	for _, host := range []string{"[fd00:1::5]:8080", "fd00:1::5", "[fd00:1::5]"} {
		req := &http.Request{Header: http.Header{}, URL: &url.URL{}}

		_, err := engine.OnRequest(req, host)
		require.ErrorIs(t, err, api.ErrBlocked, "host %q must match the rule's bare v6 literal", host)
	}
}

func TestEngine_OnRequest_NetworkHookBlockIgnoresOtherIPv6Hosts(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Interception: &api.NetworkInterceptionConfig{
			Rules: []api.NetworkHookRule{
				{
					Phase:  "before",
					Action: "block",
					Hosts:  []string{"fd00:1::5"},
				},
			},
		},
	})

	for _, host := range []string{"[fd00:1::6]:8080", "fd01:1::5", "[200::1]:443"} {
		req := &http.Request{Header: http.Header{}, URL: &url.URL{}}

		_, err := engine.OnRequest(req, host)
		require.NoError(t, err, "host %q must not match", host)
	}
}

func TestEngine_OnResponse_NetworkHookBlockMatchesBracketedIPv6Host(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Interception: &api.NetworkInterceptionConfig{
			Rules: []api.NetworkHookRule{
				{
					Phase:  "after",
					Action: "block",
					Hosts:  []string{"200::1"},
				},
			},
		},
	})

	resp := &http.Response{Header: http.Header{}, StatusCode: http.StatusOK}
	req := &http.Request{Header: http.Header{}, URL: &url.URL{}}

	_, err := engine.OnResponse(resp, req, "[200::1]:8443")
	require.ErrorIs(t, err, api.ErrBlocked)

	_, err = engine.OnResponse(resp, req, "200::1")
	require.ErrorIs(t, err, api.ErrBlocked)
}

func TestEngine_OnRequest_SecretReplacementUsesBracketedIPv6Host(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Secrets: map[string]api.Secret{
			"API_KEY": {
				Value: "real-secret",
				Hosts: []string{"fd00:100::1"},
			},
		},
	})

	placeholder := engine.GetPlaceholder("API_KEY")

	req := &http.Request{
		Header: http.Header{"Authorization": []string{"Bearer " + placeholder}},
		URL:    &url.URL{},
	}

	got, err := engine.OnRequest(req, "[fd00:100::1]:8080")
	require.NoError(t, err, "a bracketed v6 host must match the secret's host list")
	assert.Equal(t, "Bearer real-secret", got.Header.Get("Authorization"))
}

func TestEngine_OnRequest_SecretNotReplacedForOtherIPv6Host(t *testing.T) {
	engine := NewEngine(&api.NetworkConfig{
		Secrets: map[string]api.Secret{
			"API_KEY": {
				Value: "real-secret",
				Hosts: []string{"fd00:100::1"},
			},
		},
	})

	placeholder := engine.GetPlaceholder("API_KEY")

	req := &http.Request{
		Header: http.Header{"Authorization": []string{"Bearer " + placeholder}},
		URL:    &url.URL{},
	}

	// The placeholder is offered to a host the secret is not scoped to, so the
	// engine refuses (leak protection) instead of sending the real value.
	_, err := engine.OnRequest(req, "[fd00:100::2]:8080")
	require.ErrorIs(t, err, api.ErrSecretLeak)
}
