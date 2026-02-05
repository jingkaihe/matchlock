#!/bin/bash
set -e

INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
ARCH="${ARCH:-x86_64}"

echo "Installing Firecracker..."

# Get latest version
echo "Fetching latest release..."
FIRECRACKER_VERSION=$(curl -s https://api.github.com/repos/firecracker-microvm/firecracker/releases/latest | grep tag_name | cut -d '"' -f 4)

if [ -z "$FIRECRACKER_VERSION" ]; then
    echo "Failed to get latest version, using v1.6.0"
    FIRECRACKER_VERSION="v1.6.0"
fi

echo "Version: $FIRECRACKER_VERSION"

# Check if already installed
if command -v firecracker &> /dev/null; then
    INSTALLED_VERSION=$(firecracker --version | head -1 | awk '{print $2}')
    echo "Firecracker $INSTALLED_VERSION already installed"
    
    if [ "$INSTALLED_VERSION" = "${FIRECRACKER_VERSION#v}" ]; then
        echo "Already at latest version"
        exit 0
    fi
    
    read -p "Upgrade to $FIRECRACKER_VERSION? [y/N] " -n 1 -r
    echo
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        exit 0
    fi
fi

# Create temp directory
TEMP_DIR=$(mktemp -d)
trap "rm -rf $TEMP_DIR" EXIT

cd "$TEMP_DIR"

# Download
DOWNLOAD_URL="https://github.com/firecracker-microvm/firecracker/releases/download/${FIRECRACKER_VERSION}/firecracker-${FIRECRACKER_VERSION}-${ARCH}.tgz"
echo "Downloading from $DOWNLOAD_URL..."
curl -L -o firecracker.tgz "$DOWNLOAD_URL"

# Extract
echo "Extracting..."
tar xzf firecracker.tgz

# Find binary
FIRECRACKER_BIN=$(find . -name "firecracker-*-${ARCH}" -type f | head -1)
JAILER_BIN=$(find . -name "jailer-*-${ARCH}" -type f | head -1)

if [ -z "$FIRECRACKER_BIN" ]; then
    echo "Error: Could not find firecracker binary"
    exit 1
fi

# Install
echo "Installing to $INSTALL_DIR..."
sudo install -m 755 "$FIRECRACKER_BIN" "$INSTALL_DIR/firecracker"

if [ -n "$JAILER_BIN" ]; then
    sudo install -m 755 "$JAILER_BIN" "$INSTALL_DIR/jailer"
fi

# Verify
echo ""
echo "Installation complete!"
firecracker --version

# Check KVM
echo ""
if [ -c /dev/kvm ]; then
    echo "✓ KVM is available"
    if [ -r /dev/kvm ] && [ -w /dev/kvm ]; then
        echo "✓ KVM is accessible by current user"
    else
        echo "⚠ KVM exists but may not be accessible"
        echo "  Run: sudo usermod -aG kvm $USER"
        echo "  Then log out and back in"
    fi
else
    echo "⚠ KVM not available"
    echo "  Enable virtualization in BIOS/UEFI"
    echo "  Or run: sudo modprobe kvm kvm_intel (or kvm_amd)"
fi
