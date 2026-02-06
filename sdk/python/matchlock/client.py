"""Matchlock client implementation."""

import base64
import json
import os
import subprocess
from typing import Any

from .types import (
    Config,
    CreateOptions,
    ExecResult,
    FileInfo,
    MatchlockError,
    RPCError,
)


class Client:
    """Client for interacting with Matchlock sandboxes via JSON-RPC.
    
    Example:
        with Client() as client:
            client.create()
            result = client.exec("echo hello")
            print(result.stdout)
    """
    
    def __init__(self, config: Config | None = None):
        """Initialize the client.
        
        Args:
            config: Client configuration. If None, uses default config
                   with MATCHLOCK_BIN env var or "matchlock" binary.
        """
        if config is None:
            config = Config(
                binary_path=os.environ.get("MATCHLOCK_BIN", "matchlock"),
                use_sudo=True,
            )
        
        self._config = config
        self._process: subprocess.Popen | None = None
        self._request_id = 0
        self._vm_id: str | None = None
    
    def __enter__(self) -> "Client":
        self.start()
        return self
    
    def __exit__(self, exc_type: Any, exc_val: Any, exc_tb: Any) -> None:
        self.close()
    
    @property
    def vm_id(self) -> str | None:
        """Return the ID of the current VM, or None if not created."""
        return self._vm_id
    
    def start(self) -> None:
        """Start the matchlock RPC process."""
        if self._process is not None:
            return
        
        cmd = [self._config.binary_path, "rpc"]
        if self._config.use_sudo:
            cmd = ["sudo"] + cmd
        
        self._process = subprocess.Popen(
            cmd,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            bufsize=1,
        )
    
    def close(self) -> None:
        """Close the sandbox and cleanup resources."""
        if self._process is None or self._process.poll() is not None:
            return
        
        try:
            self._send_request("close")
        except Exception:
            pass
        
        try:
            self._process.terminate()
            self._process.wait(timeout=2)
        except Exception:
            try:
                self._process.kill()
                self._process.wait(timeout=1)
            except Exception:
                pass
        
        self._vm_id = None
        self._process = None
    
    def _send_request(self, method: str, params: dict[str, Any] | None = None) -> Any:
        """Send a JSON-RPC request and return the result."""
        if self._process is None or self._process.poll() is not None:
            raise MatchlockError("Matchlock process not running")
        
        self._request_id += 1
        request: dict[str, Any] = {
            "jsonrpc": "2.0",
            "method": method,
            "id": self._request_id,
        }
        if params:
            request["params"] = params
        
        request_json = json.dumps(request)
        assert self._process.stdin is not None
        self._process.stdin.write(request_json + "\n")
        self._process.stdin.flush()
        
        # Read response (skip event notifications)
        assert self._process.stdout is not None
        while True:
            line = self._process.stdout.readline()
            if not line:
                raise MatchlockError("Matchlock process closed unexpectedly")
            
            response = json.loads(line)
            
            # Skip notifications (no id field)
            if "id" not in response:
                continue
            
            if response.get("id") != self._request_id:
                continue
            
            if "error" in response and response["error"]:
                err = response["error"]
                raise RPCError(err["code"], err["message"])
            
            return response.get("result")
    
    def create(self, opts: CreateOptions | None = None) -> str:
        """Create and start a new sandbox VM.
        
        Args:
            opts: Creation options. If None, uses defaults.
        
        Returns:
            The VM ID.
        """
        if opts is None:
            opts = CreateOptions()
        
        params: dict[str, Any] = {
            "image": opts.image,
            "resources": {
                "cpus": opts.cpus,
                "memory_mb": opts.memory_mb,
                "disk_size_mb": opts.disk_size_mb,
                "timeout_seconds": opts.timeout_seconds,
            },
        }
        
        if opts.allowed_hosts or opts.block_private_ips or opts.secrets:
            network: dict[str, Any] = {
                "allowed_hosts": opts.allowed_hosts,
                "block_private_ips": opts.block_private_ips,
            }
            if opts.secrets:
                network["secrets"] = {
                    s.name: {"value": s.value, "hosts": s.hosts}
                    for s in opts.secrets
                }
            params["network"] = network
        
        if opts.mounts:
            params["vfs"] = {
                "mounts": {k: v.to_dict() for k, v in opts.mounts.items()}
            }
        
        result = self._send_request("create", params)
        self._vm_id = result["id"]
        return self._vm_id
    
    def exec(self, command: str, working_dir: str = "") -> ExecResult:
        """Execute a command in the sandbox.
        
        Args:
            command: The command to execute.
            working_dir: Optional working directory.
        
        Returns:
            ExecResult with exit code, stdout, stderr, and duration.
        """
        params: dict[str, str] = {"command": command}
        if working_dir:
            params["working_dir"] = working_dir
        
        result = self._send_request("exec", params)
        
        return ExecResult(
            exit_code=result["exit_code"],
            stdout=base64.b64decode(result["stdout"]).decode("utf-8", errors="replace"),
            stderr=base64.b64decode(result["stderr"]).decode("utf-8", errors="replace"),
            duration_ms=result["duration_ms"],
        )
    
    def write_file(self, path: str, content: bytes | str, mode: int = 0o644) -> None:
        """Write a file to the sandbox.
        
        Args:
            path: Path in the sandbox (e.g., /workspace/script.py).
            content: File content as bytes or string.
            mode: File permissions (default: 0644).
        """
        if isinstance(content, str):
            content = content.encode("utf-8")
        
        params = {
            "path": path,
            "content": base64.b64encode(content).decode("ascii"),
            "mode": mode,
        }
        self._send_request("write_file", params)
    
    def read_file(self, path: str) -> bytes:
        """Read a file from the sandbox.
        
        Args:
            path: Path in the sandbox.
        
        Returns:
            File content as bytes.
        """
        result = self._send_request("read_file", {"path": path})
        return base64.b64decode(result["content"])
    
    def list_files(self, path: str) -> list[FileInfo]:
        """List files in a directory.
        
        Args:
            path: Directory path in the sandbox.
        
        Returns:
            List of FileInfo objects.
        """
        result = self._send_request("list_files", {"path": path})
        return [
            FileInfo(
                name=f["name"],
                size=f["size"],
                mode=f["mode"],
                is_dir=f["is_dir"],
            )
            for f in result.get("files", [])
        ]
