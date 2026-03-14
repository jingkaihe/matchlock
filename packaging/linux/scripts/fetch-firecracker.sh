#!/usr/bin/env bash
set -euo pipefail

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
    echo "unsupported arch: $ARCH" >&2
    exit 1
    ;;
esac

mkdir -p "$OUTDIR"
TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

URL="https://github.com/firecracker-microvm/firecracker/releases/download/${VERSION}/firecracker-${VERSION}-${FC_ARCH}.tgz"
ARCHIVE="$TMPDIR/firecracker.tgz"

curl -fsSL "$URL" -o "$ARCHIVE"
tar -xzf "$ARCHIVE" -C "$TMPDIR"

SRC="$TMPDIR/release-${VERSION}-${FC_ARCH}/${BINARY}-${VERSION}-${FC_ARCH}"
DEST="$OUTDIR/$BINARY"

if [ ! -f "$SRC" ]; then
  echo "missing binary in archive: $SRC" >&2
  exit 1
fi

install -m 0755 "$SRC" "$DEST"
