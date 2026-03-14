# Linux packaging plan

This document captures the initial upstream packaging strategy for Matchlock on Debian, Ubuntu, Fedora, CentOS Stream, and RHEL-like systems.

## Recommendation

Use GoReleaser + nfpm to generate upstream `.deb` and `.rpm` artifacts directly from the Matchlock release pipeline.

This is the fastest path to a usable Linux package story because the repo already has portable Linux build targets for:

- `matchlock`
- `guest-init`

Those are currently produced by the cross-build tasks in `/home/jingkaihe/workspace/matchlock/mise.toml` and uploaded as GitHub release artifacts.

## Why this approach

Native distro packaging for each target family is possible, but it creates extra maintenance overhead early:

- separate spec/control files per distro family
- distro-specific repo hosting and signing workflows
- different dependency names and scriptlet behavior
- slower iteration while Matchlock install/runtime requirements are still evolving

GoReleaser + nfpm gives one source of truth for package metadata while still producing native package formats.

## Package shape

The initial package should install:

- `/usr/bin/matchlock`
- `/usr/bin/guest-init`
- `/usr/libexec/matchlock/firecracker`
- `/usr/libexec/matchlock/jailer`
- `/usr/lib/sysctl.d/99-matchlock.conf`
- `/usr/share/doc/matchlock/README.packaging.md`

Installing `guest-init` next to `matchlock` keeps the current runtime lookup logic working without wrapper scripts or extra environment variables.

## Package lifecycle rules

Package install scripts should do only machine-safe, non-user-specific setup:

- apply `cap_net_admin,cap_net_raw` to `/usr/bin/matchlock`
- attempt to enable `net.ipv4.ip_forward=1` immediately
- attempt to load `nf_tables`

Package install scripts should **not** do the following:

- download Firecracker from the network
- run `usermod` on arbitrary local users
- guess which account should be added to `kvm` or `netdev`
- create user-specific state under home directories

That work remains either:

- a documented manual step, or
- explicit admin commands such as `sudo matchlock setup linux` and `sudo matchlock setup user <name>`

## Runtime dependencies

Matchlock requires host tools beyond the main binary.

Current direct dependencies from the codebase include:

- `e2fsprogs` for `debugfs`, `mkfs.ext4`, and related tools
- `erofs-utils` for `mkfs.erofs`
- `libcap` tooling to apply file capabilities during install
- `nftables`
- `kmod`
- `procps`

Firecracker and jailer are bundled into the package under `/usr/libexec/matchlock/`, and the Linux runtime prefers those packaged paths before falling back to `PATH`.

## Distro dependency mapping

Initial nfpm dependency mapping:

### Debian / Ubuntu

- `e2fsprogs`
- `erofs-utils`
- `libcap2-bin`
- `nftables`
- `kmod`
- `procps`

### Fedora / RHEL / CentOS Stream

- `e2fsprogs`
- `erofs-utils`
- `libcap`
- `nftables`
- `kmod`
- `procps-ng`

## Gaps left intentionally open

This starter implementation does **not** solve every Linux-host prerequisite. It deliberately leaves these for manual admin action or a later packaging refinement:

- `kvm` group membership
- `/dev/net/tun` creation/ownership on hosts where it is missing or restricted

Those are host-policy decisions and should not be done implicitly by package managers for an unknown target user.

## Setup commands

The Linux setup flow keeps one simple fallback command:

- `sudo matchlock setup linux` to repair or bootstrap the host for the current user

For explicit user enrollment, Matchlock also supports:

- `sudo matchlock setup user <name>`

## Local build command

Use:

```bash
mise run package:linux
```

That runs a GoReleaser snapshot build and emits raw binaries plus `.deb` and `.rpm` packages under `dist/`.

## Recommended next steps

1. Publish the upstream `.deb` and `.rpm` artifacts on GitHub Releases.
2. Add a small installation guide to the README once packages are actually published.
3. Split Linux admin setup into package-safe and user-specific subcommands.
4. Optionally add a dedicated `matchlock-firecracker` package later if upstream wants a fully self-contained package experience.
