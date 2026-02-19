# Codex Example

Run OpenAI Codex CLI inside a matchlock micro-VM with GitHub repository bootstrap and secret injection.

## What's Inside

- Ubuntu 24.04 base image
- `gh` CLI + `git`
- `codex` CLI (`@openai/codex`)
- Non-root `agent` user with passwordless `sudo`
- Entrypoint that resolves repo slug, writes `~/.codex/auth.json` from `OPENAI_API_KEY`, clones with `GH_TOKEN`, then launches Codex TUI in `--yolo` mode (with optional initial prompt)
- Helper propagates local git identity/editor config (`user.name`, `user.email`, `core.editor`) into the VM when available

## Build the Image

### Using Docker

```bash
docker build -t codex:latest examples/codex
docker save codex:latest | matchlock image import codex:latest
```

### Using Matchlock

```bash
matchlock build -t codex:latest --build-cache-size 30000 examples/codex
```

## Run

From repo root, use the helper script in the codex example dir:

```bash
./examples/codex/matchlock-codex run
./examples/codex/matchlock-codex run "Fix failing tests in pkg/policy and add coverage"
./examples/codex/matchlock-codex run --cpus 4 --memory 8192 jingkaihe/matchlock
```


Add `--privileged` when you need privileged sandbox mode:

```bash
./examples/codex/matchlock-codex run --privileged
```

You can also pass an explicit GitHub repo slug:

```bash
./examples/codex/matchlock-codex run jingkaihe/matchlock "Implement issue #27 codex example"
```

If you omit the repo slug, the helper resolves it from your local `git remote get-url origin` and passes it into the VM. The clone is performed inside the VM by `git` over HTTPS using `GH_TOKEN`, so your token must be a valid GitHub PAT for the target repo.

## Secrets

The helper passes both values to matchlock secret injection:

- `GH_TOKEN` for `github.com` clone/auth traffic
- `OPENAI_API_KEY` for `api.openai.com`

The VM only sees placeholders; matchlock replaces them in-flight on matching hosts.
