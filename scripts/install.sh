#!/usr/bin/env bash
set -euo pipefail

REPO="jingkaihe/matchlock"
API_BASE_URL="https://api.github.com/repos/${REPO}/releases"

MATCHLOCK_VERSION="${MATCHLOCK_VERSION:-}"

usage() {
  cat <<'EOF'
Usage: ./scripts/install.sh [--version <version>]

Examples:
  ./scripts/install.sh
  ./scripts/install.sh --version 0.1.29
  MATCHLOCK_VERSION=0.1.29 ./scripts/install.sh
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version)
      if [ "$#" -lt 2 ]; then
        echo "Error: --version requires a value" >&2
        usage
        exit 1
      fi
      MATCHLOCK_VERSION="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Error: unknown argument: $1" >&2
      usage
      exit 1
      ;;
  esac
done

release_tag() {
  if [ -z "$MATCHLOCK_VERSION" ]; then
    return 0
  fi

  case "$MATCHLOCK_VERSION" in
    v*) printf '%s\n' "$MATCHLOCK_VERSION" ;;
    *) printf 'v%s\n' "$MATCHLOCK_VERSION" ;;
  esac
}

release_label() {
  if [ -z "$MATCHLOCK_VERSION" ]; then
    echo "latest"
  else
    release_tag
  fi
}

release_download_base() {
  if [ -z "$MATCHLOCK_VERSION" ]; then
    printf 'https://github.com/%s/releases/latest/download\n' "$REPO"
  else
    printf 'https://github.com/%s/releases/download/%s\n' "$REPO" "$(release_tag)"
  fi
}

release_api_url() {
  if [ -z "$MATCHLOCK_VERSION" ]; then
    printf '%s/latest\n' "$API_BASE_URL"
  else
    printf '%s/tags/%s\n' "$API_BASE_URL" "$(release_tag)"
  fi
}

need_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "Error: required command not found: $1" >&2
    exit 1
  fi
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo "amd64" ;;
    arm64|aarch64) echo "arm64" ;;
    *)
      echo "Error: unsupported architecture: $(uname -m)" >&2
      exit 1
      ;;
  esac
}

install_macos() {
  need_cmd brew
  echo "Detected macOS; installing Matchlock via Homebrew..."
  brew tap jingkaihe/essentials
  brew install matchlock
}

download_asset() {
  local asset="$1"
  local out="$2"
  need_cmd curl
  curl -fL "$(release_download_base)/${asset}" -o "$out"
}

release_json() {
  need_cmd curl
  curl -fsSL "$(release_api_url)"
}

find_release_asset() {
  local pattern="$1"
  local json
  need_cmd python3
  json="$(release_json)"
  python3 -c '
import json
import re
import sys

pattern = re.compile(sys.argv[1])
data = json.load(sys.stdin)
for asset in data.get("assets", []):
    name = asset.get("name", "")
    if pattern.search(name):
        print(name)
        sys.exit(0)
sys.exit(1)
' "$pattern" <<<"$json"
}

require_release_asset() {
  local pattern="$1"
  local kind="$2"
  local asset

  if ! asset="$(find_release_asset "$pattern")"; then
    echo "Error: could not find a matching ${kind} package in release '$(release_label)' for ${REPO}." >&2
    echo "Expected a release asset matching: ${pattern}" >&2
    if [ -z "$MATCHLOCK_VERSION" ]; then
      echo "See: https://github.com/${REPO}/releases/latest" >&2
    else
      echo "See: https://github.com/${REPO}/releases/tag/$(release_tag)" >&2
    fi
    exit 1
  fi

  printf '%s\n' "$asset"
}

install_linux_deb() {
  local arch="$1"
  local tmp
  local asset
  tmp="$(mktemp --suffix=.deb)"

  asset="$(require_release_asset "^matchlock_.*_linux_${arch}\\.deb$" ".deb")"
  echo "Installing Linux .deb package: ${asset}"
  download_asset "$asset" "$tmp"

  sudo dpkg -i "$tmp" || sudo apt-get install -f -y
  rm -f "$tmp"
}

install_linux_rpm() {
  local arch="$1"
  local tmp
  local asset
  tmp="$(mktemp --suffix=.rpm)"

  asset="$(require_release_asset "^matchlock_.*_linux_${arch}\\.rpm$" ".rpm")"
  echo "Installing Linux .rpm package: ${asset}"
  download_asset "$asset" "$tmp"

  if command -v dnf >/dev/null 2>&1; then
    sudo dnf install -y "$tmp"
  else
    sudo rpm -Uvh "$tmp"
  fi
  rm -f "$tmp"
}

install_linux() {
  local arch
  arch="$(detect_arch)"
  echo "Detected Linux (${arch})."

  if command -v dpkg >/dev/null 2>&1; then
    echo "Using Debian/Ubuntu package flow."
    install_linux_deb "$arch"
    return
  fi

  if command -v rpm >/dev/null 2>&1; then
    echo "Using RPM package flow."
    install_linux_rpm "$arch"
    return
  fi

  echo "Error: unsupported Linux packaging environment; expected dpkg or rpm" >&2
  exit 1
}

main() {
  echo "Starting Matchlock installation..."
  echo "Requested release: $(release_label)"
  case "$(uname -s)" in
    Darwin)
      install_macos
      ;;
    Linux)
      install_linux
      ;;
    *)
      echo "Error: unsupported operating system: $(uname -s)" >&2
      exit 1
      ;;
  esac

  cat <<'EOF'

Install complete.

Next steps:
  matchlock diagnose

If diagnose reports missing setup, run:
  sudo matchlock setup linux

To enroll a specific user explicitly, run:
  sudo matchlock setup user <name>
EOF
}

main "$@"
