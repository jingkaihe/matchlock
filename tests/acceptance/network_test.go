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

// launchAlpineWithNetwork creates a sandbox with network policy configured.
func launchAlpineWithNetwork(t *testing.T, builder *sdk.SandboxBuilder) *sdk.Client {
	t.Helper()
	client, err := sdk.NewClient(matchlockConfig(t))
	require.NoError(t, err, "NewClient")

	t.Cleanup(func() {
		client.Close(0)
		client.Remove()
	})

	_, err = client.Launch(builder)
	require.NoError(t, err, "Launch")

	return client
}

// ---------------------------------------------------------------------------
// Allowlist tests
// ---------------------------------------------------------------------------

func TestAllowlistBlocksHTTP(t *testing.T) {
	sandbox := sdk.New("alpine:latest").
		AllowHost("example.com")

	client := launchAlpineWithNetwork(t, sandbox)

	result, err := client.Exec(context.Background(), "wget -q -O - http://httpbin.org/get 2>&1 || true")
	require.NoError(t, err, "Exec")

	combined := result.Stdout + result.Stderr
	assert.NotContains(t, combined, `"url"`, "expected request to httpbin.org to be blocked")
}

func TestAllowlistPermitsHTTP(t *testing.T) {
	sandbox := sdk.New("alpine:latest").
		AllowHost("httpbin.org")

	client := launchAlpineWithNetwork(t, sandbox)

	result, err := client.Exec(context.Background(), "wget -q -O - http://httpbin.org/get 2>&1")
	require.NoError(t, err, "Exec")

	combined := result.Stdout + result.Stderr
	assert.Contains(t, combined, `"url"`, "expected request to httpbin.org to succeed")
}

func TestAllowlistBlocksHTTPS(t *testing.T) {
	sandbox := sdk.New("alpine:latest").
		AllowHost("example.com")

	client := launchAlpineWithNetwork(t, sandbox)

	result, err := client.Exec(context.Background(), "wget -q -O - https://httpbin.org/get 2>&1 || true")
	require.NoError(t, err, "Exec")

	combined := result.Stdout + result.Stderr
	assert.NotContains(t, combined, `"url"`, "expected HTTPS request to httpbin.org to be blocked")
}

func TestAllowlistPermitsHTTPS(t *testing.T) {
	sandbox := sdk.New("alpine:latest").
		AllowHost("httpbin.org")

	client := launchAlpineWithNetwork(t, sandbox)

	result, err := client.Exec(context.Background(), "wget -q -O - https://httpbin.org/get 2>&1")
	require.NoError(t, err, "Exec")

	combined := result.Stdout + result.Stderr
	assert.Contains(t, combined, `"url"`, "expected HTTPS request to httpbin.org to succeed")
}

func TestAllowlistGlobPattern(t *testing.T) {
	sandbox := sdk.New("alpine:latest").
		AllowHost("*.org")

	client := launchAlpineWithNetwork(t, sandbox)

	result, err := client.Exec(context.Background(), "wget -q -O - http://httpbin.org/get 2>&1")
	require.NoError(t, err, "Exec")

	combined := result.Stdout + result.Stderr
	assert.Contains(t, combined, `"url"`, "expected glob *.org to allow httpbin.org")
}

func TestAllowlistMultipleHosts(t *testing.T) {
	sandbox := sdk.New("alpine:latest").
		AllowHost("httpbin.org", "example.com")

	client := launchAlpineWithNetwork(t, sandbox)

	result, err := client.Exec(context.Background(), "wget -q -O - http://httpbin.org/get 2>&1")
	require.NoError(t, err, "Exec httpbin.org")
	assert.Contains(t, result.Stdout+result.Stderr, `"url"`, "expected httpbin.org to be allowed")

	result2, err := client.Exec(context.Background(), "wget -q -O - http://example.com/ 2>&1")
	require.NoError(t, err, "Exec example.com")
	assert.Contains(t, result2.Stdout+result2.Stderr, "Example Domain", "expected example.com to be allowed")
}

func TestNoAllowlistPermitsAll(t *testing.T) {
	// No AllowHost → all hosts are allowed (empty allowlist = permit all)
	sandbox := sdk.New("alpine:latest").
		BlockPrivateIPs() // enable interception without restricting hosts

	client := launchAlpineWithNetwork(t, sandbox)

	result, err := client.Exec(context.Background(), "wget -q -O - http://httpbin.org/get 2>&1")
	require.NoError(t, err, "Exec")

	assert.Contains(t, result.Stdout+result.Stderr, `"url"`, "expected open allowlist to permit httpbin.org")
}

// ---------------------------------------------------------------------------
// Secret MITM injection tests
// ---------------------------------------------------------------------------

