"""Tests for matchlock.builder (Sandbox)."""

from matchlock.builder import Sandbox
from matchlock.types import (
    CreateOptions,
    ImageConfig,
    MountConfig,
    VFSHookRule,
    VFSInterceptionConfig,
)


class TestSandboxInit:
    def test_image_set(self):
        s = Sandbox("alpine:latest")
        opts = s.options()
        assert opts.image == "alpine:latest"

    def test_returns_create_options(self):
        s = Sandbox("ubuntu:22.04")
        assert isinstance(s.options(), CreateOptions)


class TestSandboxResources:
    def test_with_cpus(self):
        opts = Sandbox("img").with_cpus(4).options()
        assert opts.cpus == 4

    def test_with_memory(self):
        opts = Sandbox("img").with_memory(1024).options()
        assert opts.memory_mb == 1024

    def test_with_disk_size(self):
        opts = Sandbox("img").with_disk_size(2048).options()
        assert opts.disk_size_mb == 2048

    def test_with_timeout(self):
        opts = Sandbox("img").with_timeout(600).options()
        assert opts.timeout_seconds == 600

    def test_with_workspace(self):
        opts = Sandbox("img").with_workspace("/code").options()
        assert opts.workspace == "/code"


class TestSandboxChaining:
    def test_fluent_chaining(self):
        opts = (
            Sandbox("python:3.12")
            .with_cpus(2)
            .with_memory(512)
            .with_disk_size(4096)
            .with_timeout(300)
            .with_workspace("/home")
            .options()
        )
        assert opts.image == "python:3.12"
        assert opts.cpus == 2
        assert opts.memory_mb == 512
        assert opts.disk_size_mb == 4096
        assert opts.timeout_seconds == 300
        assert opts.workspace == "/home"

    def test_all_methods_return_sandbox(self):
        s = Sandbox("img")
        assert isinstance(s.with_cpus(1), Sandbox)
        assert isinstance(s.with_memory(1), Sandbox)
        assert isinstance(s.with_disk_size(1), Sandbox)
        assert isinstance(s.with_timeout(1), Sandbox)
        assert isinstance(s.with_workspace("/x"), Sandbox)
        assert isinstance(s.with_vfs_interception(VFSInterceptionConfig()), Sandbox)
        assert isinstance(s.with_env("K", "V"), Sandbox)
        assert isinstance(s.with_env_map({"K": "V"}), Sandbox)
        assert isinstance(s.allow_host("x.com"), Sandbox)
        assert isinstance(s.block_private_ips(), Sandbox)
        assert isinstance(s.with_block_private_ips(False), Sandbox)
        assert isinstance(s.allow_private_ips(), Sandbox)
        assert isinstance(s.unset_block_private_ips(), Sandbox)
        assert isinstance(s.add_secret("k", "v"), Sandbox)
        assert isinstance(s.with_dns_servers("1.1.1.1"), Sandbox)
        assert isinstance(s.with_network_mtu(1200), Sandbox)
        assert isinstance(s.mount("/p", MountConfig()), Sandbox)
        assert isinstance(s.mount_host_dir("/g", "/h"), Sandbox)
        assert isinstance(s.mount_host_dir_readonly("/g", "/h"), Sandbox)
        assert isinstance(s.mount_memory("/m"), Sandbox)
        assert isinstance(s.mount_overlay("/o", "/h"), Sandbox)


class TestSandboxNetwork:
    def test_env_single(self):
        opts = Sandbox("img").with_env("FOO", "bar").options()
        assert opts.env == {"FOO": "bar"}

    def test_env_map_merge(self):
        opts = (
            Sandbox("img")
            .with_env("A", "1")
            .with_env_map({"A": "2", "B": "3"})
            .options()
        )
        assert opts.env == {"A": "2", "B": "3"}

    def test_allow_host_single(self):
        opts = Sandbox("img").allow_host("api.example.com").options()
        assert opts.allowed_hosts == ["api.example.com"]

    def test_allow_host_multiple(self):
        opts = Sandbox("img").allow_host("a.com", "b.com").options()
        assert opts.allowed_hosts == ["a.com", "b.com"]

    def test_allow_host_cumulative(self):
        opts = Sandbox("img").allow_host("a.com").allow_host("b.com", "c.com").options()
        assert opts.allowed_hosts == ["a.com", "b.com", "c.com"]

    def test_block_private_ips(self):
        opts = Sandbox("img").block_private_ips().options()
        assert opts.block_private_ips is True
        assert opts.block_private_ips_set is True

    def test_with_block_private_ips_false(self):
        opts = Sandbox("img").with_block_private_ips(False).options()
        assert opts.block_private_ips is False
        assert opts.block_private_ips_set is True

    def test_allow_private_ips(self):
        opts = Sandbox("img").allow_private_ips().options()
        assert opts.block_private_ips is False
        assert opts.block_private_ips_set is True

    def test_unset_block_private_ips(self):
        opts = Sandbox("img").block_private_ips().unset_block_private_ips().options()
        assert opts.block_private_ips is False
        assert opts.block_private_ips_set is False

    def test_dns_servers(self):
        opts = Sandbox("img").with_dns_servers("1.1.1.1", "1.0.0.1").options()
        assert opts.dns_servers == ["1.1.1.1", "1.0.0.1"]

    def test_dns_servers_chained(self):
        opts = (
            Sandbox("img")
            .with_dns_servers("1.1.1.1")
            .with_dns_servers("8.8.8.8")
            .options()
        )
        assert opts.dns_servers == ["1.1.1.1", "8.8.8.8"]

    def test_dns_servers_default_empty(self):
        opts = Sandbox("img").options()
        assert opts.dns_servers == []

    def test_network_mtu(self):
        opts = Sandbox("img").with_network_mtu(1200).options()
        assert opts.network_mtu == 1200


