import { once } from "node:events";
import { execFile, spawn } from "node:child_process";
import type { ChildProcessWithoutNullStreams } from "node:child_process";
import { isIP } from "node:net";
import { promisify } from "node:util";
import { minimatch } from "minimatch";
import { cloneCreateOptions, Sandbox } from "./builder";
import { MatchlockError, RPCError } from "./errors";
import {
  VFS_HOOK_ACTION_ALLOW,
  VFS_HOOK_ACTION_BLOCK,
  VFS_HOOK_OP_READ,
  VFS_HOOK_OP_READDIR,
  VFS_HOOK_OP_WRITE,
  VFS_HOOK_PHASE_AFTER,
  VFS_HOOK_PHASE_BEFORE,
  type BinaryLike,
  type Config,
  type CreateOptions,
  type ExecOptions,
  type ExecResult,
  type ExecStreamOptions,
  type ExecStreamResult,
  type FileInfo,
  type HostIPMapping,
  type PortForward,
  type PortForwardBinding,
  type RequestOptions,
  type StreamWriter,
  type VFSActionRequest,
  type VFSHookAction,
  type VFSHookEvent,
  type VFSHookRule,
  type VFSMutateRequest,
  type VFSInterceptionConfig,
} from "./types";

type JSONValue = null | boolean | number | string | JSONObject | JSONArray;
type JSONObject = { [key: string]: JSONValue };
type JSONArray = JSONValue[];

interface JSONRPCRequest {
  jsonrpc: "2.0";
  method: string;
  params?: JSONObject;
  id: number;
}

interface JSONRPCError {
  code: number;
  message: string;
}

interface JSONRPCResponse {
  jsonrpc?: string;
  id?: number;
  result?: JSONValue;
  error?: JSONRPCError;
}

interface JSONRPCNotification {
  method?: string;
  params?: JSONObject;
}

interface PendingRequest {
  resolve: (result: JSONValue) => void;
  reject: (error: unknown) => void;
  onNotification?: (method: string, params: JSONObject) => void;
}

interface CompiledVFSHook {
  name: string;
  ops: Set<string>;
  path: string;
  timeoutMs: number;
  dangerous: boolean;
  callback: (client: Client, event: VFSHookEvent) => Promise<void>;
}

interface CompiledVFSMutateHook {
  name: string;
  ops: Set<string>;
  path: string;
  callback: (request: VFSMutateRequest) => Promise<BinaryLike | null | undefined>;
}

interface CompiledVFSActionHook {
  name: string;
  ops: Set<string>;
  path: string;
  callback: (request: VFSActionRequest) => Promise<VFSHookAction>;
}

interface WireVFSHookRule {
  name?: string;
  phase?: string;
  ops?: string[];
  path?: string;
  action?: string;
  timeout_ms?: number;
}

interface WireVFSInterceptionConfig {
  emit_events?: boolean;
  rules?: WireVFSHookRule[];
}

const DEFAULT_CPUS = 1;
const DEFAULT_MEMORY_MB = 512;
const DEFAULT_DISK_SIZE_MB = 5120;
const DEFAULT_TIMEOUT_SECONDS = 300;

const execFileAsync = promisify(execFile);

export function defaultConfig(config: Config = {}): Required<Config> {
  return {
    binaryPath: config.binaryPath ?? process.env.MATCHLOCK_BIN ?? "matchlock",
    useSudo: config.useSudo ?? false,
  };
}

function toError(value: unknown): Error {
  if (value instanceof Error) {
    return value;
  }
  return new Error(String(value));
}

function toBuffer(content: BinaryLike): Buffer {
  if (typeof content === "string") {
    return Buffer.from(content, "utf8");
  }
  if (Buffer.isBuffer(content)) {
    return content;
  }
  if (content instanceof Uint8Array) {
    return Buffer.from(content);
  }
  if (content instanceof ArrayBuffer) {
    return Buffer.from(content);
  }
  throw new MatchlockError("unsupported content type");
}

function lowerSet(values: string[] | undefined): Set<string> {
  return new Set((values ?? []).map((value) => value.toLowerCase()));
}

function asObject(value: JSONValue | undefined): JSONObject {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    return {};
  }
  return value as JSONObject;
}

function asNumber(value: JSONValue | undefined, fallback = 0): number {
  if (typeof value === "number" && Number.isFinite(value)) {
    return value;
  }
  return fallback;
}

function asString(value: JSONValue | undefined, fallback = ""): string {
  return typeof value === "string" ? value : fallback;
}

function getUID(): number {
  return typeof process.geteuid === "function" ? process.geteuid() : 0;
}

function getGID(): number {
  return typeof process.getegid === "function" ? process.getegid() : 0;
}

