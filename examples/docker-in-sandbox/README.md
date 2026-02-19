# Docker in Matchlock

This example demonstrates how to run Docker inside a matchlock sandbox.
It boots `/sbin/init` (systemd) in a nested PID namespace so Docker is started
via `docker.service` and pins Docker to `iptables-legacy` for bridge setup.

## Build the Image

### Using Docker

Currently this is the fastest way of building the image:

```bash
docker build -t docker-in-matchlock:latest examples/docker-in-sandbox
docker save docker-in-matchlock:latest | matchlock image import docker-in-matchlock:latest
```

### Using Matchlock

You can use `matchlock` if you don't have Docker installed:

```bash
# `--build-cache-size 30000` so that you can repeatedly build reliably without running out of device space
# Otherwise you can omit it, and later on introduce it as matchlock automatically grows the disk
matchlock build -t docker-in-matchlock:latest --build-cache-size 30000 examples/docker-in-sandbox
```

## Rebuild the Matchlock Kernels

Docker bridge/NAT networking requires netfilter support in the guest kernel.
Rebuild kernels after updating `guest/kernel/*.config`:

```bash
mise run kernel:build
```

## Run the Docker Sandbox

```bash
matchlock run --image docker-in-matchlock:latest \
  --cpus 2 --memory 4096 \
  --rm=false \
  --privileged \
  --
```

Then connect and use Docker:

```bash
matchlock exec vm-XXXX -it bash

# Inside the sandbox:
docker info
docker run hello-world
```
