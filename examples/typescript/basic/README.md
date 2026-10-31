# TypeScript SDK Basic Example

Run from the repository root:

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd examples/typescript/basic
npm install
npm run start
```

The script launches a `node:22-alpine` sandbox, installs `@anthropic-ai/sdk`, and streams output from the Anthropic Node SDK in real time via `execStream`.

The example creates `/workspace` on the guest's native filesystem and uploads its script through the SDK; it does not mount a host directory.
