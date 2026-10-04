# Matchlock TypeScript SDK

TypeScript client for [Matchlock](https://github.com/jingkaihe/matchlock) micro-VM sandboxes.

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

`client.launch(...)` starts image ENTRYPOINT/CMD in detached mode. Use `client.create(...)` when you want a VM without auto-starting image command.

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

- Fluent sandbox builder (`Sandbox`) with network, secrets, env, and image config
- Typed network interception rules and local callback hooks via `withNetworkInterception(...)`
- Supports fully offline mode via `.withNoNetwork()` (no guest NIC / no egress)
- JSON-RPC `create`, `exec`, `exec_stream`, `exec_pipe`, `exec_tty`, `log`, `log_stream`, `write_file`, `read_file`, `list_files`, `port_forward`, `cancel`, `close`
- Streaming stdout/stderr via `execStream`, bidirectional stdin/stdout/stderr via `execPipe`, and per-command working directory and user selection for pipe and interactive PTY execution
- Guest-native filesystems with explicit file read/write/list and stream transfers
- Managed ext4 block volume creation, listing, and removal
- VM log access via `log()` and `logStream()`
- Interactive PTY shell/commands via `execInteractive` (stdin/stdout + resize events)
- Port forwarding API parity (`portForward`, `portForwardWithAddresses`)
- Lifecycle control (`close`, `remove`, `vmId`)

## Guest Filesystem and File Operations

Files live on the guest's native filesystem, not in a shared host directory.
The writable root filesystem belongs to the sandbox; copy out results before
removing it, or use a managed block volume for persistent data. File operations
are explicit transfers over the SDK's JSON-RPC connection and the guest-agent
vsock service. They do not expose a host filesystem or install filesystem hooks.

`/workspace` is an ordinary guest path, not an automatically mounted directory.
Create it before using it. Commands use the image's `WORKDIR` by default; pass
`{ workingDir: "/workspace" }` to select an existing guest directory for an execution.

```ts
import { Client, Sandbox } from "matchlock-sdk";

const client = new Client();
try {
  await client.create(new Sandbox("alpine:latest").options());
  await client.exec("mkdir -p /workspace");
  await client.writeFile("/workspace/hello.txt", "Hello, world!");
  await client.writeFileMode("/workspace/script.sh", "#!/bin/sh\necho hi", 0o755);
  const content = await client.readFile("/workspace/hello.txt");
  console.log(content.toString("utf8"));
  const files = await client.listFiles("/workspace");
  console.log(files);
  const result = await client.exec("./script.sh", { workingDir: "/workspace" });
  console.log(result.stdout); // "hi\n"
} finally {
  await client.close();
}
```

## Stdin/Stdout File Transfers

`execPipe` transfers stdin/stdout/stderr without a PTY. Its input accepts Node
readable streams or iterables, and output writers receive `Buffer` chunks. This
example uploads a local binary file while streaming its contents back to another
local file:

```ts
import { createReadStream, createWriteStream } from "node:fs";
import { finished } from "node:stream/promises";
import { Client } from "matchlock-sdk";

const client = new Client();
try {
  await client.create({ image: "alpine:latest" });
  await client.exec("mkdir -p /workspace");
  const source = createReadStream("input.bin");
  const output = createWriteStream("output.bin");
  try {
    const [result] = await Promise.all([
      client.execPipe("tee /workspace/input.bin", {
        stdin: source,
        stdout: output,
        stderr: process.stderr,
      }).finally(() => output.end()),
      finished(output),
    ]);
    if (result.exitCode !== 0) {
      throw new Error(`File transfer exited with ${result.exitCode}`);
    }
  } finally {
    source.destroy();
    output.destroy();
  }
} finally {
  await client.close();
}
```

The SDK does not close output writers, so end and await file streams before
using their contents. `execStream` also accepts binary output writers when no
stdin is needed. Buffered `exec` returns text; use `readFile` (a `Buffer`) or
streaming output for binary data.

## Managed Block Volumes

Named volumes are managed ext4 disk images, separate from the sandbox's writable
root filesystem. When attached, the guest accesses them as block devices; they
are not shared host directories. They persist independently of a VM until removed.

```ts
import { Client } from "matchlock-sdk";

const client = new Client();
const volume = await client.volumeCreate("sdk-data", 1024);
console.log(volume.name, volume.size, volume.path);
console.log(await client.volumeList());

// When the volume is no longer attached or needed:
// await client.volumeRemove("sdk-data");
```

These helpers manage volumes without creating a VM. `VolumeInfo.path` is the host
path of the backing disk image, not a directory exposed to the guest. The
TypeScript builder and `CreateOptions` do not currently expose block-disk
attachment. Use the CLI's `--disk` option to launch a VM with a volume created above:

```bash
matchlock run --image alpine:latest --disk @sdk-data:/data -- sh -c 'echo persisted > /data/hello.txt'
```

## File and Storage API Reference

| Method | Description |
|---|---|
| `client.exec(command, { workingDir })` | Execute in an existing guest directory — returns `ExecResult` |
| `client.execStream(command, options)` | Stream output to `options.stdout` / `options.stderr` — returns `ExecStreamResult` |
| `client.execPipe(command, options)` | Stream `options.stdin`, `options.stdout`, and `options.stderr` without a PTY — returns `ExecPipeResult` |
| `client.writeFile(path, content, options?)` | Write `BinaryLike` content with mode `0o644` |
| `client.writeFileMode(path, content, mode, options?)` | Write content with explicit guest permissions |
| `client.readFile(path, options?)` | Read a guest file — returns `Buffer` |
| `client.listFiles(path, options?)` | List a guest directory — returns `FileInfo[]` |
| `client.volumeCreate(name, sizeMb = 10240)` | Create a named ext4 block volume — returns `VolumeInfo` |
| `client.volumeList()` | List managed block volumes — returns `VolumeInfo[]` |
| `client.volumeRemove(name)` | Delete a managed block volume that is no longer in use |
| `sandbox.withDiskSize(mb)` | Set the sandbox's writable root disk size |
| `sandbox.withImageConfig(config)` | Merge image metadata, including `workingDir`; does not create directories |

Client methods above are asynchronous. File-operation `options` use
`RequestOptions` (`signal`, `timeoutMs`); exec options also accept `workingDir`.

| Type | Fields / values |
|---|---|
| `BinaryLike` | `string`, `Buffer`, `Uint8Array`, or `ArrayBuffer` |
| `FileInfo` | `name`, `size`, `mode`, `isDir` |
| `VolumeInfo` | `name`, `size` (human-readable string), `path` (backing ext4 image) |
| `ExecPipeOptions` | `stdin`, `stdout`, `stderr`, `workingDir`, `signal`, `timeoutMs` |
| `StreamReader` | Node readable stream, async iterable, or iterable of `BinaryLike` chunks |
| `StreamWriter` | Node writable stream or callback receiving `Buffer` chunks |

## Development

```bash
cd sdk/typescript
npm install
npm run typecheck
npm test
npm run build
```
