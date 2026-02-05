#!/bin/bash
set -e

# Download pre-built kernel from Firecracker's CI artifacts
# This is faster than building from source

OUTPUT_DIR="${OUTPUT_DIR:-/opt/sandbox}"
KERNEL_VERSION="${KERNEL_VERSION:-5.10}"

echo "Downloading pre-built Firecracker kernel..."

mkdir -p "$OUTPUT_DIR"

# Firecracker provides pre-built kernels
# These are maintained by the Firecracker team
KERNEL_URL="https://s3.amazonaws.com/spec.ccfc.min/img/quickstart_guide/x86_64/kernels/vmlinux.bin"

echo "Downloading kernel..."
curl -L -o "$OUTPUT_DIR/kernel" "$KERNEL_URL"

chmod 644 "$OUTPUT_DIR/kernel"

echo "Kernel downloaded to $OUTPUT_DIR/kernel"
echo "Size: $(du -h $OUTPUT_DIR/kernel | cut -f1)"

# Verify it's an ELF binary
if file "$OUTPUT_DIR/kernel" | grep -q "ELF"; then
    echo "✓ Valid ELF binary"
else
    echo "⚠ Warning: May not be a valid kernel"
fi
