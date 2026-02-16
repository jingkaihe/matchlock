"""Tests for matchlock.client.

These tests mock the subprocess to avoid needing a real matchlock binary.
"""

import base64
import io
import json
import threading
from unittest.mock import MagicMock, patch

import pytest

from matchlock.builder import Sandbox
from matchlock.client import (
    Client,
    _LocalVFSActionHook,
    _LocalVFSMutateHook,
    _LocalVFSHook,
    _PendingRequest,
)
from matchlock.types import (
    Config,
    CreateOptions,
    ExecResult,
    ExecStreamResult,
    FileInfo,
    MatchlockError,
    MountConfig,
    RPCError,
    VFS_HOOK_ACTION_ALLOW,
    VFS_HOOK_ACTION_BLOCK,
    VFSHookRule,
    VFSInterceptionConfig,
)


class FakeProcess:
    """A fake subprocess.Popen that simulates the matchlock RPC process."""

    def __init__(self):
        self.stdin = io.StringIO()
        self._stdout_lines: list[str] = []
        self._stdout_lock = threading.Lock()
        self._stdout_event = threading.Event()
        self._closed = False
        self._returncode = None
        self.stdout = self

    def poll(self):
        return self._returncode

    def readline(self) -> str:
        while True:
            with self._stdout_lock:
                if self._closed and not self._stdout_lines:
                    return ""
                if self._stdout_lines:
                    return self._stdout_lines.pop(0)
            self._stdout_event.wait(timeout=0.1)
            self._stdout_event.clear()

    def push_response(self, data: dict) -> None:
        with self._stdout_lock:
            self._stdout_lines.append(json.dumps(data) + "\n")
        self._stdout_event.set()

    def close_stdout(self):
        with self._stdout_lock:
            self._closed = True
        self._stdout_event.set()

    def wait(self, timeout=None):
        pass

    def kill(self):
        self._returncode = -9


def make_client_with_fake(config=None) -> tuple[Client, FakeProcess]:
    fake = FakeProcess()
    if config is None:
        config = Config(binary_path="fake-matchlock")
    client = Client(config)
    client._process = fake
    client._reader_thread = threading.Thread(target=client._reader_loop, daemon=True)
    client._reader_thread.start()
    return client, fake


class TestPendingRequest:
    def test_event_starts_unset(self):
        pr = _PendingRequest()
        assert not pr.event.is_set()
        assert pr.result is None
        assert pr.error is None
        assert pr.on_notification is None

    def test_with_notification_callback(self):
        def cb(method, params):
            return None

        pr = _PendingRequest(on_notification=cb)
        assert pr.on_notification is cb


class TestClientInit:
    def test_default_config(self):
        client = Client()
        assert client._config.binary_path == "matchlock"
        assert client._config.use_sudo is False

    def test_custom_config(self):
        config = Config(binary_path="/opt/matchlock", use_sudo=True)
        client = Client(config)
        assert client._config.binary_path == "/opt/matchlock"
        assert client._config.use_sudo is True

    def test_env_var_config(self):
        with patch.dict("os.environ", {"MATCHLOCK_BIN": "/custom/path"}):
            client = Client()
            assert client._config.binary_path == "/custom/path"

    def test_vm_id_initially_none(self):
        client = Client()
        assert client.vm_id is None


class TestClientContextManager:
    @patch("subprocess.Popen")
    def test_enter_starts(self, mock_popen):
        fake = FakeProcess()
        mock_popen.return_value = fake
        client = Client(Config(binary_path="fake"))
        result = client.__enter__()
        assert result is client
        assert client._process is not None
        fake.close_stdout()

    @patch("subprocess.Popen")
    def test_exit_closes(self, mock_popen):
        fake = FakeProcess()
        mock_popen.return_value = fake
        client = Client(Config(binary_path="fake"))
        client.__enter__()

        def respond_close():
            import time

            time.sleep(0.05)
            fake.push_response({"jsonrpc": "2.0", "id": 1, "result": {}})
            fake.close_stdout()

        t = threading.Thread(target=respond_close, daemon=True)
        t.start()
        client.__exit__(None, None, None)
        assert client._closed is True
        t.join(timeout=2)


