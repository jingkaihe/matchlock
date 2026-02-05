#!/bin/sh
set -e

IMG=/tmp/rootfs.ext4
MNTDIR=/tmp/rootfs-build
CACHE_DIR=/Users/jingkaihe/.cache/matchlock

# Remove existing image
rm -f $IMG

# Create 256MB ext4 image
dd if=/dev/zero of=$IMG bs=1M count=256
mkfs.ext4 -L rootfs $IMG

# Mount and populate
mkdir -p $MNTDIR
mount -o loop $IMG $MNTDIR

# Install Alpine base system using apk
apk add --root $MNTDIR --initdb -X https://dl-cdn.alpinelinux.org/alpine/v3.23/main --allow-untrusted alpine-base busybox-openrc openrc

# Create essential directories
mkdir -p $MNTDIR/dev $MNTDIR/proc $MNTDIR/sys $MNTDIR/tmp $MNTDIR/run
mkdir -p $MNTDIR/workspace $MNTDIR/etc/ssl/certs
mkdir -p $MNTDIR/lib/modules

# Copy kernel modules for vsock and fuse from host
KVER=$(uname -r)
mkdir -p $MNTDIR/lib/modules/$KVER
cp -a /lib/modules/$KVER/kernel/net/vmw_vsock $MNTDIR/lib/modules/$KVER/ 2>/dev/null || true
cp -a /lib/modules/$KVER/kernel/fs/fuse $MNTDIR/lib/modules/$KVER/ 2>/dev/null || true
cp -a /lib/modules/$KVER/kernel/drivers/virtio $MNTDIR/lib/modules/$KVER/ 2>/dev/null || true

# Copy modules.dep and related files
cp /lib/modules/$KVER/modules.* $MNTDIR/lib/modules/$KVER/ 2>/dev/null || true

# Copy guest binaries
cp $CACHE_DIR/guest-agent $MNTDIR/usr/bin/guest-agent
cp $CACHE_DIR/guest-fused $MNTDIR/usr/bin/guest-fused
chmod +x $MNTDIR/usr/bin/guest-agent $MNTDIR/usr/bin/guest-fused

# Create init script
cat > $MNTDIR/init << 'INITEOF'
#!/bin/sh

# Mount essential filesystems
mount -t proc none /proc
mount -t sysfs none /sys
mount -t devtmpfs devtmpfs /dev
mkdir -p /dev/pts /dev/shm
mount -t devpts devpts /dev/pts
mount -t tmpfs tmpfs /dev/shm
mount -t tmpfs tmpfs /run
mount -t tmpfs tmpfs /tmp

# Set hostname
hostname matchlock

# Load required modules
modprobe virtio_vsock 2>/dev/null || true
modprobe vsock 2>/dev/null || true
modprobe fuse 2>/dev/null || true
modprobe virtio_net 2>/dev/null || true

# Configure networking
ip link set lo up
ip link set eth0 up 2>/dev/null || true
udhcpc -i eth0 -q 2>/dev/null || true

# Parse kernel cmdline for workspace path
WORKSPACE=/workspace
for param in $(cat /proc/cmdline); do
    case "$param" in
        matchlock.workspace=*)
            WORKSPACE="${param#matchlock.workspace=}"
            ;;
    esac
done

# Create workspace directory
mkdir -p "$WORKSPACE"

# Start guest-fused (VFS mount)
echo "Starting guest-fused..."
/usr/bin/guest-fused &

# Wait a moment for FUSE to initialize
sleep 1

# Start guest-agent
echo "Starting guest-agent..."
exec /usr/bin/guest-agent
INITEOF
chmod +x $MNTDIR/init

# Create /etc/passwd and /etc/group
cat > $MNTDIR/etc/passwd << 'EOF'
root:x:0:0:root:/root:/bin/sh
nobody:x:65534:65534:nobody:/:/sbin/nologin
EOF

cat > $MNTDIR/etc/group << 'EOF'
root:x:0:
nobody:x:65534:
EOF

# Create /etc/resolv.conf
cat > $MNTDIR/etc/resolv.conf << 'EOF'
nameserver 8.8.8.8
nameserver 8.8.4.4
EOF

# Create /etc/hosts
cat > $MNTDIR/etc/hosts << 'EOF'
127.0.0.1   localhost matchlock
::1         localhost
EOF

# Create /etc/fstab
touch $MNTDIR/etc/fstab

# Setup CA certificates directory
mkdir -p $MNTDIR/etc/ssl/certs

echo "Rootfs contents:"
ls -la $MNTDIR/
ls -la $MNTDIR/usr/bin/

# Unmount
sync
umount $MNTDIR
rmdir $MNTDIR

echo "Rootfs created at $IMG"
ls -lh $IMG
