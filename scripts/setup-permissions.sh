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
fi
sysctl -w net.ipv4.ip_forward=1 >/dev/null
echo "✓ Enabled IP forwarding"

# 4. Ensure /dev/net/tun is accessible
if [ ! -c /dev/net/tun ]; then
    mkdir -p /dev/net
    mknod /dev/net/tun c 10 200
fi
chmod 0666 /dev/net/tun
echo "✓ /dev/net/tun is accessible"

# 5. Set up persistent iptables rules for matchlock subnet
# This avoids needing iptables at runtime

# Detect default interface
DEFAULT_IFACE=$(ip route show default | awk '/default/ {print $5}' | head -1)
if [ -z "$DEFAULT_IFACE" ]; then
    echo "⚠ Could not detect default interface, using ens2"
    DEFAULT_IFACE="ens2"
fi

SUBNET="192.168.100.0/24"

# Add MASQUERADE rule if not exists
if ! iptables -t nat -C POSTROUTING -s "$SUBNET" -o "$DEFAULT_IFACE" -j MASQUERADE 2>/dev/null; then
    iptables -t nat -A POSTROUTING -s "$SUBNET" -o "$DEFAULT_IFACE" -j MASQUERADE
    echo "✓ Added NAT MASQUERADE rule for $SUBNET via $DEFAULT_IFACE"
else
    echo "✓ NAT MASQUERADE rule already exists"
fi

# Add generic FORWARD rules for the subnet (instead of per-interface)
if ! iptables -C FORWARD -s "$SUBNET" -j ACCEPT 2>/dev/null; then
    iptables -I FORWARD 1 -s "$SUBNET" -j ACCEPT
    echo "✓ Added FORWARD rule for outgoing traffic from $SUBNET"
else
    echo "✓ FORWARD rule for outgoing traffic already exists"
fi

if ! iptables -C FORWARD -d "$SUBNET" -j ACCEPT 2>/dev/null; then
    iptables -I FORWARD 2 -d "$SUBNET" -j ACCEPT
    echo "✓ Added FORWARD rule for incoming traffic to $SUBNET"
else
    echo "✓ FORWARD rule for incoming traffic already exists"
fi

# 6. Save iptables rules (distro-specific)
if command -v iptables-save &>/dev/null; then
    if [ -d /etc/iptables ]; then
        iptables-save > /etc/iptables/rules.v4 2>/dev/null || true
    elif [ -f /etc/sysconfig/iptables ]; then
        iptables-save > /etc/sysconfig/iptables 2>/dev/null || true
    fi
    echo "✓ Saved iptables rules"
fi

echo ""
echo "Setup complete! Please log out and back in for group changes to take effect."
echo "Then you can run: matchlock run echo 'Hello'"