export class Client {
  private readonly config: Required<Config>;
  private process: ChildProcessWithoutNullStreams | undefined;
  private requestID = 0;
  private pending = new Map<number, PendingRequest>();
  private writeLock: Promise<void> = Promise.resolve();
  private readBuffer = "";
  private vmIDValue = "";
  private lastVMID = "";
  private closed = false;
  private closing = false;

  private vfsHooks: CompiledVFSHook[] = [];
  private vfsMutateHooks: CompiledVFSMutateHook[] = [];
  private vfsActionHooks: CompiledVFSActionHook[] = [];
  private vfsHookActive = false;

  constructor(config: Config = {}) {
    this.config = defaultConfig(config);
  }

  get vmId(): string {
    return this.vmIDValue;
  }

  async start(): Promise<void> {
    if (this.closed) {
      throw new MatchlockError("client is closed");
    }
    if (this.process && this.process.exitCode === null && !this.process.killed) {
      return;
    }

    const command = this.config.useSudo ? "sudo" : this.config.binaryPath;
    const args = this.config.useSudo
      ? [this.config.binaryPath, "rpc"]
      : ["rpc"];

    const child = spawn(command, args, {
      stdio: ["pipe", "pipe", "pipe"],
    });

    child.stderr.on("data", () => {
      // Drain stderr so the child cannot block on full pipes.
    });

    child.stdout.on("data", (chunk: Buffer) => {
      this.readBuffer += chunk.toString("utf8");
      this.processReadBuffer();
    });

    child.on("close", () => {
      this.handleProcessClosed();
    });

    child.on("error", (error) => {
      this.handleProcessClosed(error);
    });

    this.process = child;
  }

  async close(timeoutSeconds = 0): Promise<void> {
    if (this.closed || this.closing) {
      return;
    }
    this.closing = true;
    this.lastVMID = this.vmIDValue;
    this.setLocalVFSHooks([], [], []);

    try {
      if (!this.isRunning()) {
        return;
      }

      const effectiveTimeout = timeoutSeconds > 0 ? timeoutSeconds : 2;

      try {
        await this.sendRequest(
          "close",
          { timeout_seconds: effectiveTimeout },
          {
            timeoutMs: (effectiveTimeout + 5) * 1000,
          },
        );
      } catch {
        // Best effort shutdown.
      }

      if (this.process?.stdin.writable) {
        this.process.stdin.end();
      }

      await this.waitForProcessExit(effectiveTimeout * 1000);
    } finally {
      this.closed = true;
      this.closing = false;
    }
  }

  async remove(): Promise<void> {
    const vmID = this.vmIDValue || this.lastVMID;
    if (!vmID) {
      return;
    }

    try {
      await execFileAsync(this.config.binaryPath, ["rm", vmID]);
    } catch (error) {
      const err = toError(error);
      throw new MatchlockError(`matchlock rm ${vmID}: ${err.message}`);
    }
  }

  async launch(sandbox: Sandbox): Promise<string> {
    return this.create(sandbox.options());
  }

  async create(opts: CreateOptions = {}): Promise<string> {
    const options = cloneCreateOptions(opts);
    if (!options.image) {
      throw new MatchlockError("image is required (e.g., alpine:latest)");
    }
    if ((options.networkMtu ?? 0) < 0) {
      throw new MatchlockError("network mtu must be > 0");
    }
    for (const mapping of options.addHosts ?? []) {
      this.validateAddHost(mapping);
    }

    const [wireVFS, localHooks, localMutateHooks, localActionHooks] =
      this.compileVFSHooks(options.vfsInterception);

    const resources = {
      cpus: options.cpus || DEFAULT_CPUS,
      memory_mb: options.memoryMb || DEFAULT_MEMORY_MB,
      disk_size_mb: options.diskSizeMb || DEFAULT_DISK_SIZE_MB,
      timeout_seconds: options.timeoutSeconds || DEFAULT_TIMEOUT_SECONDS,
    };

    const params: JSONObject = {
      image: options.image,
      resources,
    };

    if (options.privileged) {
      params.privileged = true;
    }

    const network = this.buildCreateNetworkParams(options);
    if (network) {
      params.network = network;
    }

    if (
      (options.mounts && Object.keys(options.mounts).length > 0) ||
      options.workspace ||
      wireVFS
    ) {
      const vfs: JSONObject = {};
      if (options.mounts && Object.keys(options.mounts).length > 0) {
        const mounts: JSONObject = {};
        for (const [guestPath, config] of Object.entries(options.mounts)) {
          const mount: JSONObject = {
            type: config.type ?? "memory",
          };
          if (config.hostPath) {
            mount.host_path = config.hostPath;
          }
          if (config.readonly) {
            mount.readonly = true;
          }
          mounts[guestPath] = mount;
        }
        vfs.mounts = mounts;
      }
      if (options.workspace) {
        vfs.workspace = options.workspace;
      }
      if (wireVFS) {
        vfs.interception = wireVFS as unknown as JSONValue;
      }
      params.vfs = vfs;
    }

    if (options.env && Object.keys(options.env).length > 0) {
      params.env = options.env;
    }

    if (options.imageConfig) {
      const imageConfig: JSONObject = {};
      if (options.imageConfig.user) {
        imageConfig.user = options.imageConfig.user;
      }
      if (options.imageConfig.workingDir) {
        imageConfig.working_dir = options.imageConfig.workingDir;
      }
      if (options.imageConfig.entrypoint) {
        imageConfig.entrypoint = [...options.imageConfig.entrypoint];
      }
      if (options.imageConfig.cmd) {
        imageConfig.cmd = [...options.imageConfig.cmd];
      }
      if (options.imageConfig.env) {
        imageConfig.env = { ...options.imageConfig.env };
      }
      params.image_config = imageConfig;
    }

    const result = asObject(await this.sendRequest("create", params));
    const id = asString(result.id);
    if (!id) {
      throw new MatchlockError("invalid create response: missing id");
    }

    this.vmIDValue = id;
    this.setLocalVFSHooks(localHooks, localMutateHooks, localActionHooks);

    if ((options.portForwards ?? []).length > 0) {
      await this.portForwardMappings(
        options.portForwardAddresses,
        options.portForwards ?? [],
      );
    }

    return this.vmIDValue;
  }

