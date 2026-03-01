"""Type definitions for the Matchlock SDK."""

from dataclasses import dataclass, field
from typing import Any, Callable, Literal, TypeAlias

VFS_HOOK_PHASE_BEFORE = "before"
VFS_HOOK_PHASE_AFTER = "after"
VFS_HOOK_ACTION_ALLOW = "allow"
VFS_HOOK_ACTION_BLOCK = "block"
NETWORK_HOOK_PHASE_BEFORE = "before"
NETWORK_HOOK_PHASE_AFTER = "after"
NETWORK_HOOK_ACTION_ALLOW = "allow"
NETWORK_HOOK_ACTION_BLOCK = "block"
NETWORK_HOOK_ACTION_MUTATE = "mutate"

VFS_HOOK_OP_STAT = "stat"
VFS_HOOK_OP_READDIR = "readdir"
VFS_HOOK_OP_OPEN = "open"
VFS_HOOK_OP_CREATE = "create"
VFS_HOOK_OP_MKDIR = "mkdir"
VFS_HOOK_OP_CHMOD = "chmod"
VFS_HOOK_OP_REMOVE = "remove"
VFS_HOOK_OP_REMOVE_ALL = "remove_all"
VFS_HOOK_OP_RENAME = "rename"
VFS_HOOK_OP_SYMLINK = "symlink"
VFS_HOOK_OP_READLINK = "readlink"
VFS_HOOK_OP_READ = "read"
VFS_HOOK_OP_WRITE = "write"
VFS_HOOK_OP_CLOSE = "close"
VFS_HOOK_OP_SYNC = "sync"
VFS_HOOK_OP_TRUNCATE = "truncate"

VFSHookPhase: TypeAlias = Literal["", "before", "after"]
VFSHookOp: TypeAlias = Literal[
    "stat",
    "readdir",
    "open",
    "create",
    "mkdir",
    "chmod",
    "remove",
    "remove_all",
    "rename",
    "symlink",
    "readlink",
    "read",
    "write",
    "close",
    "sync",
    "truncate",
]
VFSHookAction: TypeAlias = Literal["allow", "block"]
NetworkHookPhase: TypeAlias = Literal["", "before", "after"]
NetworkHookAction: TypeAlias = Literal["allow", "block", "mutate"]


@dataclass
class Config:
    """Client configuration."""

    binary_path: str = "matchlock"
    """Path to the matchlock binary."""

    use_sudo: bool = False
    """Whether to run matchlock with sudo (required for TAP devices on Linux)."""


@dataclass
class MountConfig:
    """VFS mount configuration."""

    type: str = "memory"
    """Mount type: memory, host_fs, or overlay."""

    host_path: str = ""
    """Host path for host_fs mounts."""

    readonly: bool = False
    """Whether the mount is read-only."""

    def to_dict(self) -> dict[str, Any]:
        d: dict[str, Any] = {"type": self.type}
        if self.host_path:
            d["host_path"] = self.host_path
        if self.readonly:
            d["readonly"] = self.readonly
        return d


@dataclass
class VFSHookRule:
    """Single VFS interception rule."""

    name: str = ""
    """Optional rule name."""

    phase: VFSHookPhase = ""
    """Rule phase: before or after (empty defaults to before server-side)."""

    ops: list[VFSHookOp] = field(default_factory=list)
    """Operation filters: read, write, create, ... (empty = all)."""

    path: str = ""
    """filepath-style glob pattern (empty = all)."""

    action: VFSHookAction = "allow"
    """Wire action: allow or block."""

    timeout_ms: int = 0
    """Timeout for SDK-local callback hooks in milliseconds."""

    hook: Callable[["VFSHookEvent"], Any] | None = None
    """SDK-local safe after-hook callback: hook(event) -> Any."""

    dangerous_hook: Callable[[Any, "VFSHookEvent"], Any] | None = None
    """SDK-local re-entrant after-hook callback: dangerous_hook(client, event) -> Any."""

    mutate_hook: Callable[["VFSMutateRequest"], bytes | str | None] | None = None
    """SDK-local before-write mutate callback: mutate_hook(request) -> bytes|str|None."""

    action_hook: Callable[["VFSActionRequest"], VFSHookAction] | None = None
    """SDK-local before-op decision callback: action_hook(request) -> allow|block."""

    def to_dict(self) -> dict[str, Any]:
        d: dict[str, Any] = {"action": self.action}
        if self.name:
            d["name"] = self.name
        if self.phase:
            d["phase"] = self.phase
        if self.ops:
            d["ops"] = self.ops
        if self.path:
            d["path"] = self.path
        if self.timeout_ms > 0:
            d["timeout_ms"] = self.timeout_ms
        return d


