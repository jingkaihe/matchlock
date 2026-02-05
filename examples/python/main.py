#!/usr/bin/env python3
"""Matchlock Python SDK Example

Usage: python3 examples/python/main.py
With secrets: ANTHROPIC_API_KEY=sk-xxx python3 examples/python/main.py
"""

import os
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent.parent.parent / "sdk" / "python"))

from matchlock import Client, Config, CreateOptions, Secret

config = Config(binary_path="./bin/matchlock", use_sudo=True)

# Example with secrets - the API key is replaced in HTTP requests to api.anthropic.com
opts = CreateOptions(image="standard")
if api_key := os.environ.get("ANTHROPIC_API_KEY"):
    opts.secrets = [Secret(name="ANTHROPIC_API_KEY", value=api_key, hosts=["api.anthropic.com"])]

with Client(config) as client:
    vm_id = client.create(opts)
    print(f"Created VM: {vm_id}")

    result = client.exec("echo 'Hello from sandbox!'")
    print(f"Output: {result.stdout.strip()}, Exit: {result.exit_code}, Duration: {result.duration_ms}ms")

    client.write_file("/workspace/test.sh", "#!/bin/sh\nls -la /workspace\n", mode=0o755)

    result = client.exec("sh /workspace/test.sh")
    print(f"Script output:\n{result.stdout}")

    files = client.list_files("/workspace")
    print(f"Files: {len(files)} items")

    result = client.exec("exit 42")
    print(f"Failed command exit code: {result.exit_code}")
