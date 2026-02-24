//go:build acceptance

package acceptance

import (
	"context"
	"strings"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSDKNetworkInterceptionMutatesRequest(t *testing.T) {
	t.Parallel()

	client := launchAlpineWithNetwork(t, sdk.New("alpine:latest").
		AllowHost("httpbin.org").
		WithNetworkInterception(&sdk.NetworkInterceptionConfig{
			Rules: []sdk.NetworkHookRule{
				{
					Phase:         sdk.NetworkHookPhaseBefore,
					Action:        sdk.NetworkHookActionMutate,
					Hosts:         []string{"httpbin.org"},
					Methods:       []string{"GET"},
					Path:          "/anything/v1",
					SetHeaders:    map[string]string{"X-Hook": "set"},
					DeleteHeaders: []string{"X-Remove"},
					SetQuery:      map[string]string{"trace": "hooked"},
					DeleteQuery:   []string{"drop"},
					RewritePath:   "/anything/v2",
				},
			},
		}),
	)

	result, err := client.Exec(context.Background(), `sh -c 'wget -q -O - --header "X-Remove: 1" "http://httpbin.org/anything/v1?drop=1"'`)
	require.NoError(t, err, "Exec")

	combined := result.Stdout + result.Stderr
	assert.Contains(t, combined, "/anything/v2?trace=hooked")
	assert.Contains(t, combined, `"trace": "hooked"`)
	assert.NotContains(t, combined, `"drop": "1"`)
	assert.Contains(t, combined, `"X-Hook": "set"`)
	assert.NotContains(t, combined, "X-Remove")
}

func TestSDKNetworkInterceptionMutatesResponse(t *testing.T) {
	t.Parallel()

	client := launchAlpineWithNetwork(t, sdk.New("alpine:latest").
		AllowHost("httpbin.org").
		WithNetworkInterception(&sdk.NetworkInterceptionConfig{
			Rules: []sdk.NetworkHookRule{
				{
					Phase:                 sdk.NetworkHookPhaseAfter,
					Action:                sdk.NetworkHookActionMutate,
					Hosts:                 []string{"httpbin.org"},
					Path:                  "/response-headers",
					SetResponseHeaders:    map[string]string{"X-Intercepted": "true"},
					DeleteResponseHeaders: []string{"X-Upstream"},
					BodyReplacements: []sdk.NetworkBodyTransform{
						{Find: "foo", Replace: "bar"},
					},
				},
			},
		}),
	)

	result, err := client.Exec(context.Background(), `sh -c 'wget -S -O - "http://httpbin.org/response-headers?X-Upstream=1&body=foo" 2>&1'`)
	require.NoError(t, err, "Exec")

	combined := result.Stdout + result.Stderr
	combinedLower := strings.ToLower(combined)
	assert.Contains(t, combinedLower, "x-intercepted: true")
	assert.NotContains(t, combinedLower, "x-upstream: 1")
	assert.Contains(t, combined, `"body": "bar"`)
}

func TestSDKNetworkInterceptionMutatesSSEDataLines(t *testing.T) {
	t.Parallel()

	client := launchAlpineWithNetwork(t, sdk.New("alpine:latest").
		AllowHost("httpbingo.org").
		WithNetworkInterception(&sdk.NetworkInterceptionConfig{
			Rules: []sdk.NetworkHookRule{
				{
					Phase:  sdk.NetworkHookPhaseAfter,
					Action: sdk.NetworkHookActionMutate,
					Hosts:  []string{"httpbingo.org"},
					Path:   "/sse",
					BodyReplacements: []sdk.NetworkBodyTransform{
						{Find: `"id"`, Replace: `"sid"`},
					},
				},
			},
		}),
	)

	result, err := client.Exec(context.Background(), `sh -c 'wget -q -O - "http://httpbingo.org/sse?count=2" 2>&1'`)
	require.NoError(t, err, "Exec")

	combined := result.Stdout + result.Stderr
	assert.Contains(t, combined, `data: {"sid":0`)
	assert.Contains(t, combined, `data: {"sid":1`)
	assert.NotContains(t, combined, `data: {"id":0`)
}