@dataclass
class VFSInterceptionConfig:
    """Host-side VFS interception configuration."""

    emit_events: bool = False
    """Emit file-operation events from host-side VFS interception."""

    rules: list[VFSHookRule] = field(default_factory=list)
    """Interception rules."""

    def to_dict(self) -> dict[str, Any]:
        d: dict[str, Any] = {}
        if self.emit_events:
            d["emit_events"] = True
        if self.rules:
            d["rules"] = [r.to_dict() for r in self.rules]
        return d


@dataclass
class NetworkBodyTransform:
    """Literal replacement applied to response bodies."""

    find: str
    """Find string."""

    replace: str = ""
    """Replacement string."""

    def to_dict(self) -> dict[str, Any]:
        d: dict[str, Any] = {"find": self.find}
        if self.replace:
            d["replace"] = self.replace
        return d


@dataclass
class NetworkHookRequest:
    """Input delivered to an SDK-local network callback hook."""

    phase: NetworkHookPhase = ""
    host: str = ""
    method: str = ""
    path: str = ""
    query: dict[str, str] | None = None
    request_headers: dict[str, list[str]] | None = None
    status_code: int = 0
    response_headers: dict[str, list[str]] | None = None
    is_sse: bool = False


@dataclass
class NetworkHookRequestMutation:
    """Request mutation returned by an SDK-local network callback hook."""

    headers: dict[str, list[str]] | None = None
    query: dict[str, str] | None = None
    path: str = ""


@dataclass
class NetworkHookResponseMutation:
    """Response mutation returned by an SDK-local network callback hook."""

    headers: dict[str, list[str]] | None = None
    body_replacements: list[NetworkBodyTransform] = field(default_factory=list)
    set_body: bytes | str | None = None


@dataclass
class NetworkHookResult:
    """Result returned by an SDK-local network callback hook."""

    action: NetworkHookAction = "allow"
    request: NetworkHookRequestMutation | None = None
    response: NetworkHookResponseMutation | None = None


@dataclass
class NetworkHookRule:
    """Single network interception rule."""

    name: str = ""
    """Optional rule name."""

    phase: NetworkHookPhase = ""
    """Rule phase: before or after (empty defaults to before server-side)."""

    hosts: list[str] = field(default_factory=list)
    """Host glob filters (empty = all hosts)."""

    methods: list[str] = field(default_factory=list)
    """HTTP method filters (empty = all methods)."""

    path: str = ""
    """URL path glob (empty = all paths)."""

    action: NetworkHookAction = "allow"
    """Rule action: allow, block, or mutate."""

    set_headers: dict[str, str] = field(default_factory=dict)
    """Before-phase request header upserts."""

    delete_headers: list[str] = field(default_factory=list)
    """Before-phase request headers to delete."""

    set_query: dict[str, str] = field(default_factory=dict)
    """Before-phase query key upserts."""

    delete_query: list[str] = field(default_factory=list)
    """Before-phase query keys to delete."""

    rewrite_path: str = ""
    """Before-phase request path rewrite target."""

    set_response_headers: dict[str, str] = field(default_factory=dict)
    """After-phase response header upserts."""

    delete_response_headers: list[str] = field(default_factory=list)
    """After-phase response headers to delete."""

    body_replacements: list[NetworkBodyTransform] = field(default_factory=list)
    """After-phase literal response-body replacements."""

    timeout_ms: int = 0
    """Timeout for SDK-local callback hooks in milliseconds."""

    hook: Callable[[NetworkHookRequest], NetworkHookResult | None] | None = None
    """SDK-local callback hook."""

    def to_dict(self) -> dict[str, Any]:
        d: dict[str, Any] = {"action": self.action}
        if self.name:
            d["name"] = self.name
        if self.phase:
            d["phase"] = self.phase
        if self.hosts:
            d["hosts"] = self.hosts
        if self.methods:
            d["methods"] = self.methods
        if self.path:
            d["path"] = self.path
        if self.set_headers:
            d["set_headers"] = self.set_headers
        if self.delete_headers:
            d["delete_headers"] = self.delete_headers
        if self.set_query:
            d["set_query"] = self.set_query
        if self.delete_query:
            d["delete_query"] = self.delete_query
        if self.rewrite_path:
            d["rewrite_path"] = self.rewrite_path
        if self.set_response_headers:
            d["set_response_headers"] = self.set_response_headers
        if self.delete_response_headers:
            d["delete_response_headers"] = self.delete_response_headers
        if self.body_replacements:
            d["body_replacements"] = [x.to_dict() for x in self.body_replacements]
        if self.timeout_ms > 0:
            d["timeout_ms"] = self.timeout_ms
        return d


