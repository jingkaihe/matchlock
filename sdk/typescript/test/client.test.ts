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

  it("throws when process is not running", async () => {
    const fake = installFakeProcess();
    fake.close();

    const client = new Client();
    await expect(client.exec("echo hi")).rejects.toThrow("not running");
  });
});
