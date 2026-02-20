# Matchlock TypeScript SDK

TypeScript client for [Matchlock](https://github.com/jingkaihe/matchlock), with feature parity across the existing Go and Python SDKs.

## Requirements

- Node.js 22+
- `matchlock` CLI installed and available on `PATH` (or configured with `binaryPath`)

## Install (from source)

```bash
cd sdk/typescript
npm install
npm run build
```

## Quick Start

```ts
import { Client, Sandbox } from "matchlock-sdk";

const sandbox = new Sandbox("alpine:latest")
  .withCPUs(2)
  .withMemory(1024)
  .allowHost("api.openai.com")
  .addSecret("API_KEY", process.env.API_KEY ?? "", "api.openai.com");

const client = new Client();

try {
  await client.launch(sandbox);
  const result = await client.exec("echo hello from sandbox");
  console.log(result.stdout);
} finally {
  await client.close();
}
```

## Highlights

- Fluent sandbox builder (`Sandbox`) with network, secrets, mounts, env, VFS hooks, image config
- JSON-RPC `create`, `exec`, `exec_stream`, `write_file`, `read_file`, `list_files`, `port_forward`, `cancel`, `close`
- Streaming stdout/stderr via `execStream`
- Local VFS callbacks (`hook`, `dangerousHook`, `mutateHook`, `actionHook`)
- Port forwarding API parity (`portForward`, `portForwardWithAddresses`)
- Lifecycle control (`close`, `remove`, `vmId`)

## Development

```bash
cd sdk/typescript
npm install
npm run typecheck
npm test
npm run build
```
