"""Type definitions for the Matchlock SDK."""

from dataclasses import dataclass, field
from typing import Any


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

    secrets: list[Secret] = field(default_factory=list)
    """Secrets to inject (replaced in HTTP requests to allowed hosts)."""

    workspace: str = ""
    """Guest mount point for VFS (default: /workspace)."""


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


@dataclass
class BuildResult:
    """Result of an image build or import operation."""

    rootfs_path: str
    """Path to the built rootfs image."""

    digest: str
    """Image digest."""

    size: int
    """Image size in bytes."""

    cached: bool = False
    """Whether the result came from cache."""


@dataclass
class ImageInfo:
    """Information about a cached image."""

    tag: str
    """Image tag."""

    source: str
    """Image source (local, registry, import, tag)."""

    size: int
    """Image size in bytes."""

    created_at: str
    """Creation timestamp."""

    digest: str = ""
    """Image digest."""


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
    IMAGE_FAILED = -32003

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

    def is_image_error(self) -> bool:
        return self.code == self.IMAGE_FAILED
