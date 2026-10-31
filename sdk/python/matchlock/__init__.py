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
    ExecPipeResult,
    ExecInteractiveResult,
    ExecStreamResult,
    FileInfo,
    HostIPMapping,
    ImageConfig,
    MatchlockError,
    NetworkBodyTransform,
    NetworkHookRequest,
    NetworkHookRequestMutation,
    NetworkHookResponseMutation,
    NetworkHookResult,
    NetworkHookAction,
    NetworkHookPhase,
    NetworkHookRule,
    NetworkInterceptionConfig,
    NETWORK_HOOK_ACTION_ALLOW,
    NETWORK_HOOK_ACTION_BLOCK,
    NETWORK_HOOK_ACTION_MUTATE,
    NETWORK_HOOK_PHASE_AFTER,
    NETWORK_HOOK_PHASE_BEFORE,
    PortForward,
    PortForwardBinding,
    RPCError,
    VolumeInfo,
    Secret,
)

from importlib.metadata import version as _version

__version__ = _version("matchlock")

__all__ = [
    "Client",
    "Config",
    "CreateOptions",
    "ExecResult",
    "ExecPipeResult",
    "ExecInteractiveResult",
    "ExecStreamResult",
    "FileInfo",
    "HostIPMapping",
    "ImageConfig",
    "MatchlockError",
    "NetworkBodyTransform",
    "NetworkHookRequest",
    "NetworkHookRequestMutation",
    "NetworkHookResponseMutation",
    "NetworkHookResult",
    "NetworkHookAction",
    "NetworkHookPhase",
    "NetworkHookRule",
    "NetworkInterceptionConfig",
    "NETWORK_HOOK_ACTION_ALLOW",
    "NETWORK_HOOK_ACTION_BLOCK",
    "NETWORK_HOOK_ACTION_MUTATE",
    "NETWORK_HOOK_PHASE_AFTER",
    "NETWORK_HOOK_PHASE_BEFORE",
    "PortForward",
    "PortForwardBinding",
    "RPCError",
    "Sandbox",
    "Secret",
    "VolumeInfo",
]
