//go:build acceptance

package acceptance

import (
	"strings"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/sdk"
)

// launchAlpineWithNetwork creates a sandbox with network policy configured.
func launchAlpineWithNetwork(t *testing.T, builder *sdk.SandboxBuilder) *sdk.Client {
	t.Helper()
	client, err := sdk.NewClient(matchlockConfig(t))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, err = client.Launch(builder)
	if err != nil {
		client.Close(0)
		t.Fatalf("Launch: %v", err)
	}

	t.Cleanup(func() {
		client.Close(0)
		client.Remove()
	})
	return client
}

// ---------------------------------------------------------------------------
// Allowlist tests
// ---------------------------------------------------------------------------

func TestAllowlistBlocksHTTP(t *testing.T) {
	sandbox := sdk.New("alpine:latest").
		AllowHost("example.com")

	client := launchAlpineWithNetwork(t, sandbox)

	result, err := client.Exec("wget -q -O - http://httpbin.org/get 2>&1 || true")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	combined := result.Stdout + result.Stderr
	if strings.Contains(combined, `"url"`) {
		t.Errorf("expected request to httpbin.org to be blocked, but it succeeded: %s", combined)
	}
}

func TestAllowlistPermitsHTTP(t *testing.T) {
	sandbox := sdk.New("alpine:latest").
		AllowHost("httpbin.org")

	client := launchAlpineWithNetwork(t, sandbox)

	result, err := client.Exec("wget -q -O - http://httpbin.org/get 2>&1")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	combined := result.Stdout + result.Stderr
	if !strings.Contains(combined, `"url"`) {
		t.Errorf("expected request to httpbin.org to succeed, got: %s", combined)
	}
}

func TestAllowlistBlocksHTTPS(t *testing.T) {
	sandbox := sdk.New("alpine:latest").
		AllowHost("example.com")

	client := launchAlpineWithNetwork(t, sandbox)

	result, err := client.Exec("wget -q -O - https://httpbin.org/get 2>&1 || true")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	combined := result.Stdout + result.Stderr
	if strings.Contains(combined, `"url"`) {
		t.Errorf("expected HTTPS request to httpbin.org to be blocked, but it succeeded: %s", combined)
	}
}

func TestAllowlistPermitsHTTPS(t *testing.T) {
	sandbox := sdk.New("alpine:latest").
		AllowHost("httpbin.org")

	client := launchAlpineWithNetwork(t, sandbox)

	result, err := client.Exec("wget -q -O - https://httpbin.org/get 2>&1")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	combined := result.Stdout + result.Stderr
	if !strings.Contains(combined, `"url"`) {
		t.Errorf("expected HTTPS request to httpbin.org to succeed, got: %s", combined)
	}
}

func TestAllowlistGlobPattern(t *testing.T) {
	sandbox := sdk.New("alpine:latest").
		AllowHost("*.org")

	client := launchAlpineWithNetwork(t, sandbox)

	result, err := client.Exec("wget -q -O - http://httpbin.org/get 2>&1")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	combined := result.Stdout + result.Stderr
	if !strings.Contains(combined, `"url"`) {
		t.Errorf("expected glob *.org to allow httpbin.org, got: %s", combined)
	}
}

func TestAllowlistMultipleHosts(t *testing.T) {
	sandbox := sdk.New("alpine:latest").
		AllowHost("httpbin.org", "example.com")

	client := launchAlpineWithNetwork(t, sandbox)

	result, err := client.Exec("wget -q -O - http://httpbin.org/get 2>&1")
	if err != nil {
		t.Fatalf("Exec httpbin.org: %v", err)
	}
	if !strings.Contains(result.Stdout+result.Stderr, `"url"`) {
		t.Errorf("expected httpbin.org to be allowed")
	}

	result2, err := client.Exec("wget -q -O - http://example.com/ 2>&1")
	if err != nil {
		t.Fatalf("Exec example.com: %v", err)
	}
	if !strings.Contains(result2.Stdout+result2.Stderr, "Example Domain") {
		t.Errorf("expected example.com to be allowed, got: %s", result2.Stdout+result2.Stderr)
	}
}

