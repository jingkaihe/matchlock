#!/bin/bash
set -e

# Build a Linux kernel for Firecracker micro-VMs
# Requires Docker to build with GCC 11 for kernel compatibility

KERNEL_VERSION="${KERNEL_VERSION:-6.1.137}"
OUTPUT_DIR="${OUTPUT_DIR:-$HOME/.cache/matchlock}"
USE_DOCKER="${USE_DOCKER:-true}"

echo "Building Linux kernel $KERNEL_VERSION for Firecracker..."

mkdir -p "$OUTPUT_DIR"

if [ "$USE_DOCKER" = "true" ]; then
    echo "Building in Docker container (recommended for GCC compatibility)..."
    
    docker run --rm -v "$OUTPUT_DIR:/output" ubuntu:22.04 bash -c "
        set -e
        apt-get update && apt-get install -y build-essential bc flex bison libelf-dev libssl-dev wget
        
        cd /tmp
        wget -q \"https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-${KERNEL_VERSION}.tar.xz\"
        tar xf \"linux-${KERNEL_VERSION}.tar.xz\"
        cd \"linux-${KERNEL_VERSION}\"
        
        cat > .config << 'KCONFIG'
# Matchlock kernel config for Firecracker v1.8+
# Requires CONFIG_ACPI and CONFIG_PCI for proper virtio support

CONFIG_LOCALVERSION=\"\"
CONFIG_DEFAULT_HOSTNAME=\"matchlock\"
CONFIG_64BIT=y
CONFIG_X86_64=y
CONFIG_X86=y
CONFIG_OUTPUT_FORMAT=\"elf64-x86-64\"
CONFIG_MMU=y
CONFIG_SMP=y
CONFIG_NR_CPUS=64
CONFIG_SCHED_SMT=y

# KVM Guest support
CONFIG_HYPERVISOR_GUEST=y
CONFIG_PARAVIRT=y
CONFIG_PARAVIRT_SPINLOCKS=y
CONFIG_KVM_GUEST=y
CONFIG_ARCH_CPUIDLE_HALTPOLL=y
CONFIG_PARAVIRT_TIME_ACCOUNTING=y
CONFIG_GENERIC_CPU=y

# Block layer
CONFIG_BLOCK=y
CONFIG_BLK_DEV=y
CONFIG_BLK_DEV_LOOP=y
CONFIG_BLK_MQ_VIRTIO=y

# Virtio (required for Firecracker)
CONFIG_VIRTIO_ANCHOR=y
CONFIG_VIRTIO_MENU=y
CONFIG_VIRTIO=y
CONFIG_VIRTIO_MMIO=y
CONFIG_VIRTIO_MMIO_CMDLINE_DEVICES=y
CONFIG_VIRTIO_BLK=y
CONFIG_VIRTIO_NET=y
CONFIG_VIRTIO_CONSOLE=y
CONFIG_VIRTIO_BALLOON=y
CONFIG_HW_RANDOM_VIRTIO=y
CONFIG_VIRTIO_INPUT=n

# Vsock (required for host-guest communication)
CONFIG_VSOCKETS=y
CONFIG_VIRTIO_VSOCKETS=y
CONFIG_VIRTIO_VSOCKETS_COMMON=y

# Network
CONFIG_NET=y
CONFIG_PACKET=y
CONFIG_UNIX=y
CONFIG_INET=y
CONFIG_IP_MULTICAST=y
CONFIG_IP_ADVANCED_ROUTER=y
CONFIG_IP_MULTIPLE_TABLES=y
CONFIG_IP_ROUTE_MULTIPATH=y
CONFIG_TCP_CONG_CUBIC=y
CONFIG_DEFAULT_TCP_CONG=\"cubic\"
CONFIG_IPV6=y
CONFIG_NETDEVICES=y
CONFIG_NET_CORE=y
CONFIG_TUN=y
CONFIG_VETH=y

# File systems
CONFIG_EXT4_FS=y
CONFIG_EXT4_USE_FOR_EXT2=y
CONFIG_TMPFS=y
CONFIG_TMPFS_POSIX_ACL=y
CONFIG_DEVTMPFS=y
CONFIG_DEVTMPFS_MOUNT=y
CONFIG_DEVTMPFS_SAFE=y
CONFIG_PROC_FS=y
CONFIG_PROC_SYSCTL=y
CONFIG_SYSFS=y
CONFIG_FUSE_FS=y
CONFIG_OVERLAY_FS=y

# TTY/Serial
CONFIG_TTY=y
CONFIG_VT=n
CONFIG_SERIAL_8250=y
CONFIG_SERIAL_8250_CONSOLE=y
CONFIG_SERIAL_8250_NR_UARTS=4
CONFIG_SERIAL_8250_RUNTIME_UARTS=4
CONFIG_PRINTK=y
CONFIG_EARLY_PRINTK=y

# Init/boot
CONFIG_BLK_DEV_INITRD=y
CONFIG_RD_GZIP=y
CONFIG_BINFMT_ELF=y
CONFIG_BINFMT_SCRIPT=y

# Kernel options
CONFIG_PREEMPT_NONE=y
CONFIG_NO_HZ_IDLE=y
CONFIG_HIGH_RES_TIMERS=y
CONFIG_POSIX_TIMERS=y
CONFIG_FUTEX=y
CONFIG_EPOLL=y
CONFIG_SIGNALFD=y
CONFIG_TIMERFD=y
CONFIG_EVENTFD=y
CONFIG_AIO=y
CONFIG_IO_URING=y
CONFIG_ADVISE_SYSCALLS=y
CONFIG_MEMBARRIER=y
CONFIG_KALLSYMS=y

# Memory
CONFIG_SPARSEMEM=y
CONFIG_SPARSEMEM_VMEMMAP=y

# ACPI and PCI (required for Firecracker v1.8+)
# Note: ACPI conflicts with virtio-mmio, so we only enable PCI
CONFIG_ACPI=n
CONFIG_PCI=y

# Disable unnecessary features
CONFIG_MODULES=n
CONFIG_SOUND=n
CONFIG_USB_SUPPORT=n
CONFIG_WIRELESS=n
CONFIG_WLAN=n
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
CONFIG_PROFILING=n
CONFIG_WEXT_CORE=n
CONFIG_WEXT_PROC=n
CONFIG_CFG80211=n
KCONFIG

        make olddefconfig
        make -j\$(nproc) vmlinux
        cp vmlinux /output/kernel
        echo \"Kernel ${KERNEL_VERSION} built successfully\"
    "
else
    echo "ERROR: Native build requires GCC 11 for kernel compatibility."
    echo "Please use Docker build (default) or install GCC 11."
    exit 1
fi

echo "Kernel built: $OUTPUT_DIR/kernel"
echo "Size: $(du -h $OUTPUT_DIR/kernel | cut -f1)"
