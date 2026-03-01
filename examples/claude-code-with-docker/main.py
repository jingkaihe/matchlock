#!/usr/bin/env -S uv run
# /// script
# requires-python = ">=3.12"
# dependencies = ["matchlock"]
# ///
"""Launch a Docker-enabled Claude Code sandbox via the Python SDK."""

from __future__ import annotations

import argparse
import logging
import os
import sys

from matchlock import Client, Config, Sandbox

try:
    import termios
    import tty
except ImportError:  # pragma: no cover
    termios = None
    tty = None


logging.basicConfig(format="%(levelname)s %(message)s", level=logging.INFO)
log = logging.getLogger(__name__)


class _FDReader:
    def __init__(self, fd: int):
        self._fd = fd

    def read(self, size: int = 4096) -> bytes:
        chunk_size = size if size > 0 else 4096
        try:
            return os.read(self._fd, chunk_size)
        except OSError:
            return b""


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Launch a Docker-enabled Claude Code sandbox and attach an interactive shell.",
    )
    parser.add_argument(
        "--image",
        default="claude-code-with-docker:latest",
        help="Sandbox image to run (default: %(default)s)",
    )
    parser.add_argument(
        "--cpus",
        type=float,
        default=2.0,
        help="vCPU count (supports fractional values, default: %(default)s)",
    )
    parser.add_argument(
        "--memory", type=int, default=4096, help="Memory in MB (default: %(default)s)"
    )
    return parser.parse_args()


def build_sandbox(args: argparse.Namespace, api_key: str) -> Sandbox:
    return (
        Sandbox(args.image)
        .with_cpus(args.cpus)
        .with_memory(args.memory)
        .allow_host(
            "docker.io",
            "*.docker.io",
            "*.docker.com",
            "*.r2.cloudflarestorage.com",
            "api.anthropic.com",
            "*.anthropic.com",
        )
        .add_secret("ANTHROPIC_API_KEY", api_key, "api.anthropic.com")
        .with_privileged()
    )


def run_interactive_shell(client: Client) -> None:
    if (
        not sys.stdin.isatty()
        or not sys.stdout.isatty()
        or termios is None
        or tty is None
    ):
        raise RuntimeError("interactive terminal required (POSIX TTY)")

    fd = sys.stdin.fileno()
    size = os.get_terminal_size(fd)
    rows = size.lines
    cols = size.columns

    print(
        """
+------------------------------------------------------------------------+
| Connected to sandbox shell as agent.                                   |
| Try:                                                                   |
|   docker run --rm hello-world                                          |
|   claude --dangerously-skip-permissions                                |
+------------------------------------------------------------------------+
"""
    )

    old_state = termios.tcgetattr(fd)
    tty.setraw(fd)
    try:
        stdin_reader = _FDReader(fd)
        result = client.exec_interactive(
            "su agent -s /bin/bash",
            stdin=stdin_reader,
            stdout=sys.stdout,
            working_dir="/home/agent",
            rows=rows,
            cols=cols,
        )
    finally:
        termios.tcsetattr(fd, termios.TCSADRAIN, old_state)

    print(f"\nShell exited: code={result.exit_code} duration_ms={result.duration_ms}")


def main() -> None:
    args = parse_args()

    api_key = os.environ.get("ANTHROPIC_API_KEY", "")
    if not api_key:
        raise RuntimeError("ANTHROPIC_API_KEY is required")

    config = Config(binary_path=os.environ.get("MATCHLOCK_BIN", "matchlock"))
    sandbox = build_sandbox(args, api_key)

    client = Client(config)
    try:
        with client:
            vm_id = client.launch(sandbox)
            log.info("sandbox ready vm=%s image=%s", vm_id, args.image)
            run_interactive_shell(client)
    finally:
        try:
            client.remove()
        except Exception as exc:  # noqa: BLE001
            log.warning("remove failed (ignored): %s", exc)


if __name__ == "__main__":
    main()
