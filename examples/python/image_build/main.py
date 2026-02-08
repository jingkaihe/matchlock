#!/usr/bin/env -S uv run
# /// script
# requires-python = ">=3.12"
# dependencies = ["matchlock"]
# ///
"""Dockerfile build example — build from a Dockerfile, launch, and verify.

Usage: sudo uv run examples/python/image_build/main.py
"""

import logging
import os
import tempfile

from matchlock import Client, Sandbox

logging.basicConfig(format="%(levelname)s %(message)s", level=logging.INFO)
log = logging.getLogger(__name__)

# Create a temporary build context with a Dockerfile
with tempfile.TemporaryDirectory(prefix="matchlock-example-build-") as context_dir:
    dockerfile = os.path.join(context_dir, "Dockerfile")
    with open(dockerfile, "w") as f:
        f.write("""\
FROM alpine:latest
RUN apk add --no-cache curl jq
RUN echo "Built by matchlock SDK" > /built-by.txt
CMD ["cat", "/built-by.txt"]
""")

    with Client() as client:
        # Build the image from Dockerfile
        log.info("building image from Dockerfile context=%s", context_dir)
        result = client.build_dockerfile(
            context_dir=context_dir,
            dockerfile=dockerfile,
            tag="example-app:latest",
        )
        log.info(
            "build complete rootfs=%s digest=%s size=%.1f MB",
            result.rootfs_path,
            result.digest,
            result.size / (1024 * 1024),
        )

        # List images to confirm it's cached
        images = client.image_list()
        log.info("cached images: %d", len(images))
        for img in images:
            print(f"  {img.tag:<30}  {img.source:<10}  {img.size / (1024 * 1024):.1f} MB")

        # Launch a sandbox from the freshly built image
        sandbox = Sandbox("example-app:latest")
        vm_id = client.launch(sandbox)
        log.info("sandbox ready vm=%s", vm_id)

        # Verify the built image contents
        res = client.exec("cat /built-by.txt")
        print(f"Output: {res.stdout}", end="")

        # Check that the tools installed in the Dockerfile are available
        res = client.exec("curl --version | head -1")
        print(f"curl: {res.stdout}", end="")

        res = client.exec("jq --version")
        print(f"jq: {res.stdout}", end="")

        # Clean up the built image
        client.image_remove("example-app:latest")
        log.info("cleaned up tag=example-app:latest")

    client.remove()
