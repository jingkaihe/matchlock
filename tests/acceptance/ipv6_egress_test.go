//go:build acceptance

package acceptance

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/sdk"
	"github.com/stretchr/testify/require"
)

// TestSDKIPv6EgressState inventories the real guest's IPv6 state to gather the
// facts needed to answer: what happens to guest IPv6 under the interception
// policy? It is an evidence probe: it runs in a real guest and logs the kernel's
// /proc IPv6 tables plus the effective disable_ipv6 sysctl, and it asserts the
// contract that inventory documents.
//
// CONTRACT CHANGE: this probe was originally written when the interception was
// IPv4-family only and ALL guest IPv6 was dropped, and it asserted only that the
// probe ran. With the IPv6 interception path (US-001..US-008) an intercepted
// sandbox gives the guest a per-VM unique-local address on eth0 and a default
// route through the host gateway, and guest IPv6 is REDIRECTED into the same
// proxy (TCP 80/443, catch-all TCP, DNS 53) with the residual traffic dropped by
// the ip6 table - "redirected, not dropped". The assertions below therefore
// require that new state: a global fd00::/8 address on eth0 AND a ::/0 default
// route. The raw inventory is still logged for evidence, and the refusal/drop
// half of the contract is asserted by
// TestSDKIPv6InterceptionAllowedRefusedAndNoLeak.
//
// Darwin: the darwin backend is IPv4-only by design - the dual-stack proxy and
// the guest ULA below are Linux-backend features - so the test skips there with
// the same guard and reason as its sibling
// TestSDKIPv4InterceptionParityWithDualStackProxy instead of failing on an
// assertion the platform cannot satisfy. The darwin surface still COMPILES the
// test (see scripts/darwin-crosscompile-gate.sh and
// tests/acceptance/darwin_ipv6_guard_test.go, which pins the guard).
func TestSDKIPv6EgressState(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("IPv6 interception (and therefore the dual-stack proxy) is a Linux-backend feature")
	}

	client := launchAlpineWithNetwork(t, sdk.New("alpine:latest").
		AllowHost("httpbin.org").
		WithCPUs(0.5))

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	const cmd = "sh -c '" +
		"echo ==if_inet6==; cat /proc/net/if_inet6 2>&1;" +
		"echo ==ipv6_route==; cat /proc/net/ipv6_route 2>&1;" +
		"echo ==v6_sysctl==; cat /proc/sys/net/ipv6/conf/all/disable_ipv6 2>&1; echo; cat /proc/sys/net/ipv6/conf/default/disable_ipv6 2>&1;" +
		"echo ==ula_addr_present==; if ip -6 addr show dev eth0 2>/dev/null | grep -q \"inet6 fd00:\"; then echo YES; else echo NO; fi;" +
		"echo ==default_route_present==; if ip -6 route show default 2>/dev/null | grep -q \"^default\"; then echo YES; else echo NO; fi'"

	res, err := client.Exec(ctx, cmd)
	require.NoError(t, err, "probe IPv6 state must run")
	require.Equal(t, 0, res.ExitCode, "probe must exit 0 (stdout=%q stderr=%q)", res.Stdout, res.Stderr)

	t.Logf("guest IPv6 inventory (exit=%d):\n<stdout>\n%s\n</stdout>\n<stderr>\n%s\n</stderr>",
		res.ExitCode, res.Stdout, res.Stderr)
	require.NotEmpty(t, res.Stdout, "probe must produce output")

	// The contract: IPv6 is redirected rather than dropped, which requires the
	// guest to own an on-link unique-local address and a default route toward the
	// host gateway the ip6 table redirects from. Discovery is done in the guest
	// (the raw inventory above is what makes a failure diagnosable).
	require.Contains(t, res.Stdout, "==ula_addr_present==\nYES",
		"an intercepted sandbox must configure a per-VM unique-local IPv6 address on the guest link (stdout=%q)", res.Stdout)
	require.Contains(t, res.Stdout, "==default_route_present==\nYES",
		"an intercepted sandbox must give the guest an IPv6 default route toward the host gateway (stdout=%q)", res.Stdout)
}
