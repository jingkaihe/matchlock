#!/usr/bin/env -S uv run
# /// script
# requires-python = ">=3.12"
# dependencies = ["matchlock"]
# ///
"""Python SDK port-forward example with nginx.

This example:
1. Starts nginx in the guest and forwards host 8080 -> guest 80 at create-time.
2. Adds a runtime forward host 18080 -> guest 80 after launch.

Run:
  uv run examples/python/port_forward/main.py
"""

from __future__ import annotations

import logging
import os
import time
from urllib.request import urlopen

from matchlock import Client, Config, Sandbox

logging.basicConfig(format="%(levelname)s %(message)s", level=logging.INFO)
log = logging.getLogger(__name__)


def wait_for_http(url: str, attempts: int = 30, delay_seconds: float = 0.25) -> str:
    """Poll URL until it serves HTTP 200 or retries are exhausted."""
    last_error: Exception | None = None
    for _ in range(attempts):
        try:
            with urlopen(url, timeout=1.5) as response:
                if response.status == 200:
                    return response.read().decode("utf-8", errors="replace")
        except Exception as exc:  # noqa: BLE001
            last_error = exc
        time.sleep(delay_seconds)
    raise RuntimeError(f"timed out waiting for {url}; last_error={last_error!r}")


def main() -> None:
    sandbox = Sandbox("nginx:alpine").with_port_forward(8080, 80)
    config = Config(binary_path=os.environ.get("MATCHLOCK_BIN", "matchlock"))

    client = Client(config)
    try:
        with client:
            vm_id = client.launch(sandbox)
            log.info("sandbox ready vm=%s", vm_id)

            # Customize nginx page content in-guest.
            client.write_file("/usr/share/nginx/html/index.html", "hello matchlock\n")

            body_8080 = wait_for_http("http://127.0.0.1:8080")
            log.info("create-time forward works: 127.0.0.1:8080 -> guest:80")
            print(body_8080)

            bindings = client.port_forward("18080:80")
            for binding in bindings:
                log.info(
                    "runtime forward added: %s:%d -> guest:%d",
                    binding.address,
                    binding.local_port,
                    binding.remote_port,
                )

            body_18080 = wait_for_http("http://127.0.0.1:18080")
            log.info("runtime forward works: 127.0.0.1:18080 -> guest:80")
            print(body_18080)
    finally:
        try:
            client.remove()
        except Exception as exc:  # noqa: BLE001
            log.warning("remove failed (ignored): %s", exc)


if __name__ == "__main__":
    main()
