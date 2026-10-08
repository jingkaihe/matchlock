# Vendored Firecracker assets

Matchlock's Linux host setup needs the Firecracker VMM (`firecracker`) and its
`jailer`. To make `matchlock setup linux` and the Linux packaging pipeline work
**offline**, the Firecracker release tarballs are vendored directly into this
repository and embedded into the Linux binary.

## What is vendored

| Arch      | Archive                                    | SHA-256                                                       |
|-----------|--------------------------------------------|---------------------------------------------------------------|
| aarch64   | `firecracker-v1.10.1-aarch64.tgz`          | `9e3641071de140979afaac0c52fdc107baeba398bdb5709c12f77ee469207fcd` |
| x86_64    | `firecracker-v1.10.1-x86_64.tgz`           | `36112969952b0e34fadcfca769d48a55dc22cbba99af17e02bd0e24fc35adc77` |

- **Version:** `v1.10.1`
- **Source:** <https://github.com/firecracker-microvm/firecracker/releases/download/v1.10.1>
- **Upstream release commit:** `1fcdaec088da9e30c2c111f9feb2649118488361`
  (peeled `refs/tags/v1.10.1^{}`; the annotated tag object is `5d76ebed49c1ccc46c828fdf8efb4a812fcde765`)
- **License / NOTICE:** each archive contains top-level `LICENSE`, `NOTICE`, and
  `THIRD-PARTY` files inside its `release-<version>-<arch>/` directory. The
  binaries are distributed under the terms of those files (upstream is Apache-2.0).

The authoritative copy of these facts is `manifest.json` in this directory; the
Go package reads it via `go:embed` and verifies each archive's SHA-256 at extract
time.

## Where it is consumed

- `cmd/matchlock/setup_linux.go` → `matchlock setup linux` extracts
  `firecracker` + `jailer` from the embedded archive (no network).
- `packaging/linux/scripts/fetch-firecracker.sh` → used by GoReleaser
  `before.hooks` to stage the two binaries for `.deb`/`.rpm`.
- `scripts/install-firecracker.sh` → the `mise run install:firecracker` target.

All three read `manifest.json`, verify the archive SHA-256, and extract only the
exact manifest-listed members. The Go extractor used by `matchlock setup linux`
writes via a same-directory temp file + rename, so an interrupted install never
leaves a partial executable. The two shell scripts stage binaries with
`install`; they are full archives verified against the manifest before any write
occurs.

## How to update to a new Firecracker version

1. Download the new `firecracker-<version>-<arch>.tgz` for both `aarch64` and
   `x86_64` from the upstream release page.
2. Get the exact upstream commit for that tag:
   `git ls-remote https://github.com/firecracker-microvm/firecracker.git 'refs/tags/<tag>^{}'`
3. Replace the two tarballs in `tarballs/` and record the new archive SHA-256s,
   release commit, and tag object in `manifest.json`.
4. Update the hardcoded version in `.goreleaser.yaml` (`before.hooks` call
   `fetch-firecracker.sh <VERSION> ...` and the nfpm `contents` use
   `.goreleaser-firecracker/<arch>`), and the expected version/provenance
   assertions in `internal/assets/firecracker/assets_test.go`.
5. Re-run `go test ./internal/assets/firecracker/` and the shell extraction checks.

## Scope / boundary (what this does NOT vendor)

Vendoring Firecracker removes the **Firecracker download** from both `setup linux`
and Linux packaging — the one download that was previously non-deterministic
(the old code fetched the "latest" release at runtime).

It does **not** vendor these, which still require network access at runtime:

- **Go modules** — fetched by `go mod download`; cached in the module cache, not
  in this repo.
- **Guest kernels** — `pkg/kernel` pulls prebuilt kernels from
  `ghcr.io/jingkaihe/matchlock/kernel:<version>` on first run.
- **Container rootfs images** — `matchlock run --image <ref>` pulls user-selected
  images from registries on demand.

## Platform note

Firecracker is a Linux VMM and is only consumed by the **Linux** backend. The
**macOS** backend uses Apple's Virtualization.framework (`pkg/vm/darwin`) and
does not use these binaries; `scripts/install-firecracker.sh` refuses to run on
non-Linux hosts for that reason.