  private resolveCreateBlockPrivateIPs(opts: CreateOptions): {
    value: boolean;
    hasOverride: boolean;
  } {
    if (opts.blockPrivateIPsSet) {
      return { value: !!opts.blockPrivateIPs, hasOverride: true };
    }
    if (opts.blockPrivateIPs) {
      return { value: true, hasOverride: true };
    }
    return { value: false, hasOverride: false };
  }

  private buildCreateNetworkParams(opts: CreateOptions): JSONObject | undefined {
    const hasAllowedHosts = (opts.allowedHosts?.length ?? 0) > 0;
    const hasAddHosts = (opts.addHosts?.length ?? 0) > 0;
    const hasSecrets = (opts.secrets?.length ?? 0) > 0;
    const hasDNSServers = (opts.dnsServers?.length ?? 0) > 0;
    const hasHostname = (opts.hostname?.length ?? 0) > 0;
    const hasMTU = (opts.networkMtu ?? 0) > 0;

    const blockPrivate = this.resolveCreateBlockPrivateIPs(opts);

    const includeNetwork =
      hasAllowedHosts ||
      hasAddHosts ||
      hasSecrets ||
      hasDNSServers ||
      hasHostname ||
      hasMTU ||
      blockPrivate.hasOverride;

    if (!includeNetwork) {
      return undefined;
    }

    const network: JSONObject = {
      allowed_hosts: opts.allowedHosts ?? [],
      block_private_ips: blockPrivate.hasOverride ? blockPrivate.value : true,
    };

    if (hasAddHosts) {
      network.add_hosts = (opts.addHosts ?? []).map((mapping) => ({
        host: mapping.host,
        ip: mapping.ip,
      }));
    }

    if (hasSecrets) {
      const secrets: JSONObject = {};
      for (const secret of opts.secrets ?? []) {
        secrets[secret.name] = {
          value: secret.value,
          hosts: secret.hosts ?? [],
        };
      }
      network.secrets = secrets;
    }

    if (hasDNSServers) {
      network.dns_servers = opts.dnsServers ?? [];
    }

    if (hasHostname) {
      network.hostname = opts.hostname ?? "";
    }

    if (hasMTU) {
      network.mtu = opts.networkMtu ?? 0;
    }

    return network;
  }

  async exec(command: string, options: ExecOptions = {}): Promise<ExecResult> {
    return this.execWithDir(command, options.workingDir ?? "", options);
  }

  async execWithDir(
    command: string,
    workingDir = "",
    options: RequestOptions = {},
  ): Promise<ExecResult> {
    const params: JSONObject = { command };
    if (workingDir) {
      params.working_dir = workingDir;
    }

    const result = asObject(await this.sendRequest("exec", params, options));

    return {
      exitCode: asNumber(result.exit_code),
      stdout: Buffer.from(asString(result.stdout), "base64").toString("utf8"),
      stderr: Buffer.from(asString(result.stderr), "base64").toString("utf8"),
      durationMs: asNumber(result.duration_ms),
    };
  }

  async execStream(
    command: string,
    options: ExecStreamOptions = {},
  ): Promise<ExecStreamResult> {
    return this.execStreamWithDir(
      command,
      options.workingDir ?? "",
      options.stdout,
      options.stderr,
      options,
    );
  }

