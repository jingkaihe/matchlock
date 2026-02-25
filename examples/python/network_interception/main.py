#!/usr/bin/env -S uv run
# /// script
# requires-python = ">=3.12"
# dependencies = ["matchlock"]
# ///
"""Python SDK callback-based network interception example.

Usage:
  uv run examples/python/network_interception/main.py
"""

from __future__ import annotations

import logging

from matchlock import (
    Client,
    NetworkHookRequest,
    NetworkHookResponseMutation,
    NetworkHookResult,
    NetworkHookRule,
    NetworkInterceptionConfig,
    Sandbox,
)

logging.basicConfig(format="%(levelname)s %(message)s", level=logging.INFO)
log = logging.getLogger(__name__)


def after_hook(req: NetworkHookRequest) -> NetworkHookResult | None:
    # This callback runs only when host/path/phase prefilters match.
    if req.status_code != 200:
        return None
    return NetworkHookResult(
        action="mutate",
        response=NetworkHookResponseMutation(
            headers={"X-Intercepted": ["callback"]},
            set_body=b'{"msg":"from-callback"}',
        ),
    )


def main() -> None:
    sandbox = (
        Sandbox("alpine:latest")
        .allow_host("httpbin.org")
        .with_network_interception(
            NetworkInterceptionConfig(
                rules=[
                    NetworkHookRule(
                        name="dynamic-response-callback",
                        phase="after",
                        hosts=["httpbin.org"],
                        path="/response-headers",
                        hook=after_hook,
                    )
                ]
            )
        )
    )

    with Client() as client:
        vm_id = client.launch(sandbox)
        log.info("sandbox ready vm=%s", vm_id)

        result = client.exec(
            'sh -c \'wget -S -O - "http://httpbin.org/response-headers?X-Upstream=1&body=foo" 2>&1\''
        )
        output = result.stdout + result.stderr
        print(output, end="")

        lowered = output.lower()
        if '{"msg":"from-callback"}' not in output:
            raise RuntimeError("expected callback to replace response body")
        if "x-upstream: 1" in lowered:
            raise RuntimeError('expected header "X-Upstream" to be removed')
        if "x-intercepted: callback" not in lowered:
            raise RuntimeError('expected header "X-Intercepted: callback"')

        print("OK: callback hook intercepted and mutated the response")
        try:
            client.remove()
        except Exception as exc:  # noqa: BLE001
            log.warning("remove failed (ignored): %s", exc)


if __name__ == "__main__":
    main()
