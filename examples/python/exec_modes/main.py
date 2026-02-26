#!/usr/bin/env -S uv run
# /// script
# requires-python = ">=3.12"
# dependencies = ["matchlock"]
# ///
"""Python SDK exec mode examples: stream, pipe, and interactive TTY.

Run:
  uv run examples/python/exec_modes/main.py
"""

from __future__ import annotations

import logging
import os
import sys
import io

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
    """Read bytes directly from a TTY fd without Python buffering."""

    def __init__(self, fd: int):
        self._fd = fd

    def read(self, size: int = 4096) -> bytes:
        chunk_size = size if size > 0 else 4096
        try:
            return os.read(self._fd, chunk_size)
        except OSError:
            return b""


def run_exec_stream(client: Client) -> None:
    print("== exec_stream ==")
    result = client.exec_stream(
        "echo stream:start; sleep 1; echo stream:end",
        stdout=sys.stdout,
        stderr=sys.stderr,
        working_dir="/workspace",
    )
    print(f"stream exit={result.exit_code} duration_ms={result.duration_ms}")


def run_exec_pipe(client: Client) -> None:
    print("\n== exec_pipe ==")
    stdout_buf = io.StringIO()
    stderr_buf = io.StringIO()
    result = client.exec_pipe(
        "cat; echo pipe-stderr >&2",
        stdin=io.BytesIO(b"hello from stdin\n"),
        stdout=stdout_buf,
        stderr=stderr_buf,
        working_dir="/workspace",
    )
    print(f"pipe exit={result.exit_code} duration_ms={result.duration_ms}")
    print("pipe stdout:")
    print(stdout_buf.getvalue(), end="")
    print("pipe stderr:")
    print(stderr_buf.getvalue(), end="")


def run_exec_interactive(client: Client) -> None:
    print("\n== exec_interactive ==")
    if (
        not sys.stdin.isatty()
        or not sys.stdout.isatty()
        or termios is None
        or tty is None
    ):
        print("interactive terminal skipped (requires a POSIX TTY).")
        return

    fd = sys.stdin.fileno()
    size = os.get_terminal_size(fd)
    rows = size.lines
    cols = size.columns

    print("Connected to sandbox shell. Type 'exit' or press Ctrl-D to quit.")

    old_state = termios.tcgetattr(fd)
    tty.setraw(fd)
    try:
        stdin_reader = _FDReader(fd)
        result = client.exec_interactive(
            "sh",
            stdin=stdin_reader,
            stdout=sys.stdout,
            working_dir="/workspace",
            rows=rows,
            cols=cols,
        )
    finally:
        termios.tcsetattr(fd, termios.TCSADRAIN, old_state)

    print(f"\nShell exited: code={result.exit_code} duration_ms={result.duration_ms}")


def main() -> None:
    sandbox = (
        Sandbox("alpine:latest").with_workspace("/workspace").mount_memory("/workspace")
    )

    config = Config(binary_path=os.environ.get("MATCHLOCK_BIN", "matchlock"))

    client = Client(config)
    try:
        with client:
            vm_id = client.launch(sandbox)
            log.info("sandbox ready vm=%s binary=%s", vm_id, config.binary_path)

            run_exec_stream(client)
            run_exec_pipe(client)
            run_exec_interactive(client)
    finally:
        try:
            client.remove()
        except Exception as exc:  # noqa: BLE001
            log.warning("remove failed (ignored): %s", exc)


if __name__ == "__main__":
    main()