class TestClientCreate:
    def test_create_requires_image(self):
        client, fake = make_client_with_fake()
        try:
            with pytest.raises(MatchlockError, match="image is required"):
                client.create(CreateOptions())
        finally:
            fake.close_stdout()

    def test_create_success(self):
        client, fake = make_client_with_fake()
        try:

            def respond():
                import time

                time.sleep(0.05)
                fake.push_response(
                    {
                        "jsonrpc": "2.0",
                        "id": 1,
                        "result": {"id": "vm-abc123"},
                    }
                )

            t = threading.Thread(target=respond, daemon=True)
            t.start()
            vm_id = client.create(CreateOptions(image="alpine:latest"))
            assert vm_id == "vm-abc123"
            assert client.vm_id == "vm-abc123"
            t.join(timeout=2)
        finally:
            fake.close_stdout()

    def test_create_with_resources(self):
        client, fake = make_client_with_fake()
        try:

            def respond():
                import time

                time.sleep(0.05)
                fake.push_response(
                    {
                        "jsonrpc": "2.0",
                        "id": 1,
                        "result": {"id": "vm-res"},
                    }
                )

            t = threading.Thread(target=respond, daemon=True)
            t.start()
            opts = CreateOptions(
                image="img",
                cpus=2,
                memory_mb=512,
                disk_size_mb=2048,
                timeout_seconds=300,
            )
            vm_id = client.create(opts)
            assert vm_id == "vm-res"
            t.join(timeout=2)
        finally:
            fake.close_stdout()

    def test_create_with_network(self):
        client, fake = make_client_with_fake()
        try:

            def respond():
                import time

                time.sleep(0.05)
                fake.push_response(
                    {
                        "jsonrpc": "2.0",
                        "id": 1,
                        "result": {"id": "vm-net"},
                    }
                )

            t = threading.Thread(target=respond, daemon=True)
            t.start()
            from matchlock.types import Secret

            opts = CreateOptions(
                image="img",
                allowed_hosts=["a.com"],
                block_private_ips=True,
                secrets=[Secret(name="K", value="V", hosts=["a.com"])],
            )
            vm_id = client.create(opts)
            assert vm_id == "vm-net"

            req_line = fake.stdin.getvalue().splitlines()[0]
            req = json.loads(req_line)
            assert req["method"] == "create"
            assert req["params"]["network"] == {
                "allowed_hosts": ["a.com"],
                "block_private_ips": True,
                "secrets": {"K": {"value": "V", "hosts": ["a.com"]}},
            }
            t.join(timeout=2)
        finally:
            fake.close_stdout()

    def test_create_defaults_block_private_ips_when_network_is_set(self):
        client, fake = make_client_with_fake()
        try:

            def respond():
                import time

                time.sleep(0.05)
                fake.push_response(
                    {
                        "jsonrpc": "2.0",
                        "id": 1,
                        "result": {"id": "vm-net-default-private"},
                    }
                )

            t = threading.Thread(target=respond, daemon=True)
            t.start()
            opts = CreateOptions(
                image="img",
                allowed_hosts=["api.openai.com"],
            )
            vm_id = client.create(opts)
            assert vm_id == "vm-net-default-private"

            req_line = fake.stdin.getvalue().splitlines()[0]
            req = json.loads(req_line)
            assert req["method"] == "create"
            assert req["params"]["network"]["block_private_ips"] is True
            t.join(timeout=2)
        finally:
            fake.close_stdout()

    def test_create_sends_network_mtu_with_default_private_ip_blocking(self):
        client, fake = make_client_with_fake()
        try:

            def respond():
                import time

                time.sleep(0.05)
                fake.push_response(
                    {
                        "jsonrpc": "2.0",
                        "id": 1,
                        "result": {"id": "vm-net-mtu"},
                    }
                )

            t = threading.Thread(target=respond, daemon=True)
            t.start()
            opts = CreateOptions(
                image="img",
                network_mtu=1200,
            )
            vm_id = client.create(opts)
            assert vm_id == "vm-net-mtu"

            req_line = fake.stdin.getvalue().splitlines()[0]
            req = json.loads(req_line)
            assert req["method"] == "create"
            assert req["params"]["network"]["mtu"] == 1200
            assert req["params"]["network"]["block_private_ips"] is True
            t.join(timeout=2)
        finally:
            fake.close_stdout()

    def test_create_respects_explicit_disable_block_private_ips(self):
        client, fake = make_client_with_fake()
        try:

            def respond():
                import time

                time.sleep(0.05)
                fake.push_response(
                    {
                        "jsonrpc": "2.0",
                        "id": 1,
                        "result": {"id": "vm-net-private-off"},
                    }
                )

            t = threading.Thread(target=respond, daemon=True)
            t.start()
            opts = CreateOptions(
                image="img",
                network_mtu=1200,
                block_private_ips=False,
                block_private_ips_set=True,
            )
            vm_id = client.create(opts)
            assert vm_id == "vm-net-private-off"

            req_line = fake.stdin.getvalue().splitlines()[0]
            req = json.loads(req_line)
            assert req["method"] == "create"
            assert req["params"]["network"]["block_private_ips"] is False
            assert req["params"]["network"]["mtu"] == 1200
            t.join(timeout=2)
        finally:
            fake.close_stdout()

    def test_create_omits_network_when_no_network_overrides(self):
        client, fake = make_client_with_fake()
        try:

            def respond():
                import time

                time.sleep(0.05)
                fake.push_response(
                    {
                        "jsonrpc": "2.0",
                        "id": 1,
                        "result": {"id": "vm-no-net"},
                    }
                )

            t = threading.Thread(target=respond, daemon=True)
            t.start()
            vm_id = client.create(CreateOptions(image="img"))
            assert vm_id == "vm-no-net"

            req_line = fake.stdin.getvalue().splitlines()[0]
            req = json.loads(req_line)
            assert req["method"] == "create"
            assert "network" not in req["params"]
            t.join(timeout=2)
        finally:
            fake.close_stdout()

    def test_create_rejects_negative_network_mtu(self):
        client, fake = make_client_with_fake()
        try:
            with pytest.raises(MatchlockError, match="network mtu must be > 0"):
                client.create(CreateOptions(image="img", network_mtu=-1))
        finally:
            fake.close_stdout()

    def test_create_with_vfs(self):
        client, fake = make_client_with_fake()
        try:

            def respond():
                import time

                time.sleep(0.05)
                fake.push_response(
                    {
                        "jsonrpc": "2.0",
                        "id": 1,
                        "result": {"id": "vm-vfs"},
                    }
                )

            t = threading.Thread(target=respond, daemon=True)
            t.start()
            opts = CreateOptions(
                image="img",
                workspace="/code",
                mounts={"/data": MountConfig(type="real_fs", host_path="/h")},
            )
            vm_id = client.create(opts)
            assert vm_id == "vm-vfs"
            t.join(timeout=2)
        finally:
            fake.close_stdout()

    def test_create_with_vfs_interception(self):
        client, fake = make_client_with_fake()
        try:

            def respond():
                import time

                time.sleep(0.05)
                fake.push_response(
                    {
                        "jsonrpc": "2.0",
                        "id": 1,
                        "result": {"id": "vm-vfs-hooks"},
                    }
                )

            t = threading.Thread(target=respond, daemon=True)
            t.start()
            opts = CreateOptions(
                image="img",
                vfs_interception=VFSInterceptionConfig(
                    rules=[
                        VFSHookRule(
                            phase="before",
                            ops=["create"],
                            path="/workspace/blocked.txt",
                            action="block",
                        )
                    ],
                ),
            )
            vm_id = client.create(opts)
            assert vm_id == "vm-vfs-hooks"

            req_line = fake.stdin.getvalue().splitlines()[0]
            req = json.loads(req_line)
            assert req["method"] == "create"
            assert req["params"]["vfs"]["interception"] == {
                "rules": [
                    {
                        "phase": "before",
                        "ops": ["create"],
                        "path": "/workspace/blocked.txt",
                        "action": "block",
                    }
                ],
            }
            t.join(timeout=2)
        finally:
            fake.close_stdout()

    def test_create_with_env(self):
        client, fake = make_client_with_fake()
        try:

            def respond():
                import time

                time.sleep(0.05)
                fake.push_response(
                    {
                        "jsonrpc": "2.0",
                        "id": 1,
                        "result": {"id": "vm-env"},
                    }
                )

            t = threading.Thread(target=respond, daemon=True)
            t.start()
            opts = CreateOptions(image="img", env={"FOO": "bar", "BAR": "baz"})
            vm_id = client.create(opts)
            assert vm_id == "vm-env"

            req_line = fake.stdin.getvalue().strip().splitlines()[0]
            req = json.loads(req_line)
            assert req["method"] == "create"
            assert req["params"]["env"] == {"FOO": "bar", "BAR": "baz"}
            t.join(timeout=2)
        finally:
            fake.close_stdout()

    def test_create_with_vfs_callback_hook(self):
        client, fake = make_client_with_fake()
        try:

            def respond():
                import time

                time.sleep(0.05)
                fake.push_response(
                    {
                        "jsonrpc": "2.0",
                        "id": 1,
                        "result": {"id": "vm-vfs-callback"},
                    }
                )

            t = threading.Thread(target=respond, daemon=True)
            t.start()
            opts = CreateOptions(
                image="img",
                vfs_interception=VFSInterceptionConfig(
                    rules=[
                        VFSHookRule(
                            phase="after",
                            ops=["write"],
                            path="/workspace/*",
                            hook=lambda event: None,
                        )
                    ],
                ),
            )
            vm_id = client.create(opts)
            assert vm_id == "vm-vfs-callback"

            req_line = fake.stdin.getvalue().splitlines()[0]
            req = json.loads(req_line)
            assert req["params"]["vfs"]["interception"] == {
                "emit_events": True,
            }
            t.join(timeout=2)
        finally:
            fake.close_stdout()

    def test_create_rejects_before_callback_hook(self):
        client, fake = make_client_with_fake()
        try:
            opts = CreateOptions(
                image="img",
                vfs_interception=VFSInterceptionConfig(
                    rules=[
                        VFSHookRule(
                            name="before",
                            phase="before",
                            hook=lambda event: None,
                        )
                    ]
                ),
            )
            with pytest.raises(MatchlockError, match="phase=after"):
                client.create(opts)
        finally:
            fake.close_stdout()

    def test_create_with_vfs_dangerous_hook(self):
        client, fake = make_client_with_fake()
        try:

            def respond():
                import time

                time.sleep(0.05)
                fake.push_response(
                    {
                        "jsonrpc": "2.0",
                        "id": 1,
                        "result": {"id": "vm-vfs-dangerous-callback"},
                    }
                )

            t = threading.Thread(target=respond, daemon=True)
            t.start()
            opts = CreateOptions(
                image="img",
                vfs_interception=VFSInterceptionConfig(
                    rules=[
                        VFSHookRule(
                            phase="after",
                            ops=["write"],
                            path="/workspace/*",
                            dangerous_hook=lambda c, event: None,
                        )
                    ],
                ),
            )
            vm_id = client.create(opts)
            assert vm_id == "vm-vfs-dangerous-callback"

            req_line = fake.stdin.getvalue().splitlines()[0]
            req = json.loads(req_line)
            assert req["params"]["vfs"]["interception"] == {
                "emit_events": True,
            }
            t.join(timeout=2)
        finally:
            fake.close_stdout()

    def test_create_rejects_before_dangerous_hook(self):
        client, fake = make_client_with_fake()
        try:
            opts = CreateOptions(
                image="img",
                vfs_interception=VFSInterceptionConfig(
                    rules=[
                        VFSHookRule(
                            name="before-dangerous",
                            phase="before",
                            dangerous_hook=lambda c, event: None,
                        )
                    ]
                ),
            )
            with pytest.raises(MatchlockError, match="phase=after"):
                client.create(opts)
        finally:
            fake.close_stdout()

    def test_create_with_vfs_mutate_hook(self):
        client, fake = make_client_with_fake()
        try:

            def respond():
                import time

                time.sleep(0.05)
                fake.push_response(
                    {
                        "jsonrpc": "2.0",
                        "id": 1,
                        "result": {"id": "vm-vfs-mutate-callback"},
                    }
                )

            t = threading.Thread(target=respond, daemon=True)
            t.start()
            opts = CreateOptions(
                image="img",
                vfs_interception=VFSInterceptionConfig(
                    rules=[
                        VFSHookRule(
                            phase="before",
                            ops=["write"],
                            path="/workspace/*",
                            mutate_hook=lambda req: b"mutated",
                        )
                    ],
                ),
            )
            vm_id = client.create(opts)
            assert vm_id == "vm-vfs-mutate-callback"

            req_line = fake.stdin.getvalue().splitlines()[0]
            req = json.loads(req_line)
            assert "vfs" not in req["params"] or "interception" not in req[
                "params"
            ].get("vfs", {})
            t.join(timeout=2)
        finally:
            fake.close_stdout()

    def test_create_rejects_after_mutate_hook(self):
        client, fake = make_client_with_fake()
        try:
            opts = CreateOptions(
                image="img",
                vfs_interception=VFSInterceptionConfig(
                    rules=[
                        VFSHookRule(
                            name="after-mutate",
                            phase="after",
                            mutate_hook=lambda req: b"x",
                        )
                    ]
                ),
            )
            with pytest.raises(MatchlockError, match="phase=before"):
                client.create(opts)
        finally:
            fake.close_stdout()

    def test_create_passes_through_wire_exec_after_action(self):
        client, fake = make_client_with_fake()
        try:

            def respond():
                import time

                time.sleep(0.05)
                fake.push_response(
                    {
                        "jsonrpc": "2.0",
                        "id": 1,
                        "result": {"id": "vm-wire-exec"},
                    }
                )

            t = threading.Thread(target=respond, daemon=True)
            t.start()
            opts = CreateOptions(
                image="img",
                vfs_interception=VFSInterceptionConfig(
                    rules=[
                        VFSHookRule(
                            name="wire-exec",
                            phase="after",
                            action="exec_after",
                        )
                    ]
                ),
            )
            vm_id = client.create(opts)
            assert vm_id == "vm-wire-exec"

            req_line = fake.stdin.getvalue().splitlines()[0]
            req = json.loads(req_line)
            assert req["params"]["vfs"]["interception"] == {
                "rules": [
                    {
                        "name": "wire-exec",
                        "phase": "after",
                        "action": "exec_after",
                    }
                ]
            }
            t.join(timeout=2)
        finally:
            fake.close_stdout()

    def test_create_rejects_wire_mutate_write_action(self):
        client, fake = make_client_with_fake()
        try:
            opts = CreateOptions(
                image="img",
                vfs_interception=VFSInterceptionConfig(
                    rules=[
                        VFSHookRule(
                            name="wire-mutate",
                            phase="before",
                            action="mutate_write",
                        )
                    ]
                ),
            )
            with pytest.raises(MatchlockError, match="requires mutate_hook callback"):
                client.create(opts)
        finally:
            fake.close_stdout()

    def test_create_with_vfs_action_hook(self):
        client, fake = make_client_with_fake()
        try:

            def respond():
                import time

                time.sleep(0.05)
                fake.push_response(
                    {
                        "jsonrpc": "2.0",
                        "id": 1,
                        "result": {"id": "vm-vfs-action-callback"},
                    }
                )

            t = threading.Thread(target=respond, daemon=True)
            t.start()
            opts = CreateOptions(
                image="img",
                vfs_interception=VFSInterceptionConfig(
                    rules=[
                        VFSHookRule(
                            phase="before",
                            ops=["write"],
                            path="/workspace/*",
                            action_hook=lambda req: VFS_HOOK_ACTION_ALLOW,
                        )
                    ],
                ),
            )
            vm_id = client.create(opts)
            assert vm_id == "vm-vfs-action-callback"

            req_line = fake.stdin.getvalue().splitlines()[0]
            req = json.loads(req_line)
            assert "vfs" not in req["params"] or "interception" not in req[
                "params"
            ].get("vfs", {})
            t.join(timeout=2)
        finally:
            fake.close_stdout()

    def test_create_rpc_error(self):
        client, fake = make_client_with_fake()
        try:

            def respond():
                import time

                time.sleep(0.05)
                fake.push_response(
                    {
                        "jsonrpc": "2.0",
                        "id": 1,
                        "error": {"code": -32000, "message": "VM failed to start"},
                    }
                )

            t = threading.Thread(target=respond, daemon=True)
            t.start()
            with pytest.raises(RPCError) as exc_info:
                client.create(CreateOptions(image="bad:image"))
            assert exc_info.value.code == -32000
            assert "VM failed" in exc_info.value.message
            t.join(timeout=2)
        finally:
            fake.close_stdout()


