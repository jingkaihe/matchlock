package sdk

import (
	"context"
	"os"
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
				Phase:  VFSHookPhaseBefore,
				Ops:    []VFSHookOp{VFSHookOpCreate},
				Path:   "/workspace/blocked.txt",
				Action: VFSHookActionBlock,
			},
			{
				Name:  "after-callback",
				Phase: VFSHookPhaseAfter,
				Ops:   []VFSHookOp{VFSHookOpWrite},
				Path:  "/workspace/*",
				Hook: func(ctx context.Context, client *Client, event VFSHookEvent) error {
					return nil
				},
			},
		},
	}

	wire, local, localMutate, localAction, maxDepth, err := compileVFSHooks(cfg)
	require.NoError(t, err)
	require.NotNil(t, wire)
	assert.Equal(t, int32(2), maxDepth)
	assert.True(t, wire.EmitEvents)
	require.Len(t, wire.Rules, 1)
	assert.Equal(t, "block", wire.Rules[0].Action)
	require.Len(t, local, 1)
	require.Len(t, localMutate, 0)
	require.Len(t, localAction, 0)
	assert.Equal(t, "after-callback", local[0].name)
}

func TestVFSHookActionConstants(t *testing.T) {
	assert.Equal(t, "allow", string(VFSHookActionAllow))
	assert.Equal(t, "block", string(VFSHookActionBlock))
}

