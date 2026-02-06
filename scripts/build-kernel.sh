#!/bin/bash
set -e

# Build Linux kernels for Firecracker/Virtualization.framework
# Uses a multi-stage Dockerfile for idempotent, layer-cached builds.
# Requires Docker with BuildKit.

KERNEL_VERSION="${KERNEL_VERSION:-6.1.137}"
OUTPUT_DIR="${OUTPUT_DIR:-$HOME/.cache/matchlock/kernels/$KERNEL_VERSION}"
OUTPUT_DIR=$(eval echo "$OUTPUT_DIR")
ARCH="${ARCH:-all}"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DOCKERFILE_DIR="$SCRIPT_DIR/../guest/kernel"

print_usage() {
    echo "Usage: $0 [OPTIONS]"
    echo ""
    echo "Options:"
    echo "  KERNEL_VERSION=6.1.137   Linux kernel version (default: 6.1.137)"
    echo "  OUTPUT_DIR=path          Output directory (default: ~/.cache/matchlock/kernels/\$VERSION)"
    echo "  ARCH=x86_64|arm64|all    Architecture to build (default: all)"
    echo ""
    echo "Examples:"
    echo "  $0                       # Build both architectures"
    echo "  ARCH=arm64 $0            # Build only arm64"
    echo "  KERNEL_VERSION=6.1.140 $0"
}

build_x86_64() {
    echo "Building Linux kernel $KERNEL_VERSION for x86_64..."
    DOCKER_BUILDKIT=1 docker build \
        --build-arg KERNEL_VERSION="$KERNEL_VERSION" \
        --target output-x86_64 \
        --output "type=local,dest=$OUTPUT_DIR" \
        -f "$DOCKERFILE_DIR/Dockerfile" \
        "$DOCKERFILE_DIR"
    echo "Built: $OUTPUT_DIR/kernel ($(du -h "$OUTPUT_DIR/kernel" | cut -f1))"
}

build_arm64() {
    echo "Building Linux kernel $KERNEL_VERSION for arm64..."
    DOCKER_BUILDKIT=1 docker build \
        --build-arg KERNEL_VERSION="$KERNEL_VERSION" \
        --target output-arm64 \
        --output "type=local,dest=$OUTPUT_DIR" \
        -f "$DOCKERFILE_DIR/Dockerfile" \
        "$DOCKERFILE_DIR"
    echo "Built: $OUTPUT_DIR/kernel-arm64 ($(du -h "$OUTPUT_DIR/kernel-arm64" | cut -f1))"
}

if [ "$1" = "-h" ] || [ "$1" = "--help" ]; then
    print_usage
    exit 0
fi

mkdir -p "$OUTPUT_DIR"

echo "============================================"
echo "Matchlock Kernel Builder"
echo "============================================"
echo "Version:    $KERNEL_VERSION"
echo "Output:     $OUTPUT_DIR"
echo "Arch:       $ARCH"
echo "Dockerfile: $DOCKERFILE_DIR/Dockerfile"
echo "============================================"

case "$ARCH" in
    x86_64|amd64)
        build_x86_64
        ;;
    arm64|aarch64)
        build_arm64
        ;;
    all)
        build_x86_64
        build_arm64
        ;;
    *)
        echo "ERROR: Unsupported architecture: $ARCH"
        echo "Supported: x86_64, arm64, all"
        exit 1
        ;;
esac

echo ""
echo "Build complete!"
echo "Kernels are in: $OUTPUT_DIR"
ls -lh "$OUTPUT_DIR"