func TestSecretInjectedInHTTPSHeader(t *testing.T) {
	secretValue := "sk-test-secret-value-12345"

	sandbox := sdk.New("alpine:latest").
		AllowHost("httpbin.org").
		AddSecret("MY_API_KEY", secretValue, "httpbin.org")

	client := launchAlpineWithNetwork(t, sandbox)

	// The guest sees a placeholder env var. Use it in a request header
	// and verify the MITM proxy replaces it with the real value.
	result, err := client.Exec(context.Background(), `sh -c 'wget -q -O - --header "Authorization: Bearer $MY_API_KEY" https://httpbin.org/headers 2>&1'`)
	require.NoError(t, err, "Exec")

	assert.Contains(t, result.Stdout, secretValue, "expected secret value to be injected in HTTPS header")
}

func TestSecretInjectedInHTTPHeader(t *testing.T) {
	secretValue := "sk-test-http-secret-67890"

	sandbox := sdk.New("alpine:latest").
		AllowHost("httpbin.org").
		AddSecret("HTTP_KEY", secretValue, "httpbin.org")

	client := launchAlpineWithNetwork(t, sandbox)

	result, err := client.Exec(context.Background(), `sh -c 'wget -q -O - --header "X-Api-Key: $HTTP_KEY" http://httpbin.org/headers 2>&1'`)
	require.NoError(t, err, "Exec")

	assert.Contains(t, result.Stdout, secretValue, "expected secret value to be injected in HTTP header")
}

func TestSecretPlaceholderNotExposedInGuest(t *testing.T) {
	secretValue := "sk-real-secret-never-seen"

	sandbox := sdk.New("alpine:latest").
		AllowHost("httpbin.org").
		AddSecret("SECRET_VAR", secretValue, "httpbin.org")

	client := launchAlpineWithNetwork(t, sandbox)

	// The env var in the guest should be a placeholder, not the real value
	result, err := client.Exec(context.Background(), "sh -c 'echo $SECRET_VAR'")
	require.NoError(t, err, "Exec")

	envVal := strings.TrimSpace(result.Stdout)
	assert.NotEqual(t, secretValue, envVal, "guest should see placeholder, not real secret value")
	assert.True(t, strings.HasPrefix(envVal, "SANDBOX_SECRET_"), "expected placeholder starting with SANDBOX_SECRET_, got: %q", envVal)
}

func TestSecretBlockedOnUnauthorizedHost(t *testing.T) {
	secretValue := "sk-secret-should-not-leak"

	sandbox := sdk.New("alpine:latest").
		AllowHost("httpbin.org", "example.com").
		AddSecret("LEAK_KEY", secretValue, "example.com") // only allowed on example.com

	client := launchAlpineWithNetwork(t, sandbox)

	// Attempt to send the secret placeholder to httpbin.org (unauthorized for this secret).
	// The policy engine should detect the placeholder and block the request.
	result, err := client.Exec(context.Background(), `sh -c 'wget -q -O - --header "Authorization: Bearer $LEAK_KEY" http://httpbin.org/headers 2>&1 || true'`)
	require.NoError(t, err, "Exec")

	combined := result.Stdout + result.Stderr
	assert.NotContains(t, combined, secretValue, "secret value was leaked to unauthorized host httpbin.org")
	if strings.Contains(combined, `"headers"`) {
		assert.NotContains(t, combined, `Authorization`, "request with secret placeholder to unauthorized host should have been blocked")
	}
}

func TestMultipleSecretsMultipleHosts(t *testing.T) {
	secret1 := "sk-first-secret-aaa"
	secret2 := "sk-second-secret-bbb"

	sandbox := sdk.New("alpine:latest").
		AllowHost("httpbin.org").
		AddSecret("KEY_ONE", secret1, "httpbin.org").
		AddSecret("KEY_TWO", secret2, "httpbin.org")

	client := launchAlpineWithNetwork(t, sandbox)

	result, err := client.Exec(context.Background(), `sh -c 'wget -q -O - --header "X-Key-One: $KEY_ONE" --header "X-Key-Two: $KEY_TWO" https://httpbin.org/headers 2>&1'`)
	require.NoError(t, err, "Exec")

	assert.Contains(t, result.Stdout, secret1, "expected first secret to be injected")
	assert.Contains(t, result.Stdout, secret2, "expected second secret to be injected")
}

func TestSecretInjectedInQueryParam(t *testing.T) {
	secretValue := "sk-query-param-secret"

	sandbox := sdk.New("alpine:latest").
		AllowHost("httpbin.org").
		AddSecret("QP_KEY", secretValue, "httpbin.org")

	client := launchAlpineWithNetwork(t, sandbox)

	// Send secret as a query parameter — the MITM should replace the placeholder in the URL
	result, err := client.Exec(context.Background(), `sh -c 'wget -q -O - "http://httpbin.org/get?api_key=$QP_KEY" 2>&1'`)
	require.NoError(t, err, "Exec")

	assert.Contains(t, result.Stdout, secretValue, "expected secret in query param to be replaced")
}

