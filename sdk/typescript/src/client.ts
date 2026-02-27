import { cloneCreateOptions, Sandbox } from "./builder";
import { MatchlockError } from "./errors";
import {
  VFS_HOOK_OP_READ,
  VFS_HOOK_OP_READDIR,
  VFS_HOOK_OP_WRITE,
  type BinaryLike,
  type Config,
  type CreateOptions,
  type ExecInteractiveOptions,
  type ExecInteractiveResult,
  type ExecOptions,
  type ExecPipeOptions,
  type ExecPipeResult,
  type ExecResult,
  type ExecStreamOptions,
  type ExecStreamResult,
  type FileInfo,
  type PortForward,
  type PortForwardBinding,
  type RequestOptions,
  type StreamReader,
  type StreamWriter,
  type VolumeInfo,
} from "./types";
import { buildCreateParams, validateCreateOptions } from "./client/create";
import { ExecAPI } from "./client/exec";
import { NetworkHooks } from "./client/network-hooks";
import { parsePortForwards } from "./client/port-forward";
import { RPCTransport } from "./client/transport";
import { asNumber, asObject, asString, defaultConfig, toBuffer, toError, validateAddHost } from "./client/utils";
import { VFSHooks } from "./client/vfs-hooks";
import { volumeCreate, volumeList, volumeRemove } from "./client/volumes";
import type { JSONObject, JSONValue } from "./client/wire";

export { defaultConfig } from "./client/utils";

export class Client {
  private readonly config: Required<Config>;
  private readonly transport: RPCTransport;
  private readonly execAPI: ExecAPI;
  private readonly vfsHooks = new VFSHooks();
  private readonly networkHooks = new NetworkHooks();

  private vmIDValue = "";
  private lastVMID = "";
  private closed = false;
  private closing = false;

  constructor(config: Config = {}) {
    this.config = defaultConfig(config);
    this.transport = new RPCTransport(
      this.config,
      (method, params) => {
        this.handleNotification(method, params);
      },
      () => {
        void this.networkHooks.stop();
      },
    );
    this.execAPI = new ExecAPI(
      (method, params, options, onNotification) =>
        this.sendRequest(method, params, options, onNotification),
      (method, params) => this.transport.sendFireAndForget(method, params),
    );
  }

  get vmId(): string {
    return this.vmIDValue;
  }

  async start(): Promise<void> {
    if (this.closed) {
      throw new MatchlockError("client is closed");
    }
    await this.transport.start();
  }

  async close(timeoutSeconds = 0): Promise<void> {
    if (this.closed || this.closing) {
      return;
    }
    this.closing = true;
    this.lastVMID = this.vmIDValue;
    this.vfsHooks.clearLocalHooks();
    await this.networkHooks.stop();

    try {
      if (!this.transport.isRunning()) {
        return;
      }
      await this.transport.close(timeoutSeconds);
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
      await this.transport.execCLI(["rm", vmID]);
    } catch (error) {
      const err = toError(error);
      throw new MatchlockError(`matchlock rm ${vmID}: ${err.message}`);
    }
  }

  async volumeCreate(name: string, sizeMb = 10240): Promise<VolumeInfo> {
    return volumeCreate((args) => this.transport.execCLI(args), name, sizeMb);
  }

  async volumeList(): Promise<VolumeInfo[]> {
    return volumeList((args) => this.transport.execCLI(args));
  }

  async volumeRemove(name: string): Promise<void> {
    await volumeRemove((args) => this.transport.execCLI(args), name);
  }

  async launch(sandbox: Sandbox): Promise<string> {
    const options = cloneCreateOptions(sandbox.options());
    options.launchEntrypoint = true;
    return this.create(options);
  }

