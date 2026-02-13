#!/usr/bin/env -S uv run
# /// script
# requires-python = ">=3.12"
# dependencies = ["matchlock"]
# ///
"""VFS interception hooks example.

Usage:
  uv run examples/python/vfs_hooks/main.py
"""

from __future__ import annotations

import logging
import time

from matchlock import (
    Client,
    Sandbox,
    VFSHookEvent,
    VFSHookRule,
    VFSInterceptionConfig,
    VFSActionRequest,
    VFSMutateRequest,
    VFS_HOOK_ACTION_BLOCK,
    VFS_HOOK_OP_CREATE,
    VFS_HOOK_OP_WRITE,
    VFS_HOOK_PHASE_AFTER,
    VFS_HOOK_PHASE_BEFORE,
)

logging.basicConfig(format="%(levelname)s %(message)s", level=logging.INFO)
log = logging.getLogger(__name__)


def after_write_hook(client: Client, event: VFSHookEvent) -> None:
    log.info(
        "after hook op=%s path=%s size=%d mode=%o uid=%d gid=%d",
        event.op,
        event.path,
        event.size,
        event.mode,
        event.uid,
        event.gid,
    )
    client.exec(
        "echo 1 >> /tmp/hook_runs; "
        "if [ ! -f /workspace/hook.log ]; then "
        "echo hook > /workspace/hook.log; "
        "fi"
    )


def mutate_write_hook(req: VFSMutateRequest) -> bytes:
    log.info(
        "mutate hook path=%s size=%d mode=%o uid=%d gid=%d",
        req.path,
        req.size,
        req.mode,
        req.uid,
        req.gid,
    )
    return (
        f"mutated-by-hook size={req.size} "
        f"mode={oct(req.mode)} uid={req.uid} gid={req.gid}"
    ).encode("utf-8")


def block_create_hook(req: VFSActionRequest) -> str:
    _ = req
    return VFS_HOOK_ACTION_BLOCK


sandbox = Sandbox("alpine:latest").with_vfs_interception(
    VFSInterceptionConfig(
        max_exec_depth=1,
        rules=[
            VFSHookRule(
                name="block-create",
                phase=VFS_HOOK_PHASE_BEFORE,
                ops=[VFS_HOOK_OP_CREATE],
                path="/workspace/blocked.txt",
                action_hook=block_create_hook,
            ),
            VFSHookRule(
                name="mutate-write",
                phase=VFS_HOOK_PHASE_BEFORE,
                ops=[VFS_HOOK_OP_WRITE],
                path="/workspace/mutated.txt",
                mutate_hook=mutate_write_hook,
            ),
            VFSHookRule(
                name="audit-after-write",
                phase=VFS_HOOK_PHASE_AFTER,
                ops=[VFS_HOOK_OP_WRITE],
                path="/workspace/*",
                timeout_ms=2000,
                hook=after_write_hook,
            ),
        ],
    )
)

with Client() as client:
    vm_id = client.launch(sandbox)
    log.info("sandbox ready vm=%s", vm_id)

    client.exec(
        "rm -f /tmp/hook_runs /workspace/hook.log "
        "/workspace/blocked.txt /workspace/mutated.txt /workspace/trigger.txt"
    )

    try:
        client.write_file("/workspace/blocked.txt", "blocked")
        log.warning("blocked write unexpectedly succeeded")
    except Exception as exc:  # noqa: BLE001
        log.info("blocked write rejected as expected: %s", exc)

    client.write_file("/workspace/mutated.txt", "original-content", mode=0o640)
    mutated = client.read_file("/workspace/mutated.txt").decode("utf-8", errors="replace")
    print(f"mutated file content: {mutated.strip()!r}")

    client.write_file("/workspace/trigger.txt", "trigger", mode=0o600)
    time.sleep(0.4)

    runs = client.exec(
        "if [ -f /tmp/hook_runs ]; then wc -l < /tmp/hook_runs; else echo 0; fi"
    )
    print(f"hook exec runs: {runs.stdout.strip()}")

    hook_log = client.read_file("/workspace/hook.log").decode("utf-8", errors="replace")
    print(f"hook log content: {hook_log.strip()!r}")

client.remove()
