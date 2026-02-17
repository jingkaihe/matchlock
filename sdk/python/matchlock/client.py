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
import fnmatch
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
    VFSActionRequest,
    VFSHookEvent,
    VFS_HOOK_ACTION_ALLOW,
    VFS_HOOK_ACTION_BLOCK,
    VFSInterceptionConfig,
    VFS_HOOK_PHASE_AFTER,
    VFS_HOOK_PHASE_BEFORE,
    VFSMutateRequest,
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


class _LocalVFSHook:
    __slots__ = ("name", "ops", "path", "timeout_ms", "dangerous", "hook")

    def __init__(
        self,
        name: str,
        ops: set[str],
        path: str,
        timeout_ms: int,
        dangerous: bool,
        hook: Callable[..., Any],
    ) -> None:
        self.name = name
        self.ops = ops
        self.path = path
        self.timeout_ms = timeout_ms
        self.dangerous = dangerous
        self.hook = hook


class _LocalVFSMutateHook:
    __slots__ = ("name", "ops", "path", "hook")

    def __init__(
        self,
        name: str,
        ops: set[str],
        path: str,
        hook: Callable[[VFSMutateRequest], bytes | str | None],
    ) -> None:
        self.name = name
        self.ops = ops
        self.path = path
        self.hook = hook