class TestClientLaunch:
    def test_launch_delegates_to_create(self):
        client, fake = make_client_with_fake()
        try:

            def respond():
                import time

                time.sleep(0.05)
                fake.push_response(
                    {
                        "jsonrpc": "2.0",
                        "id": 1,
                        "result": {"id": "vm-launch"},
                    }
                )

            t = threading.Thread(target=respond, daemon=True)
            t.start()
            sandbox = Sandbox("alpine:latest").with_cpus(2)
            vm_id = client.launch(sandbox)
            assert vm_id == "vm-launch"
            t.join(timeout=2)
        finally:
            fake.close_stdout()


class TestClientExec:
    def test_exec_success(self):
        client, fake = make_client_with_fake()
        try:
            stdout_b64 = base64.b64encode(b"hello\n").decode()
            stderr_b64 = base64.b64encode(b"").decode()

            def respond():
                import time

                time.sleep(0.05)
                fake.push_response(
                    {
                        "jsonrpc": "2.0",
                        "id": 1,
                        "result": {
                            "exit_code": 0,
                            "stdout": stdout_b64,
                            "stderr": stderr_b64,
                            "duration_ms": 42,
                        },
                    }
                )

            t = threading.Thread(target=respond, daemon=True)
            t.start()
            result = client.exec("echo hello")
            assert isinstance(result, ExecResult)
            assert result.exit_code == 0
            assert result.stdout == "hello\n"
            assert result.stderr == ""
            assert result.duration_ms == 42
            t.join(timeout=2)
        finally:
            fake.close_stdout()

    def test_exec_with_working_dir(self):
        client, fake = make_client_with_fake()
        try:
            stdout_b64 = base64.b64encode(b"/workspace\n").decode()
            stderr_b64 = base64.b64encode(b"").decode()

            def respond():
                import time

                time.sleep(0.05)
                fake.push_response(
                    {
                        "jsonrpc": "2.0",
                        "id": 1,
                        "result": {
                            "exit_code": 0,
                            "stdout": stdout_b64,
                            "stderr": stderr_b64,
                            "duration_ms": 10,
                        },
                    }
                )

            t = threading.Thread(target=respond, daemon=True)
            t.start()
            result = client.exec("pwd", working_dir="/workspace")
            assert result.stdout == "/workspace\n"
            t.join(timeout=2)
        finally:
            fake.close_stdout()

    def test_exec_nonzero_exit(self):
        client, fake = make_client_with_fake()
        try:
            stderr_b64 = base64.b64encode(b"not found\n").decode()
            stdout_b64 = base64.b64encode(b"").decode()

            def respond():
                import time

                time.sleep(0.05)
                fake.push_response(
                    {
                        "jsonrpc": "2.0",
                        "id": 1,
                        "result": {
                            "exit_code": 127,
                            "stdout": stdout_b64,
                            "stderr": stderr_b64,
                            "duration_ms": 5,
                        },
                    }
                )

            t = threading.Thread(target=respond, daemon=True)
            t.start()
            result = client.exec("nonexistent_cmd")
            assert result.exit_code == 127
            assert result.stderr == "not found\n"
            t.join(timeout=2)
        finally:
            fake.close_stdout()


