#!/bin/bash
set -e

# Simple setup script for matchlock on macOS/Apple Silicon
# Downloads pre-built kernel and creates minimal rootfs

OUTPUT_DIR="${OUTPUT_DIR:-$HOME/.cache/matchlock}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"

echo "=== Matchlock macOS Setup (Simple) ==="
echo "Output directory: $OUTPUT_DIR"
mkdir -p "$OUTPUT_DIR"

# Step 1: Build guest binaries (static, for arm64)
echo ""
echo "=== Building guest binaries (arm64) ==="
cd "$PROJECT_ROOT"

# Build guest-agent for arm64 Linux
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o "$OUTPUT_DIR/guest-agent" ./cmd/guest-agent
echo "✓ Built guest-agent"

# Build guest-fused for arm64 Linux
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o "$OUTPUT_DIR/guest-fused" ./cmd/guest-fused
echo "✓ Built guest-fused"

# Step 2: Download arm64 kernel
echo ""
echo "=== Downloading arm64 Linux kernel ==="

# Download kernel from Lima's alpine-lima releases (known working with Virtualization.framework)
KERNEL_URL="https://github.com/lima-vm/alpine-lima/releases/download/v0.2.36/vmlinuz-lts-6.6.32-0-virt"

if [ ! -f "$OUTPUT_DIR/kernel" ]; then
    echo "Downloading kernel..."
    curl -L -o "$OUTPUT_DIR/kernel.gz" "$KERNEL_URL"
    
    # Check if it's gzipped and decompress
    if file "$OUTPUT_DIR/kernel.gz" | grep -q "gzip"; then
        echo "Decompressing kernel..."
        gunzip -c "$OUTPUT_DIR/kernel.gz" > "$OUTPUT_DIR/kernel"
        rm "$OUTPUT_DIR/kernel.gz"
    else
        mv "$OUTPUT_DIR/kernel.gz" "$OUTPUT_DIR/kernel"
    fi
    echo "✓ Downloaded kernel"
else
    echo "✓ Kernel already exists"
fi

# Verify kernel
file "$OUTPUT_DIR/kernel"

# Step 3: Download pre-built Alpine rootfs and inject guest binaries
echo ""
echo "=== Creating rootfs ==="

ROOTFS_IMG="$OUTPUT_DIR/rootfs-standard.ext4"
ALPINE_URL="https://github.com/lima-vm/alpine-lima/releases/download/v0.2.36/alpine-lima-std-3.20.3-aarch64.iso"

if [ ! -f "$ROOTFS_IMG" ]; then
    echo "Downloading Alpine Lima image..."
    curl -L -o "$OUTPUT_DIR/alpine.iso" "$ALPINE_URL"
    
    echo "Creating rootfs from Alpine..."
    
    # For now, create a simple ext4 image using hdiutil (macOS native)
    # This won't work directly - we need a different approach
    
    # Alternative: Use qemu-img if available, or download a pre-built rootfs
    if command -v qemu-img &> /dev/null; then
        echo "Using qemu-img to create disk..."
        qemu-img create -f raw "$ROOTFS_IMG" 512M
    else
        echo "Creating raw disk image..."
        dd if=/dev/zero of="$ROOTFS_IMG" bs=1M count=512 2>/dev/null
    fi
    
    # Note: We can't easily create ext4 on macOS without Docker or Linux tools
    # For testing, let's download a pre-built rootfs instead
    
    echo ""
    echo "⚠ Note: Creating ext4 rootfs requires Linux tools."
    echo "  For a quick test, you can download a pre-built rootfs:"
    echo ""
    echo "  Option 1: Use Lima's pre-built image"
    echo "    limactl start template://alpine"
    echo "    (then copy the disk image)"
    echo ""
    echo "  Option 2: Use Docker to create the rootfs"
    echo "    Run: ./scripts/setup-macos.sh (requires Docker)"
    echo ""
    
    # Clean up
    rm -f "$OUTPUT_DIR/alpine.iso" "$ROOTFS_IMG"
    
    echo "For now, let's create a minimal test using a different approach..."
fi

# Step 4: Build and sign the matchlock binary
echo ""
echo "=== Building and signing matchlock binary ==="

cd "$PROJECT_ROOT"
mkdir -p bin
go build -o bin/matchlock ./cmd/matchlock
echo "✓ Built matchlock binary"

# Create entitlements file
ENTITLEMENTS="$PROJECT_ROOT/matchlock.entitlements"
cat > "$ENTITLEMENTS" << 'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>com.apple.security.virtualization</key>
    <true/>
</dict>
</plist>
EOF

# Sign the binary with entitlements
echo "Signing binary with virtualization entitlement..."
codesign --sign - --entitlements "$ENTITLEMENTS" --force bin/matchlock
echo "✓ Signed matchlock binary"

# Verify the signature
if codesign -d --entitlements - bin/matchlock 2>&1 | grep -q "virtualization"; then
    echo "✓ Entitlement verified"
else
    echo "⚠ Could not verify entitlement"
fi

echo ""
echo "=== Partial Setup Complete ==="
echo ""
echo "Files created:"
echo "  - $OUTPUT_DIR/kernel (Linux arm64 kernel)"
echo "  - $OUTPUT_DIR/guest-agent (Guest agent binary)"  
echo "  - $OUTPUT_DIR/guest-fused (Guest FUSE daemon)"
echo "  - $PROJECT_ROOT/bin/matchlock (Signed CLI)"
echo ""
echo "⚠ Missing: rootfs image"
echo ""
echo "To create rootfs, you need Docker. Install Docker Desktop and run:"
echo "  ./scripts/setup-macos.sh"
echo ""
echo "Or use the image builder once you have Docker:"
echo "  ./bin/matchlock build alpine:latest"
