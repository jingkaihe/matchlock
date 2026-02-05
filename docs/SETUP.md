# Matchlock Setup Guide

This guide covers the complete setup for running Matchlock sandboxes, including Firecracker installation, kernel building, and rootfs creation.

## Prerequisites

### System Requirements

- **OS**: Linux (x86_64)
- **Kernel**: 4.14+ with KVM support
- **RAM**: 4GB minimum (8GB recommended for building)
- **Disk**: 10GB free space

### Required Packages

**Debian/Ubuntu:**
```bash
sudo apt-get update
sudo apt-get install -y \
    build-essential \
    gcc \
    make \
    flex \
    bison \
    libelf-dev \
    libssl-dev \
    bc \
    wget \
    curl \
    git \
    qemu-utils \
    e2fsprogs
```

**Fedora/RHEL:**
```bash
sudo dnf install -y \
    @development-tools \
    gcc \
    make \
    flex \
    bison \
    elfutils-libelf-devel \
    openssl-devel \
    bc \
    wget \
    curl \
    git \
    qemu-img \
    e2fsprogs
```

**Alpine (for rootfs building):**
```bash
# Install Alpine's apk-tools on the host for rootfs creation
# This is optional - the script can also use Docker
sudo apt-get install -y alpine-base  # if available
# Or use Docker method (see below)
```

### KVM Access

Ensure your user has KVM access:

```bash
# Check KVM is available
ls -la /dev/kvm

# Add user to kvm group
sudo usermod -aG kvm $USER

# Re-login or run
newgrp kvm
```

## 1. Install Firecracker

### Option A: Download Pre-built Binary (Recommended)

```bash
# Get latest release
FIRECRACKER_VERSION=$(curl -s https://api.github.com/repos/firecracker-microvm/firecracker/releases/latest | grep tag_name | cut -d '"' -f 4)

# Download
curl -L -o firecracker.tgz \
    "https://github.com/firecracker-microvm/firecracker/releases/download/${FIRECRACKER_VERSION}/firecracker-${FIRECRACKER_VERSION}-x86_64.tgz"

# Extract
tar xzf firecracker.tgz

# Install
sudo mv release-${FIRECRACKER_VERSION}-x86_64/firecracker-${FIRECRACKER_VERSION}-x86_64 /usr/local/bin/firecracker
sudo chmod +x /usr/local/bin/firecracker

# Verify
firecracker --version
```

### Option B: Build from Source

```bash
git clone https://github.com/firecracker-microvm/firecracker.git
cd firecracker
tools/devtool build
sudo cp build/cargo_target/x86_64-unknown-linux-musl/release/firecracker /usr/local/bin/
```

## 2. Build the Kernel

The kernel must be built with specific options for Firecracker compatibility.

### Quick Build

```bash
cd /path/to/matchlock

# Build with defaults (kernel 6.1.94, output to /opt/sandbox)
sudo mkdir -p /opt/sandbox
sudo chown $USER:$USER /opt/sandbox
./scripts/build-kernel.sh
```

### Custom Build

```bash
# Specify version and output directory
KERNEL_VERSION=6.6.30 OUTPUT_DIR=./images ./scripts/build-kernel.sh
```

### Build Options

| Variable | Default | Description |
|----------|---------|-------------|
| `KERNEL_VERSION` | 6.1.94 | Linux kernel version |
| `OUTPUT_DIR` | /opt/sandbox | Output directory for kernel |
| `BUILD_DIR` | /tmp/kernel-build | Temporary build directory |

### Kernel Configuration

The build script creates a minimal kernel with:

- **Virtualization**: KVM guest, paravirt, virtio
- **Network**: virtio-net, TUN/TAP
- **Storage**: virtio-blk, ext4
- **Vsock**: virtio-vsockets (for host-guest communication)
- **FUSE**: For VFS mounting
- **Console**: Serial 8250

Unnecessary features are disabled (modules, USB, sound, wireless, etc.) to minimize size.

### Expected Output

```
Building Linux kernel 6.1.94 for Firecracker...
Downloading kernel source...
Extracting kernel source...
Configuring kernel for Firecracker...
[build output...]
Kernel built successfully: /opt/sandbox/kernel
Size: 12M
```

## 3. Build the Rootfs

The rootfs is an Alpine Linux-based ext4 image with the guest agent and FUSE daemon.

### Build Guest Binaries First

```bash
cd /path/to/matchlock

# Build static binaries for the guest
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/guest-agent ./cmd/guest-agent
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/guest-fused ./cmd/guest-fused
```

### Build Rootfs

```bash
# Build standard image (Python, Node.js, Git)
sudo IMAGE=standard OUTPUT_DIR=/opt/sandbox ./scripts/build-rootfs.sh
```

### Image Variants

| Variant | Packages | Size |
|---------|----------|------|
| `minimal` | Base Alpine, curl, wget | ~50MB |
| `standard` | Python 3, Node.js, npm, Git, jq | ~200MB |
| `full` | Go, Rust, Cargo, GCC, dev tools | ~500MB |

### Build Options

| Variable | Default | Description |
|----------|---------|-------------|
| `IMAGE` | standard | Image variant |
| `OUTPUT_DIR` | /opt/sandbox | Output directory |
| `BUILD_DIR` | /tmp/rootfs-build | Temporary build directory |
| `ROOTFS_SIZE` | 512M | Image size |

### What's Included

The rootfs contains:

- **Base System**: Alpine Linux 3.19
- **Init**: OpenRC
- **Network**: Pre-configured eth0 (192.168.100.2/24)
- **Services**:
  - `guest-agent`: Command execution over vsock
  - `guest-fused`: VFS mounting at /workspace