class TestSandboxSecrets:
    def test_add_secret_no_hosts(self):
        opts = Sandbox("img").add_secret("KEY", "value").options()
        assert len(opts.secrets) == 1
        s = opts.secrets[0]
        assert s.name == "KEY"
        assert s.value == "value"
        assert s.hosts == []

    def test_add_secret_with_hosts(self):
        opts = Sandbox("img").add_secret("KEY", "val", "a.com", "b.com").options()
        assert opts.secrets[0].hosts == ["a.com", "b.com"]

    def test_multiple_secrets(self):
        opts = (
            Sandbox("img")
            .add_secret("A", "1", "a.com")
            .add_secret("B", "2", "b.com")
            .options()
        )
        assert len(opts.secrets) == 2
        assert opts.secrets[0].name == "A"
        assert opts.secrets[1].name == "B"


class TestSandboxMounts:
    def test_mount_host_dir(self):
        opts = Sandbox("img").mount_host_dir("/guest", "/host").options()
        m = opts.mounts["/guest"]
        assert m.type == "real_fs"
        assert m.host_path == "/host"
        assert m.readonly is False

    def test_mount_host_dir_readonly(self):
        opts = Sandbox("img").mount_host_dir_readonly("/guest", "/host").options()
        m = opts.mounts["/guest"]
        assert m.type == "real_fs"
        assert m.host_path == "/host"
        assert m.readonly is True

    def test_mount_memory(self):
        opts = Sandbox("img").mount_memory("/tmp").options()
        m = opts.mounts["/tmp"]
        assert m.type == "memory"

    def test_mount_overlay(self):
        opts = Sandbox("img").mount_overlay("/data", "/host/data").options()
        m = opts.mounts["/data"]
        assert m.type == "overlay"
        assert m.host_path == "/host/data"

    def test_mount_custom(self):
        cfg = MountConfig(type="real_fs", host_path="/custom", readonly=True)
        opts = Sandbox("img").mount("/workspace/custom", cfg).options()
        m = opts.mounts["/workspace/custom"]
        assert m.type == "real_fs"
        assert m.readonly is True

    def test_multiple_mounts(self):
        opts = (
            Sandbox("img")
            .mount_host_dir("/a", "/ha")
            .mount_memory("/b")
            .mount_overlay("/c", "/hc")
            .options()
        )
        assert len(opts.mounts) == 3
        assert "/a" in opts.mounts
        assert "/b" in opts.mounts
        assert "/c" in opts.mounts


class TestSandboxVFSInterception:
    def test_with_vfs_interception(self):
        cfg = VFSInterceptionConfig(
            rules=[
                VFSHookRule(
                    phase="before",
                    ops=["create"],
                    path="/workspace/blocked.txt",
                    action="block",
                )
            ],
        )
        opts = Sandbox("img").with_vfs_interception(cfg).options()
        assert opts.vfs_interception is not None
        assert len(opts.vfs_interception.rules) == 1
        assert opts.vfs_interception.rules[0].action == "block"


class TestSandboxImageConfig:
    def test_with_user_then_image_config_preserves_user(self):
        opts = (
            Sandbox("img")
            .with_user("nobody")
            .with_image_config(ImageConfig(working_dir="/tmp"))
            .options()
        )
        assert opts.image_config is not None
        assert opts.image_config.user == "nobody"
        assert opts.image_config.working_dir == "/tmp"

    def test_with_image_config_then_user_overrides(self):
        opts = (
            Sandbox("img")
            .with_image_config(ImageConfig(working_dir="/tmp", user="root"))
            .with_user("nobody")
            .options()
        )
        assert opts.image_config.user == "nobody"
        assert opts.image_config.working_dir == "/tmp"

    def test_with_image_config_merges_fields(self):
        opts = (
            Sandbox("img")
            .with_entrypoint("python3")
            .with_image_config(ImageConfig(cmd=["app.py"]))
            .options()
        )
        assert opts.image_config.entrypoint == ["python3"]
        assert opts.image_config.cmd == ["app.py"]


class TestSandboxIndependence:
    def test_separate_instances_are_independent(self):
        s1 = Sandbox("img1").allow_host("a.com")
        s2 = Sandbox("img2").allow_host("b.com")
        assert s1.options().allowed_hosts == ["a.com"]
        assert s2.options().allowed_hosts == ["b.com"]
