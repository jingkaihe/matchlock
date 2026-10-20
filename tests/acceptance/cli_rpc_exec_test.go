//go:build acceptance

package acceptance

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/sdk"
	"github.com/jingkaihe/matchlock/pkg/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCLIRPCExec proves change 2 end to end: the in-VM runner creates its VM
// through the SDK, whose client launches `matchlock rpc` as a subprocess and
// creates the VM over JSON-RPC. The separate `matchlock exec` CLI process must
// then be able to enter that VM.
//
// Before the fix the RPC controller never started the exec relay, so the
// control socket was absent and exec failed with
// "exec socket not found for <vm> (was it started with --rm=false?)" even
// though the VM was running. The test therefore asserts the socket exists and
// completes a real exec through it.
func TestCLIRPCExec(t *testing.T) {
	client, err := sdk.NewClient(matchlockConfig(t))
	require.NoError(t, err, "NewClient")
	t.Cleanup(func() { cleanupRPCExecVM(t, client) })

	// --no-network keeps the test independent of TAP/CAP_NET_ADMIN (this
	// sandbox runs with NoNewPrivs) while still exercising the RPC create path.
	id, err := client.Launch(sdk.New("alpine:latest").
		WithCPUs(acceptanceDefaultCPUs).
		WithNoNetwork())
	require.NoError(t, err, "Launch via RPC")
	require.NotEmpty(t, id, "Launch returned an empty VM id")
	require.Equal(t, id, client.VMID(), "client.VMID after Launch")

	// handleCreate must publish the same exec socket a `run`-created VM gets.
	socketPath := state.NewManager().ExecSocketPath(id)
	require.Eventuallyf(t, func() bool {
		_, statErr := os.Stat(socketPath)
		return statErr == nil
	}, 30*time.Second, 100*time.Millisecond,
		"exec relay socket %s never appeared for RPC-created VM %s", socketPath, id)

	// A separate CLI process must reach the RPC-created VM. Reading PID 1's
	// comm also locks the documented isolation: the exec runs in its own PID
	// namespace, so PID 1 is the session shell rather than the VM init. The
	// trailing `:` is a builtin, so it cannot be exec-replaced and /proc/1
	// reliably refers to the session's shell.
	stdout, stderr, code := runCLIWithTimeout(t, 30*time.Second,
		"exec", id, "--", "sh", "-c", "echo rpc-exec-ok; cat /proc/1/comm; :")
	require.Equalf(t, 0, code, "matchlock exec into RPC-created VM failed\nstdout: %s\nstderr: %s", stdout, stderr)
	assert.Contains(t, stdout, "rpc-exec-ok", "exec stdout should carry the command output")
	assert.Contains(t, strings.Fields(stdout), "sh",
		"exec should run in a fresh PID namespace (session PID 1 is the shell): %s", stdout)

	// The running VM must stay visible to the CLI so the exec status check
	// (state row = running) keeps working.
	listOut, listStderr, listCode := runCLI(t, "list")
	require.Equalf(t, 0, listCode, "matchlock list failed: %s", listStderr)
	assert.Contains(t, listOut, id, "running RPC VM should be listed")
}

// cleanupRPCExecVM closes the RPC client and removes the VM, then requires that
// `matchlock list` no longer reports it.
//
// client.Remove() shells out to `matchlock rm`; under a NoNewPrivs sandbox rm
// cannot reconcile the nftables rule ("netlink receive: operation not
// permitted") and leaves the stopped row behind. state.Manager.Remove is the
// documented bypass that still deletes the row and the VM directory.
func cleanupRPCExecVM(t *testing.T, client *sdk.Client) {
	t.Helper()

	id := client.VMID()
	_ = client.Close(0)
	if id == "" {
		return
	}
	_ = client.Remove()

	stateMgr := state.NewManager()
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, _, _ = runCLI(t, "rm", id)
		if !rpcExecVMListed(t, id) {
			return
		}
		if err := stateMgr.Remove(id); err != nil {
			t.Logf("state.Remove(%s): %v", id, err)
		} else if !rpcExecVMListed(t, id) {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("VM %s is still listed by `matchlock list` after removal", id)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func rpcExecVMListed(t *testing.T, vmID string) bool {
	t.Helper()
	listOut, _, _ := runCLI(t, "list")
	return strings.Contains(listOut, vmID)
}