@dataclass
class NetworkInterceptionConfig:
    """Host-side network interception configuration."""

    rules: list[NetworkHookRule] = field(default_factory=list)
    """Interception rules."""

    def to_dict(self) -> dict[str, Any]:
        d: dict[str, Any] = {}
        if self.rules:
            d["rules"] = [r.to_dict() for r in self.rules]
        return d


@dataclass
class VFSMutateRequest:
    """Input to SDK-local mutate hooks."""

    path: str
    size: int
    mode: int
    uid: int
    gid: int


@dataclass
class VFSActionRequest:
    """Input to SDK-local action hooks."""

    op: str
    path: str
    size: int
    mode: int
    uid: int
    gid: int


@dataclass
class VFSHookEvent:
    """Metadata delivered to SDK-local after hooks."""

    op: str
    path: str
    size: int
    mode: int
    uid: int
    gid: int


@dataclass
class Secret:
    """Secret to inject into the sandbox.

    The secret value is replaced with a placeholder env var in the sandbox.
    When HTTP requests are made to allowed hosts, the placeholder is replaced
    with the actual value by the MITM proxy.
    """

    name: str
    """Environment variable name (e.g., 'ANTHROPIC_API_KEY')."""

    value: str
    """The actual secret value."""

    hosts: list[str] = field(default_factory=list)
    """Hosts where this secret can be used (supports wildcards)."""


@dataclass
class ImageConfig:
    """OCI image metadata for user/entrypoint/cmd/workdir/env."""

    user: str = ""
    """Run as user (uid, uid:gid, or username)."""

    working_dir: str = ""
    """Working directory from the image."""

    entrypoint: list[str] = field(default_factory=list)
    """Image entrypoint."""

    cmd: list[str] = field(default_factory=list)
    """Image default command."""

    env: dict[str, str] = field(default_factory=dict)
    """Environment variables from the image."""

    def to_dict(self) -> dict[str, Any]:
        d: dict[str, Any] = {}
        if self.user:
            d["user"] = self.user
        if self.working_dir:
            d["working_dir"] = self.working_dir
        if self.entrypoint:
            d["entrypoint"] = self.entrypoint
        if self.cmd:
            d["cmd"] = self.cmd
        if self.env:
            d["env"] = self.env
        return d


