#!/bin/bash
set -e

KERNEL_VERSION="${KERNEL_VERSION:-6.1.94}"
OUTPUT_DIR="${OUTPUT_DIR:-/opt/sandbox}"
BUILD_DIR="${BUILD_DIR:-/tmp/kernel-build}"

echo "Building Linux kernel $KERNEL_VERSION for Firecracker..."

mkdir -p "$BUILD_DIR" "$OUTPUT_DIR"
cd "$BUILD_DIR"

if [ ! -f "linux-$KERNEL_VERSION.tar.xz" ]; then
    echo "Downloading kernel source..."
    wget -q "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-$KERNEL_VERSION.tar.xz"
fi

if [ ! -d "linux-$KERNEL_VERSION" ]; then
    echo "Extracting kernel source..."
    tar xf "linux-$KERNEL_VERSION.tar.xz"
fi

cd "linux-$KERNEL_VERSION"

echo "Configuring kernel for Firecracker..."
cat > .config << 'EOF'
# Minimal Firecracker kernel config
CONFIG_64BIT=y
CONFIG_SMP=y
CONFIG_HYPERVISOR_GUEST=y
CONFIG_PARAVIRT=y
CONFIG_KVM_GUEST=y

# Processor
CONFIG_MCORE2=y
CONFIG_X86_64=y

# Block devices
CONFIG_BLK_DEV=y
CONFIG_VIRTIO_BLK=y
CONFIG_BLK_DEV_LOOP=y

# Network
CONFIG_NET=y
CONFIG_INET=y
CONFIG_VIRTIO_NET=y
CONFIG_TUN=y
CONFIG_VETH=y

# Virtio
CONFIG_VIRTIO=y
CONFIG_VIRTIO_PCI=y
CONFIG_VIRTIO_MMIO=y
CONFIG_VIRTIO_BALLOON=y
CONFIG_VIRTIO_CONSOLE=y

# Vsock
CONFIG_VSOCKETS=y
CONFIG_VIRTIO_VSOCKETS=y

# File systems
CONFIG_EXT4_FS=y
CONFIG_TMPFS=y
CONFIG_DEVTMPFS=y
CONFIG_DEVTMPFS_MOUNT=y
CONFIG_FUSE_FS=y
CONFIG_OVERLAY_FS=y

# Crypto
CONFIG_CRYPTO=y
CONFIG_CRYPTO_SHA256=y

# Console
CONFIG_SERIAL_8250=y
CONFIG_SERIAL_8250_CONSOLE=y
CONFIG_VT=n
CONFIG_PRINTK=y
CONFIG_EARLY_PRINTK=y

# Init
CONFIG_BLK_DEV_INITRD=y
CONFIG_RD_GZIP=y

# Kernel options
CONFIG_NO_HZ_IDLE=y
CONFIG_HIGH_RES_TIMERS=y
CONFIG_PREEMPT_VOLUNTARY=y
CONFIG_SCHED_MC=y

# Memory
CONFIG_MEMORY_HOTPLUG=n
CONFIG_SPARSEMEM_VMEMMAP=y

# Disable unnecessary features
CONFIG_MODULES=n
CONFIG_SOUND=n
CONFIG_USB=n
CONFIG_WIRELESS=n
CONFIG_BLUETOOTH=n
CONFIG_NFS_FS=n
CONFIG_CIFS=n
CONFIG_DEBUG_INFO=n
CONFIG_DEBUG_KERNEL=n
CONFIG_WATCHDOG=n
CONFIG_INPUT=n
CONFIG_SELINUX=n
CONFIG_SECURITY=n
CONFIG_AUDIT=n
EOF

make olddefconfig
make -j$(nproc) vmlinux

cp vmlinux "$OUTPUT_DIR/kernel"

echo "Kernel built successfully: $OUTPUT_DIR/kernel"
echo "Size: $(du -h $OUTPUT_DIR/kernel | cut -f1)"