func TestNoAllowlistPermitsAll(t *testing.T) {
	// No AllowHost → all hosts are allowed (empty allowlist = permit all)
	sandbox := sdk.New("alpine:latest").
		BlockPrivateIPs() // enable interception without restricting hosts

	client := launchAlpineWithNetwork(t, sandbox)

	result, err := client.Exec("wget -q -O - http://httpbin.org/get 2>&1")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if !strings.Contains(result.Stdout+result.Stderr, `"url"`) {
		t.Errorf("expected open allowlist to permit httpbin.org, got: %s", result.Stdout+result.Stderr)
	}
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
	result, err := client.Exec(`sh -c 'wget -q -O - --header "Authorization: Bearer $MY_API_KEY" https://httpbin.org/headers 2>&1'`)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if !strings.Contains(result.Stdout, secretValue) {
		t.Errorf("expected secret value to be injected in HTTPS header, got: %s", result.Stdout)
	}
}

func TestSecretInjectedInHTTPHeader(t *testing.T) {
	secretValue := "sk-test-http-secret-67890"

	sandbox := sdk.New("alpine:latest").
		AllowHost("httpbin.org").
		AddSecret("HTTP_KEY", secretValue, "httpbin.org")

	client := launchAlpineWithNetwork(t, sandbox)

	result, err := client.Exec(`sh -c 'wget -q -O - --header "X-Api-Key: $HTTP_KEY" http://httpbin.org/headers 2>&1'`)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if !strings.Contains(result.Stdout, secretValue) {
		t.Errorf("expected secret value to be injected in HTTP header, got: %s", result.Stdout)
	}
}

func TestSecretPlaceholderNotExposedInGuest(t *testing.T) {
	secretValue := "sk-real-secret-never-seen"

	sandbox := sdk.New("alpine:latest").
		AllowHost("httpbin.org").
		AddSecret("SECRET_VAR", secretValue, "httpbin.org")

	client := launchAlpineWithNetwork(t, sandbox)

	// The env var in the guest should be a placeholder, not the real value
	result, err := client.Exec("sh -c 'echo $SECRET_VAR'")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	envVal := strings.TrimSpace(result.Stdout)
	if envVal == secretValue {
		t.Errorf("guest should see placeholder, not real secret value")
	}
	if !strings.HasPrefix(envVal, "SANDBOX_SECRET_") {
		t.Errorf("expected placeholder starting with SANDBOX_SECRET_, got: %q", envVal)
	}
}

