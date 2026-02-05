#!/bin/bash
# Setup script to allow running matchlock without sudo
# Run this once with: sudo ./scripts/setup-permissions.sh

set -e

USER_NAME="${1:-$SUDO_USER}"
if [ -z "$USER_NAME" ]; then
    echo "Usage: sudo $0 [username]"
    exit 1
fi

MATCHLOCK_BIN="${2:-./bin/matchlock}"

echo "Setting up matchlock for user: $USER_NAME"

# 1. Add user to kvm group for /dev/kvm access
if ! groups "$USER_NAME" | grep -q '\bkvm\b'; then
    usermod -aG kvm "$USER_NAME"
    echo "✓ Added $USER_NAME to kvm group"
else
    echo "✓ User already in kvm group"
fi

# 2. Set capabilities on the binary
if [ -f "$MATCHLOCK_BIN" ]; then
    setcap 'cap_net_admin,cap_net_raw+eip' "$MATCHLOCK_BIN"
    echo "✓ Set capabilities on $MATCHLOCK_BIN"
else
    echo "⚠ Binary not found at $MATCHLOCK_BIN - build it first with: go build -o bin/matchlock ./cmd/matchlock"
fi

# 3. Enable IP forwarding permanently
if ! grep -q "net.ipv4.ip_forward = 1" /etc/sysctl.conf 2>/dev/null; then
    echo "net.ipv4.ip_forward = 1" >> /etc/sysctl.conf
    sysctl -p
    echo "✓ Enabled IP forwarding"
else
    echo "✓ IP forwarding already enabled"
fi

# 4. Ensure /dev/net/tun is accessible
if [ ! -c /dev/net/tun ]; then
    mkdir -p /dev/net
    mknod /dev/net/tun c 10 200
fi
chmod 0666 /dev/net/tun
echo "✓ /dev/net/tun is accessible"

echo ""
echo "Setup complete! Please log out and back in for group changes to take effect."
echo "Then you can run: matchlock run echo 'Hello'"