  async execStreamWithDir(
    command: string,
    workingDir = "",
    stdout?: StreamWriter,
    stderr?: StreamWriter,
    options: RequestOptions = {},
  ): Promise<ExecStreamResult> {
    const params: JSONObject = { command };
    if (workingDir) {
      params.working_dir = workingDir;
    }

    const onNotification = (method: string, payload: JSONObject): void => {
      const data = asString(payload.data);
      if (!data) {
        return;
      }
      let decoded: Buffer;
      try {
        decoded = Buffer.from(data, "base64");
      } catch {
        return;
      }

      if (method === "exec_stream.stdout") {
        this.writeStreamChunk(stdout, decoded);
      } else if (method === "exec_stream.stderr") {
        this.writeStreamChunk(stderr, decoded);
      }
    };

    const result = asObject(
      await this.sendRequest("exec_stream", params, options, onNotification),
    );

    return {
      exitCode: asNumber(result.exit_code),
      durationMs: asNumber(result.duration_ms),
    };
  }

  async writeFile(
    path: string,
    content: BinaryLike,
    options: RequestOptions = {},
  ): Promise<void> {
    await this.writeFileMode(path, content, 0o644, options);
  }

  async writeFileMode(
    path: string,
    content: BinaryLike,
    mode: number,
    options: RequestOptions = {},
  ): Promise<void> {
    const original = toBuffer(content);

    await this.applyLocalActionHooks(VFS_HOOK_OP_WRITE, path, original.length, mode);
    const mutated = await this.applyLocalWriteMutations(path, original, mode);

    await this.sendRequest(
      "write_file",
      {
        path,
        content: mutated.toString("base64"),
        mode,
      },
      options,
    );
  }

  async readFile(path: string, options: RequestOptions = {}): Promise<Buffer> {
    await this.applyLocalActionHooks(VFS_HOOK_OP_READ, path, 0, 0);

    const result = asObject(
      await this.sendRequest("read_file", { path }, options),
    );
    return Buffer.from(asString(result.content), "base64");
  }

  async listFiles(path: string, options: RequestOptions = {}): Promise<FileInfo[]> {
    await this.applyLocalActionHooks(VFS_HOOK_OP_READDIR, path, 0, 0);

    const result = asObject(
      await this.sendRequest("list_files", { path }, options),
    );
    const files = Array.isArray(result.files) ? result.files : [];

    return files.map((entry) => {
      const file = asObject(entry as JSONValue);
      return {
        name: asString(file.name),
        size: asNumber(file.size),
        mode: asNumber(file.mode),
        isDir: Boolean(file.is_dir),
      };
    });
  }

  async portForward(...specs: string[]): Promise<PortForwardBinding[]> {
    return this.portForwardWithAddresses(undefined, ...specs);
  }

  async portForwardWithAddresses(
    addresses: string[] | undefined,
    ...specs: string[]
  ): Promise<PortForwardBinding[]> {
    const forwards = this.parsePortForwards(specs);
    return this.portForwardMappings(addresses, forwards);
  }

  private async portForwardMappings(
    addresses: string[] | undefined,
    forwards: PortForward[],
  ): Promise<PortForwardBinding[]> {
    if (forwards.length === 0) {
      return [];
    }

    const wireForwards = forwards.map((forward) => ({
      local_port: forward.localPort,
      remote_port: forward.remotePort,
    }));

    const result = asObject(
      await this.sendRequest("port_forward", {
        forwards: wireForwards as unknown as JSONValue,
        addresses: addresses && addresses.length > 0 ? [...addresses] : ["127.0.0.1"],
      }),
    );

    const bindings = Array.isArray(result.bindings) ? result.bindings : [];
    return bindings.map((entry) => {
      const binding = asObject(entry as JSONValue);
      return {
        address: asString(binding.address),
        localPort: asNumber(binding.local_port),
        remotePort: asNumber(binding.remote_port),
      };
    });
  }

  private parsePortForwards(specs: string[]): PortForward[] {
    return specs.map((spec) => this.parsePortForward(spec));
  }

  private parsePortForward(spec: string): PortForward {
    const trimmed = spec.trim();
    if (!trimmed) {
      throw new MatchlockError('invalid port-forward spec: empty spec');
    }

    const parts = trimmed.split(":");
    if (parts.length === 1) {
      const remotePort = this.parsePort(parts[0], "remote");
      return { localPort: remotePort, remotePort };
    }
    if (parts.length === 2) {
      const localPort = this.parsePort(parts[0], "local");
      const remotePort = this.parsePort(parts[1], "remote");
      return { localPort, remotePort };
    }

    throw new MatchlockError(
      `invalid port-forward spec: ${JSON.stringify(trimmed)} (expected [LOCAL_PORT:]REMOTE_PORT)`,
    );
  }

