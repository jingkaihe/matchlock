import { beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("node:child_process", async () => {
  const actual = await vi.importActual<typeof import("node:child_process")>(
    "node:child_process",
  );
  return {
    ...actual,
    spawn: vi.fn(),
    execFile: vi.fn(),
  };
});

import { execFile, spawn } from "node:child_process";
import { Client, MatchlockError, RPCError, Sandbox, VFS_HOOK_ACTION_BLOCK } from "../src";
import { FakeProcess } from "./helpers";

const mockedSpawn = vi.mocked(spawn);
const mockedExecFile = vi.mocked(execFile as unknown as (...args: unknown[]) => unknown);

function installFakeProcess(): FakeProcess {
  const fake = new FakeProcess();
  mockedSpawn.mockReturnValue(fake as never);
  return fake;
}

function withTimeout<T>(promise: Promise<T>, timeoutMs = 250): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const timer = setTimeout(() => {
      reject(new Error(`timed out after ${timeoutMs}ms`));
    }, timeoutMs);
    promise.then(
      (value) => {
        clearTimeout(timer);
        resolve(value);
      },
      (error) => {
        clearTimeout(timer);
        reject(error);
      },
    );
  });
}

describe("Client", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockedExecFile.mockImplementation((...args: unknown[]) => {
      const cb = args[args.length - 1];
      if (typeof cb === "function") {
        cb(null, "", "");
      }
      return {};
    });
  });

  it("starts rpc process with sudo when configured", async () => {
    const fake = installFakeProcess();
    const client = new Client({ binaryPath: "/opt/matchlock", useSudo: true });

    await client.start();

    expect(mockedSpawn).toHaveBeenCalledWith(
      "sudo",
      ["/opt/matchlock", "rpc"],
      { stdio: ["pipe", "pipe", "pipe"] },
    );

    fake.close();
    await client.close();
  });

  it("requires image for create", async () => {
    const client = new Client();
    await expect(client.create({})).rejects.toThrow("image is required");
  });

  it("sets launch_entrypoint on launch", async () => {
    const fake = installFakeProcess();
    const client = new Client();

    const launchPromise = client.launch(new Sandbox("alpine:latest"));
    const request = await fake.waitForRequest("create");
    expect(request.params?.launch_entrypoint).toBe(true);

    fake.pushResponse({ jsonrpc: "2.0", id: request.id, result: { id: "vm-launch" } });
    await expect(launchPromise).resolves.toBe("vm-launch");

    fake.close();
    await client.close();
  });

  it("sends create payload with network defaults", async () => {
    const fake = installFakeProcess();
    const client = new Client();

    const createPromise = client.create(
      new Sandbox("alpine:latest")
        .allowHost("api.openai.com")
        .addHost("api.internal", "10.0.0.10")
        .withDNSServers("1.1.1.1")
        .withHostname("sandbox")
        .withNetworkMTU(1200)
        .options(),
    );

    const request = await fake.waitForRequest("create");
    expect(request.params?.network).toEqual({
      allowed_hosts: ["api.openai.com"],
      add_hosts: [{ host: "api.internal", ip: "10.0.0.10" }],
      block_private_ips: true,
      dns_servers: ["1.1.1.1"],
      hostname: "sandbox",
      mtu: 1200,
    });

    fake.pushResponse({ jsonrpc: "2.0", id: request.id, result: { id: "vm-net" } });
    await expect(createPromise).resolves.toBe("vm-net");

    fake.close();
    await client.close();
  });

  it("sends create payload with network interception", async () => {
    const fake = installFakeProcess();
    const client = new Client();

    const createPromise = client.create(
      new Sandbox("alpine:latest")
        .withNetworkInterception({
          rules: [
            {
              phase: "after",
              action: "mutate",
              hosts: ["api.example.com"],
              path: "/v1/*",
              setResponseHeaders: { "X-Intercepted": "true" },
              bodyReplacements: [{ find: "foo", replace: "bar" }],
              timeoutMs: 1500,
            },
          ],
        })
        .options(),
    );

    const request = await fake.waitForRequest("create");
    expect(request.params?.network).toEqual({
      allowed_hosts: [],
      block_private_ips: true,
      intercept: true,
      interception: {
        rules: [
          {
            phase: "after",
            action: "mutate",
            hosts: ["api.example.com"],
            path: "/v1/*",
            set_response_headers: { "X-Intercepted": "true" },
            body_replacements: [{ find: "foo", replace: "bar" }],
            timeout_ms: 1500,
          },
        ],
      },
    });

    fake.pushResponse({ jsonrpc: "2.0", id: request.id, result: { id: "vm-net-hooks" } });
    await expect(createPromise).resolves.toBe("vm-net-hooks");

    fake.close();
    await client.close();
  });

  it("sends create payload with forced interception only", async () => {
    const fake = installFakeProcess();
    const client = new Client();

    const createPromise = client.create(
      new Sandbox("alpine:latest").withNetworkInterception().options(),
    );

    const request = await fake.waitForRequest("create");
    expect(request.params?.network).toEqual({
      allowed_hosts: [],
      block_private_ips: true,
      intercept: true,
    });

    fake.pushResponse({ jsonrpc: "2.0", id: request.id, result: { id: "vm-net-force" } });
    await expect(createPromise).resolves.toBe("vm-net-force");

    fake.close();
    await client.close();
  });

  it("sends create payload with callback-based network interception rule", async () => {
    const fake = installFakeProcess();
    const client = new Client();

    const createPromise = client.create(
      new Sandbox("alpine:latest")
        .withNetworkInterception({
          rules: [
            {
              name: "callback-after",
              phase: "after",
              hosts: ["api.example.com"],
              path: "/v1/*",
              timeoutMs: 1500,
              hook: async () => undefined,
            },
          ],
        })
        .options(),
    );

    const request = await fake.waitForRequest("create");
    const network = request.params?.network as Record<string, unknown>;
    expect(network.intercept).toBe(true);
    const interception = network.interception as Record<string, unknown>;
    expect(typeof interception.callback_socket).toBe("string");
    expect(String(interception.callback_socket)).not.toBe("");
    const rules = interception.rules as Array<Record<string, unknown>>;
    expect(rules).toHaveLength(1);
    expect(typeof rules[0].callback_id).toBe("string");
    expect(String(rules[0].callback_id)).not.toBe("");

    fake.pushResponse({
      jsonrpc: "2.0",
      id: request.id,
      result: { id: "vm-net-callback" },
    });
    await expect(createPromise).resolves.toBe("vm-net-callback");

    fake.close();
    await client.close();
  });

  it("rejects callback network interception rule with non-allow action", async () => {
    const client = new Client();
    await expect(
      client.create(
        new Sandbox("alpine:latest")
          .withNetworkInterception({
            rules: [
              {
                name: "bad-callback",
                phase: "after",
                action: "block",
                hook: async () => undefined,
              },
            ],
          })
          .options(),
      ),
    ).rejects.toThrow("callback hooks cannot set action");
  });

  it("respects explicit block_private_ips=false", async () => {
    const fake = installFakeProcess();
    const client = new Client();

    const createPromise = client.create({
      image: "alpine:latest",
      networkMtu: 1200,
      blockPrivateIPs: false,
      blockPrivateIPsSet: true,
    });

    const request = await fake.waitForRequest("create");
    expect((request.params?.network as Record<string, unknown>).block_private_ips).toBe(
      false,
    );

    fake.pushResponse({
      jsonrpc: "2.0",
      id: request.id,
      result: { id: "vm-private-off" },
    });
    await expect(createPromise).resolves.toBe("vm-private-off");

    fake.close();
    await client.close();
  });

  it("sends create payload with no_network", async () => {
    const fake = installFakeProcess();
    const client = new Client();

    const createPromise = client.create(
      new Sandbox("alpine:latest").withNoNetwork().options(),
    );

    const request = await fake.waitForRequest("create");
    expect(request.params?.network).toEqual({
      no_network: true,
    });

    fake.pushResponse({
      jsonrpc: "2.0",
      id: request.id,
      result: { id: "vm-no-network" },
    });
    await expect(createPromise).resolves.toBe("vm-no-network");

    fake.close();
    await client.close();
  });

  it("rejects no_network combined with allowed hosts", async () => {
    const client = new Client();
    await expect(
      client.create({
        image: "alpine:latest",
        noNetwork: true,
        allowedHosts: ["api.openai.com"],
      }),
    ).rejects.toThrow(
      "no network cannot be combined with allowed hosts, secrets, forced interception, or network interception rules",
    );
  });

  it("keeps vmId when post-create port_forward fails", async () => {
    const fake = installFakeProcess();
    const client = new Client();

    const createPromise = client.create({
      image: "alpine:latest",
      portForwards: [{ localPort: 18080, remotePort: 8080 }],
    });

    const createReq = await fake.waitForRequest("create");
    fake.pushResponse({
      jsonrpc: "2.0",
      id: createReq.id,
      result: { id: "vm-created" },
    });

    const pfReq = await fake.waitForRequest("port_forward");
    fake.pushResponse({
      jsonrpc: "2.0",
      id: pfReq.id,
      error: {
        code: RPCError.VM_FAILED,
        message: "bind: address already in use",
      },
    });

    await expect(createPromise).rejects.toBeInstanceOf(RPCError);
    expect(client.vmId).toBe("vm-created");

    fake.close();
    await client.close();
  });

  it("rejects invalid add-host mappings", async () => {
    const client = new Client();
    await expect(
      client.create({
        image: "alpine:latest",
        addHosts: [{ host: "bad host", ip: "10.0.0.10" }],
      }),
    ).rejects.toThrow("invalid add-host mapping");
  });

  it("supports exec and working directory", async () => {
    const fake = installFakeProcess();
    const client = new Client();

    const execPromise = client.exec("pwd", { workingDir: "/workspace" });
    const request = await fake.waitForRequest("exec");
    expect(request.params).toEqual({ command: "pwd", working_dir: "/workspace" });

    fake.pushResponse({
      jsonrpc: "2.0",
      id: request.id,
      result: {
        exit_code: 0,
        stdout: Buffer.from("/workspace\n").toString("base64"),
        stderr: Buffer.from("").toString("base64"),
        duration_ms: 5,
      },
    });

    await expect(execPromise).resolves.toEqual({
      exitCode: 0,
      stdout: "/workspace\n",
      stderr: "",
      durationMs: 5,
    });

    fake.close();
    await client.close();
  });

  it("streams exec output from notifications", async () => {
    const fake = installFakeProcess();
    const client = new Client();

    let out = "";
    let err = "";

    const streamPromise = client.execStream("cmd", {
      stdout: (chunk) => {
        out += chunk.toString("utf8");
      },
      stderr: (chunk) => {
        err += chunk.toString("utf8");
      },
    });

    const req = await fake.waitForRequest("exec_stream");
    fake.pushNotification("exec_stream.stdout", {
      id: req.id,
      data: Buffer.from("line1\n").toString("base64"),
    });
    fake.pushNotification("exec_stream.stderr", {
      id: req.id,
      data: Buffer.from("warn\n").toString("base64"),
    });
    fake.pushNotification("exec_stream.stdout", {
      id: req.id,
      data: Buffer.from("line2\n").toString("base64"),
    });

    fake.pushResponse({
      jsonrpc: "2.0",
      id: req.id,
      result: { exit_code: 0, duration_ms: 42 },
    });

    await expect(streamPromise).resolves.toEqual({ exitCode: 0, durationMs: 42 });
    expect(out).toBe("line1\nline2\n");
    expect(err).toBe("warn\n");

    fake.close();
    await client.close();
  });

  it("supports execPipe with stdin/stdout/stderr streaming", async () => {
    const fake = installFakeProcess();
    const client = new Client();

    let out = "";
    let err = "";
    const pipePromise = client.execPipe("cat", {
      stdin: [Buffer.from("hello stdin\n")],
      stdout: (chunk) => {
        out += chunk.toString("utf8");
      },
      stderr: (chunk) => {
        err += chunk.toString("utf8");
      },
    });

    const req = await fake.waitForRequest("exec_pipe");
    fake.pushNotification("exec_pipe.ready", { id: req.id });

    const stdinReq = await fake.waitForRequest("exec_pipe.stdin");
    expect((stdinReq.params as Record<string, unknown>).id).toBe(req.id);
    expect(
      Buffer.from(
        String((stdinReq.params as Record<string, unknown>).data),
        "base64",
      ).toString("utf8"),
    ).toBe("hello stdin\n");

    const eofReq = await fake.waitForRequest("exec_pipe.stdin_eof");
    expect((eofReq.params as Record<string, unknown>).id).toBe(req.id);

    fake.pushNotification("exec_pipe.stdout", {
      id: req.id,
      data: Buffer.from("line\n").toString("base64"),
    });
    fake.pushNotification("exec_pipe.stderr", {
      id: req.id,
      data: Buffer.from("warn\n").toString("base64"),
    });
    fake.pushResponse({
      jsonrpc: "2.0",
      id: req.id,
      result: { exit_code: 0, duration_ms: 64 },
    });

    await expect(pipePromise).resolves.toEqual({ exitCode: 0, durationMs: 64 });
    expect(out).toBe("line\n");
    expect(err).toBe("warn\n");

    fake.close();
    await client.close();
  });

  it("supports execInteractive with stdin, stdout, and resize", async () => {
    const fake = installFakeProcess();
    const client = new Client();

    let out = "";
    const interactivePromise = client.execInteractive("sh", {
      stdin: [Buffer.from("exit\n")],
      stdout: (chunk) => {
        out += chunk.toString("utf8");
      },
      rows: 30,
      cols: 100,
      resize: [[40, 120]],
    });

    const req = await fake.waitForRequest("exec_tty");
    expect(req.params).toEqual({ command: "sh", rows: 30, cols: 100 });
    fake.pushNotification("exec_tty.ready", { id: req.id });

    const stdinReq = await fake.waitForRequest("exec_tty.stdin");
    expect((stdinReq.params as Record<string, unknown>).id).toBe(req.id);
    expect(
      Buffer.from(
        String((stdinReq.params as Record<string, unknown>).data),
        "base64",
      ).toString("utf8"),
    ).toBe("exit\n");

    const resizeReq = await fake.waitForRequest("exec_tty.resize");
    expect((resizeReq.params as Record<string, unknown>).id).toBe(req.id);
    expect((resizeReq.params as Record<string, unknown>).rows).toBe(40);
    expect((resizeReq.params as Record<string, unknown>).cols).toBe(120);

    const eofReq = await fake.waitForRequest("exec_tty.stdin_eof");
    expect((eofReq.params as Record<string, unknown>).id).toBe(req.id);

    fake.pushNotification("exec_tty.stdout", {
      id: req.id,
      data: Buffer.from("/workspace # ").toString("base64"),
    });
    fake.pushResponse({
      jsonrpc: "2.0",
      id: req.id,
      result: { exit_code: 0, duration_ms: 101 },
    });

    await expect(interactivePromise).resolves.toEqual({
      exitCode: 0,
      durationMs: 101,
    });
    expect(out).toBe("/workspace # ");

    fake.close();
    await client.close();
  });

  it("does not hang execPipe when stdin iterable does not terminate", async () => {
    const fake = installFakeProcess();
    const client = new Client();

    async function* blockedInput(): AsyncGenerator<Buffer> {
      await new Promise<void>(() => {});
    }

    const pipePromise = client.execPipe("cat", {
      stdin: blockedInput(),
    });

    const req = await fake.waitForRequest("exec_pipe");
    fake.pushNotification("exec_pipe.ready", { id: req.id });
    fake.pushResponse({
      jsonrpc: "2.0",
      id: req.id,
      result: { exit_code: 0, duration_ms: 8 },
    });

    await expect(withTimeout(pipePromise)).resolves.toEqual({
      exitCode: 0,
      durationMs: 8,
    });

    fake.close();
    await client.close();
  });

  it("does not hang execInteractive when resize iterable does not terminate", async () => {
    const fake = installFakeProcess();
    const client = new Client();

    async function* blockedResize(): AsyncGenerator<[number, number]> {
      await new Promise<void>(() => {});
    }

    const interactivePromise = client.execInteractive("sh", {
      resize: blockedResize(),
    });

    const req = await fake.waitForRequest("exec_tty");
    fake.pushNotification("exec_tty.ready", { id: req.id });
    fake.pushResponse({
      jsonrpc: "2.0",
      id: req.id,
      result: { exit_code: 0, duration_ms: 13 },
    });

    await expect(withTimeout(interactivePromise)).resolves.toEqual({
      exitCode: 0,
      durationMs: 13,
    });

    fake.close();
    await client.close();
  });

  it("applies mutate hooks for write_file", async () => {
    const fake = installFakeProcess();
    const client = new Client();

    const createPromise = client.create({
      image: "alpine:latest",
      vfsInterception: {
        rules: [
          {
            phase: "before",
            ops: ["write"],
            path: "/workspace/*",
            mutateHook: (request) =>
              Buffer.from(`size=${request.size};mode=${request.mode.toString(8)}`),
          },
        ],
      },
    });

    const createReq = await fake.waitForRequest("create");
    fake.pushResponse({
      jsonrpc: "2.0",
      id: createReq.id,
      result: { id: "vm-write" },
    });
    await createPromise;

    const writePromise = client.writeFile("/workspace/test.txt", Buffer.from("abcd"));
    const writeReq = await fake.waitForRequest("write_file");
    const content = Buffer.from(
      String((writeReq.params as Record<string, unknown>).content),
      "base64",
    ).toString("utf8");

    expect(content).toBe("size=4;mode=644");

    fake.pushResponse({ jsonrpc: "2.0", id: writeReq.id, result: {} });
    await writePromise;

    fake.close();
    await client.close();
  });

  it("blocks write_file when action hook returns block", async () => {
    const fake = installFakeProcess();
    const client = new Client();

    const createPromise = client.create({
      image: "alpine:latest",
      vfsInterception: {
        rules: [
          {
            phase: "before",
            ops: ["write"],
            path: "/workspace/*",
            actionHook: () => VFS_HOOK_ACTION_BLOCK,
          },
        ],
      },
    });

    const createReq = await fake.waitForRequest("create");
    fake.pushResponse({
      jsonrpc: "2.0",
      id: createReq.id,
      result: { id: "vm-action" },
    });
    await createPromise;

    const requestCount = fake.requests.length;
    await expect(
      client.writeFile("/workspace/test.txt", Buffer.from("blocked")),
    ).rejects.toThrow("blocked operation");
    expect(fake.requests.length).toBe(requestCount);

    fake.close();
    await client.close();
  });

  it("reads files and lists directories", async () => {
    const fake = installFakeProcess();
    const client = new Client();

    const readPromise = client.readFile("/workspace/file.txt");
    const readReq = await fake.waitForRequest("read_file");
    fake.pushResponse({
      jsonrpc: "2.0",
      id: readReq.id,
      result: { content: Buffer.from("hello").toString("base64") },
    });
    await expect(readPromise).resolves.toEqual(Buffer.from("hello"));

    const listPromise = client.listFiles("/workspace");
    const listReq = await fake.waitForRequest("list_files");
    fake.pushResponse({
      jsonrpc: "2.0",
      id: listReq.id,
      result: {
        files: [
          { name: "hello.txt", size: 5, mode: 0o644, is_dir: false },
          { name: "subdir", size: 0, mode: 0o755, is_dir: true },
        ],
      },
    });

    await expect(listPromise).resolves.toEqual([
      { name: "hello.txt", size: 5, mode: 0o644, isDir: false },
      { name: "subdir", size: 0, mode: 0o755, isDir: true },
    ]);

    fake.close();
    await client.close();
  });

  it("sends cancel rpc when aborted", async () => {
    const fake = installFakeProcess();
    const client = new Client();

    const abort = new AbortController();
    const execPromise = client.exec("sleep 60", { signal: abort.signal });
    const execReq = await fake.waitForRequest("exec");

    abort.abort(new MatchlockError("cancelled by test"));

    const cancelReq = await fake.waitForRequest("cancel");
    expect((cancelReq.params as Record<string, unknown>).id).toBe(execReq.id);
    await expect(execPromise).rejects.toThrow("cancelled by test");

    fake.close();
    await client.close();
  });

  it("parses and sends port forwards", async () => {
    const fake = installFakeProcess();
    const client = new Client();

    const pfPromise = client.portForward("8080", "18081:81");
    const req = await fake.waitForRequest("port_forward");

    expect(req.params).toEqual({
      forwards: [
        { local_port: 8080, remote_port: 8080 },
        { local_port: 18081, remote_port: 81 },
      ],
      addresses: ["127.0.0.1"],
    });

    fake.pushResponse({
      jsonrpc: "2.0",
      id: req.id,
      result: {
        bindings: [
          { address: "127.0.0.1", local_port: 8080, remote_port: 8080 },
          { address: "127.0.0.1", local_port: 18081, remote_port: 81 },
        ],
      },
    });

    await expect(pfPromise).resolves.toEqual([
      { address: "127.0.0.1", localPort: 8080, remotePort: 8080 },
      { address: "127.0.0.1", localPort: 18081, remotePort: 81 },
    ]);

    fake.close();
    await client.close();
  });

  it("routes event notifications to local after hooks", async () => {
    const fake = installFakeProcess();
    const client = new Client();

    let seen = "";
    const createPromise = client.create({
      image: "alpine:latest",
      vfsInterception: {
        rules: [
          {
            phase: "after",
            ops: ["write"],
            path: "/workspace/*",
            hook: (event) => {
              seen = `${event.op}:${event.path}`;
            },
          },
        ],
      },
    });

    const createReq = await fake.waitForRequest("create");
    expect((createReq.params?.vfs as Record<string, unknown>).interception).toEqual({
      emit_events: true,
    });

    fake.pushResponse({
      jsonrpc: "2.0",
      id: createReq.id,
      result: { id: "vm-hook" },
    });
    await createPromise;

    fake.pushNotification("event", {
      file: {
        op: "write",
        path: "/workspace/file.txt",
        size: 1,
        mode: 0o644,
        uid: 1000,
        gid: 1000,
      },
    });

    await new Promise((resolve) => setTimeout(resolve, 30));
    expect(seen).toBe("write:/workspace/file.txt");

    fake.close();
    await client.close();
  });

  it("supports remove with current vm id", async () => {
    const fake = installFakeProcess();
    const client = new Client();

    const createPromise = client.create({ image: "alpine:latest" });
    const createReq = await fake.waitForRequest("create");
    fake.pushResponse({
      jsonrpc: "2.0",
      id: createReq.id,
      result: { id: "vm-remove" },
    });
    await createPromise;

    await client.remove();
    expect(mockedExecFile).toHaveBeenCalled();

    fake.close();
    await client.close();
  });

  it("creates volume via cli and parses output", async () => {
    const client = new Client({ binaryPath: "matchlock" });

    mockedExecFile.mockImplementationOnce((...args: unknown[]) => {
      const cb = args[args.length - 1];
      if (typeof cb === "function") {
        cb(null, '{"name":"cache","size":"16.0 MB","path":"/tmp/cache.ext4"}\n', "");
      }
      return {} as never;
    });

    await expect(client.volumeCreate("cache", 16)).resolves.toEqual({
      name: "cache",
      size: "16.0 MB",
      path: "/tmp/cache.ext4",
    });
    expect(mockedExecFile).toHaveBeenCalledWith(
      "matchlock",
      ["volume", "create", "cache", "--size", "16", "--json"],
      { encoding: "utf8" },
      expect.any(Function),
    );
  });

  it("lists volumes via cli and parses rows", async () => {
    const client = new Client({ binaryPath: "matchlock" });

    mockedExecFile.mockImplementationOnce((...args: unknown[]) => {
      const cb = args[args.length - 1];
      if (typeof cb === "function") {
        cb(
          null,
          '[{"name":"cache","size":"16.0 MB","path":"/tmp/cache.ext4"},{"name":"data","size":"32.0 MB","path":"/tmp/data.ext4"}]\n',
          "",
        );
      }
      return {} as never;
    });

    await expect(client.volumeList()).resolves.toEqual([
      { name: "cache", size: "16.0 MB", path: "/tmp/cache.ext4" },
      { name: "data", size: "32.0 MB", path: "/tmp/data.ext4" },
    ]);
    expect(mockedExecFile).toHaveBeenCalledWith(
      "matchlock",
      ["volume", "ls", "--json"],
      { encoding: "utf8" },
      expect.any(Function),
    );
  });

  it("removes volume via cli", async () => {
    const client = new Client({ binaryPath: "matchlock" });

    await client.volumeRemove("cache");

    expect(mockedExecFile).toHaveBeenCalledWith(
      "matchlock",
      ["volume", "rm", "cache"],
      { encoding: "utf8" },
      expect.any(Function),
    );
  });

  it("rejects volume operations with invalid inputs", async () => {
    const client = new Client({ binaryPath: "matchlock" });

    await expect(client.volumeCreate("  ", 16)).rejects.toThrow("volume name is required");
    await expect(client.volumeCreate("cache", 0)).rejects.toThrow("volume size must be > 0");
    await expect(client.volumeRemove("  ")).rejects.toThrow("volume name is required");
  });

  it("throws when process is not running", async () => {
    const fake = installFakeProcess();
    fake.close();

    const client = new Client();
    await expect(client.exec("echo hi")).rejects.toThrow("not running");
  });
});
