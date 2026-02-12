# Browser Use Example (Kodelet + Playwright MCP)

Run Kodelet with full browser automation inside a matchlock sandbox. Kodelet uses the [Playwright MCP server](https://github.com/microsoft/playwright-mcp) to navigate pages, click elements, fill forms, and extract content — all in a headless Chromium instance running inside the microVM.

## What's Inside

- **Ubuntu 24.04** base with Chromium dependencies
- **Kodelet** CLI with the Playwright MCP server pre-configured
- **Toad** TUI for interactive chat mode
- **Headless Chromium** via `@playwright/mcp`

## Build the Image

### Using Docker

Currently this is the fastest way of building the image:

```bash
docker build -t browser-use:latest examples/playwright
docker save browser-use:latest | matchlock image import browser-use:latest
```

### Using Matchlock

You can use `matchlock` if you don't have Docker installed

```bash
# `--build-cache-size 30000` so that you can repeatly build reliably without running out of device space
# Otherwise you can omit it, and later on introduce it as matchlock automatically grows the disk
matchlock build -t browser-use:latest --build-cache-size 30000 examples/playwright
```

## Run

> **Important:** This image requires more resources than the defaults (1 CPU / 512MB). Use `--cpus 2 --memory 4096` (or higher) to avoid boot timeouts and OOM kills.

### Interactive Mode

Drop into an interactive Kodelet session (via Toad TUI) with the Playwright MCP tools available:

```bash
matchlock run --image browser-use:latest \
  --cpus 2 --memory 4096 \
  --secret ANTHROPIC_API_KEY@api.anthropic.com \
  --allow-host "*" \
  -it
```

> **Note:** `--allow-host "*"` permits all outbound traffic so the browser can reach any website. Narrow this down to specific domains if you want tighter control.

Once inside, Kodelet has access to Playwright MCP tools like `browser_navigate`, `browser_click`, `browser_snapshot`, `browser_fill_form`, etc. Just ask it to do things in natural language.

### One-Shot Mode

Give Kodelet a task directly:

```bash
matchlock run --image browser-use:latest \
  --cpus 2 --memory 4096 \
  --secret ANTHROPIC_API_KEY@api.anthropic.com \
  --allow-host "*" \
  -it \
  -- "Go to news.ycombinator.com and tell me the top 5 stories, using mcp tool to do it"
```

## How It Works

1. **matchlock build** creates an ext4 rootfs from the Dockerfile with Node.js, Kodelet, Toad, and Playwright pre-installed
2. **matchlock run** boots a Firecracker microVM (or Virtualization.framework on macOS) with that rootfs
3. The `--secret ANTHROPIC_API_KEY@api.anthropic.com` flag means the real API key **never enters the VM** — matchlock's MITM proxy injects it on-the-fly into requests to `api.anthropic.com`
4. Inside the VM, the entrypoint launches Kodelet (interactive via Toad TUI, or one-shot via `kodelet run`)
5. Kodelet uses MCP tools (`browser_navigate`, `browser_click`, `browser_snapshot`, etc.) to control headless Chromium
6. All browser traffic flows through the VM's network, governed by `--allow-host` rules

## Secret Injection

The `ANTHROPIC_API_KEY` is injected via matchlock's transparent MITM proxy. Inside the VM, the environment variable contains a placeholder (e.g., `SANDBOX_SECRET_a1b2c3d4...`). When Kodelet makes API calls to `api.anthropic.com`, the host-side proxy replaces the placeholder with the real key in-flight. This means:

- The real key is **never visible** inside the sandbox
- Even if the agent is compromised, the key cannot be exfiltrated
- The key only works for requests to the designated host (`api.anthropic.com`)

## Available MCP Tools

Kodelet has access to the Playwright MCP toolset (with `--caps=core`).

The MCP is running in [code mode](https://blog.cloudflare.com/code-mode/) for the purpose of token preservation.

See the [Playwright MCP docs](https://github.com/microsoft/playwright-mcp) for the full list.
