#!/usr/bin/env -S uv run
# /// script
# requires-python = ">=3.12"
# dependencies = ["matchlock"]
# ///
"""Python SDK network interception example with Anthropic API key injection.

Usage:
  ANTHROPIC_API_KEY=sk-xxx uv run examples/python/network_interception/main.py
"""

from __future__ import annotations

import logging
import os
import sys

from matchlock import (
    Client,
    NetworkHookRequest,
    NetworkHookRequestMutation,
    NetworkHookResult,
    NetworkHookRule,
    NetworkInterceptionConfig,
    Sandbox,
)

logging.basicConfig(format="%(levelname)s %(message)s", level=logging.INFO)
log = logging.getLogger(__name__)

SCRIPT = """\
# /// script
# requires-python = ">=3.12"
# dependencies = ["anthropic"]
# ///
import anthropic

client = anthropic.Anthropic(api_key="placeholder")
with client.messages.stream(
    model="claude-haiku-4-5-20251001",
    max_tokens=1000,
    messages=[{"role": "user", "content": "Explain TCP to me"}],
) as stream:
    for text in stream.text_stream:
        print(text, end="", flush=True)
print()
"""


def main() -> None:
    api_key = os.environ.get("ANTHROPIC_API_KEY", "")

    def before_hook(req: NetworkHookRequest) -> NetworkHookResult:
        headers = {
            key: list(values) for key, values in (req.request_headers or {}).items()
        }
        headers["X-Api-Key"] = [api_key]
        return NetworkHookResult(
            action="mutate",
            request=NetworkHookRequestMutation(headers=headers),
        )

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
        .with_network_interception(
            NetworkInterceptionConfig(
                rules=[
                    NetworkHookRule(
                        name="inject-anthropic-api-key",
                        phase="before",
                        hosts=["api.anthropic.com"],
                        hook=before_hook,
                    )
                ]
            )
        )
    )

    client = Client()
    try:
        with client:
            vm_id = client.launch(sandbox)
            log.info("sandbox ready vm=%s", vm_id)

            version = client.exec("python3 --version")
            print(version.stdout, end="")

            client.exec("pip install --quiet uv")

            client.write_file("/ask.py", SCRIPT)

            result = client.exec_stream(
                "uv run /ask.py",
                stdout=sys.stdout,
                stderr=sys.stderr,
            )
            print()
            log.info(
                "done exit_code=%d duration_ms=%d", result.exit_code, result.duration_ms
            )
    finally:
        try:
            client.remove()
        except Exception as exc:  # noqa: BLE001
            log.warning("remove failed (ignored): %s", exc)


if __name__ == "__main__":
    main()
