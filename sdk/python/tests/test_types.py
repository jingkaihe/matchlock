"""Tests for matchlock.types."""

from matchlock.types import (
    Config,
    CreateOptions,
    ExecResult,
    ExecStreamResult,
    FileInfo,
    MatchlockError,
    MountConfig,
    RPCError,
    Secret,
)


class TestConfig:
    def test_defaults(self):
        c = Config()
        assert c.binary_path == "matchlock"
        assert c.use_sudo is False

    def test_custom_values(self):
        c = Config(binary_path="/usr/local/bin/matchlock", use_sudo=True)
        assert c.binary_path == "/usr/local/bin/matchlock"
        assert c.use_sudo is True


class TestMountConfig:
    def test_defaults(self):
        m = MountConfig()
        assert m.type == "memory"
        assert m.host_path == ""
        assert m.readonly is False

    def test_to_dict_minimal(self):
        m = MountConfig()
        assert m.to_dict() == {"type": "memory"}

    def test_to_dict_with_host_path(self):
        m = MountConfig(type="real_fs", host_path="/tmp/data")
        assert m.to_dict() == {"type": "real_fs", "host_path": "/tmp/data"}

    def test_to_dict_readonly(self):
        m = MountConfig(type="real_fs", host_path="/src", readonly=True)
        d = m.to_dict()
        assert d == {"type": "real_fs", "host_path": "/src", "readonly": True}

    def test_to_dict_readonly_false_omitted(self):
        m = MountConfig(type="overlay", host_path="/data", readonly=False)
        d = m.to_dict()
        assert "readonly" not in d


class TestSecret:
    def test_creation(self):
        s = Secret(name="API_KEY", value="sk-123", hosts=["api.example.com"])
        assert s.name == "API_KEY"
        assert s.value == "sk-123"
        assert s.hosts == ["api.example.com"]

    def test_default_hosts(self):
        s = Secret(name="TOKEN", value="abc")
        assert s.hosts == []

    def test_multiple_hosts(self):
        s = Secret(name="KEY", value="val", hosts=["a.com", "b.com"])
        assert len(s.hosts) == 2


class TestCreateOptions:
    def test_defaults(self):
        opts = CreateOptions()
        assert opts.image == ""
        assert opts.cpus == 0
        assert opts.memory_mb == 0
        assert opts.disk_size_mb == 0
        assert opts.timeout_seconds == 0
        assert opts.allowed_hosts == []
        assert opts.block_private_ips is False
        assert opts.mounts == {}
        assert opts.env == {}
        assert opts.secrets == []
        assert opts.workspace == ""

    def test_with_image(self):
        opts = CreateOptions(image="alpine:latest")
        assert opts.image == "alpine:latest"

    def test_mutable_defaults_are_independent(self):
        a = CreateOptions()
        b = CreateOptions()
        a.allowed_hosts.append("x.com")
        assert b.allowed_hosts == []

        a.env["FOO"] = "bar"
        assert b.env == {}

        a.secrets.append(Secret(name="K", value="V"))
        assert b.secrets == []


class TestExecResult:
    def test_fields(self):
        r = ExecResult(exit_code=0, stdout="hello\n", stderr="", duration_ms=42)
        assert r.exit_code == 0
        assert r.stdout == "hello\n"
        assert r.stderr == ""
        assert r.duration_ms == 42

    def test_nonzero_exit(self):
        r = ExecResult(exit_code=1, stdout="", stderr="error\n", duration_ms=10)
        assert r.exit_code == 1
        assert r.stderr == "error\n"


class TestExecStreamResult:
    def test_fields(self):
        r = ExecStreamResult(exit_code=0, duration_ms=100)
        assert r.exit_code == 0
        assert r.duration_ms == 100


class TestFileInfo:
    def test_file(self):
        f = FileInfo(name="hello.txt", size=13, mode=0o644, is_dir=False)
        assert f.name == "hello.txt"
        assert f.size == 13
        assert f.mode == 0o644
        assert f.is_dir is False

    def test_directory(self):
        f = FileInfo(name="subdir", size=0, mode=0o755, is_dir=True)
        assert f.is_dir is True


class TestMatchlockError:
    def test_is_exception(self):
        assert issubclass(MatchlockError, Exception)

    def test_message(self):
        e = MatchlockError("something went wrong")
        assert str(e) == "something went wrong"


class TestRPCError:
    def test_inherits_matchlock_error(self):
        assert issubclass(RPCError, MatchlockError)

    def test_message_format(self):
        e = RPCError(-32000, "VM failed")
        assert str(e) == "[-32000] VM failed"
        assert e.code == -32000
        assert e.message == "VM failed"

    def test_is_vm_error(self):
        e = RPCError(RPCError.VM_FAILED, "fail")
        assert e.is_vm_error() is True
        assert e.is_exec_error() is False
        assert e.is_file_error() is False

    def test_is_exec_error(self):
        e = RPCError(RPCError.EXEC_FAILED, "fail")
        assert e.is_exec_error() is True
        assert e.is_vm_error() is False

    def test_is_file_error(self):
        e = RPCError(RPCError.FILE_FAILED, "fail")
        assert e.is_file_error() is True
        assert e.is_vm_error() is False

    def test_error_codes(self):
        assert RPCError.PARSE_ERROR == -32700
        assert RPCError.INVALID_REQUEST == -32600
        assert RPCError.METHOD_NOT_FOUND == -32601
        assert RPCError.INVALID_PARAMS == -32602
        assert RPCError.INTERNAL_ERROR == -32603
        assert RPCError.VM_FAILED == -32000
        assert RPCError.EXEC_FAILED == -32001
        assert RPCError.FILE_FAILED == -32002