- **Directories**: /workspace, /tmp, /dev, /proc, /sys

### Expected Output

```
Building standard rootfs for Firecracker...
Installing Alpine Linux base...
Installing standard packages...
Configuring system...
Installing guest agent...
Rootfs built successfully: /opt/sandbox/rootfs-standard.ext4
Size: 180M
```

## 4. Using Docker for Rootfs Building

If you don't have Alpine's apk-tools on your host, use Docker:

```bash
# Create a Docker-based build script
cat > build-rootfs-docker.sh << 'EOF'
#!/bin/bash
docker run --rm --privileged \
    -v /tmp:/tmp \
    -v $(pwd)/scripts:/scripts:ro \
    -v /opt/sandbox:/opt/sandbox \
    alpine:3.19 \
    sh -c "apk add --no-cache bash e2fsprogs && /scripts/build-rootfs.sh"
EOF
chmod +x build-rootfs-docker.sh

# Build guest binaries first
CGO_ENABLED=0 go build -o /tmp/guest-agent ./cmd/guest-agent
CGO_ENABLED=0 go build -o /tmp/guest-fused ./cmd/guest-fused

# Run Docker build
sudo ./build-rootfs-docker.sh
```

## 5. Verify Installation

### Check Files

```bash
ls -la /opt/sandbox/
# Should show:
# - kernel (vmlinux, ~12MB)
# - rootfs-standard.ext4 (~180MB)
```

### Test Firecracker Manually

```bash
# Create a test config
cat > /tmp/fc-config.json << EOF
{
  "boot-source": {
    "kernel_image_path": "/opt/sandbox/kernel",
    "boot_args": "console=ttyS0 reboot=k panic=1 pci=off"
  },
  "drives": [{
    "drive_id": "rootfs",
    "path_on_host": "/opt/sandbox/rootfs-standard.ext4",
    "is_root_device": true,
    "is_read_only": false
  }],
  "machine-config": {
    "vcpu_count": 1,
    "mem_size_mib": 256
  }
}
EOF

# Run Firecracker (requires KVM)
rm -f /tmp/fc.sock
firecracker --api-sock /tmp/fc.sock --config-file /tmp/fc-config.json

# In another terminal, you should see boot messages
# Login with root:sandbox
```

### Test with Matchlock

```bash
# Build the sandbox CLI
go build -o bin/sandbox ./cmd/sandbox

# Set environment variables
export SANDBOX_KERNEL=/opt/sandbox/kernel
export SANDBOX_ROOTFS=/opt/sandbox/rootfs-standard.ext4

# Run a command (requires root for TAP devices)
sudo -E ./bin/sandbox run echo "Hello from sandbox"
```

## 6. Environment Configuration

### System-wide Setup

```bash
# Add to /etc/environment or ~/.bashrc
export SANDBOX_KERNEL=/opt/sandbox/kernel
export SANDBOX_ROOTFS=/opt/sandbox/rootfs-standard.ext4
```

### Capabilities (Alternative to Root)

Instead of running as root, grant capabilities:

```bash
# Allow TAP device creation
sudo setcap cap_net_admin+ep ./bin/sandbox

# Note: Firecracker itself may still need /dev/kvm access
```

## Troubleshooting

### KVM Permission Denied

```bash
# Check KVM device permissions
ls -la /dev/kvm
# crw-rw---- 1 root kvm ...

# Add user to kvm group
sudo usermod -aG kvm $USER
newgrp kvm
```

### Kernel Build Fails

```bash
# Missing dependencies - install build tools
sudo apt-get install -y build-essential flex bison libelf-dev libssl-dev bc

# Out of memory - increase swap or use a larger instance
sudo fallocate -l 4G /swapfile
sudo mkswap /swapfile
sudo swapon /swapfile
```

### Rootfs Mount Fails

```bash
# Check loop device availability
ls /dev/loop*

# Load loop module
sudo modprobe loop

# Increase loop devices if needed
sudo mknod -m 0660 /dev/loop8 b 7 8
```

### Guest Agent Not Starting

```bash
# Check guest-agent binary is in rootfs
sudo mount -o loop /opt/sandbox/rootfs-standard.ext4 /mnt
ls -la /mnt/usr/local/bin/
# Should show guest-agent and guest-fused
sudo umount /mnt

# Rebuild rootfs with guest binaries
CGO_ENABLED=0 go build -o /tmp/guest-agent ./cmd/guest-agent
CGO_ENABLED=0 go build -o /tmp/guest-fused ./cmd/guest-fused
sudo IMAGE=standard ./scripts/build-rootfs.sh
```

### Firecracker Crashes

```bash
# Check kernel log
dmesg | tail -50

# Run with logging
firecracker --api-sock /tmp/fc.sock --config-file config.json 2>&1 | tee fc.log
```

## Directory Structure

After setup, your installation should look like:

```
/opt/sandbox/
├── kernel                    # Linux kernel (vmlinux)
├── rootfs-minimal.ext4       # Minimal rootfs (optional)
├── rootfs-standard.ext4      # Standard rootfs
└── rootfs-full.ext4          # Full rootfs (optional)

/usr/local/bin/
└── firecracker               # Firecracker binary

~/.sandbox/
└── mitm/
    ├── ca.crt                # MITM CA certificate
    └── ca.key                # MITM CA private key
```

## Next Steps

1. **Run Tests**: `go test ./...`
2. **Build CLI**: `go build -o bin/sandbox ./cmd/sandbox`
3. **Try Examples**: See [EXAMPLES.md](EXAMPLES.md)
4. **Read Architecture**: See [ARCHITECTURE.md](ARCHITECTURE.md)
