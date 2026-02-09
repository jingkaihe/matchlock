"""Matchlock client implementation.

Usage with builder API:

    from matchlock import Client, Sandbox

    sandbox = Sandbox("python:3.12-alpine") \\
        .allow_host("api.openai.com") \\
        .add_secret("API_KEY", os.environ["API_KEY"], "api.openai.com")

    with Client() as client:
        vm_id = client.launch(sandbox)
        result = client.exec("echo hello")
        print(result.stdout)
"""

import base64
import json
import os
import subprocess
import threading
from typing import IO, Any, Callable

from .builder import Sandbox
from .types import (
    Config,
    CreateOptions,
    ExecResult,
    ExecStreamResult,
    FileInfo,
    MatchlockError,
    RPCError,
)


class _PendingRequest:
    __slots__ = ("event", "result", "error", "on_notification")

    def __init__(
        self,
        on_notification: Callable[[str, dict[str, Any]], None] | None = None,
    ) -> None:
        self.event = threading.Event()
        self.result: Any = None
        self.error: Exception | None = None
        self.on_notification = on_notification


class Client:
    """Client for interacting with Matchlock sandboxes via JSON-RPC.

    All public methods are thread-safe.
    """

    def __init__(self, config: Config | None = None):
        if config is None:
            config = Config(
                binary_path=os.environ.get("MATCHLOCK_BIN", "matchlock"),
            )

        self._config = config
        self._process: subprocess.Popen[str] | None = None
        self._request_id = 0
        self._id_lock = threading.Lock()
        self._vm_id: str | None = None

        self._write_lock = threading.Lock()
        self._pending_lock = threading.Lock()
        self._pending: dict[int, _PendingRequest] = {}
        self._reader_thread: threading.Thread | None = None
        self._closed = False
        self._last_vm_id: str | None = None

    def __enter__(self) -> "Client":
        self.start()
        return self

    def __exit__(self, exc_type: Any, exc_val: Any, exc_tb: Any) -> None:
        self.close()

    @property
    def vm_id(self) -> str | None:
        return self._vm_id

    def start(self) -> None:
        if self._process is not None:
            return

        cmd = [self._config.binary_path, "rpc"]
        if self._config.use_sudo:
            cmd = ["sudo"] + cmd

        self._process = subprocess.Popen(
            cmd,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            text=True,
            bufsize=1,
        )

        self._reader_thread = threading.Thread(target=self._reader_loop, daemon=True)
        self._reader_thread.start()

    def close(self, timeout: float = 0) -> None:
        """Close the sandbox and clean up resources.

        Args:
            timeout: Seconds to wait for the process to exit. 0 kills the
                process immediately. When a non-zero timeout expires the
                process is forcefully killed.
        """
        if self._closed:
            return
        self._closed = True
        self._last_vm_id = self._vm_id

        if self._process is None or self._process.poll() is not None:
            return

        if timeout <= 0:
            try:
                assert self._process.stdin is not None
                self._process.stdin.close()
            except Exception:
                pass
            try:
                self._process.kill()
                self._process.wait(timeout=1)
            except Exception:
                pass
            return

        try:
            self._send_request("close", {"timeout_seconds": timeout})
        except Exception:
            pass

        try:
            assert self._process.stdin is not None
            self._process.stdin.close()
        except Exception:
            pass

        try:
            self._process.wait(timeout=timeout)
        except Exception:
            try:
                self._process.kill()
                self._process.wait(timeout=1)
            except Exception:
                pass

    def remove(self) -> None:
        """Remove the stopped VM state directory.

        Must be called after close(). Uses the matchlock CLI binary
        configured in Config.binary_path.
        """
        vm_id = self._vm_id or self._last_vm_id
        if not vm_id:
            return
        subprocess.run(
            [self._config.binary_path, "rm", vm_id],
            capture_output=True,
            text=True,
            check=True,
        )

    # ── Reader loop ──────────────────────────────────────────────────

    def _reader_loop(self) -> None:
        assert self._process is not None
        assert self._process.stdout is not None
        stdout = self._process.stdout

        while True:
            line = stdout.readline()
            if not line:
                with self._pending_lock:
                    err = MatchlockError("Matchlock process closed unexpectedly")
                    for p in self._pending.values():
                        p.error = err
                        p.event.set()
                    self._pending.clear()
                return

            try:
                msg = json.loads(line)
            except json.JSONDecodeError:
                continue

            msg_id = msg.get("id")

            if msg_id is None:
                self._handle_notification(msg)
                continue

            with self._pending_lock:
                pending = self._pending.get(msg_id)

            if pending is None:
                continue

            if msg.get("error"):
                err_data = msg["error"]
                pending.error = RPCError(err_data["code"], err_data["message"])
            else:
                pending.result = msg.get("result")

            pending.event.set()

    def _handle_notification(self, msg: dict[str, Any]) -> None:
        method = msg.get("method", "")
        if method not in ("exec_stream.stdout", "exec_stream.stderr"):
            return

        params = msg.get("params", {})
        req_id = params.get("id")
        if req_id is None:
            return

        with self._pending_lock:
            pending = self._pending.get(req_id)

        if pending is not None and pending.on_notification is not None:
            pending.on_notification(method, params)

    # ── RPC transport ────────────────────────────────────────────────

    def _next_id(self) -> int:
        with self._id_lock:
            self._request_id += 1
            return self._request_id

    def _send_request(
        self,
        method: str,
        params: dict[str, Any] | None = None,
        on_notification: Callable[[str, dict[str, Any]], None] | None = None,
    ) -> Any:
        if self._process is None or self._process.poll() is not None:
            raise MatchlockError("Matchlock process not running")

        req_id = self._next_id()
        pending = _PendingRequest(on_notification=on_notification)

        with self._pending_lock:
            self._pending[req_id] = pending

        try:
            request: dict[str, Any] = {
                "jsonrpc": "2.0",
                "method": method,
                "id": req_id,
            }
            if params:
                request["params"] = params

            data = json.dumps(request) + "\n"

            with self._write_lock:
                assert self._process.stdin is not None
                self._process.stdin.write(data)
                self._process.stdin.flush()

            pending.event.wait()

            if pending.error is not None:
                raise pending.error

            return pending.result
        finally:
            with self._pending_lock:
                self._pending.pop(req_id, None)

    # ── Public API ───────────────────────────────────────────────────

    def create(self, opts: CreateOptions | None = None) -> str:
        if opts is None:
            opts = CreateOptions()

        if not opts.image:
            raise MatchlockError("image is required (e.g., alpine:latest)")

        params: dict[str, Any] = {"image": opts.image}

        resources: dict[str, Any] = {}
        if opts.cpus:
            resources["cpus"] = opts.cpus
        if opts.memory_mb:
            resources["memory_mb"] = opts.memory_mb
        if opts.disk_size_mb:
            resources["disk_size_mb"] = opts.disk_size_mb
        if opts.timeout_seconds:
            resources["timeout_seconds"] = opts.timeout_seconds
        if resources:
            params["resources"] = resources

        if (
            opts.allowed_hosts
            or opts.block_private_ips
            or opts.secrets
            or opts.dns_servers
        ):
            network: dict[str, Any] = {
                "allowed_hosts": opts.allowed_hosts,
                "block_private_ips": opts.block_private_ips,
            }
            if opts.secrets:
                network["secrets"] = {
                    s.name: {"value": s.value, "hosts": s.hosts} for s in opts.secrets
                }
            if opts.dns_servers:
                network["dns_servers"] = opts.dns_servers
            params["network"] = network

        if opts.mounts or opts.workspace:
            vfs: dict[str, Any] = {}
            if opts.mounts:
                vfs["mounts"] = {k: v.to_dict() for k, v in opts.mounts.items()}
            if opts.workspace:
                vfs["workspace"] = opts.workspace
            params["vfs"] = vfs

        result = self._send_request("create", params)
        self._vm_id = result["id"]
        return self._vm_id

    def launch(self, sandbox: Sandbox) -> str:
        return self.create(sandbox.options())

    def exec(self, command: str, working_dir: str = "") -> ExecResult:
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

    def exec_stream(
        self,
        command: str,
        stdout: IO[str] | None = None,
        stderr: IO[str] | None = None,
        working_dir: str = "",
    ) -> ExecStreamResult:
        """Execute a command and stream stdout/stderr in real-time.

        Args:
            command: The command to execute.
            stdout: File-like object to write stdout to (e.g., sys.stdout).
            stderr: File-like object to write stderr to (e.g., sys.stderr).
            working_dir: Optional working directory.

        Returns:
            ExecStreamResult with exit code and duration (no stdout/stderr).
        """
        params: dict[str, str] = {"command": command}
        if working_dir:
            params["working_dir"] = working_dir

        def on_notification(method: str, notif_params: dict[str, Any]) -> None:
            data_b64 = notif_params.get("data", "")
            try:
                decoded = base64.b64decode(data_b64).decode("utf-8", errors="replace")
            except Exception:
                return
            if method == "exec_stream.stdout" and stdout is not None:
                stdout.write(decoded)
                stdout.flush()
            elif method == "exec_stream.stderr" and stderr is not None:
                stderr.write(decoded)
                stderr.flush()

        result = self._send_request(
            "exec_stream", params, on_notification=on_notification
        )

        return ExecStreamResult(
            exit_code=result["exit_code"],
            duration_ms=result["duration_ms"],
        )

    def write_file(self, path: str, content: bytes | str, mode: int = 0o644) -> None:
        if isinstance(content, str):
            content = content.encode("utf-8")

        params: dict[str, Any] = {
            "path": path,
            "content": base64.b64encode(content).decode("ascii"),
            "mode": mode,
        }
        self._send_request("write_file", params)

    def read_file(self, path: str) -> bytes:
        result = self._send_request("read_file", {"path": path})
        return base64.b64decode(result["content"])

    def list_files(self, path: str) -> list[FileInfo]:
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
