#!/bin/bash
set -e

# Setup script for matchlock on macOS/Apple Silicon
# This script downloads/builds all necessary components

OUTPUT_DIR="${OUTPUT_DIR:-$HOME/.cache/matchlock}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"

echo "=== Matchlock macOS Setup ==="
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

# Use a pre-built kernel from a reliable source
# Option 1: Build from Lima project's kernel (they maintain arm64 kernels for macOS VMs)
KERNEL_URL="https://github.com/lima-vm/alpine-lima/releases/download/v0.2.36/vmlinuz-lts-6.6.32-0-virt"

if [ ! -f "$OUTPUT_DIR/kernel" ]; then
    echo "Downloading kernel from Lima project..."
    curl -L -o "$OUTPUT_DIR/kernel.gz" "$KERNEL_URL"
    
    # Check if it's gzipped and decompress
    if file "$OUTPUT_DIR/kernel.gz" | grep -q "gzip"; then
        gunzip -c "$OUTPUT_DIR/kernel.gz" > "$OUTPUT_DIR/kernel"
        rm "$OUTPUT_DIR/kernel.gz"
    else
        mv "$OUTPUT_DIR/kernel.gz" "$OUTPUT_DIR/kernel"
    fi
    echo "✓ Downloaded kernel"
else
    echo "✓ Kernel already exists"
fi

# Step 3: Build rootfs using Docker
echo ""
echo "=== Building arm64 rootfs ==="

if ! command -v docker &> /dev/null; then
    echo "⚠ Docker not found. Please install Docker Desktop for Mac."
    echo "  You can also manually create a rootfs or download one."
    exit 1
fi

ROOTFS_IMG="$OUTPUT_DIR/rootfs-standard.ext4"

if [ ! -f "$ROOTFS_IMG" ]; then
    echo "Building rootfs with Docker..."
    
    # Create a Dockerfile for building the rootfs
    DOCKER_BUILD_DIR=$(mktemp -d)
    
    cat > "$DOCKER_BUILD_DIR/Dockerfile" << 'DOCKERFILE'
FROM --platform=linux/arm64 alpine:3.19

