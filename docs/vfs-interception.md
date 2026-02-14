# VFS Interception

VFS interception lets you inspect and control filesystem operations on mounted guest paths from the host side.

## Rule Model

Each rule has:
- `phase`: `before` or `after`
- `ops`: operation filter (`create`, `write`, `read`, etc.)
- `path`: filepath-style glob (for example `/workspace/*`)

Behavior by phase:
- `before`: supports wire `action=block`, SDK `action_hook` callbacks, and SDK `mutate_hook` callbacks
- `after`: supports SDK `hook` and `dangerous_hook` callbacks

## Host Rules vs SDK-Local Hooks

There are two different enforcement paths:

- Host wire rules (`action=block`) are sent to the sandbox and evaluated inside host VFS interception.
- SDK-local callbacks (`action_hook`, `mutate_hook`, `hook`, `dangerous_hook`) run in your SDK process.

For `action_hook`, matching is based on the SDK API operation:

- `WriteFile`/`WriteFileMode` -> `op=write`
- `ReadFile` -> `op=read`
- `ListFiles` -> `op=readdir`

So an `action_hook` with `ops=[create]` will not match `WriteFile`.
If you want to block create of a file, use a host wire rule with `action=block` and `ops=[create]`.
If you want to block SDK write calls directly, use `action_hook` with `ops=[write]`.

Example:

```go
Rules: []sdk.VFSHookRule{
	// Host-side VFS create enforcement.
	{
		Phase:  sdk.VFSHookPhaseBefore,
		Ops:    []sdk.VFSHookOp{sdk.VFSHookOpCreate},
		Path:   "/workspace/blocked-create.txt",
		Action: sdk.VFSHookActionBlock,
	},
	// SDK-local write call enforcement.
	{
		Phase: sdk.VFSHookPhaseBefore,
		Ops:   []sdk.VFSHookOp{sdk.VFSHookOpWrite},
		Path:  "/workspace/blocked-write.txt",
		ActionHook: func(ctx context.Context, req sdk.VFSActionRequest) sdk.VFSHookAction {
			return sdk.VFSHookActionBlock
		},
	},
}
```

## Go SDK

Use typed constants for phases and ops:

```go
sandbox := sdk.New("alpine:latest").WithVFSInterception(&sdk.VFSInterceptionConfig{
	Rules: []sdk.VFSHookRule{
		{
			Phase:  sdk.VFSHookPhaseBefore,
			Ops:    []sdk.VFSHookOp{sdk.VFSHookOpCreate},
			Path:   "/workspace/blocked.txt",
			Action: sdk.VFSHookActionBlock,
		},
		{
			Phase: sdk.VFSHookPhaseBefore,
			Ops:   []sdk.VFSHookOp{sdk.VFSHookOpWrite},
			Path:  "/workspace/mutated.txt",
			MutateHook: func(ctx context.Context, req sdk.VFSMutateRequest) ([]byte, error) {
				return []byte("mutated-by-hook"), nil
			},
		},
		{
			Phase: sdk.VFSHookPhaseAfter,
			Ops:   []sdk.VFSHookOp{sdk.VFSHookOpWrite},
			Path:  "/workspace/*",
			Hook: func(ctx context.Context, event sdk.VFSHookEvent) error {
				fmt.Printf("op=%s path=%s size=%d mode=%#o uid=%d gid=%d\n",
					event.Op, event.Path, event.Size, event.Mode, event.UID, event.GID)
				return nil
			},
		},
		{
			Phase: sdk.VFSHookPhaseAfter,
			Ops:   []sdk.VFSHookOp{sdk.VFSHookOpWrite},
			Path:  "/workspace/trigger.txt",
			DangerousHook: func(ctx context.Context, client *sdk.Client, event sdk.VFSHookEvent) error {
				_, err := client.Exec(ctx, "echo hook >> /workspace/hook.log")
				return err
			},
		},
	},
})
```

See full runnable examples:
- [`examples/go/basic/main.go`](../examples/go/basic/main.go)
- [`examples/go/vfs_hooks/main.go`](../examples/go/vfs_hooks/main.go)

## Python SDK

Use exported constants for phases and ops:

```python
from matchlock import (
    Sandbox,
    VFSInterceptionConfig,
    VFSHookRule,
    VFSMutateRequest,
    VFS_HOOK_ACTION_BLOCK,
    VFS_HOOK_PHASE_BEFORE,
    VFS_HOOK_PHASE_AFTER,
    VFS_HOOK_OP_CREATE,
    VFS_HOOK_OP_WRITE,
)

def after_write(event):
    print(
        f"op={event.op} path={event.path} size={event.size} "
        f"mode={oct(event.mode)} uid={event.uid} gid={event.gid}"
    )

def dangerous_after_write(client, event):
    client.exec("echo hook >> /workspace/hook.log")

def mutate_write(req: VFSMutateRequest) -> bytes:
    return b"mutated-by-hook"

sandbox = Sandbox("alpine:latest").with_vfs_interception(
    VFSInterceptionConfig(
        rules=[
            VFSHookRule(
                phase=VFS_HOOK_PHASE_BEFORE,
                ops=[VFS_HOOK_OP_CREATE],
                path="/workspace/blocked.txt",
                action=VFS_HOOK_ACTION_BLOCK,
            ),
            VFSHookRule(
                phase=VFS_HOOK_PHASE_BEFORE,
                ops=[VFS_HOOK_OP_WRITE],
                path="/workspace/mutated.txt",
                mutate_hook=mutate_write,
            ),
            VFSHookRule(
                phase=VFS_HOOK_PHASE_AFTER,
                ops=[VFS_HOOK_OP_WRITE],
                path="/workspace/*",
                hook=after_write,
            ),
            VFSHookRule(
                phase=VFS_HOOK_PHASE_AFTER,
                ops=[VFS_HOOK_OP_WRITE],
                path="/workspace/trigger.txt",
                dangerous_hook=dangerous_after_write,
            ),
        ],
    )
)
```

See full runnable examples:
- [`examples/python/basic/main.py`](../examples/python/basic/main.py)
- [`examples/python/vfs_hooks/main.py`](../examples/python/vfs_hooks/main.py)

## Recursion and Safety

- `hook` callbacks are `after`-only and run with recursion suppression enabled.
- `dangerous_hook` callbacks are `after`-only and bypass recursion suppression.
- When SDK after-event callbacks (`hook`/`dangerous_hook`) are present, event emission is enabled automatically for interception.

## Host-Side Dynamic Mutate (Go, In-Process)

If you are wiring `pkg/vfs` directly in host Go code, `mutate_write` can be dynamic per write:

```go
hooks := vfs.NewHookEngine([]vfs.HookRule{
	{
		Phase:  vfs.HookPhaseBefore,
		Ops:    []vfs.HookOp{vfs.HookOpWrite},
		Action: vfs.HookActionMutateWrite,
		MutateWriteFunc: func(ctx context.Context, req vfs.MutateWriteRequest) ([]byte, error) {
			// Decide replacement bytes dynamically from metadata.
			// req has: path, size, offset, mode, uid, gid.
			return []byte("prefix:" + req.Path), nil
		},
	},
})
```

Notes:
- This is host in-process only (`pkg/vfs` / `pkg/sandbox` integration), not JSON-RPC payload.
- Returning an error from `MutateWriteFunc` fails the write.
