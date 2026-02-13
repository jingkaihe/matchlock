"""Type definitions for the Matchlock SDK."""

from dataclasses import dataclass, field
from typing import Any, Callable


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
    """Mount type: memory, real_fs, or overlay."""

    host_path: str = ""
    """Host path for real_fs mounts."""

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

    phase: str = ""
    """Rule phase: before or after (empty defaults to before server-side)."""

    ops: list[str] = field(default_factory=list)
    """Operation filters: read, write, create, ... (empty = all)."""

    path: str = ""
    """filepath-style glob pattern (empty = all)."""

    action: str = "allow"
    """Action: allow, block, mutate_write, exec_after."""

    data: str = ""
    """Replacement payload when action=mutate_write."""

    command: str = ""
    """Command to run when action=exec_after."""

    timeout_ms: int = 0
    """Timeout for command action in milliseconds."""

    hook: Callable[[Any], Any] | None = None
    """SDK-local after-hook callback: hook(client) -> Any."""

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
        if self.data:
            d["data"] = self.data
        if self.command:
            d["command"] = self.command
        if self.timeout_ms > 0:
            d["timeout_ms"] = self.timeout_ms
        return d


@dataclass
class VFSInterceptionConfig:
    """Host-side VFS interception configuration."""

    max_exec_depth: int = 0
    """Maximum nested hook-triggered exec depth (0 = server default)."""

    emit_events: bool = False
    """Emit file-operation events from host-side VFS interception."""

    rules: list[VFSHookRule] = field(default_factory=list)
    """Interception rules."""

    def to_dict(self) -> dict[str, Any]:
        d: dict[str, Any] = {}
        if self.max_exec_depth > 0:
            d["max_exec_depth"] = self.max_exec_depth
        if self.emit_events:
            d["emit_events"] = True
        if self.rules:
            d["rules"] = [r.to_dict() for r in self.rules]
        return d


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

    cpus: int = 0
    """Number of vCPUs (0 = use default)."""

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

    mounts: dict[str, MountConfig] = field(default_factory=dict)
    """VFS mount configurations keyed by guest path."""

    vfs_interception: VFSInterceptionConfig | None = None
    """Host-side VFS interception rules."""

    secrets: list[Secret] = field(default_factory=list)
    """Secrets to inject (replaced in HTTP requests to allowed hosts)."""

    workspace: str = ""
    """Guest mount point for VFS (default: /workspace)."""

    dns_servers: list[str] = field(default_factory=list)
    """DNS servers to use (default: 8.8.8.8, 8.8.4.4)."""

    image_config: ImageConfig | None = None
    """OCI image metadata (USER, ENTRYPOINT, CMD, WORKDIR, ENV)."""


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
