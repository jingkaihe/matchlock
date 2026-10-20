#!/usr/bin/env bash
set -euo pipefail

# fetch-firecracker.sh <version> <arch> <output-dir> <binary-name>
#
# Extracts a Firecracker binary (<binary-name> is one of firecracker|jailer)
# from the tarball vendored in this repository for the given architecture.
# It verifies the archive SHA-256 against internal/assets/firecracker/manifest.json
# and extracts exactly the member named in the manifest. It NEVER contacts the
# network.
#
# The four-argument interface is retained for compatibility with the GoReleaser
# `before.hooks` in .goreleaser.yaml.

if [ "$#" -ne 4 ]; then
  echo "usage: $0 <version> <arch> <output-dir> <binary-name>" >&2
  exit 1
fi

VERSION="$1"
ARCH="$2"
OUTDIR="$3"
BINARY="$4"

case "$ARCH" in
  amd64) FC_ARCH="x86_64" ;;
  arm64) FC_ARCH="aarch64" ;;
  *)
    echo "unsupported arch: $ARCH (expected amd64|arm64)" >&2
    exit 1
    ;;
esac

case "$BINARY" in
  firecracker|jailer) ;;
  *)
    echo "unsupported binary: $BINARY (expected firecracker|jailer)" >&2
    exit 1
    ;;
esac

# Resolve the repo root: .../packaging/linux/scripts -> .../ (three levels up).
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
MANIFEST="$REPO_ROOT/internal/assets/firecracker/manifest.json"
TARBALLS_DIR="$REPO_ROOT/internal/assets/firecracker/tarballs"

if [ ! -f "$MANIFEST" ]; then
  echo "missing manifest: $MANIFEST" >&2
  exit 1
fi

# Resolve FILE, EXPECTED_SHA, MEMBER from the manifest. Fail hard on mismatch
# BEFORE doing any filesystem work. Capture the Python stdout in a variable
# first so a nonzero Python exit is seen by `set -e`.
META="$(python3 - "$MANIFEST" "$VERSION" "$FC_ARCH" "$BINARY" <<'PY'
import json, sys
manifest, version, arch, binary = sys.argv[1:]
try:
    with open(manifest) as f:
        m = json.load(f)
except Exception as e:
    print(f"cannot read manifest: {e}", file=sys.stderr); sys.exit(2)
if m.get("version") != version:
    print(f"manifest version {m.get('version')} != requested {version}", file=sys.stderr); sys.exit(2)
for a in m["archives"]:
    if a["arch"] == arch:
        member = a["members"].get(binary)
        if not member:
            print(f"binary {binary} not present in {a['file']}", file=sys.stderr); sys.exit(2)
        print(a["file"], a["sha256"], member)
        break
else:
    print(f"no vendored archive for arch {arch}", file=sys.stderr); sys.exit(2)
PY
)"

read -r FILE EXPECTED_SHA MEMBER <<<"$META"

ARCHIVE="$TARBALLS_DIR/$FILE"
if [ ! -f "$ARCHIVE" ]; then
  echo "vendored archive missing: $ARCHIVE" >&2
  echo "Run the Firecracker vendor step to restore it." >&2
  exit 1
fi

ACTUAL_SHA="$(sha256sum "$ARCHIVE" | awk '{print $1}')"
if [ "$ACTUAL_SHA" != "$EXPECTED_SHA" ]; then
  echo "checksum mismatch for $FILE: got $ACTUAL_SHA want $EXPECTED_SHA" >&2
  exit 1
fi

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

tar -xzf "$ARCHIVE" -C "$TMPDIR" "$MEMBER"
SRC="$TMPDIR/$MEMBER"
if [ ! -f "$SRC" ]; then
  echo "missing binary in archive: $SRC" >&2
  exit 1
fi

mkdir -p "$OUTDIR"
install -m 0755 "$SRC" "$OUTDIR/$BINARY"
echo "Installed $OUTDIR/$BINARY from vendored $FILE (sha256 ${EXPECTED_SHA:0:12}...)"
