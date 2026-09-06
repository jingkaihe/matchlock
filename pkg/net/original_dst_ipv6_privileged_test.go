//go:build linux

package net

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireNetAdmin skips the test unless it runs as root with an explicit
// opt-in. Installing a NAT rule needs CAP_NET_ADMIN, and running it in a
// container shares the host kernel, so the opt-in keeps a stray `go test` from
// ever touching a real ruleset.
func requireNetAdmin(t *testing.T) {
	t.Helper()

	if os.Geteuid() != 0 {
		t.Skip("requires root (CAP_NET_ADMIN) to install a NAT rule")
	}
	if os.Getenv("MATCHLOCK_PRIVILEGED_TESTS") != "1" {
		t.Skip("set MATCHLOCK_PRIVILEGED_TESTS=1 to run the privileged original-destination test")
	}
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("nft is not installed")
	}
}

// freeLoopbackPort reserves and releases a loopback port so the DNAT rule can
// name a destination nothing else is listening on.
func freeLoopbackPort(t *testing.T, network, addr string) int {
	t.Helper()

	ln, err := net.Listen(network, addr)
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return port
}

// installOutputDNAT installs a single nat/output DNAT rule in its own table
// (namespace-local: only traffic originating in this network namespace is
// affected) and returns the cleanup. Locally generated traffic to a loopback
// address traverses the output nat hook, which gives the accepted socket a real
// conntrack record and therefore a real original destination. The rule matches
// on the destination port alone; the probe port is reserved and unused
// otherwise, so it cannot catch unrelated traffic.
func installOutputDNAT(t *testing.T, family, tableName string, probePort int, backendIP string, backendPort int) func() {
	t.Helper()

	table := fmt.Sprintf("table %s %s", family, tableName)
	chainName := tableName + "_output"
	chain := fmt.Sprintf("chain %s %s %s", family, tableName, chainName)
	// net.JoinHostPort brackets an IPv6 literal and leaves an IPv4 one bare,
	// which is exactly nft's requirement for a dnat target.
	target := net.JoinHostPort(backendIP, strconv.Itoa(backendPort))
	rule := fmt.Sprintf(
		"add rule %s %s %s tcp dport %d dnat to %s",
		family, tableName, chainName, probePort, target,
	)

	// nft treats a SINGLE argument as a script filename and joins MULTIPLE
	// arguments into one command, so the statement is split into argv tokens
	// (the documented `nft add rule ...` invocation). No shell is involved.
	run := func(statement string) (string, error) {
		cmd := exec.Command("nft", strings.Fields(statement)...)
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	if out, err := run("add " + table); err != nil {
		t.Fatalf("nft add %s: %v: %s", table, err, out)
	}
	cleanup := func() {
		if out, err := run("delete " + table); err != nil && !strings.Contains(out, "No such file") {
			t.Logf("nft delete %s: %v: %s", table, err, out)
		}
	}

	if out, err := run("add " + chain + " { type nat hook output priority -100 ; }"); err != nil {
		cleanup()
		t.Fatalf("nft add %s: %v: %s", chain, err, out)
	}
	if out, err := run(rule); err != nil {
		cleanup()
		t.Fatalf("nft %s: %v: %s", rule, err, out)
	}
	return cleanup
}

// TestOriginalDstIPv6ThroughRealDNAT is the real-kernel counterpart of the
// injected-getter tests: an ip6 nat/output DNAT rule redirects a loopback
// connection to a second listener, and the accepted socket must report the
// PRE-DNAT IPv6 destination through IPPROTO_IPV6/IP6T_SO_ORIGINAL_DST. That is
// exactly what the transparent proxy does for an intercepted IPv6 flow.
//
// Run it as root with MATCHLOCK_PRIVILEGED_TESTS=1, e.g.
//
//	CGO_ENABLED=0 go test -c -o /tmp/net.test ./pkg/net/
//	docker run --rm --privileged -v /tmp/net.test:/net.test:ro \
//	  -e MATCHLOCK_PRIVILEGED_TESTS=1 alpine:latest \
//	  sh -c 'apk add --no-cache nftables >/dev/null && /net.test -test.run TestOriginalDstIPv6ThroughRealDNAT -test.v'
//
// The rules live in this process's network namespace and are removed by the
// cleanup, so no host ruleset is touched.
func TestOriginalDstIPv6ThroughRealDNAT(t *testing.T) {
	requireNetAdmin(t)

	backend, err := net.Listen("tcp6", "[::1]:0")
	require.NoError(t, err)
	defer backend.Close()
	backendPort := backend.Addr().(*net.TCPAddr).Port

	probePort := freeLoopbackPort(t, "tcp6", "[::1]:0")

	accepted := make(chan *net.TCPConn, 1)
	go func() {
		conn, err := backend.Accept()
		if err != nil {
			return
		}
		accepted <- conn.(*net.TCPConn)
	}()

	cleanup := installOutputDNAT(t, "ip6", "matchlock_origdst_probe6", probePort, "::1", backendPort)
	defer cleanup()

	client, err := net.DialTimeout("tcp6", net.JoinHostPort("::1", strconv.Itoa(probePort)), 3*time.Second)
	require.NoError(t, err)
	defer client.Close()

	var conn *net.TCPConn
	select {
	case conn = <-accepted:
	case <-time.After(3 * time.Second):
		require.Fail(t, "the DNAT rule did not redirect the connection to the backend listener")
	}
	defer conn.Close()

	got, err := getOriginalDst(conn)
	require.NoError(t, err, "IP6T_SO_ORIGINAL_DST must resolve the pre-DNAT destination")
	require.NotNil(t, got)
	assert.Equal(t, "::1", got.IP.String())
	assert.Equal(t, probePort, got.Port, "the original port must survive the DNAT")
	assert.NotEqual(t, backendPort, got.Port, "the redirected port must not be reported")
}

// TestOriginalDstIPv4ThroughRealDNAT is the IPv4 control for the test above: the
// same redirection through an ip nat/output rule must keep returning the
// pre-DNAT destination via SOL_IP/SO_ORIGINAL_DST.
func TestOriginalDstIPv4ThroughRealDNAT(t *testing.T) {
	requireNetAdmin(t)

	backend, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer backend.Close()
	backendPort := backend.Addr().(*net.TCPAddr).Port

	probePort := freeLoopbackPort(t, "tcp4", "127.0.0.1:0")

	accepted := make(chan *net.TCPConn, 1)
	go func() {
		conn, err := backend.Accept()
		if err != nil {
			return
		}
		accepted <- conn.(*net.TCPConn)
	}()

	cleanup := installOutputDNAT(t, "ip", "matchlock_origdst_probe4", probePort, "127.0.0.1", backendPort)
	defer cleanup()

	client, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(probePort)), 3*time.Second)
	require.NoError(t, err)
	defer client.Close()

	var conn *net.TCPConn
	select {
	case conn = <-accepted:
	case <-time.After(3 * time.Second):
		require.Fail(t, "the DNAT rule did not redirect the connection to the backend listener")
	}
	defer conn.Close()

	got, err := getOriginalDst(conn)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "127.0.0.1", got.IP.String())
	assert.Equal(t, probePort, got.Port)
	assert.NotEqual(t, backendPort, got.Port)
}
