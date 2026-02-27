//go:build acceptance

package acceptance

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLIAllowListAddDeleteRuntime(t *testing.T) {
	bin := matchlockBin(t)

	cmd, runDone, runStderr := startPersistentRunWithArgs(t, bin, "--network-intercept")
	runPID := cmd.Process.Pid

	var vmID string
	t.Cleanup(func() {
		cleanupPersistentRun(t, bin, vmID, runPID, runDone)
	})

	vmID = waitForRunningVMID(t, runPID, runDone, runStderr)
	waitForExecReady(t, vmID, runPID, runDone, runStderr)

	addOut, addErr, addExit := runCLIWithTimeout(t, 45*time.Second,
		"allow-list", "add", vmID, "example.com,httpbin.org",
	)
	require.Equalf(t, 0, addExit, "allow-list add failed: %s", addErr)
	assert.Contains(t, addOut, "Added to "+vmID+": example.com,httpbin.org")
	assert.Contains(t, addOut, "Current allow-list: example.com,httpbin.org")

	allowedOut, allowedErr, allowedExit := runCLIWithTimeout(t, 45*time.Second,
		"exec", vmID, "--", "sh", "-c", "wget -q -T 10 -O - http://example.com/ 2>&1",
	)
	require.Equalf(t, 0, allowedExit, "exec allowed host failed: %s", allowedErr)
	assert.Contains(t, allowedOut+allowedErr, "Example Domain")

	deleteOneOut, deleteOneErr, deleteOneExit := runCLIWithTimeout(t, 45*time.Second,
		"allow-list", "delete", vmID, "httpbin.org",
	)
	require.Equalf(t, 0, deleteOneExit, "allow-list delete failed: %s", deleteOneErr)
	assert.Contains(t, deleteOneOut, "Removed from "+vmID+": httpbin.org")
	assert.Contains(t, deleteOneOut, "Current allow-list: example.com")

	blockedOut, blockedErr, blockedExit := runCLIWithTimeout(t, 45*time.Second,
		"exec", vmID, "--", "sh", "-c", "wget -q -T 10 -O - http://httpbin.org/get 2>&1 || echo BLOCKED",
	)
	require.Equalf(t, 0, blockedExit, "exec blocked host command failed: %s", blockedErr)
	combinedBlocked := blockedOut + blockedErr
	assert.Contains(t, combinedBlocked, "BLOCKED")
	assert.NotContains(t, combinedBlocked, `"url"`)

	deleteAllOut, deleteAllErr, deleteAllExit := runCLIWithTimeout(t, 45*time.Second,
		"allow-list", "delete", vmID, "example.com,httpbin.org",
	)
	require.Equalf(t, 0, deleteAllExit, "allow-list delete-all failed: %s", deleteAllErr)
	assert.Contains(t, deleteAllOut, "Removed from "+vmID+": example.com")
	assert.Contains(t, deleteAllOut, "Current allow-list: (empty: all hosts allowed)")

	openOut, openErr, openExit := runCLIWithTimeout(t, 45*time.Second,
		"exec", vmID, "--", "sh", "-c", "wget -q -T 10 -O - http://httpbin.org/get 2>&1",
	)
	require.Equalf(t, 0, openExit, "exec after clearing allow-list failed: %s", openErr)
	assert.Contains(t, openOut+openErr, `"url"`)
}
