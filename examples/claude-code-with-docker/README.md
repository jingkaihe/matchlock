# Claude Code + Docker (Python SDK)

This example installs both Docker and Claude Code in one image, launches the sandbox via the Python SDK, then attaches an interactive terminal using `exec_interactive`.

It is similar to `examples/docker-in-sandbox/` + `examples/claude-code/`, but instead of `matchlock run -it` we use SDK create/launch and then `exec` into the running sandbox.

The shell is opened as a non-root `agent` user that is part of the `docker` group, so Claude Code can run without root while still being able to use Docker.

The image pins Docker to the `vfs` storage driver and disables the containerd snapshotter so `docker run` works on Matchlock's overlay-root guest filesystem.

Note: SDK `launch` starts image `ENTRYPOINT`/`CMD` in detached mode. This script attaches immediately; verify Docker readiness manually inside the shell (`docker info`, then `docker run --rm hello-world`).

## Build the Image

### Using Docker

```bash
docker build -t claude-code-with-docker:latest examples/claude-code-with-docker
docker save claude-code-with-docker:latest | matchlock image import claude-code-with-docker:latest
```

### Or Using Matchlock

```bash
matchlock build -t claude-code-with-docker:latest --build-cache-size 30000 examples/claude-code-with-docker
```

## Run

Prerequisite: export `ANTHROPIC_API_KEY` in your shell before running this example.

From repo root:

```bash
uv run ./sdk/python examples/claude-code-with-docker/main.py
```

Optional flags:

```bash
uv run --with-editable ./sdk/python examples/claude-code-with-docker/main.py \
  --cpus 4 --memory 8192 --image claude-code-with-docker:latest # for using local sdk
```

By default the script launches the sandbox in privileged mode so Docker can run inside the VM. You can disable that with `--no-privileged`.

## What Gets Whitelisted

The SDK sandbox config allows:

- Docker Hub traffic: `docker.io`, `*.docker.io`, `*.docker.com`
- Docker layer storage CDN: `*.r2.cloudflarestorage.com`
- Anthropic API traffic: `api.anthropic.com`, `*.anthropic.com`

It injects `ANTHROPIC_API_KEY` as a Matchlock secret scoped to `api.anthropic.com`.

## After You Attach

Inside the sandbox shell, try:

```bash
docker info
docker run --rm hello-world
claude --dangerously-skip-permissions
```
