#!/bin/bash
set -e

IMAGE="${IMAGE:-standard}"
OUTPUT_DIR="${OUTPUT_DIR:-/opt/sandbox}"
BUILD_DIR="${BUILD_DIR:-/tmp/rootfs-build}"
ROOTFS_SIZE="${ROOTFS_SIZE:-512M}"

echo "Building $IMAGE rootfs for Firecracker..."

mkdir -p "$BUILD_DIR" "$OUTPUT_DIR"
cd "$BUILD_DIR"

ROOTFS_IMG="$OUTPUT_DIR/rootfs-$IMAGE.ext4"

truncate -s "$ROOTFS_SIZE" "$ROOTFS_IMG"
mkfs.ext4 -F "$ROOTFS_IMG"

MOUNT_DIR="$BUILD_DIR/rootfs"
mkdir -p "$MOUNT_DIR"
mount -o loop "$ROOTFS_IMG" "$MOUNT_DIR"

cleanup() {
    umount "$MOUNT_DIR" 2>/dev/null || true
}
trap cleanup EXIT

echo "Installing Alpine Linux base..."
ALPINE_VERSION="3.19"
ALPINE_ARCH="x86_64"
ALPINE_MIRROR="https://dl-cdn.alpinelinux.org/alpine"

mkdir -p "$MOUNT_DIR/etc/apk"
echo "$ALPINE_MIRROR/v$ALPINE_VERSION/main" > "$MOUNT_DIR/etc/apk/repositories"
echo "$ALPINE_MIRROR/v$ALPINE_VERSION/community" >> "$MOUNT_DIR/etc/apk/repositories"

apk --root "$MOUNT_DIR" --initdb add alpine-base

echo "Installing base packages..."
apk --root "$MOUNT_DIR" add \
    busybox \
    openrc \
    ca-certificates \
    curl \
    wget

case "$IMAGE" in
    minimal)
        echo "Minimal image - no additional packages"
        ;;
    standard)
        echo "Installing standard packages..."
        apk --root "$MOUNT_DIR" add \
            python3 \
            py3-pip \
            nodejs \
            npm \
            git \
            openssh-client \
            jq
        ;;
    full)
        echo "Installing full packages..."
        apk --root "$MOUNT_DIR" add \
            python3 \
            py3-pip \
            nodejs \
            npm \
            go \
            rust \
            cargo \
            git \
            openssh-client \
            jq \
            make \
            gcc \
            musl-dev \
            linux-headers
        ;;
esac

echo "Configuring system..."

mkdir -p "$MOUNT_DIR"/{dev,proc,sys,run,tmp,workspace}
chmod 1777 "$MOUNT_DIR/tmp"

cat > "$MOUNT_DIR/etc/inittab" << 'EOF'
::sysinit:/sbin/openrc sysinit
::sysinit:/sbin/openrc boot
::wait:/sbin/openrc default
ttyS0::respawn:/sbin/getty -L ttyS0 115200 vt100
::ctrlaltdel:/sbin/reboot
::shutdown:/sbin/openrc shutdown
EOF

cat > "$MOUNT_DIR/etc/fstab" << 'EOF'
/dev/vda    /           ext4    defaults,noatime  0 1
devtmpfs    /dev        devtmpfs defaults          0 0
proc        /proc       proc    defaults          0 0
sysfs       /sys        sysfs   defaults          0 0
tmpfs       /tmp        tmpfs   defaults          0 0
tmpfs       /run        tmpfs   defaults          0 0
EOF

cat > "$MOUNT_DIR/etc/network/interfaces" << 'EOF'
auto lo
iface lo inet loopback

auto eth0
iface eth0 inet static
    address 192.168.100.2
    netmask 255.255.255.0
    gateway 192.168.100.1
EOF

echo "sandbox" > "$MOUNT_DIR/etc/hostname"

cat > "$MOUNT_DIR/etc/hosts" << 'EOF'
127.0.0.1   localhost
192.168.100.2   sandbox
EOF

echo "nameserver 8.8.8.8" > "$MOUNT_DIR/etc/resolv.conf"

echo "Installing guest agent..."
if [ -f "/tmp/guest-agent" ]; then
    cp /tmp/guest-agent "$MOUNT_DIR/usr/local/bin/guest-agent"
    chmod +x "$MOUNT_DIR/usr/local/bin/guest-agent"
fi

if [ -f "/tmp/guest-fused" ]; then
    cp /tmp/guest-fused "$MOUNT_DIR/usr/local/bin/guest-fused"
    chmod +x "$MOUNT_DIR/usr/local/bin/guest-fused"
fi

cat > "$MOUNT_DIR/etc/init.d/guest-agent" << 'EOF'
#!/sbin/openrc-run
name="guest-agent"
description="Sandbox guest agent"
command="/usr/local/bin/guest-agent"
command_background="yes"
pidfile="/run/${RC_SVCNAME}.pid"
output_log="/var/log/guest-agent.log"
error_log="/var/log/guest-agent.log"

depend() {
    need net
    after firewall
}
EOF
chmod +x "$MOUNT_DIR/etc/init.d/guest-agent"

cat > "$MOUNT_DIR/etc/init.d/guest-fused" << 'EOF'
#!/sbin/openrc-run
name="guest-fused"
description="Sandbox FUSE daemon"
command="/usr/local/bin/guest-fused"
command_args="/workspace"
command_background="yes"
pidfile="/run/${RC_SVCNAME}.pid"
output_log="/var/log/guest-fused.log"
error_log="/var/log/guest-fused.log"

depend() {
    need guest-agent
}
EOF
chmod +x "$MOUNT_DIR/etc/init.d/guest-fused"

chroot "$MOUNT_DIR" /sbin/rc-update add devfs sysinit
chroot "$MOUNT_DIR" /sbin/rc-update add dmesg sysinit
chroot "$MOUNT_DIR" /sbin/rc-update add mdev sysinit
chroot "$MOUNT_DIR" /sbin/rc-update add hwclock boot
chroot "$MOUNT_DIR" /sbin/rc-update add modules boot
chroot "$MOUNT_DIR" /sbin/rc-update add sysctl boot
chroot "$MOUNT_DIR" /sbin/rc-update add hostname boot
chroot "$MOUNT_DIR" /sbin/rc-update add bootmisc boot
chroot "$MOUNT_DIR" /sbin/rc-update add networking default
chroot "$MOUNT_DIR" /sbin/rc-update add guest-agent default
chroot "$MOUNT_DIR" /sbin/rc-update add guest-fused default
chroot "$MOUNT_DIR" /sbin/rc-update add mount-ro shutdown
chroot "$MOUNT_DIR" /sbin/rc-update add killprocs shutdown
chroot "$MOUNT_DIR" /sbin/rc-update add savecache shutdown

echo "root:sandbox" | chroot "$MOUNT_DIR" chpasswd

sync
umount "$MOUNT_DIR"
trap - EXIT

echo "Rootfs built successfully: $ROOTFS_IMG"
echo "Size: $(du -h $ROOTFS_IMG | cut -f1)"
