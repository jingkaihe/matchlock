import { Client, type NetworkHookRequest, type NetworkHookResult, Sandbox } from "matchlock-sdk";

function afterHook(req: NetworkHookRequest): NetworkHookResult | null {
  // This callback runs only when host/path/phase prefilters match.
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
}

function assertExpectedOutput(output: string): void {
  const lowered = output.toLowerCase();
  if (!output.includes('{"msg":"from-callback"}')) {
    throw new Error("expected callback to replace response body");
  }
  if (lowered.includes("x-upstream: 1")) {
    throw new Error('expected header "X-Upstream" to be removed');
  }
  if (!lowered.includes("x-intercepted: callback")) {
    throw new Error('expected header "X-Intercepted: callback"');
  }
}

async function main(): Promise<void> {
  const client = new Client();

  const sandbox = new Sandbox("alpine:latest")
    .allowHost("httpbin.org")
    .withNetworkInterception({
      rules: [
        {
          name: "dynamic-response-callback",
          phase: "after",
          hosts: ["httpbin.org"],
          path: "/response-headers",
          hook: async (request) => afterHook(request),
        },
      ],
    });

  try {
    const vmId = await client.launch(sandbox);
    console.log(`sandbox ready vm=${vmId}`);

    const result = await client.exec(
      'sh -c \'wget -S -O - "http://httpbin.org/response-headers?X-Upstream=1&body=foo" 2>&1\'',
    );
    const output = `${result.stdout}${result.stderr}`;
    process.stdout.write(output);

    assertExpectedOutput(output);
    console.log("OK: callback hook intercepted and mutated the response");
  } finally {
    await client.close();
    await client.remove();
  }
}

void main().catch((error) => {
  console.error(error);
  process.exit(1);
});
