import { Client, Sandbox } from "matchlock-sdk";

const SCRIPT = `import Anthropic from "@anthropic-ai/sdk";

const anthropic = new Anthropic({
  apiKey: process.env.ANTHROPIC_API_KEY,
});

const stream = anthropic.messages
  .stream({
    model: "claude-haiku-4-5-20251001",
    max_tokens: 1024,
    messages: [{ role: "user", content: "Explain TCP/IP." }],
  })
  .on("text", (text) => {
    process.stdout.write(text);
  });

await stream.finalMessage();
process.stdout.write("\\n");
`;

function ensureSuccess(
  step: string,
  result: { exitCode: number; stdout: string; stderr: string },
): void {
  if (result.exitCode === 0) {
    return;
  }
  throw new Error(
    `${step} failed (exit=${result.exitCode})\nstdout:\n${result.stdout}\nstderr:\n${result.stderr}`,
  );
}

async function main(): Promise<void> {
  const apiKey = process.env.ANTHROPIC_API_KEY;
  if (!apiKey) {
    throw new Error("ANTHROPIC_API_KEY is required");
  }

  const client = new Client();

  try {
    const sandbox = new Sandbox("node:22-alpine")
      .withCPUs(1)
      .withMemory(512)
      .allowHost("registry.npmjs.org", "*.npmjs.org", "api.anthropic.com")
      .addSecret("ANTHROPIC_API_KEY", apiKey, "api.anthropic.com");

    const vmId = await client.launch(sandbox);
    console.log(`launched ${vmId}`);

    const install = await client.exec(
      "npm init -y >/dev/null 2>&1 && npm install --quiet --no-bin-links @anthropic-ai/sdk",
      { workingDir: "/workspace" },
    );
    ensureSuccess("install @anthropic-ai/sdk", install);

    await client.writeFile("/workspace/ask.mjs", SCRIPT);

    const stream = await client.execStream("node ask.mjs", {
      workingDir: "/workspace",
      stdout: process.stdout,
      stderr: process.stderr,
    });
    console.log(`exit=${stream.exitCode} duration_ms=${stream.durationMs}`);
  } finally {
    await client.close();
    await client.remove();
  }
}

void main().catch((error) => {
  console.error(error);
  process.exit(1);
});
