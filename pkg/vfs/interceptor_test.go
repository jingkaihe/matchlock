package vfs

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInterceptProvider_BeforeBlock(t *testing.T) {
	hooks := NewHookEngine([]HookRule{
		{
			Phase:       HookPhaseBefore,
			Ops:         []HookOp{HookOpCreate},
			PathPattern: "/blocked.txt",
			Action:      HookActionBlock,
		},
	}, 1)
	defer hooks.Close()

	provider := NewInterceptProvider(NewMemoryProvider(), hooks)

	_, err := provider.Create("/blocked.txt", 0644)
	require.Error(t, err)
	assert.True(t, os.IsPermission(err))
}

func TestInterceptProvider_BeforeActionFuncBlock(t *testing.T) {
	hooks := NewHookEngine([]HookRule{
		{
			Phase:       HookPhaseBefore,
			Ops:         []HookOp{HookOpCreate},
			PathPattern: "/blocked-func.txt",
			ActionFunc: func(ctx context.Context, req HookRequest) HookAction {
				assert.Equal(t, HookOpCreate, req.Op)
				assert.Equal(t, "/blocked-func.txt", req.Path)
				return HookActionBlock
			},
		},
	}, 1)
	defer hooks.Close()

	provider := NewInterceptProvider(NewMemoryProvider(), hooks)

	_, err := provider.Create("/blocked-func.txt", 0644)
	require.Error(t, err)
	assert.True(t, os.IsPermission(err))
}

func TestInterceptProvider_BeforeMutateWrite(t *testing.T) {
	hooks := NewHookEngine([]HookRule{
		{
			Phase:       HookPhaseBefore,
			Ops:         []HookOp{HookOpWrite},
			PathPattern: "/mutate.txt",
			Action:      HookActionMutateWrite,
			MutateWrite: []byte("mutated"),
		},
	}, 1)
	defer hooks.Close()

	provider := NewInterceptProvider(NewMemoryProvider(), hooks)

	h, err := provider.Create("/mutate.txt", 0644)
	require.NoError(t, err)
	_, err = h.Write([]byte("original"))
	require.NoError(t, err)
	require.NoError(t, h.Close())

	h, err = provider.Open("/mutate.txt", os.O_RDONLY, 0)
	require.NoError(t, err)
	defer h.Close()

	buf := make([]byte, 16)
	n, err := h.Read(buf)
	if err != nil && err != io.EOF {
		require.NoError(t, err)
	}
	assert.Equal(t, "mutated", string(buf[:n]))
}

func TestInterceptProvider_BeforeMutateWriteDynamic(t *testing.T) {
	hooks := NewHookEngine([]HookRule{
		{
			Phase:       HookPhaseBefore,
			Ops:         []HookOp{HookOpWrite},
			PathPattern: "/mutate-dyn.txt",
			Action:      HookActionMutateWrite,
			MutateWriteFunc: func(ctx context.Context, req MutateWriteRequest) ([]byte, error) {
				assert.Equal(t, os.FileMode(0644), req.Mode.Perm())
				assert.Equal(t, os.Geteuid(), req.UID)
				assert.Equal(t, os.Getegid(), req.GID)
				return []byte(strings.Repeat("X", req.Size) + ":" + req.Path), nil
			},
		},
	}, 1)
	defer hooks.Close()

	provider := NewInterceptProvider(NewMemoryProvider(), hooks)

	h, err := provider.Create("/mutate-dyn.txt", 0644)
	require.NoError(t, err)
	_, err = h.Write([]byte("payload"))
	require.NoError(t, err)
	require.NoError(t, h.Close())

	h, err = provider.Open("/mutate-dyn.txt", os.O_RDONLY, 0)
	require.NoError(t, err)
	defer h.Close()

	buf := make([]byte, 64)
	n, err := h.Read(buf)
	if err != nil && err != io.EOF {
		require.NoError(t, err)
	}
	assert.Equal(t, "XXXXXXX:/mutate-dyn.txt", string(buf[:n]))
}

