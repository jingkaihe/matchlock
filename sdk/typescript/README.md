# Matchlock TypeScript SDK

TypeScript client for [Matchlock](https://github.com/jingkaihe/matchlock), with feature parity across the existing Go and Python SDKs.

## Requirements

- Node.js 22+
- `matchlock` CLI installed and available on `PATH` (or configured with `binaryPath`)

## Install

```bash
npm install matchlock-sdk
```

## Quick Start

```ts
import { Client, Sandbox } from "matchlock-sdk";

const sandbox = new Sandbox("alpine:latest")
  .withCPUs(2)
  .withMemory(1024)
  .allowHost("api.openai.com")
  .withNetworkInterception({
    rules: [
      {
        phase: "after",
        hosts: ["api.openai.com"],
        action: "mutate",
        setResponseHeaders: { "x-intercepted": "true" },
      },
    ],
  })
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

Callback-based interception:

```ts
import { NetworkHookRequest, NetworkHookResult, Sandbox } from "matchlock-sdk";

const afterHook = async (
  req: NetworkHookRequest,
): Promise<NetworkHookResult | null> => {
  if (req.statusCode !== 200) {
    return null;
  }
  return {
    action: "mutate",
    response: {
      headers: { "X-Intercepted": ["callback"] },
      setBody: Buffer.from('{"msg":"from-callback"}'),
    },
  };
};

const sandbox = new Sandbox("alpine:latest").withNetworkInterception({
  rules: [{ phase: "after", hosts: ["api.openai.com"], hook: afterHook, timeoutMs: 1500 }],
});
```

## Highlights

- Fluent sandbox builder (`Sandbox`) with network, secrets, mounts, env, VFS hooks, image config
- Typed network interception rules and local callback hooks via `withNetworkInterception(...)`
- Supports fully offline mode via `.withNoNetwork()` (no guest NIC / no egress)
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
