#!/bin/sh
set -eu

BIN="/usr/bin/matchlock"

if [ -x "$BIN" ] && command -v setcap >/dev/null 2>&1; then
  setcap cap_net_admin,cap_net_raw+ep "$BIN" || true
fi

if command -v sysctl >/dev/null 2>&1; then
  sysctl -q -w net.ipv4.ip_forward=1 >/dev/null 2>&1 || true
fi

if command -v modprobe >/dev/null 2>&1; then
  modprobe nf_tables >/dev/null 2>&1 || true
fi

cat <<'EOF'
Matchlock package installed.

Next steps:
  1. Enroll intended users with: sudo matchlock setup user <name>
  2. Ensure users log out and back in after group changes.
  3. Run `matchlock diagnose`.

If the host still needs administrative setup, run:
  sudo matchlock setup linux
EOF