class _LocalVFSActionHook:
    __slots__ = ("name", "ops", "path", "hook")

    def __init__(
        self,
        name: str,
        ops: set[str],
        path: str,
        hook: Callable[[VFSActionRequest], str],
    ) -> None:
        self.name = name
        self.ops = ops
        self.path = path
        self.hook = hook


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

        self._vfs_hooks: list[_LocalVFSHook] = []
        self._vfs_mutate_hooks: list[_LocalVFSMutateHook] = []
        self._vfs_action_hooks: list[_LocalVFSActionHook] = []
        self._vfs_hook_active = False
        self._vfs_hook_lock = threading.Lock()

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
            timeout: Seconds to wait for the process to exit. 0 uses a short
                grace period and then force-kills if needed. When a non-zero
                timeout expires the process is forcefully killed.
        """
        if self._closed:
            return
        self._closed = True
        self._last_vm_id = self._vm_id
        self._set_local_vfs_hooks([], [], [])

        if self._process is None or self._process.poll() is not None:
            return

        effective_timeout = timeout if timeout and timeout > 0 else 2.0

        try:
            self._send_request(
                "close",
                {"timeout_seconds": effective_timeout},
                timeout=effective_timeout + 1.0,
            )
        except Exception:
            pass

        try:
            assert self._process.stdin is not None
            self._process.stdin.close()
        except Exception:
            pass

        try:
            self._process.wait(timeout=effective_timeout)
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
        if method == "event":
            params = msg.get("params", {})
            self._handle_event_notification(params)
            return

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

    def _handle_event_notification(self, params: dict[str, Any]) -> None:
        file_event = params.get("file")
        if not isinstance(file_event, dict):
            return
        op = str(file_event.get("op", "")).lower()
        path = str(file_event.get("path", ""))
        size = int(file_event.get("size") or 0)
        mode = int(file_event.get("mode") or 0)
        uid = int(file_event.get("uid") or 0)
        gid = int(file_event.get("gid") or 0)
        if not op:
            return
        self._handle_vfs_file_event(op, path, size, mode, uid, gid)

    def _handle_vfs_file_event(
        self, op: str, path: str, size: int, mode: int, uid: int, gid: int
    ) -> None:
        with self._vfs_hook_lock:
            hooks = list(self._vfs_hooks)
            active = self._vfs_hook_active
        if not hooks:
            return

        event = VFSHookEvent(op=op, path=path, size=size, mode=mode, uid=uid, gid=gid)

        safe_hooks: list[_LocalVFSHook] = []
        for hook in hooks:
            if hook.ops and op not in hook.ops:
                continue
            if hook.path and not fnmatch.fnmatch(path, hook.path):
                continue
            if hook.dangerous:
                t = threading.Thread(
                    target=self._run_single_vfs_hook, args=(hook, event), daemon=True
                )
                t.start()
                continue
            safe_hooks.append(hook)

        if not safe_hooks:
            return
        if active:
            return

        t = threading.Thread(
            target=self._run_vfs_safe_hooks_for_event,
            args=(safe_hooks, event),
            daemon=True,
        )
        t.start()

    def _run_vfs_safe_hooks_for_event(
        self, hooks: list[_LocalVFSHook], event: VFSHookEvent
    ) -> None:
        with self._vfs_hook_lock:
            if self._vfs_hook_active:
                return
            self._vfs_hook_active = True

        try:
            for hook in hooks:
                self._run_single_vfs_hook(hook, event)
        finally:
            with self._vfs_hook_lock:
                self._vfs_hook_active = False

    def _run_single_vfs_hook(self, hook: _LocalVFSHook, event: VFSHookEvent) -> None:
        try:
            if hook.dangerous:
                hook.hook(self, event)
            else:
                hook.hook(event)
        except Exception:
            pass

    def _apply_local_write_mutations(
        self, path: str, content: bytes, mode: int
    ) -> bytes:
        with self._vfs_hook_lock:
            hooks = list(self._vfs_mutate_hooks)

        if not hooks:
            return content

        uid_fn = getattr(os, "geteuid", None)
        gid_fn = getattr(os, "getegid", None)
        uid = int(uid_fn()) if callable(uid_fn) else 0
        gid = int(gid_fn()) if callable(gid_fn) else 0

        current = content
        for hook in hooks:
            if hook.ops and "write" not in hook.ops:
                continue
            if hook.path and not fnmatch.fnmatch(path, hook.path):
                continue

            request = VFSMutateRequest(
                path=path, size=len(current), mode=mode, uid=uid, gid=gid
            )
            mutated = hook.hook(request)
            if mutated is None:
                continue
            if isinstance(mutated, str):
                current = mutated.encode("utf-8")
                continue
            if not isinstance(mutated, bytes):
                raise MatchlockError(
                    f"invalid mutate_hook return type for {hook.name!r}: expected bytes|str|None"
                )
            current = mutated

        return current

    def _apply_local_action_hooks(
        self, op: str, path: str, size: int, mode: int
    ) -> None:
        with self._vfs_hook_lock:
            hooks = list(self._vfs_action_hooks)

        if not hooks:
            return

        uid_fn = getattr(os, "geteuid", None)
        gid_fn = getattr(os, "getegid", None)
        uid = int(uid_fn()) if callable(uid_fn) else 0
        gid = int(gid_fn()) if callable(gid_fn) else 0

        req = VFSActionRequest(op=op, path=path, size=size, mode=mode, uid=uid, gid=gid)
        for hook in hooks:
            if hook.ops and op not in hook.ops:
                continue
            if hook.path and not fnmatch.fnmatch(path, hook.path):
                continue

            decision = str(hook.hook(req)).strip().lower()
            if decision in ("", VFS_HOOK_ACTION_ALLOW):
                continue
            if decision == VFS_HOOK_ACTION_BLOCK:
                raise MatchlockError(
                    f"vfs action hook blocked operation: op={op} path={path} hook={hook.name!r}"
                )
            raise MatchlockError(
                f"invalid action_hook return value for {hook.name!r}: expected "
                f"{VFS_HOOK_ACTION_ALLOW!r}|{VFS_HOOK_ACTION_BLOCK!r}, got {decision!r}"
            )

    def _set_local_vfs_hooks(
        self,
        hooks: list[_LocalVFSHook],
        mutate_hooks: list[_LocalVFSMutateHook],
        action_hooks: list[_LocalVFSActionHook],
    ) -> None:
        with self._vfs_hook_lock:
            self._vfs_hooks = hooks
            self._vfs_mutate_hooks = mutate_hooks
            self._vfs_action_hooks = action_hooks
            self._vfs_hook_active = False

    def _compile_vfs_hooks(
        self, cfg: VFSInterceptionConfig | None
    ) -> tuple[
        VFSInterceptionConfig | None,
        list[_LocalVFSHook],
        list[_LocalVFSMutateHook],
        list[_LocalVFSActionHook],
    ]:
        if cfg is None:
            return None, [], [], []

        wire = VFSInterceptionConfig(
            emit_events=cfg.emit_events,
            rules=[],
        )
        local: list[_LocalVFSHook] = []
        local_mutate: list[_LocalVFSMutateHook] = []
        local_action: list[_LocalVFSActionHook] = []

        for rule in cfg.rules:
            callbacks = [
                rule.hook,
                rule.dangerous_hook,
                rule.mutate_hook,
                rule.action_hook,
            ]
            callback_count = sum(1 for cb in callbacks if cb is not None)
            if callback_count > 1:
                raise MatchlockError(
                    f"invalid vfs hook {rule.name!r}: cannot set more than one callback hook"
                )

            if (
                rule.hook is None
                and rule.dangerous_hook is None
                and rule.mutate_hook is None
                and rule.action_hook is None
            ):
                action = (rule.action or "").strip().lower()
                if action == "mutate_write":
                    raise MatchlockError(
                        f"invalid vfs hook {rule.name!r}: mutate_write requires mutate_hook callback"
                    )
                wire.rules.append(rule)
                continue

            if rule.hook is not None:
                action = (rule.action or "").strip().lower()
                if action not in ("", VFS_HOOK_ACTION_ALLOW):
                    raise MatchlockError(
                        f"invalid vfs hook {rule.name!r}: callback hooks cannot set action={rule.action!r}"
                    )
                if rule.phase.lower() != VFS_HOOK_PHASE_AFTER:
                    raise MatchlockError(
                        f"invalid vfs hook {rule.name!r}: callback hooks must use phase=after"
                    )

                ops = {op.lower() for op in rule.ops if op}
                local.append(
                    _LocalVFSHook(
                        name=rule.name,
                        ops=ops,
                        path=rule.path,
                        timeout_ms=rule.timeout_ms,
                        dangerous=False,
                        hook=rule.hook,
                    )
                )
                continue

            if rule.dangerous_hook is not None:
                action = (rule.action or "").strip().lower()
                if action not in ("", VFS_HOOK_ACTION_ALLOW):
                    raise MatchlockError(
                        f"invalid vfs hook {rule.name!r}: dangerous_hook cannot set action={rule.action!r}"
                    )
                if rule.phase.lower() != VFS_HOOK_PHASE_AFTER:
                    raise MatchlockError(
                        f"invalid vfs hook {rule.name!r}: dangerous_hook must use phase=after"
                    )

                ops = {op.lower() for op in rule.ops if op}
                local.append(
                    _LocalVFSHook(
                        name=rule.name,
                        ops=ops,
                        path=rule.path,
                        timeout_ms=rule.timeout_ms,
                        dangerous=True,
                        hook=rule.dangerous_hook,
                    )
                )
                continue

            if rule.action_hook is not None:
                action = (rule.action or "").strip().lower()
                if action not in ("", VFS_HOOK_ACTION_ALLOW):
                    raise MatchlockError(
                        f"invalid vfs hook {rule.name!r}: action_hook cannot set action={rule.action!r}"
                    )
                if rule.phase and rule.phase.lower() != VFS_HOOK_PHASE_BEFORE:
                    raise MatchlockError(
                        f"invalid vfs hook {rule.name!r}: action_hook must use phase=before"
                    )
                ops = {op.lower() for op in rule.ops if op}
                local_action.append(
                    _LocalVFSActionHook(
                        name=rule.name,
                        ops=ops,
                        path=rule.path,
                        hook=rule.action_hook,
                    )
                )
                continue

            action = (rule.action or "").strip().lower()
            if action not in ("", VFS_HOOK_ACTION_ALLOW):
                raise MatchlockError(
                    f"invalid vfs hook {rule.name!r}: mutate_hook cannot set action={rule.action!r}"
                )
            if rule.phase and rule.phase.lower() != VFS_HOOK_PHASE_BEFORE:
                raise MatchlockError(
                    f"invalid vfs hook {rule.name!r}: mutate_hook must use phase=before"
                )
            assert rule.mutate_hook is not None
            ops = {op.lower() for op in rule.ops if op}
            local_mutate.append(
                _LocalVFSMutateHook(
                    name=rule.name,
                    ops=ops,
                    path=rule.path,
                    hook=rule.mutate_hook,
                )
            )

        if local:
            wire.emit_events = True
        wire_out: VFSInterceptionConfig | None = wire
        if not wire.rules and not wire.emit_events:
            wire_out = None
        return wire_out, local, local_mutate, local_action

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
        timeout: float | None = None,
    ) -> Any:
        """Send a JSON-RPC request and wait for the result.

        Args:
            method: The RPC method name.
            params: Optional parameters for the method.
            on_notification: Callback for streaming notifications.
            timeout: Optional timeout in seconds. If the request doesn't
                complete within the timeout, a ``cancel`` RPC is sent and
                :class:`TimeoutError` is raised.
        """
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

            if not pending.event.wait(timeout=timeout):
                self._send_cancel(req_id)
                raise TimeoutError(
                    f"request {method} (id={req_id}) timed out after {timeout}s"
                )

            if pending.error is not None:
                raise pending.error

            return pending.result
        finally:
            with self._pending_lock:
                self._pending.pop(req_id, None)

    def _send_cancel(self, target_id: int) -> None:
        """Send a fire-and-forget cancel RPC for the given request ID."""
        cancel_id = self._next_id()
        request: dict[str, Any] = {
            "jsonrpc": "2.0",
            "method": "cancel",
            "params": {"id": target_id},
            "id": cancel_id,
        }
        data = json.dumps(request) + "\n"
        try:
            with self._write_lock:
                assert self._process is not None
                assert self._process.stdin is not None
                self._process.stdin.write(data)
                self._process.stdin.flush()
        except Exception:
            pass

    # ── Public API ───────────────────────────────────────────────────

    def create(self, opts: CreateOptions | None = None) -> str:
        if opts is None:
            opts = CreateOptions()

        if not opts.image:
            raise MatchlockError("image is required (e.g., alpine:latest)")
        if opts.network_mtu < 0:
            raise MatchlockError("network mtu must be > 0")

        (
            wire_vfs,
            local_hooks,
            local_mutate_hooks,
            local_action_hooks,
        ) = self._compile_vfs_hooks(opts.vfs_interception)
        self._set_local_vfs_hooks([], [], [])

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

        network = self._build_create_network_params(opts)
        if network is not None:
            params["network"] = network

        if opts.mounts or opts.workspace or wire_vfs is not None:
            vfs: dict[str, Any] = {}
            if opts.mounts:
                vfs["mounts"] = {k: v.to_dict() for k, v in opts.mounts.items()}
            if opts.workspace:
                vfs["workspace"] = opts.workspace
            if wire_vfs is not None:
                vfs["interception"] = wire_vfs.to_dict()
            params["vfs"] = vfs

        if opts.env:
            params["env"] = opts.env

        if opts.image_config is not None:
            params["image_config"] = opts.image_config.to_dict()

        result = self._send_request("create", params)
        self._vm_id = result["id"]
        self._set_local_vfs_hooks(local_hooks, local_mutate_hooks, local_action_hooks)
        return self._vm_id

    def _build_create_network_params(
        self, opts: CreateOptions
    ) -> dict[str, Any] | None:
        has_allowed_hosts = bool(opts.allowed_hosts)
        has_secrets = bool(opts.secrets)
        has_dns_servers = bool(opts.dns_servers)
        has_hostname = bool(opts.hostname)
        has_mtu = opts.network_mtu > 0

        block_private_ips, has_block_private_override = (
            self._resolve_create_block_private_ips(opts)
        )

        include_network = (
            has_allowed_hosts
            or has_secrets
            or has_dns_servers
            or has_hostname
            or has_mtu
            or has_block_private_override
        )
        if not include_network:
            return None

        # Create config merges replace network defaults wholesale. Preserve
        # default private-IP blocking unless explicitly overridden.
        if not has_block_private_override:
            block_private_ips = True

        network: dict[str, Any] = {
            "allowed_hosts": opts.allowed_hosts,
            "block_private_ips": block_private_ips,
        }
        if has_secrets:
            network["secrets"] = {
                s.name: {"value": s.value, "hosts": s.hosts} for s in opts.secrets
            }
        if has_dns_servers:
            network["dns_servers"] = opts.dns_servers
        if has_hostname:
            network["hostname"] = opts.hostname
        if has_mtu:
            network["mtu"] = opts.network_mtu

        return network

    def _resolve_create_block_private_ips(
        self, opts: CreateOptions
    ) -> tuple[bool, bool]:
        if opts.block_private_ips_set:
            return opts.block_private_ips, True
        # Backward compatibility: older callers could only express explicit true.
        if opts.block_private_ips:
            return True, True
        return False, False

    def launch(self, sandbox: Sandbox) -> str:
        return self.create(sandbox.options())

    def exec(
        self,
        command: str,
        working_dir: str = "",
        timeout: float | None = None,
    ) -> ExecResult:
        """Execute a command in the sandbox.

        Args:
            command: The command to execute.
            working_dir: Optional working directory.
            timeout: Optional timeout in seconds. If the command doesn't
                complete within the timeout, a ``cancel`` RPC is sent to
                abort the execution and :class:`TimeoutError` is raised.
        """
        params: dict[str, str] = {"command": command}
        if working_dir:
            params["working_dir"] = working_dir

        result = self._send_request("exec", params, timeout=timeout)

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
        timeout: float | None = None,
    ) -> ExecStreamResult:
        """Execute a command and stream stdout/stderr in real-time.

        Args:
            command: The command to execute.
            stdout: File-like object to write stdout to (e.g., sys.stdout).
            stderr: File-like object to write stderr to (e.g., sys.stderr).
            working_dir: Optional working directory.
            timeout: Optional timeout in seconds. If the command doesn't
                complete within the timeout, a ``cancel`` RPC is sent to
                abort the execution and :class:`TimeoutError` is raised.

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
            "exec_stream", params, on_notification=on_notification, timeout=timeout
        )

        return ExecStreamResult(
            exit_code=result["exit_code"],
            duration_ms=result["duration_ms"],
        )

    def write_file(
        self,
        path: str,
        content: bytes | str,
        mode: int = 0o644,
        timeout: float | None = None,
    ) -> None:
        """Write a file in the sandbox.

        Args:
            path: Guest path to write to.
            content: File contents (bytes or str).
            mode: File permission mode (default: 0644).
            timeout: Optional timeout in seconds.
        """
        if isinstance(content, str):
            content = content.encode("utf-8")
        self._apply_local_action_hooks("write", path, len(content), mode)
        content = self._apply_local_write_mutations(path, content, mode)

        params: dict[str, Any] = {
            "path": path,
            "content": base64.b64encode(content).decode("ascii"),
            "mode": mode,
        }
        self._send_request("write_file", params, timeout=timeout)

    def read_file(self, path: str, timeout: float | None = None) -> bytes:
        """Read a file from the sandbox.

        Args:
            path: Guest path to read.
            timeout: Optional timeout in seconds.
        """
        self._apply_local_action_hooks("read", path, 0, 0)
        result = self._send_request("read_file", {"path": path}, timeout=timeout)
        return base64.b64decode(result["content"])

    def list_files(self, path: str, timeout: float | None = None) -> list[FileInfo]:
        """List files in a directory.

        Args:
            path: Guest directory path.
            timeout: Optional timeout in seconds.
        """
        self._apply_local_action_hooks("readdir", path, 0, 0)
        result = self._send_request("list_files", {"path": path}, timeout=timeout)
        return [
            FileInfo(
                name=f["name"],
                size=f["size"],
                mode=f["mode"],
                is_dir=f["is_dir"],
            )
            for f in result.get("files", [])
        ]
