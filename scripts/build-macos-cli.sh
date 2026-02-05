#!/bin/bash
set -e

# Build and sign matchlock CLI for macOS testing
# This script ONLY builds the CLI binary with proper entitlements

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
OUTPUT_DIR="${OUTPUT_DIR:-$HOME/.cache/matchlock}"

echo "=== Building Matchlock CLI for macOS ==="
cd "$PROJECT_ROOT"

# Create output directories
mkdir -p bin "$OUTPUT_DIR"

# Build the CLI
echo "Building matchlock binary..."
go build -o bin/matchlock ./cmd/matchlock
echo "✓ Built bin/matchlock"

# Build guest binaries for arm64 Linux
echo "Building guest-agent (arm64 Linux)..."
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o "$OUTPUT_DIR/guest-agent" ./cmd/guest-agent
echo "✓ Built $OUTPUT_DIR/guest-agent"

echo "Building guest-fused (arm64 Linux)..."
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o "$OUTPUT_DIR/guest-fused" ./cmd/guest-fused
echo "✓ Built $OUTPUT_DIR/guest-fused"

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

# Verify
if codesign -d --entitlements - bin/matchlock 2>&1 | grep -q "virtualization"; then
    echo "✓ Binary signed with virtualization entitlement"
else
    echo "⚠ Warning: Could not verify entitlement"
fi

echo ""
echo "=== Build Complete ==="
echo ""
echo "Binary: $PROJECT_ROOT/bin/matchlock"
echo "Guest Agent: $OUTPUT_DIR/guest-agent"
echo "Guest FUSE: $OUTPUT_DIR/guest-fused"
echo ""
echo "Next steps:"
echo ""
echo "1. Download kernel:"
echo "   curl -L -o $OUTPUT_DIR/kernel 'https://github.com/lima-vm/alpine-lima/releases/download/v0.2.36/vmlinuz-lts-6.6.32-0-virt'"
echo ""
echo "2. Create rootfs (requires Docker):"
echo "   ./scripts/setup-macos.sh"
echo ""
echo "   Or build from container image:"
echo "   # On a Linux machine or Docker:"
echo "   ./bin/matchlock build alpine:latest"
