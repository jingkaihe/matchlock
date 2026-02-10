# Browser Use Example (Claude Code + Playwright MCP)

Run Claude Code with full browser automation inside a matchlock sandbox. Claude uses the [Playwright MCP server](https://github.com/microsoft/playwright-mcp) to navigate pages, click elements, fill forms, and extract content — all in a headless Chromium instance running inside the microVM.

## What's Inside

- **Ubuntu 24.04** base with Chromium dependencies
- **Claude Code** CLI with the Playwright MCP server pre-configured
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

> **Important:** This image requires more resources than the defaults (1 CPU / 512MB). Use `--cpus 4 --memory 8192` (or higher) to avoid boot timeouts and OOM kills.

### Interactive Mode

Drop into Claude Code with the Playwright MCP tools available:

```bash
matchlock run --image browser-use:latest \
  --cpus 4 --memory 8192 \
  --secret ANTHROPIC_API_KEY@api.anthropic.com \
  --allow-host api.anthropic.com \
  --allow-host "*.anthropic.com" \
  --allow-host "*" \
  -it \
  -- /usr/local/bin/entrypoint.sh
```

> **Note:** `--allow-host "*"` permits all outbound traffic so the browser can reach any website. Narrow this down to specific domains if you want tighter control.

Once inside, Claude has access to Playwright MCP tools like `browser_navigate`, `browser_click`, `browser_snapshot`, `browser_fill_form`, etc. Just ask it to do things in natural language.

### One-Shot Mode

Give Claude a task directly:

```bash
matchlock run --image browser-use:latest \
  --cpus 4 --memory 8192 \
  --secret ANTHROPIC_API_KEY@api.anthropic.com \
  --allow-host api.anthropic.com \
  --allow-host "*.anthropic.com" \
  --allow-host "*" \
  -it \
  -- /usr/local/bin/entrypoint.sh "Go to news.ycombinator.com and tell me the top 5 stories"
```

### Restrict Network Access

If the browser only needs to reach specific sites:

```bash
matchlock run --image browser-use:latest \
  --cpus 4 --memory 8192 \
  --secret ANTHROPIC_API_KEY@api.anthropic.com \
  --allow-host api.anthropic.com \
  --allow-host "*.anthropic.com" \
  --allow-host "*.github.com" \
  --allow-host "github.com" \
  -it \
  -- /usr/local/bin/entrypoint.sh "Go to github.com/jingkaihe/matchlock and summarise the README"
```

## How It Works

1. **matchlock build** creates an ext4 rootfs from the Dockerfile with Node.js, Claude Code, and Playwright pre-installed
2. **matchlock run** boots a Firecracker microVM (or Virtualization.framework on macOS) with that rootfs
3. The `--secret ANTHROPIC_API_KEY@api.anthropic.com` flag means the real API key **never enters the VM** — matchlock's MITM proxy injects it on-the-fly into requests to `api.anthropic.com`
4. Inside the VM, the entrypoint launches Claude Code, which starts the Playwright MCP server as a child process
5. Claude uses MCP tools (`browser_navigate`, `browser_click`, `browser_snapshot`, etc.) to control headless Chromium
6. All browser traffic flows through the VM's network, governed by `--allow-host` rules

## Secret Injection

The `ANTHROPIC_API_KEY` is injected via matchlock's transparent MITM proxy. Inside the VM, the environment variable contains a placeholder (e.g., `SANDBOX_SECRET_a1b2c3d4...`). When Claude Code makes API calls to `api.anthropic.com`, the host-side proxy replaces the placeholder with the real key in-flight. This means:

- The real key is **never visible** inside the sandbox
- Even if the agent is compromised, the key cannot be exfiltrated
- The key only works for requests to the designated host (`api.anthropic.com`)

## Available MCP Tools

Claude Code has access to the full Playwright MCP toolset:

| Tool | Description |
|------|-------------|
| `browser_navigate` | Navigate to a URL |
| `browser_click` | Click on page elements |
| `browser_fill_form` | Fill form fields |
| `browser_type` | Type text into elements |
| `browser_snapshot` | Get accessibility tree snapshot |
| `browser_take_screenshot` | Capture page screenshot |
| `browser_evaluate` | Run JavaScript on the page |
| `browser_select_option` | Select dropdown options |
| `browser_press_key` | Press keyboard keys |
| `browser_tabs` | Manage browser tabs |
| `browser_navigate_back` | Go back in history |
| `browser_wait_for` | Wait for text/elements |

See the [Playwright MCP docs](https://github.com/microsoft/playwright-mcp) for the full list.
