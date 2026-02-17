"""Fluent builder for sandbox configuration.

Usage:
    sandbox = Sandbox("python:3.12-alpine") \\
        .allow_host("api.openai.com") \\
        .add_secret("API_KEY", os.environ["API_KEY"], "api.openai.com")

    vm_id = client.launch(sandbox)
"""

from __future__ import annotations

from .types import (
    CreateOptions,
    ImageConfig,
    MountConfig,
    Secret,
    VFSInterceptionConfig,
)


class Sandbox:
    """Fluent builder for sandbox configuration."""

    def __init__(self, image: str) -> None:
        self._opts = CreateOptions(image=image)

    def with_cpus(self, cpus: int) -> Sandbox:
        self._opts.cpus = cpus
        return self

    def with_memory(self, mb: int) -> Sandbox:
        self._opts.memory_mb = mb
        return self

    def with_disk_size(self, mb: int) -> Sandbox:
        self._opts.disk_size_mb = mb
        return self

    def with_timeout(self, seconds: int) -> Sandbox:
        self._opts.timeout_seconds = seconds
        return self

    def with_workspace(self, path: str) -> Sandbox:
        self._opts.workspace = path
        return self

    def with_vfs_interception(self, config: VFSInterceptionConfig) -> Sandbox:
        self._opts.vfs_interception = config
        return self

    def with_env(self, name: str, value: str) -> Sandbox:
        self._opts.env[name] = value
        return self

    def with_env_map(self, env: dict[str, str]) -> Sandbox:
        self._opts.env.update(env)
        return self

    def allow_host(self, *hosts: str) -> Sandbox:
        self._opts.allowed_hosts.extend(hosts)
        return self

    def block_private_ips(self) -> Sandbox:
        return self.with_block_private_ips(True)

    def with_block_private_ips(self, enabled: bool) -> Sandbox:
        self._opts.block_private_ips = enabled
        self._opts.block_private_ips_set = True
        return self

    def allow_private_ips(self) -> Sandbox:
        return self.with_block_private_ips(False)

    def unset_block_private_ips(self) -> Sandbox:
        self._opts.block_private_ips = False
        self._opts.block_private_ips_set = False
        return self

    def with_network_mtu(self, mtu: int) -> Sandbox:
        self._opts.network_mtu = mtu
        return self

    def add_secret(self, name: str, value: str, *hosts: str) -> Sandbox:
        self._opts.secrets.append(Secret(name=name, value=value, hosts=list(hosts)))
        return self

    def with_dns_servers(self, *servers: str) -> Sandbox:
        self._opts.dns_servers.extend(servers)
        return self

    def with_hostname(self, hostname: str) -> Sandbox:
        self._opts.hostname = hostname
        return self

    def mount(self, guest_path: str, config: MountConfig) -> Sandbox:
        self._opts.mounts[guest_path] = config
        return self

    def mount_host_dir(self, guest_path: str, host_path: str) -> Sandbox:
        return self.mount(guest_path, MountConfig(type="real_fs", host_path=host_path))

    def mount_host_dir_readonly(self, guest_path: str, host_path: str) -> Sandbox:
        return self.mount(
            guest_path, MountConfig(type="real_fs", host_path=host_path, readonly=True)
        )

    def mount_memory(self, guest_path: str) -> Sandbox:
        return self.mount(guest_path, MountConfig(type="memory"))

    def mount_overlay(self, guest_path: str, host_path: str) -> Sandbox:
        return self.mount(guest_path, MountConfig(type="overlay", host_path=host_path))

    def with_user(self, user: str) -> Sandbox:
        """Set the user to run commands as (uid, uid:gid, or username)."""
        if self._opts.image_config is None:
            self._opts.image_config = ImageConfig()
        self._opts.image_config.user = user
        return self

    def with_entrypoint(self, *entrypoint: str) -> Sandbox:
        """Override the image entrypoint."""
        if self._opts.image_config is None:
            self._opts.image_config = ImageConfig()
        self._opts.image_config.entrypoint = list(entrypoint)
        return self

    def with_image_config(self, config: ImageConfig) -> Sandbox:
        """Merge the given image configuration into any existing config.

        Fields set in *config* override existing values; falsy fields are left unchanged.
        """
        if self._opts.image_config is None:
            self._opts.image_config = ImageConfig()
        if config.user:
            self._opts.image_config.user = config.user
        if config.working_dir:
            self._opts.image_config.working_dir = config.working_dir
        if config.entrypoint:
            self._opts.image_config.entrypoint = config.entrypoint
        if config.cmd:
            self._opts.image_config.cmd = config.cmd
        if config.env:
            self._opts.image_config.env = config.env
        return self

    def options(self) -> CreateOptions:
        return self._opts
