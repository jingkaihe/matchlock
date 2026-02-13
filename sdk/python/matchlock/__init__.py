"""Matchlock Python SDK — sandboxes for AI-generated code.

Builder API:

    from matchlock import Client, Sandbox

    sandbox = Sandbox("python:3.12-alpine") \\
        .allow_host("dl-cdn.alpinelinux.org", "api.anthropic.com") \\
        .add_secret("ANTHROPIC_API_KEY", os.environ["ANTHROPIC_API_KEY"], "api.anthropic.com")

    with Client() as client:
        vm_id = client.launch(sandbox)

        result = client.exec("echo hello")
        print(result.stdout)

        stream_result = client.exec_stream("echo streaming", stdout=sys.stdout)
"""

from .builder import Sandbox
from .client import Client
from .types import (
    Config,
    CreateOptions,
    ExecResult,
    ExecStreamResult,
    FileInfo,
    ImageConfig,
    MatchlockError,
    MountConfig,
    RPCError,
    VFSActionRequest,
    Secret,
    VFSHookOp,
    VFSHookAction,
    VFSHookEvent,
    VFSHookPhase,
    VFSHookRule,
    VFSInterceptionConfig,
    VFSMutateRequest,
    VFS_HOOK_OP_CHMOD,
    VFS_HOOK_OP_CLOSE,
    VFS_HOOK_OP_CREATE,
    VFS_HOOK_OP_MKDIR,
    VFS_HOOK_OP_OPEN,
    VFS_HOOK_OP_READ,
    VFS_HOOK_OP_READDIR,
    VFS_HOOK_OP_READLINK,
    VFS_HOOK_OP_REMOVE,
    VFS_HOOK_OP_REMOVE_ALL,
    VFS_HOOK_OP_RENAME,
    VFS_HOOK_OP_STAT,
    VFS_HOOK_OP_SYNC,
    VFS_HOOK_OP_SYMLINK,
    VFS_HOOK_OP_TRUNCATE,
    VFS_HOOK_OP_WRITE,
    VFS_HOOK_ACTION_ALLOW,
    VFS_HOOK_ACTION_BLOCK,
    VFS_HOOK_PHASE_AFTER,
    VFS_HOOK_PHASE_BEFORE,
)

from importlib.metadata import version as _version

__version__ = _version("matchlock")

__all__ = [
    "Client",
    "Config",
    "CreateOptions",
    "ExecResult",
    "ExecStreamResult",
    "FileInfo",
    "ImageConfig",
    "MatchlockError",
    "MountConfig",
    "RPCError",
    "Sandbox",
    "Secret",
    "VFSActionRequest",
    "VFSHookAction",
    "VFSHookOp",
    "VFSHookEvent",
    "VFSHookPhase",
    "VFSHookRule",
    "VFSInterceptionConfig",
    "VFSMutateRequest",
    "VFS_HOOK_OP_CHMOD",
    "VFS_HOOK_OP_CLOSE",
    "VFS_HOOK_OP_CREATE",
    "VFS_HOOK_OP_MKDIR",
    "VFS_HOOK_OP_OPEN",
    "VFS_HOOK_OP_READ",
    "VFS_HOOK_OP_READDIR",
    "VFS_HOOK_OP_READLINK",
    "VFS_HOOK_OP_REMOVE",
    "VFS_HOOK_OP_REMOVE_ALL",
    "VFS_HOOK_OP_RENAME",
    "VFS_HOOK_OP_STAT",
    "VFS_HOOK_OP_SYNC",
    "VFS_HOOK_OP_SYMLINK",
    "VFS_HOOK_OP_TRUNCATE",
    "VFS_HOOK_OP_WRITE",
    "VFS_HOOK_ACTION_ALLOW",
    "VFS_HOOK_ACTION_BLOCK",
    "VFS_HOOK_PHASE_AFTER",
    "VFS_HOOK_PHASE_BEFORE",
]
