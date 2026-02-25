import { Client, type NetworkHookRequest, type NetworkHookResult, Sandbox } from "matchlock-sdk";

const SCRIPT = `# /// script
# requires-python = ">=3.12"
# dependencies = ["anthropic"]
# ///
import anthropic

client = anthropic.Anthropic(api_key="placeholder")
with client.messages.stream(
    model="claude-haiku-4-5-20251001",
    max_tokens=1000,
    messages=[{"role": "user", "content": "Explain TCP to me"}],
) as stream:
    for text in stream.text_stream:
        print(text, end="", flush=True)
print()
`;

function injectAnthropicAPIKey(req: NetworkHookRequest, apiKey: string): NetworkHookResult {
  const headers: Record<string, string[]> = { ...(req.requestHeaders ?? {}) };
  headers["X-Api-Key"] = [apiKey];
  return {
    action: "mutate",
    request: { headers },
  };
}

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
  const apiKey = process.env.ANTHROPIC_API_KEY ?? "";
  const client = new Client();

  const sandbox = new Sandbox("python:3.12-alpine")
    .withWorkspace("/workspace")
    .mountMemory("/workspace")
    .allowHost(
      "dl-cdn.alpinelinux.org",
      "files.pythonhosted.org",
      "pypi.org",
      "astral.sh",
      "github.com",
      "objects.githubusercontent.com",
      "api.anthropic.com",
    )
    .withNetworkInterception({
      rules: [
        {
          name: "inject-anthropic-api-key",
          phase: "before",
          hosts: ["api.anthropic.com"],
          hook: async (request) => injectAnthropicAPIKey(request, apiKey),
        },
      ],
    });

  try {
    const vmId = await client.launch(sandbox);
    console.log(`sandbox ready vm=${vmId}`);

    const version = await client.exec("python3 --version");
    process.stdout.write(version.stdout);

    const install = await client.exec("pip install --quiet uv");
    ensureSuccess("pip install --quiet uv", install);

    await client.writeFile("/workspace/ask.py", SCRIPT);

    const stream = await client.execStream("uv run /workspace/ask.py", {
      stdout: process.stdout,
      stderr: process.stderr,
    });
    process.stdout.write("\n");
    console.log(`done exit_code=${stream.exitCode} duration_ms=${stream.durationMs}`);
  } finally {
    await client.close();
    await client.remove();
  }
}

void main().catch((error) => {
  console.error(error);
  process.exit(1);
});
