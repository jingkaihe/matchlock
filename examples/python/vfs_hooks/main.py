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
    VFS_HOOK_ACTION_BLOCK,
    VFS_HOOK_OP_CREATE,
    VFS_HOOK_OP_WRITE,
    VFS_HOOK_PHASE_AFTER,
    VFS_HOOK_PHASE_BEFORE,
    Client,
    Sandbox,
    VFSActionRequest,
    VFSHookEvent,
    VFSHookRule,
    VFSInterceptionConfig,
    VFSMutateRequest,
)

logging.basicConfig(format="%(levelname)s %(message)s", level=logging.INFO)
log = logging.getLogger(__name__)


def after_write_hook(event: VFSHookEvent) -> None:
    log.info(
        "after hook op=%s path=%s size=%d mode=%o uid=%d gid=%d",
        event.op,
        event.path,
        event.size,
        event.mode,
        event.uid,
        event.gid,
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


def block_action_hook(req: VFSActionRequest) -> str:
    _ = req
    return VFS_HOOK_ACTION_BLOCK


def main() -> None:
    sandbox = Sandbox("alpine:latest").with_vfs_interception(
        VFSInterceptionConfig(
            rules=[
                VFSHookRule(
                    name="host-block-create",
                    phase=VFS_HOOK_PHASE_BEFORE,
                    ops=[VFS_HOOK_OP_CREATE],
                    path="/workspace/blocked-create.txt",
                    action=VFS_HOOK_ACTION_BLOCK,
                ),
                VFSHookRule(
                    name="sdk-block-write",
                    phase=VFS_HOOK_PHASE_BEFORE,
                    ops=[VFS_HOOK_OP_WRITE],
                    path="/workspace/blocked-write.txt",
                    action_hook=block_action_hook,
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

        try:
            client.write_file("/workspace/blocked-create.txt", "blocked")
            log.warning("host create block unexpectedly succeeded")
        except Exception as exc:  # noqa: BLE001
            log.info("host create block rejected as expected: %s", exc)

        try:
            client.write_file("/workspace/blocked-write.txt", "blocked")
            log.warning("local write block unexpectedly succeeded")
        except Exception as exc:  # noqa: BLE001
            log.info("local write block rejected as expected: %s", exc)

        client.write_file("/workspace/mutated.txt", "original-content", mode=0o640)
        mutated = client.read_file("/workspace/mutated.txt").decode(
            "utf-8", errors="replace"
        )
        print(f"mutated file content: {mutated.strip()!r}")

        client.write_file("/workspace/trigger.txt", "trigger", mode=0o600)
        time.sleep(0.4)

        client.close()
        client.remove()


if __name__ == "__main__":
    main()
