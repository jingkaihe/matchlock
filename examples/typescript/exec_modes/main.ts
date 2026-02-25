import { Client, Sandbox } from "../../../sdk/typescript/src/index.js";

function isInteractiveTTY(): boolean {
  return (
    Boolean(process.stdin.isTTY) &&
    Boolean(process.stdout.isTTY) &&
    typeof process.stdin.setRawMode === "function"
  );
}

async function runExecStream(client: Client): Promise<void> {
  console.log("== execStream ==");
  const result = await client.execStream(
    "echo stream:start; sleep 1; echo stream:end",
    {
      workingDir: "/workspace",
      stdout: process.stdout,
      stderr: process.stderr,
    },
  );
  console.log(`stream exit=${result.exitCode} duration_ms=${result.durationMs}`);
}

async function runExecPipe(client: Client): Promise<void> {
  console.log("\n== execPipe ==");
  const stdoutChunks: Buffer[] = [];
  const stderrChunks: Buffer[] = [];

  const result = await client.execPipe("cat; echo pipe-stderr >&2", {
    workingDir: "/workspace",
    stdin: [Buffer.from("hello from stdin\n")],
    stdout: (chunk) => {
      stdoutChunks.push(chunk);
    },
    stderr: (chunk) => {
      stderrChunks.push(chunk);
    },
  });

  console.log(`pipe exit=${result.exitCode} duration_ms=${result.durationMs}`);
  process.stdout.write("pipe stdout:\n");
  process.stdout.write(Buffer.concat(stdoutChunks).toString("utf8"));
  process.stdout.write("pipe stderr:\n");
  process.stdout.write(Buffer.concat(stderrChunks).toString("utf8"));
}

async function runExecInteractive(client: Client): Promise<void> {
  console.log("\n== execInteractive ==");
  if (!isInteractiveTTY()) {
    console.log("interactive terminal skipped (requires a TTY).");
    return;
  }

  const rows = process.stdout.rows ?? 24;
  const cols = process.stdout.columns ?? 80;

  console.log("Connected to sandbox shell. Type 'exit' or press Ctrl-D to quit.");

  process.stdin.resume();
  process.stdin.setRawMode(true);
  try {
    const result = await client.execInteractive("sh", {
      workingDir: "/workspace",
      stdin: process.stdin,
      stdout: process.stdout,
      rows,
      cols,
    });
    console.log(`\nShell exited: code=${result.exitCode} duration_ms=${result.durationMs}`);
  } finally {
    process.stdin.setRawMode(false);
    process.stdin.pause();
  }
}

async function main(): Promise<void> {
  const client = new Client();
  const sandbox = new Sandbox("alpine:latest")
    .withWorkspace("/workspace")
    .mountMemory("/workspace");

  try {
    const vmId = await client.launch(sandbox);
    console.log(`sandbox ready vm=${vmId}`);

    await runExecStream(client);
    await runExecPipe(client);
    await runExecInteractive(client);
  } finally {
    await client.close();
    await client.remove();
  }
}

void main().catch((error) => {
  console.error(error);
  process.exit(1);
});
