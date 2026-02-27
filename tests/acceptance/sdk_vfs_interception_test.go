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
		sdk.New("alpine:latest").WithWorkspace("/workspace").MountMemory("/workspace").WithVFSInterception(&sdk.VFSInterceptionConfig{
			Rules: []sdk.VFSHookRule{
				{
					Phase:  sdk.VFSHookPhaseBefore,
					Ops:    []sdk.VFSHookOp{sdk.VFSHookOpCreate},
					Path:   "/workspace/blocked.txt",
					Action: sdk.VFSHookActionBlock,
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
		sdk.New("alpine:latest").WithWorkspace("/workspace").MountMemory("/workspace").WithVFSInterception(&sdk.VFSInterceptionConfig{
			Rules: []sdk.VFSHookRule{
				{
					Phase: sdk.VFSHookPhaseBefore,
					Ops:   []sdk.VFSHookOp{sdk.VFSHookOpWrite},
					Path:  "/workspace/mutate.txt",
					MutateHook: func(ctx context.Context, req sdk.VFSMutateRequest) ([]byte, error) {
						return []byte("mutated-by-hook"), nil
					},
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

func TestVFSInterceptionDangerousHookAllowsReentry(t *testing.T) {
	t.Parallel()

	client := launchWithBuilder(t,
		sdk.New("alpine:latest").WithWorkspace("/workspace").MountMemory("/workspace").WithVFSInterception(&sdk.VFSInterceptionConfig{
			Rules: []sdk.VFSHookRule{
				{
					Phase:     sdk.VFSHookPhaseAfter,
					Ops:       []sdk.VFSHookOp{sdk.VFSHookOpWrite},
					Path:      "/workspace/*",
					TimeoutMS: 2000,
					DangerousHook: func(ctx context.Context, hookClient *sdk.Client, _ sdk.VFSHookEvent) error {
						_, err := hookClient.Exec(ctx, "echo 1 >> /tmp/hook_runs; if [ ! -f /workspace/hook.log ]; then echo hook > /workspace/hook.log; fi")
						return err
					},
				},
			},
		}),
	)

	_, err := client.Exec(context.Background(), "rm -f /tmp/hook_runs /workspace/hook.log /workspace/trigger.txt")
	require.NoError(t, err, "cleanup")

	err = client.WriteFile(context.Background(), "/workspace/trigger.txt", []byte("trigger"))
	require.NoError(t, err, "WriteFile trigger")

	runs := waitForHookRuns(t, client, 2, 5*time.Second)
	assert.GreaterOrEqual(t, runs, 2, "dangerous hook should re-enter at least once")

	// The command writes /workspace/hook.log only once, so re-entry should be bounded.
	time.Sleep(300 * time.Millisecond)
	finalRuns := currentHookRuns(t, client)
	assert.GreaterOrEqual(t, finalRuns, 2, "dangerous hook re-entry should be visible")
	assert.LessOrEqual(t, finalRuns, 4, "re-entry should stay bounded by command logic")

	hookLog, err := client.ReadFile(context.Background(), "/workspace/hook.log")
	require.NoError(t, err, "hook log should be created")
	assert.Contains(t, string(hookLog), "hook")
}

func TestVFSInterceptionAfterCallbackSuppressesRecursion(t *testing.T) {
	t.Parallel()

	var hookRuns atomic.Int32
	var clientRef *sdk.Client

	client := launchWithBuilder(t,
		sdk.New("alpine:latest").WithWorkspace("/workspace").MountMemory("/workspace").WithVFSInterception(&sdk.VFSInterceptionConfig{
			Rules: []sdk.VFSHookRule{
				{
					Name:      "callback-after-write",
					Phase:     sdk.VFSHookPhaseAfter,
					Ops:       []sdk.VFSHookOp{sdk.VFSHookOpWrite},
					Path:      "/workspace/*",
					TimeoutMS: 2000,
					Hook: func(ctx context.Context, _ sdk.VFSHookEvent) error {
						hookRuns.Add(1)
						if clientRef == nil {
							return nil
						}
						_, err := clientRef.Exec(ctx, "echo callback > /workspace/callback.log")
						return err
					},
				},
			},
		}),
	)
	clientRef = client

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