  private parsePort(raw: string, role: "local" | "remote"): number {
    const value = raw.trim();
    if (!value) {
      throw new MatchlockError(`invalid port-forward spec: empty ${role} port`);
    }

    const port = Number.parseInt(value, 10);
    if (!Number.isFinite(port)) {
      throw new MatchlockError(`invalid port value ${JSON.stringify(value)}`);
    }

    if (port < 1 || port > 65535) {
      throw new MatchlockError(`invalid port value ${port}: must be in range 1-65535`);
    }

    return port;
  }

  private async sendRequest(
    method: string,
    params?: JSONObject,
    options: RequestOptions = {},
    onNotification?: (method: string, params: JSONObject) => void,
  ): Promise<JSONValue> {
    if (this.closed) {
      throw new MatchlockError("client is closed");
    }

    await this.start();
    if (!this.isRunning()) {
      throw new MatchlockError("Matchlock process not running");
    }

    const id = ++this.requestID;

    const request: JSONRPCRequest = {
      jsonrpc: "2.0",
      method,
      id,
    };
    if (params && Object.keys(params).length > 0) {
      request.params = params;
    }

    let resolvePending: (value: JSONValue) => void = () => {};
    let rejectPending: (reason: unknown) => void = () => {};
    const resultPromise = new Promise<JSONValue>((resolve, reject) => {
      resolvePending = resolve;
      rejectPending = reject;
    });

    this.pending.set(id, {
      resolve: resolvePending,
      reject: rejectPending,
      onNotification,
    });

    let timeoutHandle: NodeJS.Timeout | undefined;
    const onAbort = (): void => {
      this.sendCancelRequest(id);
      const reason = options.signal?.reason;
      if (reason instanceof Error) {
        rejectPending(reason);
      } else {
        rejectPending(new MatchlockError(`request ${method} was aborted`));
      }
    };

    try {
      if (options.signal?.aborted) {
        onAbort();
      } else if (options.signal) {
        options.signal.addEventListener("abort", onAbort, { once: true });
      }

      if ((options.timeoutMs ?? 0) > 0) {
        timeoutHandle = setTimeout(() => {
          this.sendCancelRequest(id);
          rejectPending(
            new MatchlockError(
              `request ${method} (id=${id}) timed out after ${options.timeoutMs}ms`,
            ),
          );
        }, options.timeoutMs);
      }

      await this.enqueueWrite(`${JSON.stringify(request)}\n`);
      return await resultPromise;
    } finally {
      this.pending.delete(id);
      if (timeoutHandle) {
        clearTimeout(timeoutHandle);
      }
      if (options.signal) {
        options.signal.removeEventListener("abort", onAbort);
      }
    }
  }

  private sendCancelRequest(targetID: number): void {
    const request: JSONRPCRequest = {
      jsonrpc: "2.0",
      method: "cancel",
      params: { id: targetID },
      id: ++this.requestID,
    };
    void this.enqueueWrite(`${JSON.stringify(request)}\n`).catch(() => {
      // Ignore cancellation write errors.
    });
  }

  private async enqueueWrite(line: string): Promise<void> {
    this.writeLock = this.writeLock
      .catch(() => {
        // Keep queue alive.
      })
      .then(async () => {
        if (!this.process || !this.process.stdin.writable) {
          throw new MatchlockError("Matchlock process not running");
        }

        await new Promise<void>((resolve, reject) => {
          this.process?.stdin.write(line, (error) => {
            if (error) {
              reject(error);
              return;
            }
            resolve();
          });
        });
      });

    return this.writeLock;
  }

  private processReadBuffer(): void {
    for (;;) {
      const newlineIndex = this.readBuffer.indexOf("\n");
      if (newlineIndex === -1) {
        break;
      }

      const line = this.readBuffer.slice(0, newlineIndex).trim();
      this.readBuffer = this.readBuffer.slice(newlineIndex + 1);
      if (!line) {
        continue;
      }

      this.handleMessage(line);
    }
  }

  private handleMessage(line: string): void {
    let parsed: JSONRPCResponse & JSONRPCNotification;
    try {
      parsed = JSON.parse(line) as JSONRPCResponse & JSONRPCNotification;
    } catch {
      return;
    }

    if (typeof parsed.id !== "number") {
      this.handleNotification(parsed);
      return;
    }

    const pending = this.pending.get(parsed.id);
    if (!pending) {
      return;
    }

    if (parsed.error) {
      pending.reject(new RPCError(parsed.error.code, parsed.error.message));
      return;
    }

    pending.resolve(parsed.result ?? null);
  }

