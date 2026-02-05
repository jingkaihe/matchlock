#!/bin/bash
set -e

# Download pre-built rootfs from Firecracker's CI artifacts
# For quick testing without building

OUTPUT_DIR="${OUTPUT_DIR:-/opt/sandbox}"

echo "Downloading pre-built Firecracker rootfs..."

mkdir -p "$OUTPUT_DIR"

# Firecracker provides a minimal rootfs for testing
ROOTFS_URL="https://s3.amazonaws.com/spec.ccfc.min/img/quickstart_guide/x86_64/rootfs/bionic.rootfs.ext4"

echo "Downloading rootfs (this may take a while)..."
curl -L -o "$OUTPUT_DIR/rootfs-quickstart.ext4" "$ROOTFS_URL"

chmod 644 "$OUTPUT_DIR/rootfs-quickstart.ext4"

echo "Rootfs downloaded to $OUTPUT_DIR/rootfs-quickstart.ext4"
echo "Size: $(du -h $OUTPUT_DIR/rootfs-quickstart.ext4 | cut -f1)"

echo ""
echo "Note: This is a basic Ubuntu rootfs without guest-agent."
echo "For full Matchlock functionality, build your own rootfs:"
echo "  make rootfs"