# Install packages
RUN apk add --no-cache \
    openrc \
    busybox-openrc \
    ca-certificates \
    curl \
    wget \
    python3 \
    py3-pip \
    nodejs \
    npm \
    git \
    openssh-client \
    jq \
    fuse \
    e2fsprogs \
    && rm -rf /var/cache/apk/*

# Create directories
RUN mkdir -p /dev /proc /sys /run /tmp /workspace && chmod 1777 /tmp

# Configure inittab for serial console
RUN echo '::sysinit:/sbin/openrc sysinit' > /etc/inittab && \
    echo '::sysinit:/sbin/openrc boot' >> /etc/inittab && \
    echo '::wait:/sbin/openrc default' >> /etc/inittab && \
    echo 'hvc0::respawn:/sbin/getty -L hvc0 115200 vt100' >> /etc/inittab && \
    echo '::ctrlaltdel:/sbin/reboot' >> /etc/inittab && \
    echo '::shutdown:/sbin/openrc shutdown' >> /etc/inittab

# Configure fstab
RUN echo '/dev/vda    /           ext4    defaults,noatime  0 1' > /etc/fstab && \
    echo 'devtmpfs    /dev        devtmpfs defaults          0 0' >> /etc/fstab && \
    echo 'proc        /proc       proc    defaults          0 0' >> /etc/fstab && \
    echo 'sysfs       /sys        sysfs   defaults          0 0' >> /etc/fstab && \
    echo 'tmpfs       /tmp        tmpfs   defaults          0 0' >> /etc/fstab && \
    echo 'tmpfs       /run        tmpfs   defaults          0 0' >> /etc/fstab

# Network config (kernel ip= handles this)
RUN echo 'auto lo' > /etc/network/interfaces && \
    echo 'iface lo inet loopback' >> /etc/network/interfaces && \
    echo 'auto eth0' >> /etc/network/interfaces && \
    echo 'iface eth0 inet manual' >> /etc/network/interfaces

# Hostname and DNS
RUN echo 'matchlock' > /etc/hostname && \
    echo '127.0.0.1   localhost localhost.localdomain' > /etc/hosts && \
    echo 'nameserver 8.8.8.8' > /etc/resolv.conf

# Root password
RUN echo 'root:matchlock' | chpasswd

# Enable services
RUN rc-update add devfs sysinit 2>/dev/null || true && \
    rc-update add dmesg sysinit 2>/dev/null || true && \
    rc-update add mdev sysinit 2>/dev/null || true && \
    rc-update add hostname boot 2>/dev/null || true && \
    rc-update add bootmisc boot 2>/dev/null || true

DOCKERFILE

    # Copy guest binaries to build context
    cp "$OUTPUT_DIR/guest-agent" "$DOCKER_BUILD_DIR/"
    cp "$OUTPUT_DIR/guest-fused" "$DOCKER_BUILD_DIR/"
    
    # Add guest binaries to Dockerfile
    cat >> "$DOCKER_BUILD_DIR/Dockerfile" << 'DOCKERFILE'

# Copy guest binaries
COPY guest-agent /usr/local/bin/guest-agent
COPY guest-fused /usr/local/bin/guest-fused
RUN chmod +x /usr/local/bin/guest-agent /usr/local/bin/guest-fused

# Create init scripts for guest services
RUN echo '#!/sbin/openrc-run' > /etc/init.d/guest-agent && \
    echo 'name="guest-agent"' >> /etc/init.d/guest-agent && \
    echo 'description="Matchlock guest agent"' >> /etc/init.d/guest-agent && \
    echo 'command="/usr/local/bin/guest-agent"' >> /etc/init.d/guest-agent && \
    echo 'command_background="yes"' >> /etc/init.d/guest-agent && \
    echo 'pidfile="/run/guest-agent.pid"' >> /etc/init.d/guest-agent && \
    echo 'output_log="/var/log/guest-agent.log"' >> /etc/init.d/guest-agent && \
    echo 'error_log="/var/log/guest-agent.log"' >> /etc/init.d/guest-agent && \
    chmod +x /etc/init.d/guest-agent && \
    rc-update add guest-agent default

RUN echo '#!/sbin/openrc-run' > /etc/init.d/guest-fused && \
    echo 'name="guest-fused"' >> /etc/init.d/guest-fused && \
    echo 'description="Matchlock FUSE daemon"' >> /etc/init.d/guest-fused && \
    echo 'command="/usr/local/bin/guest-fused"' >> /etc/init.d/guest-fused && \
    echo 'command_args="/workspace"' >> /etc/init.d/guest-fused && \
    echo 'command_background="yes"' >> /etc/init.d/guest-fused && \
    echo 'pidfile="/run/guest-fused.pid"' >> /etc/init.d/guest-fused && \
    echo 'output_log="/var/log/guest-fused.log"' >> /etc/init.d/guest-fused && \
    echo 'error_log="/var/log/guest-fused.log"' >> /etc/init.d/guest-fused && \
    echo 'depend() { need guest-agent; }' >> /etc/init.d/guest-fused && \
    chmod +x /etc/init.d/guest-fused && \
    rc-update add guest-fused default
DOCKERFILE

    # Build the Docker image
    echo "Building Docker image (arm64)..."
    docker build --platform linux/arm64 -t matchlock-rootfs "$DOCKER_BUILD_DIR"
    
    # Create a container and export the filesystem
    echo "Exporting filesystem..."
    CONTAINER_ID=$(docker create --platform linux/arm64 matchlock-rootfs)
    docker export "$CONTAINER_ID" > "$DOCKER_BUILD_DIR/rootfs.tar"
    docker rm "$CONTAINER_ID"
    
    # Create ext4 image
    echo "Creating ext4 image..."
    truncate -s 512M "$ROOTFS_IMG"
    
    # Use Docker to create the ext4 filesystem (need mkfs.ext4)
    docker run --rm --platform linux/arm64 \
        -v "$DOCKER_BUILD_DIR:/build" \
        -v "$OUTPUT_DIR:/output" \
        alpine:3.19 sh -c "
            apk add --no-cache e2fsprogs tar
            mkfs.ext4 -F /output/rootfs-standard.ext4
            mkdir -p /mnt
            mount -o loop /output/rootfs-standard.ext4 /mnt
            tar xf /build/rootfs.tar -C /mnt
            sync
            umount /mnt
        "
    
    # Cleanup
    rm -rf "$DOCKER_BUILD_DIR"
    docker rmi matchlock-rootfs 2>/dev/null || true
    
    echo "✓ Built rootfs"
else
    echo "✓ Rootfs already exists"
fi

# Step 4: Build and sign the matchlock binary
echo ""
echo "=== Building and signing matchlock binary ==="

cd "$PROJECT_ROOT"
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
codesign -d --entitlements - bin/matchlock 2>&1 | grep -q "virtualization" && echo "✓ Entitlement verified"

echo ""
echo "=== Setup Complete ==="
echo ""
echo "Files created:"
echo "  - $OUTPUT_DIR/kernel (Linux arm64 kernel)"
echo "  - $OUTPUT_DIR/rootfs-standard.ext4 (Alpine rootfs)"
echo "  - $OUTPUT_DIR/guest-agent (Guest agent binary)"
echo "  - $OUTPUT_DIR/guest-fused (Guest FUSE daemon)"
echo "  - $PROJECT_ROOT/bin/matchlock (Signed CLI)"
echo ""
echo "To test, run:"
echo "  ./bin/matchlock run echo 'Hello from macOS VM!'"
echo ""
echo "For interactive shell:"
echo "  ./bin/matchlock run -it sh"