  private handleNotification(msg: JSONRPCNotification): void {
    const method = msg.method;
    const params = msg.params ?? {};

    if (method === "exec_stream.stdout" || method === "exec_stream.stderr") {
      const reqID = asNumber(params.id, -1);
      if (reqID < 0) {
        return;
      }
      const pending = this.pending.get(reqID);
      if (pending?.onNotification) {
        pending.onNotification(method, params);
      }
      return;
    }

    if (method === "event") {
      this.handleVFSFileEventNotification(params);
    }
  }

  private handleVFSFileEventNotification(params: JSONObject): void {
    const file = asObject(params.file);
    if (Object.keys(file).length === 0) {
      return;
    }

    const op = asString(file.op).toLowerCase();
    if (!op) {
      return;
    }

    const event: VFSHookEvent = {
      op,
      path: asString(file.path),
      size: asNumber(file.size),
      mode: asNumber(file.mode),
      uid: asNumber(file.uid),
      gid: asNumber(file.gid),
    };

    this.handleVFSFileEvent(event);
  }

  private handleVFSFileEvent(event: VFSHookEvent): void {
    const hooks = [...this.vfsHooks];
    if (hooks.length === 0) {
      return;
    }

    const safeHooks: CompiledVFSHook[] = [];
    for (const hook of hooks) {
      if (!this.matchesVFSHook(hook.ops, hook.path, event.op, event.path)) {
        continue;
      }

      if (hook.dangerous) {
        void this.runSingleVFSHook(hook, event);
        continue;
      }
      safeHooks.push(hook);
    }

    if (safeHooks.length === 0) {
      return;
    }
    if (this.vfsHookActive) {
      return;
    }

    void this.runVFSSafeHooksForEvent(safeHooks, event);
  }

  private async runVFSSafeHooksForEvent(
    hooks: CompiledVFSHook[],
    event: VFSHookEvent,
  ): Promise<void> {
    if (this.vfsHookActive) {
      return;
    }
    this.vfsHookActive = true;

    try {
      for (const hook of hooks) {
        await this.runSingleVFSHook(hook, event);
      }
    } finally {
      this.vfsHookActive = false;
    }
  }

  private async runSingleVFSHook(
    hook: CompiledVFSHook,
    event: VFSHookEvent,
  ): Promise<void> {
    try {
      const run = hook.callback(this, event);
      if (hook.timeoutMs > 0) {
        await Promise.race([
          run,
          new Promise<void>((resolve) => {
            setTimeout(resolve, hook.timeoutMs);
          }),
        ]);
      } else {
        await run;
      }
    } catch {
      // Hooks are intentionally best effort.
    }
  }

  private matchesVFSHook(
    ops: Set<string>,
    path: string,
    op: string,
    actualPath: string,
  ): boolean {
    if (ops.size > 0 && !ops.has(op.toLowerCase())) {
      return false;
    }
    if (!path) {
      return true;
    }
    try {
      return minimatch(actualPath, path, { dot: true });
    } catch {
      return false;
    }
  }

  private async applyLocalWriteMutations(
    path: string,
    content: Buffer,
    mode: number,
  ): Promise<Buffer> {
    const hooks = [...this.vfsMutateHooks];
    if (hooks.length === 0) {
      return content;
    }

    let current = content;
    for (const hook of hooks) {
      if (!this.matchesVFSHook(hook.ops, hook.path, VFS_HOOK_OP_WRITE, path)) {
        continue;
      }
      const request: VFSMutateRequest = {
        path,
        size: current.length,
        mode,
        uid: getUID(),
        gid: getGID(),
      };
      const mutated = await hook.callback(request);
      if (mutated === null || mutated === undefined) {
        continue;
      }
      if (typeof mutated === "string") {
        current = Buffer.from(mutated, "utf8");
        continue;
      }
      if (
        Buffer.isBuffer(mutated) ||
        mutated instanceof Uint8Array ||
        mutated instanceof ArrayBuffer
      ) {
        current = toBuffer(mutated);
        continue;
      }
      throw new MatchlockError(
        `invalid mutate_hook return type for ${JSON.stringify(hook.name)}: expected bytes|string|undefined`,
      );
    }

    return current;
  }

