//go:build acceptance

package acceptance

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestCLIHTTPPolicyRejectsHostnameAndDirectIPv4 verifies the HTTP allow-list is
// enforced by the proxy against BOTH a non-allowlisted hostname AND a
// non-allowlisted destination reached by raw IPv4. With only httpbin.org
// allowlisted:
//
//  1. Positive control: httpbin.org over HTTPS returns HTTP 200 (networking works).
//  2. example.com is rejected by the proxy with HTTP 403.
//  3. A destination by raw public IP is also rejected with HTTP 403 (direct-IP
//     bypass). The nftables DNAT catch-all redirects every guest TCP connection
//     to the proxy, so the policy decision is deterministic and does not depend
//     on the destination being reachable.
//
// `wget -S` prints the response status line to stderr; a blocked request shows
// "HTTP/1.1 403", an allowed one "HTTP/1.1 200". We grep the raw status token and
// report the occurrence count (wget may print it once or twice per exchange). A
// blocked case yields a nonzero 403 count; the allowed case a nonzero 200 count.
func TestCLIHTTPPolicyRejectsHostnameAndDirectIPv4(t *testing.T) {
	stdout, stderr, exitCode := runCLIWithTimeout(t, 3*time.Minute,
		"run", "--image", "alpine:latest",
		"--allow-host", "httpbin.org",
		"--",
		"sh", "-c",
		"echo ALLOWED=$(wget -S -T 8 -O - https://httpbin.org/get 2>&1 | grep -c 'HTTP/1.1 200' || echo 0); "+
			"echo BADHOST=$(wget -S -T 8 -O /dev/null http://example.com/ 2>&1 | grep -c 'HTTP/1.1 403' || echo 0); "+
			"echo RAWIP=$(wget -S -T 8 -O /dev/null http://185.199.108.153/ 2>&1 | grep -c 'HTTP/1.1 403' || echo 0)",
	)
	combined := stdout + stderr
	require.Equal(t, 0, exitCode, "guest command must exit 0 (out=%q)", combined)
	// Each marker is "LABEL=<int>". The 200/403 token may appear once or twice
	// (wget -S per-request+response), so assert a nonzero count by matching the
	// digit run after '='. We rely on the count being single-digit (1 or 2); 0
	// would mean the status token was absent => FAIL.
	require.Regexp(t, "ALLOWED=[1-9]", combined, "allowed host must return HTTP 200 (out=%q)", combined)
	require.Regexp(t, "BADHOST=[1-9]", combined, "blocked hostname must return HTTP 403 (out=%q)", combined)
	require.Regexp(t, "RAWIP=[1-9]", combined, "raw-IP destination must return HTTP 403 (out=%q)", combined)
}