class TestClientExecStream:
    def test_exec_stream_with_notifications(self):
        client, fake = make_client_with_fake()
        try:
            chunk1_b64 = base64.b64encode(b"line1\n").decode()
            chunk2_b64 = base64.b64encode(b"line2\n").decode()
            err_b64 = base64.b64encode(b"warn\n").decode()

            def respond():
                import time

                time.sleep(0.05)
                fake.push_response(
                    {
                        "jsonrpc": "2.0",
                        "method": "exec_stream.stdout",
                        "params": {"id": 1, "data": chunk1_b64},
                    }
                )
                fake.push_response(
                    {
                        "jsonrpc": "2.0",
                        "method": "exec_stream.stderr",
                        "params": {"id": 1, "data": err_b64},
                    }
                )
                fake.push_response(
                    {
                        "jsonrpc": "2.0",
                        "method": "exec_stream.stdout",
                        "params": {"id": 1, "data": chunk2_b64},
                    }
                )
                time.sleep(0.05)
                fake.push_response(
                    {
                        "jsonrpc": "2.0",
                        "id": 1,
                        "result": {"exit_code": 0, "duration_ms": 200},
                    }
                )

            t = threading.Thread(target=respond, daemon=True)
            t.start()

            stdout_buf = io.StringIO()
            stderr_buf = io.StringIO()
            result = client.exec_stream("cmd", stdout=stdout_buf, stderr=stderr_buf)

            assert isinstance(result, ExecStreamResult)
            assert result.exit_code == 0
            assert result.duration_ms == 200
            assert stdout_buf.getvalue() == "line1\nline2\n"
            assert stderr_buf.getvalue() == "warn\n"
            t.join(timeout=2)
        finally:
            fake.close_stdout()

    def test_exec_stream_no_writers(self):
        client, fake = make_client_with_fake()
        try:

            def respond():
                import time

                time.sleep(0.05)
                chunk_b64 = base64.b64encode(b"data").decode()
                fake.push_response(
                    {
                        "jsonrpc": "2.0",
                        "method": "exec_stream.stdout",
                        "params": {"id": 1, "data": chunk_b64},
                    }
                )
                time.sleep(0.05)
                fake.push_response(
                    {
                        "jsonrpc": "2.0",
                        "id": 1,
                        "result": {"exit_code": 0, "duration_ms": 50},
                    }
                )

            t = threading.Thread(target=respond, daemon=True)
            t.start()
            result = client.exec_stream("cmd")
            assert result.exit_code == 0
            t.join(timeout=2)
        finally:
            fake.close_stdout()


