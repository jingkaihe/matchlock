#!/usr/bin/env -S uv run
# /// script
# requires-python = ">=3.12"
# dependencies = ["matchlock"]
# ///
"""VFS interception hooks example.

Usage:
  uv run vfs_hooks.py
"""

from __future__ import annotations

import logging
import time

from matchlock import (
    Client,
    Sandbox,
    VFSHookRule,
    VFSInterceptionConfig,
)

logging.basicConfig(format="%(levelname)s %(message)s", level=logging.INFO)
log = logging.getLogger(__name__)


def after_write_hook(client: Client) -> None:
    client.exec(
        "echo 1 >> /tmp/hook_runs; "
        "if [ ! -f /workspace/hook.log ]; then "
        "echo hook > /workspace/hook.log; "
        "fi"
    )


sandbox = Sandbox("alpine:latest").with_vfs_interception(
    VFSInterceptionConfig(
        max_exec_depth=1,
        rules=[
            VFSHookRule(
                name="block-create",
                phase="before",
                ops=["create"],
                path="/workspace/blocked.txt",
                action="block",
            ),
            VFSHookRule(
                name="mutate-write",
                phase="before",
                ops=["write"],
                path="/workspace/mutated.txt",
                action="mutate_write",
                data="mutated-by-hook",
            ),
            VFSHookRule(
                name="audit-after-write",
                phase="after",
                ops=["write"],
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

    client.write_file("/workspace/mutated.txt", "original-content")
    mutated = client.read_file("/workspace/mutated.txt").decode("utf-8", errors="replace")
    print(f"mutated file content: {mutated.strip()!r}")

    client.write_file("/workspace/trigger.txt", "trigger")
    time.sleep(0.4)

    runs = client.exec(
        "if [ -f /tmp/hook_runs ]; then wc -l < /tmp/hook_runs; else echo 0; fi"
    )
    print(f"hook exec runs: {runs.stdout.strip()}")

    hook_log = client.read_file("/workspace/hook.log").decode("utf-8", errors="replace")
    print(f"hook log content: {hook_log.strip()!r}")

client.remove()
