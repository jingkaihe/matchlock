#!/usr/bin/env bash
set -euo pipefail

# install-firecracker.sh
#
# Extracts firecracker and jailer from the Firecracker archive vendored inside
# this repository and installs them to /usr/libexec/matchlock (the path the
# Linux runtime consults first). Fully offline; no network access.
#
# This is the target of the `mise run install:firecracker` task.
#
# Linux host only: Firecracker is a Linux VMM and is consumed by the Linux
# backend. The macOS backend uses Virtualization.framework and must NOT have
# these Linux binaries installed.

if [ "$(uname -s)" != "Linux" ]; then
  echo "install-firecracker.sh is for Linux hosts only (found $(uname -s))." >&2
  exit 1
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MANIFEST="$REPO_ROOT/internal/assets/firecracker/manifest.json"
TARBALLS_DIR="$REPO_ROOT/internal/assets/firecracker/tarballs"
DEST="${MATCHLOCK_FIRECRACKER_DIR:-/usr/libexec/matchlock}"

if [ ! -f "$MANIFEST" ]; then
  echo "missing manifest: $MANIFEST" >&2
  exit 1
fi

HOST_ARCH="$(uname -m)"
case "$HOST_ARCH" in
  amd64|x86_64) FC_ARCH="x86_64" ;;
  arm64|aarch64) FC_ARCH="aarch64" ;;
  *) echo "unsupported host arch: $HOST_ARCH" >&2; exit 1 ;;
esac

# Resolve file, expected sha256, and the two member paths in one pass. Python
# stdout is captured into a variable first, so a nonzero exit is seen by
# `set -e` (the heredoc is consumed by the subshell, then `read`).
META="$(python3 - "$MANIFEST" "$FC_ARCH" <<'PY'
import json, sys
manifest, arch = sys.argv[1:]
with open(manifest) as f:
    m = json.load(f)
for a in m["archives"]:
    if a["arch"] == arch:
        print(a["file"], a["sha256"], a["members"]["firecracker"], a["members"]["jailer"])
        break
else:
    print(f"no vendored archive for arch {arch}", file=sys.stderr); sys.exit(2)
PY
)"
read -r FILE EXPECTED_SHA MEMBER_FC MEMBER_JAILER <<<"$META"

ARCHIVE="$TARBALLS_DIR/$FILE"
if [ ! -f "$ARCHIVE" ]; then
  echo "vendored archive missing: $ARCHIVE" >&2
  exit 1
fi

ACTUAL_SHA="$(sha256sum "$ARCHIVE" | awk '{print $1}')"
if [ "$ACTUAL_SHA" != "$EXPECTED_SHA" ]; then
  echo "checksum mismatch for $FILE: got $ACTUAL_SHA want $EXPECTED_SHA" >&2
  exit 1
fi

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

tar -xzf "$ARCHIVE" -C "$TMPDIR" "$MEMBER_FC" "$MEMBER_JAILER"

mkdir -p "$DEST"
install -m 0755 "$TMPDIR/$MEMBER_FC" "$DEST/firecracker"
install -m 0755 "$TMPDIR/$MEMBER_JAILER" "$DEST/jailer"

echo "Installed firecracker and jailer to $DEST (from vendored $FILE, sha256 ${EXPECTED_SHA:0:12}...)"