func TestSecretBlockedOnUnauthorizedHost(t *testing.T) {
	secretValue := "sk-secret-should-not-leak"

	sandbox := sdk.New("alpine:latest").
		AllowHost("httpbin.org", "example.com").
		AddSecret("LEAK_KEY", secretValue, "example.com") // only allowed on example.com

	client := launchAlpineWithNetwork(t, sandbox)

	// Attempt to send the secret placeholder to httpbin.org (unauthorized for this secret).
	// The policy engine should detect the placeholder and block the request.
	result, err := client.Exec(`sh -c 'wget -q -O - --header "Authorization: Bearer $LEAK_KEY" http://httpbin.org/headers 2>&1 || true'`)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	combined := result.Stdout + result.Stderr
	if strings.Contains(combined, secretValue) {
		t.Errorf("secret value was leaked to unauthorized host httpbin.org: %s", combined)
	}
	if strings.Contains(combined, `"headers"`) && strings.Contains(combined, `Authorization`) {
		t.Errorf("request with secret placeholder to unauthorized host should have been blocked: %s", combined)
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

	result, err := client.Exec(`sh -c 'wget -q -O - --header "X-Key-One: $KEY_ONE" --header "X-Key-Two: $KEY_TWO" https://httpbin.org/headers 2>&1'`)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if !strings.Contains(result.Stdout, secret1) {
		t.Errorf("expected first secret to be injected, got: %s", result.Stdout)
	}
	if !strings.Contains(result.Stdout, secret2) {
		t.Errorf("expected second secret to be injected, got: %s", result.Stdout)
	}
}

func TestSecretInjectedInQueryParam(t *testing.T) {
	secretValue := "sk-query-param-secret"

	sandbox := sdk.New("alpine:latest").
		AllowHost("httpbin.org").
		AddSecret("QP_KEY", secretValue, "httpbin.org")

	client := launchAlpineWithNetwork(t, sandbox)

	// Send secret as a query parameter — the MITM should replace the placeholder in the URL
	result, err := client.Exec(`sh -c 'wget -q -O - "http://httpbin.org/get?api_key=$QP_KEY" 2>&1'`)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if !strings.Contains(result.Stdout, secretValue) {
		t.Errorf("expected secret in query param to be replaced, got: %s", result.Stdout)
	}
}

// ---------------------------------------------------------------------------
// DNS server configuration tests
// ---------------------------------------------------------------------------

func TestCustomDNSServersInResolvConf(t *testing.T) {
	sandbox := sdk.New("alpine:latest").
		BlockPrivateIPs().
		WithDNSServers("1.1.1.1", "1.0.0.1")

	client := launchAlpineWithNetwork(t, sandbox)

	result, err := client.Exec("cat /etc/resolv.conf")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if !strings.Contains(result.Stdout, "1.1.1.1") {
		t.Errorf("resolv.conf should contain 1.1.1.1, got: %s", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "1.0.0.1") {
		t.Errorf("resolv.conf should contain 1.0.0.1, got: %s", result.Stdout)
	}
	if strings.Contains(result.Stdout, "8.8.8.8") {
		t.Errorf("resolv.conf should NOT contain default 8.8.8.8 when custom DNS is set, got: %s", result.Stdout)
	}
}

func TestDefaultDNSServersInResolvConf(t *testing.T) {
	sandbox := sdk.New("alpine:latest").
		BlockPrivateIPs()

	client := launchAlpineWithNetwork(t, sandbox)

	result, err := client.Exec("cat /etc/resolv.conf")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if !strings.Contains(result.Stdout, "8.8.8.8") {
		t.Errorf("default resolv.conf should contain 8.8.8.8, got: %s", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "8.8.4.4") {
		t.Errorf("default resolv.conf should contain 8.8.4.4, got: %s", result.Stdout)
	}
}

func TestCustomDNSServersStillResolveDomains(t *testing.T) {
	sandbox := sdk.New("alpine:latest").
		AllowHost("httpbin.org").
		WithDNSServers("1.1.1.1")

	client := launchAlpineWithNetwork(t, sandbox)

	result, err := client.Exec("wget -q -O - http://httpbin.org/get 2>&1")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if !strings.Contains(result.Stdout+result.Stderr, `"url"`) {
		t.Errorf("expected DNS resolution and HTTP request to succeed with custom DNS, got: %s", result.Stdout+result.Stderr)
	}
}

// ---------------------------------------------------------------------------
// TCP passthrough proxy tests (non-standard ports)
// ---------------------------------------------------------------------------

func TestPassthroughBlocksUnallowedHost(t *testing.T) {
	sandbox := sdk.New("alpine:latest").
		AllowHost("example.com")

	client := launchAlpineWithNetwork(t, sandbox)

	result, err := client.Exec("wget -q -T 5 -O - http://httpbin.org/get 2>&1 || true")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if strings.Contains(result.Stdout+result.Stderr, `"url"`) {
		t.Errorf("expected request to blocked host to fail, got: %s", result.Stdout+result.Stderr)
	}
}

func TestPassthroughAllowsPermittedHost(t *testing.T) {
	sandbox := sdk.New("alpine:latest").
		AllowHost("httpbin.org")

	client := launchAlpineWithNetwork(t, sandbox)

	result, err := client.Exec("wget -q -O - https://httpbin.org/get 2>&1")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if !strings.Contains(result.Stdout+result.Stderr, `"url"`) {
		t.Errorf("expected request to allowed host to succeed, got: %s", result.Stdout+result.Stderr)
	}
}

// ---------------------------------------------------------------------------
// UDP restriction tests
// ---------------------------------------------------------------------------

func TestUDPNonDNSBlocked(t *testing.T) {
	sandbox := sdk.New("alpine:latest").
		AllowHost("httpbin.org")

	client := launchAlpineWithNetwork(t, sandbox)

	result, err := client.Exec("sh -c 'echo test | timeout 3 nc -u -w 1 8.8.8.8 9999 2>&1; echo exit_code=$?'")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

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

	result, err := client.Exec("nslookup httpbin.org 2>&1 || true")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	combined := result.Stdout + result.Stderr
	if strings.Contains(combined, "not found") {
		result2, err := client.Exec("wget -q -O - http://httpbin.org/get 2>&1")
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if !strings.Contains(result2.Stdout+result2.Stderr, `"url"`) {
			t.Errorf("expected DNS resolution to work for allowed host, got: %s", result2.Stdout+result2.Stderr)
		}
		return
	}

	if strings.Contains(combined, "SERVFAIL") || strings.Contains(combined, "can't resolve") {
		t.Errorf("expected DNS resolution to work, got: %s", combined)
	}
}