  private async applyLocalActionHooks(
    op: string,
    path: string,
    size: number,
    mode: number,
  ): Promise<void> {
    const hooks = [...this.vfsActionHooks];
    if (hooks.length === 0) {
      return;
    }

    const request: VFSActionRequest = {
      op,
      path,
      size,
      mode,
      uid: getUID(),
      gid: getGID(),
    };

    for (const hook of hooks) {
      if (!this.matchesVFSHook(hook.ops, hook.path, op, path)) {
        continue;
      }

      const decision = String(await hook.callback(request)).trim().toLowerCase();
      if (decision === "" || decision === VFS_HOOK_ACTION_ALLOW) {
        continue;
      }
      if (decision === VFS_HOOK_ACTION_BLOCK) {
        throw new MatchlockError(
          `vfs action hook blocked operation: op=${op} path=${path} hook=${JSON.stringify(hook.name)}`,
        );
      }
      throw new MatchlockError(
        `invalid action_hook return value for ${JSON.stringify(hook.name)}: expected ${JSON.stringify(VFS_HOOK_ACTION_ALLOW)}|${JSON.stringify(VFS_HOOK_ACTION_BLOCK)}, got ${JSON.stringify(decision)}`,
      );
    }
  }

  private compileVFSHooks(cfg: VFSInterceptionConfig | undefined): [
    WireVFSInterceptionConfig | undefined,
    CompiledVFSHook[],
    CompiledVFSMutateHook[],
    CompiledVFSActionHook[],
  ] {
    if (!cfg) {
      return [undefined, [], [], []];
    }

    const wire: WireVFSInterceptionConfig = {
      emit_events: cfg.emitEvents,
      rules: [],
    };

    const localHooks: CompiledVFSHook[] = [];
    const localMutateHooks: CompiledVFSMutateHook[] = [];
    const localActionHooks: CompiledVFSActionHook[] = [];

    for (const rule of cfg.rules ?? []) {
      const callbackCount = Number(Boolean(rule.hook)) + Number(Boolean(rule.dangerousHook)) + Number(Boolean(rule.mutateHook)) + Number(Boolean(rule.actionHook));

      if (callbackCount > 1) {
        throw new MatchlockError(
          `invalid vfs hook ${JSON.stringify(rule.name ?? "")}: cannot set more than one callback hook`,
        );
      }

      if (
        !rule.hook &&
        !rule.dangerousHook &&
        !rule.mutateHook &&
        !rule.actionHook
      ) {
        const action = String(rule.action ?? "allow").trim().toLowerCase();
        if (action === "mutate_write") {
          throw new MatchlockError(
            `invalid vfs hook ${JSON.stringify(rule.name ?? "")}: mutate_write requires mutate_hook callback`,
          );
        }

        wire.rules?.push(this.ruleToWire(rule, action));
        continue;
      }

      if (rule.hook) {
        this.validateLocalAfterRule(rule, "callback hooks");
        localHooks.push({
          name: rule.name ?? "",
          ops: lowerSet(rule.ops),
          path: rule.path ?? "",
          timeoutMs: rule.timeoutMs ?? 0,
          dangerous: false,
          callback: async (_client, event): Promise<void> => {
            await rule.hook?.(event);
          },
        });
        continue;
      }

      if (rule.dangerousHook) {
        this.validateLocalAfterRule(rule, "dangerous_hook");
        localHooks.push({
          name: rule.name ?? "",
          ops: lowerSet(rule.ops),
          path: rule.path ?? "",
          timeoutMs: rule.timeoutMs ?? 0,
          dangerous: true,
          callback: async (client, event): Promise<void> => {
            await rule.dangerousHook?.(client, event);
          },
        });
        continue;
      }

      if (rule.actionHook) {
        const action = String(rule.action ?? "").trim().toLowerCase();
        if (action && action !== VFS_HOOK_ACTION_ALLOW) {
          throw new MatchlockError(
            `invalid vfs hook ${JSON.stringify(rule.name ?? "")}: action_hook cannot set action=${JSON.stringify(rule.action)}`,
          );
        }
        if (
          rule.phase &&
          String(rule.phase).toLowerCase() !== VFS_HOOK_PHASE_BEFORE
        ) {
          throw new MatchlockError(
            `invalid vfs hook ${JSON.stringify(rule.name ?? "")}: action_hook must use phase=before`,
          );
        }

        localActionHooks.push({
          name: rule.name ?? "",
          ops: lowerSet(rule.ops),
          path: rule.path ?? "",
          callback: async (request): Promise<VFSHookAction> =>
            (await rule.actionHook?.(request)) ?? VFS_HOOK_ACTION_ALLOW,
        });
        continue;
      }

      const action = String(rule.action ?? "").trim().toLowerCase();
      if (action && action !== VFS_HOOK_ACTION_ALLOW) {
        throw new MatchlockError(
          `invalid vfs hook ${JSON.stringify(rule.name ?? "")}: mutate_hook cannot set action=${JSON.stringify(rule.action)}`,
        );
      }
      if (rule.phase && String(rule.phase).toLowerCase() !== VFS_HOOK_PHASE_BEFORE) {
        throw new MatchlockError(
          `invalid vfs hook ${JSON.stringify(rule.name ?? "")}: mutate_hook must use phase=before`,
        );
      }

      localMutateHooks.push({
        name: rule.name ?? "",
        ops: lowerSet(rule.ops),
        path: rule.path ?? "",
        callback: async (request): Promise<BinaryLike | null | undefined> =>
          rule.mutateHook?.(request),
      });
    }

    if (localHooks.length > 0) {
      wire.emit_events = true;
    }

    if ((wire.rules?.length ?? 0) === 0) {
      wire.rules = undefined;
    }

    if (!wire.emit_events && !wire.rules) {
      return [undefined, localHooks, localMutateHooks, localActionHooks];
    }

    return [wire, localHooks, localMutateHooks, localActionHooks];
  }