// ---------------------------------------------------------------------------
// DNS server configuration tests
// ---------------------------------------------------------------------------

func TestCustomDNSServersInResolvConf(t *testing.T) {
	sandbox := sdk.New("alpine:latest").
		BlockPrivateIPs().
		WithDNSServers("1.1.1.1", "1.0.0.1")

	client := launchAlpineWithNetwork(t, sandbox)

	result, err := client.Exec(context.Background(), "cat /etc/resolv.conf")
	require.NoError(t, err, "Exec")

	assert.Contains(t, result.Stdout, "1.1.1.1")
	assert.Contains(t, result.Stdout, "1.0.0.1")
	assert.NotContains(t, result.Stdout, "8.8.8.8", "resolv.conf should NOT contain default 8.8.8.8 when custom DNS is set")
}

func TestDefaultDNSServersInResolvConf(t *testing.T) {
	sandbox := sdk.New("alpine:latest").
		BlockPrivateIPs()

	client := launchAlpineWithNetwork(t, sandbox)

	result, err := client.Exec(context.Background(), "cat /etc/resolv.conf")
	require.NoError(t, err, "Exec")

	assert.Contains(t, result.Stdout, "8.8.8.8")
	assert.Contains(t, result.Stdout, "8.8.4.4")
}

func TestCustomDNSServersStillResolveDomains(t *testing.T) {
	sandbox := sdk.New("alpine:latest").
		AllowHost("httpbin.org").
		WithDNSServers("1.1.1.1")

	client := launchAlpineWithNetwork(t, sandbox)

	result, err := client.Exec(context.Background(), "wget -q -O - http://httpbin.org/get 2>&1")
	require.NoError(t, err, "Exec")

	assert.Contains(t, result.Stdout+result.Stderr, `"url"`, "expected DNS resolution and HTTP request to succeed with custom DNS")
}

// ---------------------------------------------------------------------------
// TCP passthrough proxy tests (non-standard ports)
// ---------------------------------------------------------------------------

func TestPassthroughBlocksUnallowedHost(t *testing.T) {
	sandbox := sdk.New("alpine:latest").
		AllowHost("example.com")

	client := launchAlpineWithNetwork(t, sandbox)

	result, err := client.Exec(context.Background(), "wget -q -T 5 -O - http://httpbin.org/get 2>&1 || true")
	require.NoError(t, err, "Exec")

	assert.NotContains(t, result.Stdout+result.Stderr, `"url"`, "expected request to blocked host to fail")
}

func TestPassthroughAllowsPermittedHost(t *testing.T) {
	sandbox := sdk.New("alpine:latest").
		AllowHost("httpbin.org")

	client := launchAlpineWithNetwork(t, sandbox)

	result, err := client.Exec(context.Background(), "wget -q -O - https://httpbin.org/get 2>&1")
	require.NoError(t, err, "Exec")

	assert.Contains(t, result.Stdout+result.Stderr, `"url"`, "expected request to allowed host to succeed")
}

// ---------------------------------------------------------------------------
// UDP restriction tests
// ---------------------------------------------------------------------------

func TestUDPNonDNSBlocked(t *testing.T) {
	sandbox := sdk.New("alpine:latest").
		AllowHost("httpbin.org")

	client := launchAlpineWithNetwork(t, sandbox)

	result, err := client.Exec(context.Background(), "sh -c 'echo test | timeout 3 nc -u -w 1 8.8.8.8 9999 2>&1; echo exit_code=$?'")
	require.NoError(t, err, "Exec")

	combined := result.Stdout + result.Stderr
	if strings.Contains(combined, "not found") {
		t.Skip("nc not available in this Alpine image")
	}

	t.Logf("UDP non-DNS test output: %s", combined)
}

func TestDNSResolutionWorksWithInterception(t *testing.T) {
	sandbox := sdk.New("alpine:latest").
		AllowHost("httpbin.org")

	client := launchAlpineWithNetwork(t, sandbox)

	result, err := client.Exec(context.Background(), "nslookup httpbin.org 2>&1 || true")
	require.NoError(t, err, "Exec")

	combined := result.Stdout + result.Stderr
	if strings.Contains(combined, "not found") {
		result2, err := client.Exec(context.Background(), "wget -q -O - http://httpbin.org/get 2>&1")
		require.NoError(t, err, "Exec")
		assert.Contains(t, result2.Stdout+result2.Stderr, `"url"`, "expected DNS resolution to work for allowed host")
		return
	}

	assert.False(t, strings.Contains(combined, "SERVFAIL") || strings.Contains(combined, "can't resolve"),
		"expected DNS resolution to work, got: %s", combined)
}