  async create(opts: CreateOptions = {}): Promise<string> {
    const options = cloneCreateOptions(opts);
    validateCreateOptions(options);

    for (const mapping of options.addHosts ?? []) {
      validateAddHost(mapping);
    }

    const [wireVFS, localHooks, localMutateHooks, localActionHooks] =
      this.vfsHooks.compile(options.vfsInterception);
    let [wireNetworkInterception, localNetworkHooks] = this.networkHooks.compile(
      options.networkInterception,
    );

    await this.networkHooks.stop();
    let startedNetworkHookServer = false;
    if (localNetworkHooks.size > 0) {
      const callbackSocket = await this.networkHooks.start(localNetworkHooks);
      if (!wireNetworkInterception) {
        wireNetworkInterception = {};
      }
      wireNetworkInterception.callback_socket = callbackSocket;
      startedNetworkHookServer = true;
    }

    const params = buildCreateParams(options, wireVFS, wireNetworkInterception);

    let result: JSONObject;
    try {
      result = asObject(await this.sendRequest("create", params));
    } catch (error) {
      if (startedNetworkHookServer) {
        await this.networkHooks.stop();
      }
      throw error;
    }

    const id = asString(result.id);
    if (!id) {
      if (startedNetworkHookServer) {
        await this.networkHooks.stop();
      }
      throw new MatchlockError("invalid create response: missing id");
    }

    this.vmIDValue = id;
    this.vfsHooks.setLocalHooks(localHooks, localMutateHooks, localActionHooks);

    if ((options.portForwards ?? []).length > 0) {
      await this.portForwardMappings(
        options.portForwardAddresses,
        options.portForwards ?? [],
      );
    }

    return this.vmIDValue;
  }

  async exec(command: string, options: ExecOptions = {}): Promise<ExecResult> {
    return this.execAPI.exec(command, options);
  }

  async execWithDir(
    command: string,
    workingDir = "",
    options: RequestOptions = {},
  ): Promise<ExecResult> {
    return this.execAPI.execWithDir(command, workingDir, options);
  }

  async execStream(
    command: string,
    options: ExecStreamOptions = {},
  ): Promise<ExecStreamResult> {
    return this.execAPI.execStream(command, options);
  }

  async execStreamWithDir(
    command: string,
    workingDir = "",
    stdout?: StreamWriter,
    stderr?: StreamWriter,
    options: RequestOptions = {},
  ): Promise<ExecStreamResult> {
    return this.execAPI.execStreamWithDir(command, workingDir, stdout, stderr, options);
  }

  async execPipe(
    command: string,
    options: ExecPipeOptions = {},
  ): Promise<ExecPipeResult> {
    return this.execAPI.execPipe(command, options);
  }

  async execPipeWithDir(
    command: string,
    workingDir = "",
    stdin?: StreamReader,
    stdout?: StreamWriter,
    stderr?: StreamWriter,
    options: RequestOptions = {},
  ): Promise<ExecPipeResult> {
    return this.execAPI.execPipeWithDir(
      command,
      workingDir,
      stdin,
      stdout,
      stderr,
      options,
    );
  }

  async execInteractive(
    command: string,
    options: ExecInteractiveOptions = {},
  ): Promise<ExecInteractiveResult> {
    return this.execAPI.execInteractive(command, options);
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

    await this.vfsHooks.applyLocalActionHooks(
      VFS_HOOK_OP_WRITE,
      path,
      original.length,
      mode,
    );
    const mutated = await this.vfsHooks.applyLocalWriteMutations(path, original, mode);

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
    await this.vfsHooks.applyLocalActionHooks(VFS_HOOK_OP_READ, path, 0, 0);

    const result = asObject(
      await this.sendRequest("read_file", { path }, options),
    );
    return Buffer.from(asString(result.content), "base64");
  }

  async listFiles(path: string, options: RequestOptions = {}): Promise<FileInfo[]> {
    await this.vfsHooks.applyLocalActionHooks(VFS_HOOK_OP_READDIR, path, 0, 0);

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
    const forwards = parsePortForwards(specs);
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

  private async sendRequest(
    method: string,
    params?: JSONObject,
    options: RequestOptions = {},
    onNotification?: (method: string, params: JSONObject) => void,
  ): Promise<JSONValue> {
    if (this.closed) {
      throw new MatchlockError("client is closed");
    }

    return this.transport.sendRequest(method, params, options, onNotification);
  }

  private handleNotification(method: string, params: JSONObject): void {
    if (method === "event") {
      this.vfsHooks.handleFileEventNotification(params, this);
    }
  }
}
