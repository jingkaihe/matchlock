package sdk

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompileVFSHooks_SplitsLocalCallbacks(t *testing.T) {
	cfg := &VFSInterceptionConfig{
		MaxExecDepth: 2,
		Rules: []VFSHookRule{
			{
				Phase:   "before",
				Ops:     []string{"create"},
				Path:    "/workspace/blocked.txt",
				Action:  "block",
				Command: "ignored",
			},
			{
				Name:  "after-callback",
				Phase: "after",
				Ops:   []string{"write"},
				Path:  "/workspace/*",
				Hook: func(ctx context.Context, client *Client) error {
					return nil
				},
			},
		},
	}

	wire, local, maxDepth, err := compileVFSHooks(cfg)
	require.NoError(t, err)
	require.NotNil(t, wire)
	assert.Equal(t, int32(2), maxDepth)
	assert.True(t, wire.EmitEvents)
	require.Len(t, wire.Rules, 1)
	assert.Equal(t, "block", wire.Rules[0].Action)
	require.Len(t, local, 1)
	assert.Equal(t, "after-callback", local[0].name)
}

func TestCompileVFSHooks_RejectsBeforeCallback(t *testing.T) {
	_, _, _, err := compileVFSHooks(&VFSInterceptionConfig{
		Rules: []VFSHookRule{
			{
				Name:  "before-callback",
				Phase: "before",
				Hook: func(ctx context.Context, client *Client) error {
					return nil
				},
			},
		},
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "phase=after")
}

func TestClientVFSHook_RecursionSuppressed(t *testing.T) {
	c := &Client{}
	var runs atomic.Int32

	c.setVFSHooks([]compiledVFSHook{
		{
			path: "/workspace/*",
			callback: func(ctx context.Context, client *Client) error {
				runs.Add(1)
				// Re-emit a matching event while inside the callback.
				client.handleVFSFileEvent("write", "/workspace/nested.txt")
				return nil
			},
		},
	}, 1)

	c.handleVFSFileEvent("write", "/workspace/trigger.txt")

	require.Eventually(t, func() bool {
		return runs.Load() == 1
	}, 2*time.Second, 20*time.Millisecond)

	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, int32(1), runs.Load())
}
