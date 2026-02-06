# Multi-stage Dockerfile for building Matchlock guest kernels.
#
# Stages:
#   base            - Toolchain + kernel source (shared, heavily cached)
#   build-x86_64    - x86_64 kernel compilation
#   build-arm64     - arm64 kernel compilation
#   output-x86_64   - Minimal output (scratch) containing only the kernel binary
#   output-arm64    - Minimal output (scratch) containing only the kernel binary
#
# Usage:
#   docker build --build-arg KERNEL_VERSION=6.1.137 \
#     --target output-x86_64 --output type=local,dest=./out .

ARG KERNEL_VERSION=6.1.137

# ---------------------------------------------------------------------------
# Base: toolchain + kernel source
# ---------------------------------------------------------------------------
FROM ubuntu:22.04 AS base

ARG KERNEL_VERSION

RUN apt-get update && apt-get install -y --no-install-recommends \
        build-essential bc flex bison libelf-dev libssl-dev wget ca-certificates \
        gcc-aarch64-linux-gnu \
    && rm -rf /var/lib/apt/lists/*

RUN wget -qO /tmp/linux.tar.xz \
        "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-${KERNEL_VERSION}.tar.xz" \
    && mkdir /tmp/linux-src \
    && tar xf /tmp/linux.tar.xz -C /tmp/linux-src --strip-components=1 \
    && rm /tmp/linux.tar.xz

# ---------------------------------------------------------------------------
# x86_64 kernel (Firecracker)
# ---------------------------------------------------------------------------
FROM base AS build-x86_64

COPY x86_64.config /tmp/linux-src/.config

RUN cd /tmp/linux-src \
    && make olddefconfig \
    && make -j"$(nproc)" vmlinux

# ---------------------------------------------------------------------------
# arm64 kernel (Virtualization.framework)
# ---------------------------------------------------------------------------
FROM base AS build-arm64

COPY arm64.config /tmp/linux-src/.config

RUN cd /tmp/linux-src \
    && make ARCH=arm64 CROSS_COMPILE=aarch64-linux-gnu- olddefconfig \
    && make ARCH=arm64 CROSS_COMPILE=aarch64-linux-gnu- -j"$(nproc)" Image

# ---------------------------------------------------------------------------
# Output stages (scratch = only the kernel binary, nothing else)
# ---------------------------------------------------------------------------
FROM scratch AS output-x86_64
COPY --from=build-x86_64 /tmp/linux-src/vmlinux /kernel

FROM scratch AS output-arm64
COPY --from=build-arm64 /tmp/linux-src/arch/arm64/boot/Image /kernel-arm64