  private validateLocalAfterRule(rule: VFSHookRule, label: string): void {
    const action = String(rule.action ?? "").trim().toLowerCase();
    if (action && action !== VFS_HOOK_ACTION_ALLOW) {
      throw new MatchlockError(
        `invalid vfs hook ${JSON.stringify(rule.name ?? "")}: ${label} cannot set action=${JSON.stringify(rule.action)}`,
      );
    }
    if (String(rule.phase ?? "").toLowerCase() !== VFS_HOOK_PHASE_AFTER) {
      throw new MatchlockError(
        `invalid vfs hook ${JSON.stringify(rule.name ?? "")}: ${label} must use phase=after`,
      );
    }
  }

  private ruleToWire(rule: VFSHookRule, normalizedAction: string): WireVFSHookRule {
    const wire: WireVFSHookRule = {
      action: normalizedAction,
    };

    if (rule.name) {
      wire.name = rule.name;
    }
    if (rule.phase) {
      wire.phase = rule.phase;
    }
    if (rule.ops && rule.ops.length > 0) {
      wire.ops = [...rule.ops];
    }
    if (rule.path) {
      wire.path = rule.path;
    }
    if ((rule.timeoutMs ?? 0) > 0) {
      wire.timeout_ms = rule.timeoutMs;
    }

    return wire;
  }

  private setLocalVFSHooks(
    hooks: CompiledVFSHook[],
    mutateHooks: CompiledVFSMutateHook[],
    actionHooks: CompiledVFSActionHook[],
  ): void {
    this.vfsHooks = [...hooks];
    this.vfsMutateHooks = [...mutateHooks];
    this.vfsActionHooks = [...actionHooks];
    this.vfsHookActive = false;
  }

  private validateAddHost(mapping: HostIPMapping): void {
    if (!mapping.host || !mapping.host.trim()) {
      throw new MatchlockError("invalid add-host mapping: empty host");
    }
    if (/\s/.test(mapping.host)) {
      throw new MatchlockError(
        `invalid add-host mapping: host ${JSON.stringify(mapping.host)} contains whitespace`,
      );
    }
    if (mapping.host.includes(":")) {
      throw new MatchlockError(
        `invalid add-host mapping: host ${JSON.stringify(mapping.host)} must not contain ':'`,
      );
    }
    if (!mapping.ip || !mapping.ip.trim()) {
      throw new MatchlockError("invalid add-host mapping: empty ip");
    }
    if (!this.isValidIP(mapping.ip.trim())) {
      throw new MatchlockError(
        `invalid add-host mapping: invalid ip ${JSON.stringify(mapping.ip)}`,
      );
    }
  }

  private isValidIP(ip: string): boolean {
    return isIP(ip) !== 0;
  }

  private writeStreamChunk(writer: StreamWriter | undefined, chunk: Buffer): void {
    if (!writer) {
      return;
    }

    if (typeof writer === "function") {
      void writer(chunk);
      return;
    }

    writer.write(chunk);
  }

  private isRunning(): boolean {
    return !!this.process && this.process.exitCode === null && !this.process.killed;
  }

  private async waitForProcessExit(timeoutMs: number): Promise<void> {
    const proc = this.process;
    if (!proc) {
      return;
    }
    if (proc.exitCode !== null) {
      return;
    }

    let timer: NodeJS.Timeout | undefined;
    try {
      await Promise.race([
        once(proc, "exit").then(() => undefined),
        new Promise<void>((resolve) => {
          timer = setTimeout(resolve, timeoutMs);
        }),
      ]);
    } finally {
      if (timer) {
        clearTimeout(timer);
      }
    }

    if (proc.exitCode === null && !proc.killed) {
      proc.kill("SIGKILL");
      await once(proc, "exit").catch(() => undefined);
    }
  }

  private handleProcessClosed(error?: Error): void {
    const pending = [...this.pending.values()];
    this.pending.clear();

    const message = error
      ? `Matchlock process closed unexpectedly: ${error.message}`
      : "Matchlock process closed unexpectedly";
    for (const request of pending) {
      request.reject(new MatchlockError(message));
    }

    this.process = undefined;
  }
}