class TestClientFileOps:
    def test_write_file_string(self):
        client, fake = make_client_with_fake()
        try:

            def respond():
                import time

                time.sleep(0.05)
                fake.push_response(
                    {
                        "jsonrpc": "2.0",
                        "id": 1,
                        "result": {},
                    }
                )

            t = threading.Thread(target=respond, daemon=True)
            t.start()
            client.write_file("/workspace/test.txt", "hello")
            t.join(timeout=2)
        finally:
            fake.close_stdout()

    def test_write_file_bytes(self):
        client, fake = make_client_with_fake()
        try:

            def respond():
                import time

                time.sleep(0.05)
                fake.push_response(
                    {
                        "jsonrpc": "2.0",
                        "id": 1,
                        "result": {},
                    }
                )

            t = threading.Thread(target=respond, daemon=True)
            t.start()
            client.write_file("/workspace/bin", b"\x00\x01\x02")
            t.join(timeout=2)
        finally:
            fake.close_stdout()

    def test_write_file_applies_local_mutate_hook(self):
        client, fake = make_client_with_fake()
        try:
            client._set_local_vfs_hooks(
                [],
                [
                    _LocalVFSMutateHook(
                        name="mut",
                        ops={"write"},
                        path="/workspace/*",
                        hook=lambda req: (
                            f"size={req.size};mode={oct(req.mode)}".encode()
                        ),
                    )
                ],
                [],
            )

            def respond():
                import time

                time.sleep(0.05)
                fake.push_response({"jsonrpc": "2.0", "id": 1, "result": {}})

            t = threading.Thread(target=respond, daemon=True)
            t.start()
            client.write_file("/workspace/test.txt", b"abcd")
            req_line = fake.stdin.getvalue().splitlines()[0]
            req = json.loads(req_line)
            payload = base64.b64decode(req["params"]["content"])
            assert payload == b"size=4;mode=0o644"
            t.join(timeout=2)
        finally:
            fake.close_stdout()

    def test_write_file_mutate_hook_none_keeps_original(self):
        client, fake = make_client_with_fake()
        try:
            client._set_local_vfs_hooks(
                [],
                [
                    _LocalVFSMutateHook(
                        name="noop",
                        ops={"write"},
                        path="/workspace/*",
                        hook=lambda req: None,
                    )
                ],
                [],
            )

            def respond():
                import time

                time.sleep(0.05)
                fake.push_response({"jsonrpc": "2.0", "id": 1, "result": {}})

            t = threading.Thread(target=respond, daemon=True)
            t.start()
            client.write_file("/workspace/test.txt", b"abcd")
            req_line = fake.stdin.getvalue().splitlines()[0]
            req = json.loads(req_line)
            payload = base64.b64decode(req["params"]["content"])
            assert payload == b"abcd"
            t.join(timeout=2)
        finally:
            fake.close_stdout()

    def test_write_file_blocked_by_local_action_hook(self):
        client, fake = make_client_with_fake()
        try:
            client._set_local_vfs_hooks(
                [],
                [],
                [
                    _LocalVFSActionHook(
                        name="block-writes",
                        ops={"write"},
                        path="/workspace/*",
                        hook=lambda req: VFS_HOOK_ACTION_BLOCK,
                    )
                ],
            )

            with pytest.raises(MatchlockError, match="blocked operation"):
                client.write_file("/workspace/test.txt", b"abcd")
            assert fake.stdin.getvalue() == ""
        finally:
            fake.close_stdout()

    def test_read_file(self):
        client, fake = make_client_with_fake()
        try:
            content_b64 = base64.b64encode(b"file contents").decode()

            def respond():
                import time

                time.sleep(0.05)
                fake.push_response(
                    {
                        "jsonrpc": "2.0",
                        "id": 1,
                        "result": {"content": content_b64},
                    }
                )

            t = threading.Thread(target=respond, daemon=True)
            t.start()
            content = client.read_file("/workspace/test.txt")
            assert content == b"file contents"
            t.join(timeout=2)
        finally:
            fake.close_stdout()

    def test_read_file_blocked_by_local_action_hook(self):
        client, fake = make_client_with_fake()
        try:
            client._set_local_vfs_hooks(
                [],
                [],
                [
                    _LocalVFSActionHook(
                        name="block-reads",
                        ops={"read"},
                        path="/workspace/*",
                        hook=lambda req: VFS_HOOK_ACTION_BLOCK,
                    )
                ],
            )
            with pytest.raises(MatchlockError, match="blocked operation"):
                client.read_file("/workspace/test.txt")
            assert fake.stdin.getvalue() == ""
        finally:
            fake.close_stdout()

    def test_list_files(self):
        client, fake = make_client_with_fake()
        try:

            def respond():
                import time

                time.sleep(0.05)
                fake.push_response(
                    {
                        "jsonrpc": "2.0",
                        "id": 1,
                        "result": {
                            "files": [
                                {
                                    "name": "hello.txt",
                                    "size": 5,
                                    "mode": 0o644,
                                    "is_dir": False,
                                },
                                {
                                    "name": "subdir",
                                    "size": 0,
                                    "mode": 0o755,
                                    "is_dir": True,
                                },
                            ],
                        },
                    }
                )

            t = threading.Thread(target=respond, daemon=True)
            t.start()
            files = client.list_files("/workspace")
            assert len(files) == 2
            assert isinstance(files[0], FileInfo)
            assert files[0].name == "hello.txt"
            assert files[0].is_dir is False
            assert files[1].name == "subdir"
            assert files[1].is_dir is True
            t.join(timeout=2)
        finally:
            fake.close_stdout()

    def test_list_files_empty(self):
        client, fake = make_client_with_fake()
        try:

            def respond():
                import time

                time.sleep(0.05)
                fake.push_response(
                    {
                        "jsonrpc": "2.0",
                        "id": 1,
                        "result": {},
                    }
                )

            t = threading.Thread(target=respond, daemon=True)
            t.start()
            files = client.list_files("/empty")
            assert files == []
            t.join(timeout=2)
        finally:
            fake.close_stdout()

    def test_list_files_blocked_by_local_action_hook(self):
        client, fake = make_client_with_fake()
        try:
            client._set_local_vfs_hooks(
                [],
                [],
                [
                    _LocalVFSActionHook(
                        name="block-readdir",
                        ops={"readdir"},
                        path="/workspace*",
                        hook=lambda req: VFS_HOOK_ACTION_BLOCK,
                    )
                ],
            )
            with pytest.raises(MatchlockError, match="blocked operation"):
                client.list_files("/workspace")
            assert fake.stdin.getvalue() == ""
        finally:
            fake.close_stdout()


