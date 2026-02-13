//go:build acceptance

package acceptance

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVFSInterceptionBlockCreate(t *testing.T) {
	t.Parallel()

	client := launchWithBuilder(t,
		sdk.New("alpine:latest").WithVFSInterception(&sdk.VFSInterceptionConfig{
			Rules: []sdk.VFSHookRule{
				{
					Phase:  "before",
					Ops:    []string{"create"},
					Path:   "/workspace/blocked.txt",
					Action: "block",
				},
			},
		}),
	)

	err := client.WriteFile(context.Background(), "/workspace/blocked.txt", []byte("blocked"))
	require.Error(t, err, "blocked path should fail")

	err = client.WriteFile(context.Background(), "/workspace/ok.txt", []byte("ok"))
	require.NoError(t, err, "unmatched path should succeed")
}

func TestVFSInterceptionMutateWrite(t *testing.T) {
	t.Parallel()

	client := launchWithBuilder(t,
		sdk.New("alpine:latest").WithVFSInterception(&sdk.VFSInterceptionConfig{
			Rules: []sdk.VFSHookRule{
				{
					Phase:  "before",
					Ops:    []string{"write"},
					Path:   "/workspace/mutate.txt",
					Action: "mutate_write",
					Data:   "mutated-by-hook",
				},
			},
		}),
	)

	err := client.WriteFile(context.Background(), "/workspace/mutate.txt", []byte("original"))
	require.NoError(t, err, "WriteFile")

	got, err := client.ReadFile(context.Background(), "/workspace/mutate.txt")
	require.NoError(t, err, "ReadFile")
	assert.Equal(t, []byte("mutated-by-hook"), got)
}

func TestVFSInterceptionExecAfterSuppressesRecursion(t *testing.T) {
	t.Parallel()

	command := "echo 1 >> /tmp/hook_runs; if [ ! -f /workspace/hook.log ]; then echo hook > /workspace/hook.log; fi"

	client := launchWithBuilder(t,
		sdk.New("alpine:latest").WithVFSInterception(&sdk.VFSInterceptionConfig{
			MaxExecDepth: 1,
			Rules: []sdk.VFSHookRule{
				{
					Phase:     "after",
					Ops:       []string{"write"},
					Path:      "/workspace/*",
					Action:    "exec_after",
					Command:   command,
					TimeoutMS: 2000,
				},
			},
		}),
	)

	_, err := client.Exec(context.Background(), "rm -f /tmp/hook_runs /workspace/hook.log /workspace/trigger.txt")
	require.NoError(t, err, "cleanup")

	err = client.WriteFile(context.Background(), "/workspace/trigger.txt", []byte("trigger"))
	require.NoError(t, err, "WriteFile trigger")

	runs := waitForHookRuns(t, client, 1, 5*time.Second)
	assert.Equal(t, 1, runs, "hook exec should run once")

	// Give queued tasks a brief chance to appear; with suppression this stays at 1.
	time.Sleep(300 * time.Millisecond)
	finalRuns := currentHookRuns(t, client)
	assert.Equal(t, 1, finalRuns, "side-effect recursion should be suppressed")

	hookLog, err := client.ReadFile(context.Background(), "/workspace/hook.log")
	require.NoError(t, err, "hook log should be created")
	assert.Contains(t, string(hookLog), "hook")
}

func TestVFSInterceptionAfterCallbackSuppressesRecursion(t *testing.T) {
	t.Parallel()

	var hookRuns atomic.Int32

	client := launchWithBuilder(t,
		sdk.New("alpine:latest").WithVFSInterception(&sdk.VFSInterceptionConfig{
			MaxExecDepth: 1,
			Rules: []sdk.VFSHookRule{
				{
					Name:      "callback-after-write",
					Phase:     "after",
					Ops:       []string{"write"},
					Path:      "/workspace/*",
					TimeoutMS: 2000,
					Hook: func(ctx context.Context, hookClient *sdk.Client) error {
						hookRuns.Add(1)
						_, err := hookClient.Exec(ctx, "echo callback > /workspace/callback.log")
						return err
					},
				},
			},
		}),
	)

	_, err := client.Exec(context.Background(), "rm -f /workspace/callback.log /workspace/trigger.txt")
	require.NoError(t, err, "cleanup")

	err = client.WriteFile(context.Background(), "/workspace/trigger.txt", []byte("trigger"))
	require.NoError(t, err, "write trigger")

	require.Eventually(t, func() bool {
		return hookRuns.Load() >= 1
	}, 5*time.Second, 100*time.Millisecond, "callback should run")

	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, int32(1), hookRuns.Load(), "recursive callback events should be suppressed")

	data, err := client.ReadFile(context.Background(), "/workspace/callback.log")
	require.NoError(t, err, "callback log should exist")
	assert.Contains(t, string(data), "callback")
}

func waitForHookRuns(t *testing.T, client *sdk.Client, min int, timeout time.Duration) int {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		n := currentHookRuns(t, client)
		if n >= min {
			return n
		}
		if time.Now().After(deadline) {
			require.Failf(t, "wait for hook runs", "timed out waiting for at least %d runs (got %d)", min, n)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func currentHookRuns(t *testing.T, client *sdk.Client) int {
	t.Helper()

	res, err := client.Exec(context.Background(), "if [ -f /tmp/hook_runs ]; then wc -l < /tmp/hook_runs; else echo 0; fi")
	require.NoError(t, err, "read hook runs")

	n, err := strconv.Atoi(strings.TrimSpace(res.Stdout))
	require.NoError(t, err, "parse hook run count from %q", res.Stdout)
	return n
}
