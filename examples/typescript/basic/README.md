# TypeScript SDK Basic Example

Run from the repository root:

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd sdk/typescript
npm install
npm run build

cd ../../examples/typescript/basic
npm install
npm run start
```

The script launches a `node:22-alpine` sandbox, installs `@anthropic-ai/sdk`, and streams output from the Anthropic Node SDK in real time via `execStream`.

Note: the example installs with `npm --no-bin-links` because `/workspace` currently does not support npm symlink creation for `.bin` entries.