class TestVFSCallbackNotifications:
    def test_safe_event_callback_suppresses_recursion(self):
        client = Client(Config(binary_path="fake"))
        runs = 0
        done = threading.Event()

        def hook(event):
            nonlocal runs
            assert event.mode == 0o640
            assert event.uid == 123
            assert event.gid == 456
            runs += 1
            client._handle_event_notification(
                {"file": {"op": "write", "path": "/workspace/nested.txt"}}
            )
            done.set()

        client._set_local_vfs_hooks(
            [
                _LocalVFSHook(
                    name="after",
                    ops={"write"},
                    path="/workspace/*",
                    timeout_ms=0,
                    dangerous=False,
                    hook=hook,
                )
            ],
            [],
            [],
        )

        client._handle_event_notification(
            {
                "file": {
                    "op": "write",
                    "path": "/workspace/trigger.txt",
                    "size": 1,
                    "mode": 0o640,
                    "uid": 123,
                    "gid": 456,
                }
            }
        )
        assert done.wait(timeout=2)
        # Give nested event delivery a moment; recursion guard keeps this at one.
        threading.Event().wait(0.1)
        assert runs == 1

    def test_dangerous_event_callback_allows_recursion(self):
        client = Client(Config(binary_path="fake"))
        runs = 0
        done = threading.Event()

        def hook(c: Client, event):
            nonlocal runs
            assert event.mode == 0o640
            assert event.uid == 123
            assert event.gid == 456
            runs += 1
            if runs < 3:
                c._handle_event_notification(
                    {
                        "file": {
                            "op": "write",
                            "path": "/workspace/nested.txt",
                            "size": 1,
                            "mode": 0o640,
                            "uid": 123,
                            "gid": 456,
                        }
                    }
                )
            if runs >= 3:
                done.set()

        client._set_local_vfs_hooks(
            [
                _LocalVFSHook(
                    name="dangerous-after",
                    ops={"write"},
                    path="/workspace/*",
                    timeout_ms=0,
                    dangerous=True,
                    hook=hook,
                )
            ],
            [],
            [],
        )

        client._handle_event_notification(
            {
                "file": {
                    "op": "write",
                    "path": "/workspace/trigger.txt",
                    "size": 1,
                    "mode": 0o640,
                    "uid": 123,
                    "gid": 456,
                }
            }
        )
        assert done.wait(timeout=2)
        assert runs >= 3


