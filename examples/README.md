# Examples

## CLI Examples

```bash
# Run with any container image (auto-builds on first use)
sudo matchlock run --image alpine:latest cat /etc/alpine-release
sudo matchlock run --image python:3.12-alpine python3 --version
sudo matchlock run --image ubuntu:22.04 uname -a

# Interactive shell
sudo matchlock run --image alpine:latest -it sh

# Pre-build an image for faster subsequent runs
sudo matchlock build python:3.12-alpine

# With network allowlist
sudo matchlock run --image python:3.12-alpine --allow-host "*.openai.com" python script.py

# With volume mount
sudo matchlock run --image python:3.12-alpine -v ./code:code python /workspace/code/script.py
```

## Go SDK Examples

Run from the project root directory:

```bash
cd matchlock

# Build binaries first
make build-all

# Basic example
sudo go run examples/go/main.go

# With secrets (MITM replaces placeholder in HTTP headers)
sudo ANTHROPIC_API_KEY=sk-xxx go run examples/go/main.go

# Dockerfile build example — build from Dockerfile, launch, and verify
sudo go run examples/go/image_build/main.go
```

## Python SDK Examples

```bash
# With secrets
sudo ANTHROPIC_API_KEY=sk-xxx python3 examples/python/main.py

# Dockerfile build example — build from Dockerfile, launch, and verify
sudo uv run examples/python/image_build/main.py
```