func TestInterceptProvider_BeforeMutateWriteDynamicError(t *testing.T) {
	wantErr := errors.New("mutate denied")
	hooks := NewHookEngine([]HookRule{
		{
			Phase:       HookPhaseBefore,
			Ops:         []HookOp{HookOpWrite},
			PathPattern: "/mutate-deny.txt",
			Action:      HookActionMutateWrite,
			MutateWriteFunc: func(ctx context.Context, req MutateWriteRequest) ([]byte, error) {
				return nil, wantErr
			},
		},
	}, 1)
	defer hooks.Close()

	provider := NewInterceptProvider(NewMemoryProvider(), hooks)

	h, err := provider.Create("/mutate-deny.txt", 0644)
	require.NoError(t, err)
	_, err = h.Write([]byte("payload"))
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
}

func TestInterceptProvider_ExecAfterSuppressesRecursiveSideEffects(t *testing.T) {
	hooks := NewHookEngine([]HookRule{
		{
			Phase:       HookPhaseAfter,
			Ops:         []HookOp{HookOpWrite},
			PathPattern: "/*",
			Action:      HookActionExecAfter,
			ExecCommand: "audit",
		},
	}, 1)
	defer hooks.Close()

	var wrapped Provider
	var hookExecCount atomic.Int32

	hooks.SetExecFunc(func(ctx context.Context, command string) error {
		hookExecCount.Add(1)
		h, err := wrapped.Create("/audit.log", 0644)
		if err != nil {
			return err
		}
		defer h.Close()
		_, err = h.Write([]byte("hook"))
		return err
	})

	wrapped = NewInterceptProvider(NewMemoryProvider(), hooks)

	h, err := wrapped.Create("/file.txt", 0644)
	require.NoError(t, err)
	_, err = h.Write([]byte("payload"))
	require.NoError(t, err)
	require.NoError(t, h.Close())

	hooks.Wait()

	assert.Equal(t, int32(1), hookExecCount.Load())

	audit, err := wrapped.Open("/audit.log", os.O_RDONLY, 0)
	require.NoError(t, err)
	defer audit.Close()

	buf := make([]byte, 16)
	n, err := audit.Read(buf)
	if err != nil && err != io.EOF {
		require.NoError(t, err)
	}
	assert.Equal(t, "hook", string(buf[:n]))
}

func TestInterceptProvider_CallbackHooks(t *testing.T) {
	hooks := NewHookEngineWithCallbacks([]Hook{
		{
			Phase:   HookPhaseBefore,
			Matcher: OpPathMatcher{Ops: []HookOp{HookOpCreate}, PathPattern: "/blocked-cb.txt"},
			Before: BeforeHookFunc(func(ctx context.Context, req *HookRequest) error {
				return syscall.EPERM
			}),
		},
	}, 1)
	defer hooks.Close()

	provider := NewInterceptProvider(NewMemoryProvider(), hooks)

	_, err := provider.Create("/blocked-cb.txt", 0644)
	require.Error(t, err)
	assert.True(t, os.IsPermission(err))
}

func TestInterceptProvider_EmitsEvents(t *testing.T) {
	hooks := NewHookEngineWithCallbacks(nil, 1)
	defer hooks.Close()

	var gotWrite atomic.Int32
	var gotMode atomic.Int32
	var gotUID atomic.Int32
	var gotGID atomic.Int32
	hooks.SetEventFunc(func(req HookRequest, result HookResult) {
		if req.Op == HookOpWrite && req.Path == "/event.txt" && result.Err == nil && result.Bytes == 1 {
			gotWrite.Store(1)
			if req.Mode.Perm() == 0644 {
				gotMode.Store(1)
			}
			if req.UID == os.Geteuid() {
				gotUID.Store(1)
			}
			if req.GID == os.Getegid() {
				gotGID.Store(1)
			}
		}
	})

	provider := NewInterceptProvider(NewMemoryProvider(), hooks)

	h, err := provider.Create("/event.txt", 0644)
	require.NoError(t, err)
	_, err = h.Write([]byte("x"))
	require.NoError(t, err)
	require.NoError(t, h.Close())

	assert.Equal(t, int32(1), gotWrite.Load())
	assert.Equal(t, int32(1), gotMode.Load())
	assert.Equal(t, int32(1), gotUID.Load())
	assert.Equal(t, int32(1), gotGID.Load())
}
