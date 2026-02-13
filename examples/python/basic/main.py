#!/usr/bin/env -S uv run
# /// script
# requires-python = ">=3.12"
# dependencies = ["matchlock"]
# ///
"""Matchlock + Anthropic example.

Usage: ANTHROPIC_API_KEY=sk-xxx uv run examples/python/basic/main.py
"""

import logging
import os
import sys

from matchlock import Client, Sandbox

logging.basicConfig(format="%(levelname)s %(message)s", level=logging.INFO)
log = logging.getLogger(__name__)

sandbox = (
    Sandbox("python:3.12-alpine")
    .allow_host(
        "dl-cdn.alpinelinux.org",
        "files.pythonhosted.org",
        "pypi.org",
        "astral.sh",
        "github.com",
        "objects.githubusercontent.com",
        "api.anthropic.com",
    )
    .add_secret(
        "ANTHROPIC_API_KEY",
        os.environ.get("ANTHROPIC_API_KEY", ""),
        "api.anthropic.com",
    )
)

SCRIPT = """\
# /// script
# requires-python = ">=3.12"
# dependencies = ["anthropic"]
# ///
import anthropic, os

client = anthropic.Anthropic(api_key=os.environ["ANTHROPIC_API_KEY"])
with client.messages.stream(
    model="claude-haiku-4-5-20251001",
    max_tokens=1000,
    messages=[{"role": "user", "content": "Explain TCP/IP."}],
) as stream:
    for text in stream.text_stream:
        print(text, end="", flush=True)
print()
"""

with Client() as client:
    vm_id = client.launch(sandbox)
    log.info("sandbox ready vm=%s", vm_id)

    result = client.exec("python3 --version")
    print(result.stdout, end="")

    result = client.exec("pip install --quiet uv")
    print(result.stdout, end="")

    client.write_file("/workspace/ask.py", SCRIPT)

    result = client.exec_stream(
        "uv run /workspace/ask.py",
        stdout=sys.stdout,
        stderr=sys.stderr,
    )
    print()
    log.info("done exit_code=%d duration_ms=%d", result.exit_code, result.duration_ms)

client.remove()
