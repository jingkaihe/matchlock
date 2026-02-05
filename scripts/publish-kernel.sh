#!/bin/bash
set -e

# Publish kernels to GHCR as OCI artifacts
# Creates a multi-platform manifest with kernel binaries as layers

KERNEL_VERSION="${KERNEL_VERSION:-6.1.137}"
REGISTRY="${REGISTRY:-ghcr.io/jingkaihe/matchlock}"
INPUT_DIR="${INPUT_DIR:-$HOME/.cache/matchlock/kernels/$KERNEL_VERSION}"

print_usage() {
    echo "Usage: $0 [OPTIONS]"
    echo ""
    echo "Publishes pre-built kernels to GHCR as OCI artifacts."
    echo "Requires: crane (go-containerregistry CLI)"
    echo ""
    echo "Authentication (one of):"
    echo "  - gh CLI authenticated (auto-detected)"
    echo "  - GITHUB_TOKEN environment variable"
    echo "  - crane auth login ghcr.io"
    echo ""
    echo "Options:"
    echo "  KERNEL_VERSION=6.1.137   Kernel version tag"
    echo "  REGISTRY=ghcr.io/...     Target registry (default: ghcr.io/jingkaihe/matchlock)"
    echo "  INPUT_DIR=path           Directory containing kernel and kernel-arm64"
    echo ""
    echo "Examples:"
    echo "  $0                                  # Publish default version"
    echo "  KERNEL_VERSION=6.1.140 $0           # Publish specific version"
}

check_prereqs() {
    if ! command -v crane &> /dev/null; then
        echo "ERROR: crane not found. Install with:"
        echo "  go install github.com/google/go-containerregistry/cmd/crane@latest"
        exit 1
    fi

    if [ ! -f "$INPUT_DIR/kernel" ]; then
        echo "ERROR: x86_64 kernel not found at $INPUT_DIR/kernel"
        echo "Build with: mise run kernel:build"
        exit 1
    fi

    if [ ! -f "$INPUT_DIR/kernel-arm64" ]; then
        echo "ERROR: arm64 kernel not found at $INPUT_DIR/kernel-arm64"
        echo "Build with: mise run kernel:build"
        exit 1
    fi
}

setup_auth() {
    # Check if already authenticated
    if crane auth get ghcr.io &>/dev/null; then
        echo "Using existing crane credentials for ghcr.io"
        return 0
    fi

    # Try gh CLI
    if command -v gh &> /dev/null && gh auth status &>/dev/null; then
        echo "Authenticating crane via gh CLI..."
        gh auth token | crane auth login ghcr.io -u "$(gh api user -q .login)" --password-stdin
        return 0
    fi

    # Try GITHUB_TOKEN
    if [ -n "$GITHUB_TOKEN" ]; then
        echo "Authenticating crane via GITHUB_TOKEN..."
        echo "$GITHUB_TOKEN" | crane auth login ghcr.io -u token --password-stdin
        return 0
    fi

    echo "ERROR: No authentication found for ghcr.io"
    echo "Options:"
    echo "  1. gh auth login"
    echo "  2. export GITHUB_TOKEN=..."
    echo "  3. crane auth login ghcr.io"
    exit 1
}

publish_platform() {
    local arch="$1"
    local kernel_file="$2"
    local platform="$3"
    local tag="$REGISTRY/kernel:${KERNEL_VERSION}-${arch}"
    local kernel_name=$(basename "$kernel_file")

    echo "Publishing $arch kernel as $tag..."
    
    # Create tarball from kernel file (crane append expects a tarball)
    local tmpdir=$(mktemp -d)
    local tarball="$tmpdir/kernel.tar.gz"
    tar -czf "$tarball" -C "$(dirname "$kernel_file")" "$kernel_name"
    
    # Create OCI image with kernel as single layer (using empty base)
    crane append \
        --oci-empty-base \
        --platform "$platform" \
        -f "$tarball" \
        -t "$tag"
    
    rm -rf "$tmpdir"
    echo "Published: $tag"
}

create_manifest() {
    local manifest_tag="$REGISTRY/kernel:$KERNEL_VERSION"
    local amd64_tag="$REGISTRY/kernel:${KERNEL_VERSION}-amd64"
    local arm64_tag="$REGISTRY/kernel:${KERNEL_VERSION}-arm64"

    echo "Creating multi-platform manifest: $manifest_tag"

    # Create manifest list from platform-specific images
    crane index append \
        -t "$manifest_tag" \
        -m "$amd64_tag" \
        -m "$arm64_tag"

    echo "Published manifest: $manifest_tag"

    # Also tag as latest if requested
    if [ "$TAG_LATEST" = "true" ]; then
        echo "Tagging as latest..."
        crane tag "$manifest_tag" latest
        echo "Published: $REGISTRY/kernel:latest"
    fi
}

if [ "$1" = "-h" ] || [ "$1" = "--help" ]; then
    print_usage
    exit 0
fi

check_prereqs
setup_auth

echo "============================================"
echo "Matchlock Kernel Publisher"
echo "============================================"
echo "Version:    $KERNEL_VERSION"
echo "Registry:   $REGISTRY"
echo "Input:      $INPUT_DIR"
echo "============================================"

# Publish platform-specific images
publish_platform "amd64" "$INPUT_DIR/kernel" "linux/amd64"
publish_platform "arm64" "$INPUT_DIR/kernel-arm64" "linux/arm64"

# Create multi-platform manifest
create_manifest

echo ""
echo "============================================"
echo "Publication complete!"
echo "============================================"
echo ""
echo "Pull with:"
echo "  crane pull $REGISTRY/kernel:$KERNEL_VERSION --platform linux/amd64 kernel.tar"
echo "  crane pull $REGISTRY/kernel:$KERNEL_VERSION --platform linux/arm64 kernel-arm64.tar"
echo ""
echo "Or use matchlock which auto-downloads:"
echo "  matchlock run --image alpine:latest echo hello"
