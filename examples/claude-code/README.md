# Claude Code Example

Run Anthropic Claude Code inside a matchlock micro-VM with GitHub repository bootstrap and secret injection.

## What's Inside

- Ubuntu 24.04 base image
- `gh` CLI + `git`
- Claude Code CLI (`@anthropic-ai/claude-code`)
- Non-root `agent` user with passwordless `sudo`
- `claude-settings.json` baked into `~/.claude/settings.json` (with entrypoint fallback) so Claude can read `ANTHROPIC_API_KEY` via helper command
- Entrypoint that resolves repo slug, clones with `GH_TOKEN`, then launches Claude
- Helper propagates local git identity/editor config (`user.name`, `user.email`, `core.editor`) into the VM when available

## Build the Image

### Using Docker

```bash
docker build -t claude-code:latest examples/claude-code
docker save claude-code:latest | matchlock image import claude-code:latest
```

### Using Matchlock

```bash
matchlock build -t claude-code:latest --build-cache-size 30000 examples/claude-code
```

## Run

From repo root, use the helper script in the claude-code example dir:

```bash
./examples/claude-code/matchlock-claude run
./examples/claude-code/matchlock-claude run "Review pkg/policy for error-handling edge cases"
./examples/claude-code/matchlock-claude run --cpus 4 --memory 8192 jingkaihe/matchlock
```

Add `--privileged` when you need privileged sandbox mode:

```bash
./examples/claude-code/matchlock-claude run --privileged
```

You can also pass an explicit GitHub repo slug:

```bash
./examples/claude-code/matchlock-claude run jingkaihe/matchlock "Add tests for JSON-RPC cancel flow"
```

When an instruction is provided, the entrypoint uses `claude -p` for one-shot output. With no instruction, it starts interactive Claude Code.

If you omit the repo slug, the helper resolves it from your local `git remote get-url origin` and passes it into the VM. The clone is performed inside the VM by `git` over HTTPS using `GH_TOKEN`, so your token must be a valid GitHub PAT for the target repo.

## Why `~/.claude/settings.json` Is Required

For Claude Code, `ANTHROPIC_API_KEY` is not picked up automatically in this workflow. This example includes `examples/claude-code/claude-settings.json` and places it at `~/.claude/settings.json` in the VM:

```json
{
  "apiKeyHelper": "echo $ANTHROPIC_API_KEY"
}
```

Claude reads the key via that helper command.

This is required for matchlock secret injection: inside the VM, `ANTHROPIC_API_KEY` is a placeholder value. When Claude sends requests to `api.anthropic.com`, matchlock replaces the placeholder in-flight with the real key.

## Secrets

The helper passes both values to matchlock secret injection:

- `GH_TOKEN` for `github.com` clone/auth traffic
- `ANTHROPIC_API_KEY` for `api.anthropic.com`

The VM only sees placeholders; matchlock replaces them in-flight on matching hosts.