class TestClientProcessNotRunning:
    def test_send_request_raises_when_not_started(self):
        client = Client()
        with pytest.raises(MatchlockError, match="not running"):
            client._send_request("exec", {"command": "echo hi"})


class TestClientProcessDied:
    def test_reader_loop_handles_closed_stdout(self):
        client, fake = make_client_with_fake()

        def send_and_die():
            import time

            time.sleep(0.05)
            fake.close_stdout()

        t = threading.Thread(target=send_and_die, daemon=True)
        t.start()

        pending = _PendingRequest()
        with client._pending_lock:
            client._pending[999] = pending

        pending.event.wait(timeout=2)
        assert isinstance(pending.error, MatchlockError)
        assert "unexpectedly" in str(pending.error)
        t.join(timeout=2)


class TestClientRemove:
    @patch("subprocess.run")
    def test_remove_calls_cli(self, mock_run):
        client = Client(Config(binary_path="matchlock"))
        client._vm_id = "vm-abc"
        client.remove()
        mock_run.assert_called_once_with(
            ["matchlock", "rm", "vm-abc"],
            capture_output=True,
            text=True,
            check=True,
        )

    @patch("subprocess.run")
    def test_remove_uses_last_vm_id_after_close(self, mock_run):
        client = Client(Config(binary_path="matchlock"))
        client._vm_id = "vm-xyz"
        client._last_vm_id = "vm-xyz"
        client._vm_id = None
        client.remove()
        mock_run.assert_called_once_with(
            ["matchlock", "rm", "vm-xyz"],
            capture_output=True,
            text=True,
            check=True,
        )

    def test_remove_noop_without_vm_id(self):
        client = Client()
        client.remove()  # should not raise


class TestClientClose:
    def test_close_idempotent(self):
        client = Client()
        client._closed = True
        client.close()  # should not raise

    def test_close_when_process_already_dead(self):
        client = Client()
        fake = MagicMock()
        fake.poll.return_value = 0
        client._process = fake
        client.close()
        assert client._closed is True
