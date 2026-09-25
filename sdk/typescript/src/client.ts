import { cloneCreateOptions, Sandbox } from "./builder";
import { MatchlockError } from "./errors";
import {
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
  type LogStreamOptions,
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
import {
  asNumber,
  asObject,
  asString,
  defaultConfig,
  toBuffer,
  toError,
  validateAddHost,
} from "./client/utils";
import { volumeCreate, volumeList, volumeRemove } from "./client/volumes";
import type { JSONObject, JSONValue } from "./client/wire";

export { defaultConfig } from "./client/utils";

export class Client {
  private readonly config: Required<Config>;
  private readonly transport: RPCTransport;
  private readonly execAPI: ExecAPI;
  private readonly networkHooks = new NetworkHooks();

  private vmIDValue = "";
  private lastVMID = "";
  private closed = false;
  private closing = false;

  constructor(config: Config = {}) {
    this.config = defaultConfig(config);
    this.transport = new RPCTransport(this.config, () => {
      void this.networkHooks.stop();
    });
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

    let [wireNetworkInterception, localNetworkHooks] =
      this.networkHooks.compile(options.networkInterception);

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

    const params = buildCreateParams(options, wireNetworkInterception);

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
    return this.execAPI.execStreamWithDir(
      command,
      workingDir,
      stdout,
      stderr,
      options,
    );
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

  async log(options: RequestOptions = {}): Promise<string> {
    const result = asObject(await this.sendRequest("log", undefined, options));
    return Buffer.from(asString(result.content), "base64").toString("utf8");
  }

  async logStream(options: LogStreamOptions = {}): Promise<void> {
    const onNotification = (method: string, params: JSONObject): void => {
      if (method !== "log_stream.data") {
        return;
      }

      const data = asString(params.data);
      if (!data) {
        return;
      }

      let decoded: Buffer;
      try {
        decoded = Buffer.from(data, "base64");
      } catch {
        return;
      }
      this.writeStreamChunk(options.stdout, decoded);
    };

    await this.sendRequest("log_stream", undefined, options, onNotification);
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
    await this.sendRequest(
      "write_file",
      {
        path,
        content: toBuffer(content).toString("base64"),
        mode,
      },
      options,
    );
  }

  async readFile(path: string, options: RequestOptions = {}): Promise<Buffer> {
    const result = asObject(
      await this.sendRequest("read_file", { path }, options),
    );
    return Buffer.from(asString(result.content), "base64");
  }

  async listFiles(
    path: string,
    options: RequestOptions = {},
  ): Promise<FileInfo[]> {
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
        addresses:
          addresses && addresses.length > 0 ? [...addresses] : ["127.0.0.1"],
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

  private writeStreamChunk(
    writer: StreamWriter | undefined,
    chunk: Buffer,
  ): void {
    if (!writer) {
      return;
    }

    if (typeof writer === "function") {
      void writer(chunk);
      return;
    }

    writer.write(chunk);
  }
}