@dataclass
class CreateOptions:
    """Options for creating a sandbox."""

    image: str = ""
    """Container image reference (required, e.g., alpine:latest)."""

    privileged: bool = False
    """Skip in-guest security restrictions (seccomp, cap drop, no_new_privs)."""

    cpus: float = 0
    """Number of vCPUs (supports fractional values, 0 = use default)."""

    memory_mb: int = 0
    """Memory in megabytes (0 = use default)."""

    disk_size_mb: int = 0
    """Disk size in megabytes (0 = use default)."""

    timeout_seconds: int = 0
    """Maximum execution time in seconds (0 = use default)."""

    allowed_hosts: list[str] = field(default_factory=list)
    """List of allowed network hosts (supports wildcards like *.example.com)."""

    block_private_ips: bool = False
    """Whether to block access to private IP ranges."""

    block_private_ips_set: bool = False
    """Whether block_private_ips was explicitly set by the caller."""

    no_network: bool = False
    """Disable guest network egress entirely (no guest NIC)."""

    force_interception: bool = False
    """Force network interception even when allow-list/secrets are empty."""

    network_interception: NetworkInterceptionConfig | None = None
    """Host-side network interception rules."""

    mounts: dict[str, MountConfig] = field(default_factory=dict)
    """VFS mount configurations keyed by guest path."""

    env: dict[str, str] = field(default_factory=dict)
    """Non-secret environment variables available to commands."""

    vfs_interception: VFSInterceptionConfig | None = None
    """Host-side VFS interception rules."""

    secrets: list[Secret] = field(default_factory=list)
    """Secrets to inject (replaced in HTTP requests to allowed hosts)."""

    workspace: str = ""
    """Guest mount point for VFS (default: /workspace)."""

    dns_servers: list[str] = field(default_factory=list)
    """DNS servers to use (default: 8.8.8.8, 8.8.4.4)."""

    hostname: str = ""
    """Guest hostname (default: sandbox's id)."""

    network_mtu: int = 0
    """Network MTU override (0 = use default)."""

    image_config: ImageConfig | None = None
    """OCI image metadata (USER, ENTRYPOINT, CMD, WORKDIR, ENV)."""

    launch_entrypoint: bool = False
    """Start image ENTRYPOINT/CMD in detached mode during create."""


@dataclass
class ExecResult:
    """Result of command execution."""

    exit_code: int
    """The command's exit code."""

    stdout: str
    """Standard output."""

    stderr: str
    """Standard error."""

    duration_ms: int
    """Execution time in milliseconds."""


@dataclass
class ExecStreamResult:
    """Result of streaming command execution.

    stdout/stderr are not included here because they were delivered
    in real-time via the callback/writers.
    """

    exit_code: int
    """The command's exit code."""

    duration_ms: int
    """Execution time in milliseconds."""


@dataclass
class ExecPipeResult:
    """Result of pipe-mode command execution."""

    exit_code: int
    """The command's exit code."""

    duration_ms: int
    """Execution time in milliseconds."""


@dataclass
class ExecInteractiveResult:
    """Result of interactive TTY command execution."""

    exit_code: int
    """The command's exit code."""

    duration_ms: int
    """Execution time in milliseconds."""


@dataclass
class FileInfo:
    """File metadata."""

    name: str
    """File name."""

    size: int
    """File size in bytes."""

    mode: int
    """File mode/permissions."""

    is_dir: bool
    """Whether this is a directory."""


@dataclass
class VolumeInfo:
    """Named volume metadata."""

    name: str
    """Volume name."""

    size: str
    """Human-readable volume size (for example, ``16.0 MB``)."""

    path: str
    """Host filesystem path to the backing ext4 image."""


class MatchlockError(Exception):
    """Base exception for Matchlock errors."""

    pass


class RPCError(MatchlockError):
    """Error from Matchlock RPC."""

    PARSE_ERROR = -32700
    INVALID_REQUEST = -32600
    METHOD_NOT_FOUND = -32601
    INVALID_PARAMS = -32602
    INTERNAL_ERROR = -32603
    VM_FAILED = -32000
    EXEC_FAILED = -32001
    FILE_FAILED = -32002

    def __init__(self, code: int, message: str):
        self.code = code
        self.message = message
        super().__init__(f"[{code}] {message}")

    def is_vm_error(self) -> bool:
        return self.code == self.VM_FAILED

    def is_exec_error(self) -> bool:
        return self.code == self.EXEC_FAILED

    def is_file_error(self) -> bool:
        return self.code == self.FILE_FAILED
