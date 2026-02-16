//go:build acceptance

package acceptance

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLIRunHelpShowsDNSServersFlag(t *testing.T) {
	stdout, _, exitCode := runCLI(t, "run", "--help")
	require.Equal(t, 0, exitCode)
	assert.Contains(t, stdout, "--dns-servers")
}

func TestCLIRunHelpShowsNetworkMTUFlag(t *testing.T) {
	stdout, _, exitCode := runCLI(t, "run", "--help")
	require.Equal(t, 0, exitCode)
	assert.Contains(t, stdout, "--mtu")
}

func TestCLIDNSServersResolvConf(t *testing.T) {
	stdout, _, exitCode := runCLIWithTimeout(t, 2*time.Minute,
		"run", "--image", "alpine:latest",
		"--dns-servers", "1.1.1.1,1.0.0.1",
		"cat", "/etc/resolv.conf",
	)
	require.Equal(t, 0, exitCode)
	assert.Contains(t, stdout, "1.1.1.1")
	assert.Contains(t, stdout, "1.0.0.1")
}

func TestCLIDNSServersDefaultResolvConf(t *testing.T) {
	stdout, _, exitCode := runCLIWithTimeout(t, 2*time.Minute,
		"run", "--image", "alpine:latest",
		"cat", "/etc/resolv.conf",
	)
	require.Equal(t, 0, exitCode)
	assert.Contains(t, stdout, "8.8.8.8")
	assert.Contains(t, stdout, "8.8.4.4")
}

func TestCLIAllowedHostHTTP(t *testing.T) {
	stdout, _, exitCode := runCLIWithTimeout(t, 2*time.Minute,
		"run", "--image", "alpine:latest",
		"--allow-host", "example.com",
		"--", "sh", "-c", "wget -q -O - http://example.com/ 2>&1",
	)
	require.Equalf(t, 0, exitCode, "stdout: %s", stdout)
	assert.Contains(t, stdout, "Example Domain")
}

func TestCLIBlockedHostHTTP(t *testing.T) {
	stdout, stderr, _ := runCLIWithTimeout(t, 2*time.Minute,
		"run", "--image", "alpine:latest",
		"--allow-host", "httpbin.org",
		"--", "sh", "-c", "wget -q -T 5 -O - http://example.com/ 2>&1 || echo BLOCKED",
	)
	combined := stdout + stderr
	assert.NotContains(t, combined, "Example Domain", "expected request to example.com to be blocked")
}

func TestCLIPassthroughAllowed(t *testing.T) {
	stdout, _, exitCode := runCLIWithTimeout(t, 2*time.Minute,
		"run", "--image", "alpine:latest",
		"--allow-host", "httpbin.org",
		"--", "sh", "-c", "wget -q -O - https://httpbin.org/get 2>&1",
	)
	require.Equalf(t, 0, exitCode, "stdout: %s", stdout)
	assert.Contains(t, stdout, `"url"`)
}

func TestCLIPassthroughBlocked(t *testing.T) {
	stdout, stderr, _ := runCLIWithTimeout(t, 2*time.Minute,
		"run", "--image", "alpine:latest",
		"--allow-host", "example.com",
		"--", "sh", "-c", "wget -q -T 5 -O - https://httpbin.org/get 2>&1 || echo BLOCKED",
	)
	combined := stdout + stderr
	assert.NotContains(t, combined, `"url"`, "expected request to httpbin.org to be blocked")
}
