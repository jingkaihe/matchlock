# Vendored QEMU kernel (amd64)

Matchlock's QEMU TCG backend boots the guest with a Linux kernel via QEMU's
`-kernel`. The existing x86_64 kernel (`kernel`, built for Firecracker) is an
uncompressed ELF `vmlinux` with `CONFIG_ACPI=n`; QEMU's x86 loader rejects that
ELF (it needs a `bzImage`, or an ELF that carries a **PVH** note), and the `pc`
machine requires ACPI for PCI IRQ routing.

To make `MATCHLOCK_BACKEND=qemu` work on **amd64** without a manually supplied
`--kernel file://...`, this directory vendors a QEMU-compatible amd64 `bzImage`
built from the same kernel source/version the repo already pins, with the
QEMU-specific config deltas, and embeds it in the Linux binary so setup/run is
offline.

## What is vendored

| Artifact | SHA-256 |
|---|---|
| `kernel-qemu-bzImage-6.19.8.gz` (compressed, embedded) | `4cefea4fe557ee13a4e94b59054f423c6bd734cad8ed94ef047dd941b54713cb` |
| … decompressed `bzImage` (what QEMU `-kernel` loads) | `7245efaaf5ffd6f160270f755d6a9bd5a631da4d7e5e162f47245fcf171dab60` |
| `qemu-x86_64.config` (build input) | `7711ace2b67f515c5a46a6b866f47080d3612d93b907da9ee12df4b3fbcfb4d0` |
| `resolved-config.txt` (`.config` after `olddefconfig`) | `b3e76cfb7bbb2b8c8c821aa795d266628073dc5766974fd7e1da845cd4b65829` |

- **Kernel version:** `6.19.8` (same as `pkg/kernel.Version`).
- **Source:** `linux-6.19.8` from
  `https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.19.8.tar.xz`
  (tar SHA-256 `aada4722db8bcfa0b9732851856d405082b6a4fa2e3ab067be8db17cdd115b38`).
- **Config:** `qemu-x86_64.config` (this directory), derived from
  `guest/kernel/x86_64.config` with the QEMU-required deltas; `olddefconfig` was
  run to produce the exact `.config` that was compiled (`resolved-config.txt`).
- **Recipe:** `cp qemu-x86_64.config .config && make olddefconfig && make -j$(nproc) bzImage`.
- **License:** the Linux kernel is **GPL-2.0 with Linux-syscall-note**
  (`COPYING` in this directory). The kernel binary is distributed under the GPL;
  the corresponding **complete source** is the `linux-6.19.8` tarball
  (source URL + tar SHA-256 in `manifest.json`), which is the source that
  produced this binary.

## Provenance & reproducibility

The authoritative facts live in `manifest.json` in this directory; the Go
package reads it via `//go:embed`, verifies the **compressed** SHA-256 of the
blob and the **uncompressed** SHA-256 after extraction, and only then publishes
the kernel to the cache (atomic temp-file + rename).

> **Not hermetic.** The build happened on an Ubuntu 22.04 x86_64 host with the
> distro `gcc`/`make`/`flex`/`bison`. It is **not byte-for-byte reproducible**
> from the config alone (toolchain version, timestamps, and `CONFIG_LOCALVERSION`
> and the `root@ubuntu` host string are baked in). The vendored blob is the
> authoritative artifact; the config/source are the provenance and the recipe to
> rebuild a *functionally equivalent* kernel.

## Why these deltas

The Firecracker x86_64 config sets `CONFIG_ACPI=n` (Firecracker does not need
ACPI) and builds `vmlinux`. QEMU's `pc` board requires:

- **ACPI on** (`CONFIG_ACPI=y`) for PCI IRQ routing and the standard x86 boot
  path.
- A kernel image format QEMU's x86 loader accepts. QEMU refuses an uncompressed
  ELF `vmlinux` without a **PVH ELF note** (`Error loading uncompressed kernel
  without PVH ELF Note`). The standard `make bzImage` target produces a
  compressed self-decompressing x86 kernel that QEMU loads directly — that is
  what we build and vendor. (`CONFIG_XEN=y`/`CONFIG_XEN_PVH=y` are kept because
  they are harmless and match the source config's paravirt posture; the bzImage
  build does not *require* the PVH note.)

## Scope / boundary

This vendors the **amd64** boot kernel needed to run the QEMU **amd64** backend
offline. It is not used by:

- **Firecracker** (uses `guest/kernel/x86_64.config` → `vmlinux`, fetched from
  `ghcr.io/jingkaihe/matchlock/kernel:<version>`).
- **arm64 QEMU** (uses the standard arm64 `Image`; arm64 QEMU's `virt` board
  accepts it, and the arm64 path is unchanged).
- **macOS** (Virtualization.framework, `pkg/vm/darwin`).

## How to rebuild / update

1. Fetch `linux-<version>.tar.xz` and verify the tar SHA-256.
2. `cp qemu-x86_64.config .config && make olddefconfig && make -j$(nproc) bzImage`.
3. `gzip -9` the `bzImage`; record the compressed SHA-256 and the uncompressed
   SHA-256 (`gzip -dc bzImage.gz | sha256sum`).
4. Update `manifest.json` (compressed_sha256, sha256, config_sha256, source
   tar_sha256, version) and the table above.
