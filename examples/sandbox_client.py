#!/usr/bin/env python3
# /// script
# requires-python = ">=3.10"
# dependencies = []
# ///
"""
Matchlock Python Client

A simple client for interacting with Matchlock sandbox via JSON-RPC.
Run matchlock in RPC mode: sudo matchlock --rpc

Usage:
    uv run examples/sandbox_client.py
"""

import base64
import json
import os
import subprocess
import sys
from dataclasses import dataclass
from typing import Any


@dataclass
class ExecResult:
    exit_code: int
    stdout: str
    stderr: str
    duration_ms: int


@dataclass
class FileInfo:
    name: str
    size: int
    mode: int
    is_dir: bool


class MatchlockError(Exception):
    """Error from Matchlock RPC"""
    def __init__(self, code: int, message: str):
        self.code = code
        self.message = message
        super().__init__(f"[{code}] {message}")


class MatchlockClient:
    """Client for Matchlock sandbox via JSON-RPC over subprocess stdin/stdout"""
    
    def __init__(self, matchlock_path: str | None = None):
        if matchlock_path is None:
            matchlock_path = os.environ.get("MATCHLOCK_BIN", "./bin/matchlock")
        self.matchlock_path = matchlock_path
        self.process: subprocess.Popen | None = None
        self.request_id = 0
        self.vm_id: str | None = None
    
    def __enter__(self):
        self.start()
        return self
    
    def __exit__(self, exc_type, exc_val, exc_tb):
        self.close()
    
    def start(self):
        """Start the matchlock RPC process"""
        self.process = subprocess.Popen(
            ["sudo", self.matchlock_path, "--rpc"],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            bufsize=1,
        )
    
    def _send_request(self, method: str, params: dict | None = None) -> Any:
        """Send a JSON-RPC request and return the result"""
        if not self.process or self.process.poll() is not None:
            raise RuntimeError("Matchlock process not running")
        
        self.request_id += 1
        request = {
            "jsonrpc": "2.0",
            "method": method,
            "id": self.request_id,
        }
        if params:
            request["params"] = params
        
        request_json = json.dumps(request)
        self.process.stdin.write(request_json + "\n")
        self.process.stdin.flush()
        
        # Read response (skip event notifications)
        while True:
            line = self.process.stdout.readline()
            if not line:
                raise RuntimeError("Matchlock process closed unexpectedly")
            
            response = json.loads(line)
            
            # Skip notifications (no id field)
            if "id" not in response:
                continue
            
            if response.get("id") != self.request_id:
                continue
            
            if "error" in response and response["error"]:
                err = response["error"]
                raise MatchlockError(err["code"], err["message"])
            
            return response.get("result")
    
    def create(
        self,
        image: str = "standard",
        cpus: int = 1,
        memory_mb: int = 512,
        timeout_seconds: int = 300,
        allowed_hosts: list[str] | None = None,
        mounts: dict[str, dict] | None = None,
    ) -> str:
        """Create and start a new sandbox VM"""
        params = {
            "image": image,
            "resources": {
                "cpus": cpus,
                "memory_mb": memory_mb,
                "timeout_seconds": timeout_seconds,
            },
        }
        
        if allowed_hosts:
            params["network"] = {
                "allowed_hosts": allowed_hosts,
                "block_private_ips": True,
            }
        
        if mounts:
            params["vfs"] = {"mounts": mounts}
        
        result = self._send_request("create", params)
        self.vm_id = result["id"]
        return self.vm_id
    
    def exec(self, command: str, working_dir: str = "") -> ExecResult:
        """Execute a command in the sandbox"""
        params = {"command": command}
        if working_dir:
            params["working_dir"] = working_dir
        
        result = self._send_request("exec", params)
        
        return ExecResult(
            exit_code=result["exit_code"],
            stdout=base64.b64decode(result["stdout"]).decode("utf-8", errors="replace"),
            stderr=base64.b64decode(result["stderr"]).decode("utf-8", errors="replace"),
            duration_ms=result["duration_ms"],
        )
    
    def write_file(self, path: str, content: str | bytes, mode: int = 0o644):
        """Write a file to the sandbox"""
        if isinstance(content, str):
            content = content.encode("utf-8")
        
        params = {
            "path": path,
            "content": base64.b64encode(content).decode("ascii"),
            "mode": mode,
        }
        self._send_request("write_file", params)
    
    def read_file(self, path: str) -> bytes:
        """Read a file from the sandbox"""
        result = self._send_request("read_file", {"path": path})
        return base64.b64decode(result["content"])
    
    def list_files(self, path: str) -> list[FileInfo]:
        """List files in a directory"""
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
    
    def close(self):
        """Close the sandbox and cleanup"""
        if self.process and self.process.poll() is None:
            try:
                self._send_request("close")
            except Exception:
                pass
            try:
                self.process.terminate()
                self.process.wait(timeout=2)
            except Exception:
                self.process.kill()
                self.process.wait(timeout=1)
        self.vm_id = None


def main():
    """Example usage of the Matchlock client"""
    print("=== Matchlock Python Client Example ===\n")
    
    with MatchlockClient() as client:
        # Create a sandbox
        print("Creating sandbox...")
        vm_id = client.create(
            image="standard",
            cpus=1,
            memory_mb=512,
        )
        print(f"Created VM: {vm_id}\n")
        
        # Execute a simple command
        print("Running 'echo Hello from sandbox!'...")
        result = client.exec("echo 'Hello from sandbox!'")
        print(f"  stdout: {result.stdout.strip()}")
        print(f"  exit_code: {result.exit_code}")
        print(f"  duration: {result.duration_ms}ms\n")
        
        # Write a Python script to the sandbox
        script = '''
import sys
print(f"Python version: {sys.version}")
print("Arguments:", sys.argv[1:])
for i in range(3):
    print(f"Count: {i}")
'''
        print("Writing Python script to /workspace/test.py...")
        client.write_file("/workspace/test.py", script)
        
        # Execute the script
        print("Running the script...")
        result = client.exec("python3 /workspace/test.py arg1 arg2")
        print(f"  stdout:\n{result.stdout}")
        if result.stderr:
            print(f"  stderr: {result.stderr}")
        print(f"  exit_code: {result.exit_code}\n")
        
        # List files
        print("Listing /workspace...")
        files = client.list_files("/workspace")
        for f in files:
            ftype = "dir" if f.is_dir else "file"
            print(f"  {f.name} ({ftype}, {f.size} bytes)")
        print()
        
        # Read the file back
        print("Reading /workspace/test.py back...")
        content = client.read_file("/workspace/test.py")
        print(f"  Content length: {len(content)} bytes")
        print(f"  First line: {content.decode().split(chr(10))[0]}\n")
        
        # Test error handling
        print("Testing command that fails...")
        result = client.exec("exit 42")
        print(f"  exit_code: {result.exit_code}\n")
        
        print("Closing sandbox...")
    
    print("Done!")


if __name__ == "__main__":
    main()
