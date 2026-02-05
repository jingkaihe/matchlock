"""
Matchlock Python SDK

A Python client for interacting with Matchlock sandboxes via JSON-RPC.

Example usage:

    from matchlock import Client, CreateOptions

    with Client() as client:
        client.create(CreateOptions(image="standard", cpus=1, memory_mb=512))
        
        result = client.exec("echo 'Hello from sandbox!'")
        print(result.stdout)
        
        client.write_file("/workspace/script.py", b"print('Hello')")
        content = client.read_file("/workspace/script.py")
"""

from .client import Client
from .types import (
    Config,
    CreateOptions,
    MountConfig,
    Secret,
    ExecResult,
    FileInfo,
    MatchlockError,
    RPCError,
)

__version__ = "0.1.0"
__all__ = [
    "Client",
    "Config",
    "CreateOptions",
    "MountConfig",
    "Secret",
    "ExecResult",
    "FileInfo",
    "MatchlockError",
    "RPCError",
]