func TestCompileVFSHooks_RejectsBeforeCallback(t *testing.T) {
	_, _, _, _, _, err := compileVFSHooks(&VFSInterceptionConfig{
		Rules: []VFSHookRule{
			{
				Name:  "before-callback",
				Phase: VFSHookPhaseBefore,
				Hook: func(ctx context.Context, client *Client, event VFSHookEvent) error {
					return nil
				},
			},
		},
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "phase=after")
}

func TestCompileVFSHooks_SplitsLocalMutateHooks(t *testing.T) {
	cfg := &VFSInterceptionConfig{
		Rules: []VFSHookRule{
			{
				Name:  "dynamic-mutate",
				Phase: VFSHookPhaseBefore,
				Ops:   []VFSHookOp{VFSHookOpWrite},
				Path:  "/workspace/*",
				MutateHook: func(ctx context.Context, req VFSMutateRequest) ([]byte, error) {
					return []byte("mutated"), nil
				},
			},
			{
				Name:   "wire-rule",
				Phase:  VFSHookPhaseBefore,
				Ops:    []VFSHookOp{VFSHookOpCreate},
				Path:   "/workspace/blocked.txt",
				Action: VFSHookActionBlock,
			},
		},
	}

	wire, localAfter, localMutate, localAction, _, err := compileVFSHooks(cfg)
	require.NoError(t, err)
	require.NotNil(t, wire)
	require.Len(t, localAfter, 0)
	require.Len(t, localMutate, 1)
	require.Len(t, localAction, 0)
	require.Len(t, wire.Rules, 1)
	assert.Equal(t, "wire-rule", wire.Rules[0].Name)
}

func TestCompileVFSHooks_RejectsAfterMutateHook(t *testing.T) {
	_, _, _, _, _, err := compileVFSHooks(&VFSInterceptionConfig{
		Rules: []VFSHookRule{
			{
				Name:  "bad-mutate",
				Phase: VFSHookPhaseAfter,
				MutateHook: func(ctx context.Context, req VFSMutateRequest) ([]byte, error) {
					return []byte("x"), nil
				},
			},
		},
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "phase=before")
}

func TestCompileVFSHooks_RejectsWireMutateWriteAction(t *testing.T) {
	_, _, _, _, _, err := compileVFSHooks(&VFSInterceptionConfig{
		Rules: []VFSHookRule{
			{
				Name:   "wire-mutate",
				Phase:  VFSHookPhaseBefore,
				Action: "mutate_write",
			},
		},
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "requires MutateHook")
}

func TestCompileVFSHooks_RejectsWireExecAfterAction(t *testing.T) {
	_, _, _, _, _, err := compileVFSHooks(&VFSInterceptionConfig{
		Rules: []VFSHookRule{
			{
				Name:   "wire-exec",
				Phase:  VFSHookPhaseAfter,
				Action: "exec_after",
			},
		},
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "requires Hook")
}

func TestCompileVFSHooks_SplitsLocalActionHooks(t *testing.T) {
	cfg := &VFSInterceptionConfig{
		Rules: []VFSHookRule{
			{
				Name:  "dynamic-action",
				Phase: VFSHookPhaseBefore,
				Ops:   []VFSHookOp{VFSHookOpWrite},
				Path:  "/workspace/*",
				ActionHook: func(ctx context.Context, req VFSActionRequest) VFSHookAction {
					return VFSHookActionAllow
				},
			},
			{
				Name:   "wire-rule",
				Phase:  VFSHookPhaseBefore,
				Ops:    []VFSHookOp{VFSHookOpCreate},
				Path:   "/workspace/blocked.txt",
				Action: VFSHookActionBlock,
			},
		},
	}

	wire, localAfter, localMutate, localAction, _, err := compileVFSHooks(cfg)
	require.NoError(t, err)
	require.NotNil(t, wire)
	require.Len(t, localAfter, 0)
	require.Len(t, localMutate, 0)
	require.Len(t, localAction, 1)
	require.Len(t, wire.Rules, 1)
	assert.Equal(t, "wire-rule", wire.Rules[0].Name)
}

func TestCompileVFSHooks_RejectsAfterActionHook(t *testing.T) {
	_, _, _, _, _, err := compileVFSHooks(&VFSInterceptionConfig{
		Rules: []VFSHookRule{
			{
				Name:  "bad-action",
				Phase: VFSHookPhaseAfter,
				ActionHook: func(ctx context.Context, req VFSActionRequest) VFSHookAction {
					return VFSHookActionBlock
				},
			},
		},
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "phase=before")
}

func TestClientApplyLocalWriteMutations(t *testing.T) {
	c := &Client{}
	c.setVFSHooks(nil, []compiledVFSMutateHook{
		{
			path: "/workspace/*",
			callback: func(ctx context.Context, req VFSMutateRequest) ([]byte, error) {
				assert.Equal(t, "/workspace/file.txt", req.Path)
				assert.Equal(t, 3, req.Size)
				assert.Equal(t, uint32(0640), req.Mode)
				assert.Equal(t, os.Geteuid(), req.UID)
				assert.Equal(t, os.Getegid(), req.GID)
				return []byte("mutated"), nil
			},
		},
	}, nil, 1)

	got, err := c.applyLocalWriteMutations(context.Background(), "/workspace/file.txt", []byte("abc"), 0640)
	require.NoError(t, err)
	assert.Equal(t, []byte("mutated"), got)
}

func TestClientVFSHook_RecursionSuppressed(t *testing.T) {
	c := &Client{}
	var runs atomic.Int32

	c.setVFSHooks([]compiledVFSHook{
		{
			path: "/workspace/*",
			callback: func(ctx context.Context, client *Client, event VFSHookEvent) error {
				runs.Add(1)
				// Re-emit a matching event while inside the callback.
				client.handleVFSFileEvent("write", "/workspace/nested.txt", 0, 0, 0, 0)
				return nil
			},
		},
	}, nil, nil, 1)

	c.handleVFSFileEvent("write", "/workspace/trigger.txt", 0, 0, 0, 0)

	require.Eventually(t, func() bool {
		return runs.Load() == 1
	}, 2*time.Second, 20*time.Millisecond)

	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, int32(1), runs.Load())
}

func TestClientApplyLocalActionHooks_Block(t *testing.T) {
	c := &Client{}
	c.setVFSHooks(nil, nil, []compiledVFSActionHook{
		{
			path: "/workspace/*",
			callback: func(ctx context.Context, req VFSActionRequest) VFSHookAction {
				assert.Equal(t, VFSHookOpWrite, req.Op)
				assert.Equal(t, "/workspace/file.txt", req.Path)
				assert.Equal(t, 3, req.Size)
				assert.Equal(t, uint32(0640), req.Mode)
				assert.Equal(t, os.Geteuid(), req.UID)
				assert.Equal(t, os.Getegid(), req.GID)
				return VFSHookActionBlock
			},
		},
	}, 1)

	err := c.applyLocalActionHooks(context.Background(), VFSHookOpWrite, "/workspace/file.txt", 3, 0640)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrVFSHookBlocked)
}
