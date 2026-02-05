"""Type definitions for the Matchlock SDK."""

from dataclasses import dataclass, field
from typing import Any


@dataclass
class Config:
    """Client configuration."""
    
    binary_path: str = "matchlock"
    """Path to the matchlock binary."""
    
    use_sudo: bool = True
    """Whether to run matchlock with sudo (required for TAP devices)."""


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
        """Convert to dictionary for JSON serialization."""
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
    
    image: str = "standard"
    """Rootfs image variant: minimal, standard, or full."""
    
    cpus: int = 1
    """Number of vCPUs."""
    
    memory_mb: int = 512
    """Memory in megabytes."""
    
    timeout_seconds: int = 300
    """Maximum execution time in seconds."""
    
    allowed_hosts: list[str] = field(default_factory=list)
    """List of allowed network hosts (supports wildcards like *.example.com)."""
    
    block_private_ips: bool = True
    """Whether to block access to private IP ranges."""
    
    mounts: dict[str, MountConfig] = field(default_factory=dict)
    """VFS mount configurations keyed by guest path."""
    
    secrets: list[Secret] = field(default_factory=list)
    """Secrets to inject (replaced in HTTP requests to allowed hosts)."""


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
    
    # Error codes
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
        """Returns True if this is a VM-related error."""
        return self.code == self.VM_FAILED
    
    def is_exec_error(self) -> bool:
        """Returns True if this is an execution error."""
        return self.code == self.EXEC_FAILED
    
    def is_file_error(self) -> bool:
        """Returns True if this is a file operation error."""
        return self.code == self.FILE_FAILED
