import type {
  BinaryLike,
  ExecInteractiveOptions,
  ExecInteractiveResult,
  ExecOptions,
  ExecPipeOptions,
  ExecPipeResult,
  ExecResult,
  ExecStreamOptions,
  ExecStreamResult,
  RequestOptions,
  StreamReader,
  StreamWriter,
  TTYSize,
} from "../types";
import { asNumber, asObject, asString, toBuffer } from "./utils";
import type { JSONObject, JSONValue } from "./wire";

type NotificationHandler = (method: string, params: JSONObject) => void;
type SendRequestFunc = (
  method: string,
  params?: JSONObject,
  options?: RequestOptions,
  onNotification?: NotificationHandler,
) => Promise<JSONValue>;
type SendFireAndForgetFunc = (
  method: string,
  params?: JSONObject,
) => Promise<void>;

export class ExecAPI {
  private readonly sendRequest: SendRequestFunc;
  private readonly sendFireAndForget: SendFireAndForgetFunc;

  constructor(sendRequest: SendRequestFunc, sendFireAndForget: SendFireAndForgetFunc) {
    this.sendRequest = sendRequest;
    this.sendFireAndForget = sendFireAndForget;
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

  async execPipe(
    command: string,
    options: ExecPipeOptions = {},
  ): Promise<ExecPipeResult> {
    return this.execPipeWithDir(
      command,
      options.workingDir ?? "",
      options.stdin,
      options.stdout,
      options.stderr,
      options,
    );
  }

  async execPipeWithDir(
    command: string,
    workingDir = "",
    stdin?: StreamReader,
    stdout?: StreamWriter,
    stderr?: StreamWriter,
    options: RequestOptions = {},
  ): Promise<ExecPipeResult> {
    const params: JSONObject = { command };
    if (workingDir) {
      params.working_dir = workingDir;
    }

    const stop = this.createStopSignal();
    const ready = this.createReadySignal();
    const inputPump = this.pumpExecInput(
      stdin,
      ready.promise,
      stop,
      "exec_pipe.stdin",
      "exec_pipe.stdin_eof",
    );

    const onNotification = (method: string, payload: JSONObject): void => {
      if (method === "exec_pipe.ready") {
        const reqID = asNumber(payload.id, -1);
        if (reqID >= 0) {
          ready.markReady(reqID);
        }
        return;
      }

      const data = asString(payload.data);
      if (!data) {
        return;
      }
      const decoded = this.decodeBase64(data);
      if (!decoded) {
        return;
      }

      if (method === "exec_pipe.stdout") {
        this.writeStreamChunk(stdout, decoded);
      } else if (method === "exec_pipe.stderr") {
        this.writeStreamChunk(stderr, decoded);
      }
    };

    try {
      const result = asObject(
        await this.sendRequest("exec_pipe", params, options, onNotification),
      );
      return {
        exitCode: asNumber(result.exit_code),
        durationMs: asNumber(result.duration_ms),
      };
    } finally {
      stop.stop();
      ready.markReady(undefined);
      await inputPump.catch(() => undefined);
    }
  }

  async execInteractive(
    command: string,
    options: ExecInteractiveOptions = {},
  ): Promise<ExecInteractiveResult> {
    const rows = (options.rows ?? 24) > 0 ? options.rows ?? 24 : 24;
    const cols = (options.cols ?? 80) > 0 ? options.cols ?? 80 : 80;

    const params: JSONObject = {
      command,
      rows,
      cols,
    };
    if (options.workingDir) {
      params.working_dir = options.workingDir;
    }

    const stop = this.createStopSignal();
    const ready = this.createReadySignal();
    const inputPump = this.pumpExecInput(
      options.stdin,
      ready.promise,
      stop,
      "exec_tty.stdin",
      "exec_tty.stdin_eof",
    );
    const resizePump = this.pumpTTYResize(options.resize, ready.promise, stop);

    const onNotification = (method: string, payload: JSONObject): void => {
      if (method === "exec_tty.ready") {
        const reqID = asNumber(payload.id, -1);
        if (reqID >= 0) {
          ready.markReady(reqID);
        }
        return;
      }
      if (method !== "exec_tty.stdout") {
        return;
      }

      const data = asString(payload.data);
      if (!data) {
        return;
      }
      const decoded = this.decodeBase64(data);
      if (!decoded) {
        return;
      }
      this.writeStreamChunk(options.stdout, decoded);
    };

    try {
      const result = asObject(
        await this.sendRequest("exec_tty", params, options, onNotification),
      );
      return {
        exitCode: asNumber(result.exit_code),
        durationMs: asNumber(result.duration_ms),
      };
    } finally {
      stop.stop();
      ready.markReady(undefined);
      await Promise.all([
        inputPump.catch(() => undefined),
        resizePump.catch(() => undefined),
      ]);
    }
  }

  private createReadySignal(): {
    promise: Promise<number | undefined>;
    markReady: (reqID: number | undefined) => void;
  } {
    let resolved = false;
    let resolveFn: (reqID: number | undefined) => void = () => {};
    const promise = new Promise<number | undefined>((resolve) => {
      resolveFn = resolve;
    });
    return {
      promise,
      markReady: (reqID: number | undefined): void => {
        if (resolved) {
          return;
        }
        resolved = true;
        resolveFn(reqID);
      },
    };
  }

  private createStopSignal(): {
    promise: Promise<void>;
    isStopped: () => boolean;
    stop: () => void;
  } {
    let stopped = false;
    let resolveFn: () => void = () => {};
    const promise = new Promise<void>((resolve) => {
      resolveFn = resolve;
    });
    return {
      promise,
      isStopped: (): boolean => stopped,
      stop: (): void => {
        if (stopped) {
          return;
        }
        stopped = true;
        resolveFn();
      },
    };
  }

  private decodeBase64(data: string): Buffer | undefined {
    try {
      return Buffer.from(data, "base64");
    } catch {
      return undefined;
    }
  }

  private isAsyncIterable<T>(value: unknown): value is AsyncIterable<T> {
    if (!value || typeof value !== "object") {
      return false;
    }
    return typeof (value as AsyncIterable<T>)[Symbol.asyncIterator] === "function";
  }

  private isIterable<T>(value: unknown): value is Iterable<T> {
    if (!value || typeof value !== "object") {
      return false;
    }
    return typeof (value as Iterable<T>)[Symbol.iterator] === "function";
  }

  private toAsyncIterable<T>(value: AsyncIterable<T> | Iterable<T>): AsyncIterable<T> {
    if (this.isAsyncIterable<T>(value)) {
      return value;
    }
    return (async function* toAsync(): AsyncGenerator<T> {
      for (const item of value) {
        yield item;
      }
    })();
  }

  private async pumpExecInput(
    stdin: StreamReader | undefined,
    readyPromise: Promise<number | undefined>,
    stop: { promise: Promise<void>; isStopped: () => boolean },
    chunkMethod: string,
    eofMethod: string,
  ): Promise<void> {
    const reqID = await Promise.race([
      readyPromise,
      stop.promise.then(() => undefined),
    ]);
    if (stop.isStopped() || reqID === undefined) {
      return;
    }

    if (!stdin) {
      await this.sendFireAndForget(eofMethod, { id: reqID }).catch(() => undefined);
      return;
    }

    let shouldSendEOF = true;
    try {
      const chunks = this.isAsyncIterable<BinaryLike>(stdin) || this.isIterable<BinaryLike>(stdin)
        ? this.toAsyncIterable<BinaryLike>(stdin)
        : undefined;
      if (!chunks) {
        return;
      }
      const iterator = chunks[Symbol.asyncIterator]();
      try {
        for (;;) {
          const next = await this.nextAsyncItem(iterator, stop.promise);
          if (!next) {
            shouldSendEOF = false;
            return;
          }
          if (next.done) {
            return;
          }
          if (stop.isStopped()) {
            shouldSendEOF = false;
            return;
          }
          const chunk = next.value;
          let encoded = "";
          try {
            encoded = toBuffer(chunk).toString("base64");
          } catch {
            continue;
          }
          if (!encoded) {
            continue;
          }
          await this.sendFireAndForget(chunkMethod, {
            id: reqID,
            data: encoded,
          }).catch(() => undefined);
        }
      } finally {
        this.closeAsyncIterator(iterator);
      }
    } finally {
      if (shouldSendEOF && !stop.isStopped()) {
        await this.sendFireAndForget(eofMethod, { id: reqID }).catch(() => undefined);
      }
    }
  }

  private async pumpTTYResize(
    resize: AsyncIterable<TTYSize> | Iterable<TTYSize> | undefined,
    readyPromise: Promise<number | undefined>,
    stop: { promise: Promise<void>; isStopped: () => boolean },
  ): Promise<void> {
    if (!resize) {
      return;
    }

    const reqID = await Promise.race([
      readyPromise,
      stop.promise.then(() => undefined),
    ]);
    if (stop.isStopped() || reqID === undefined) {
      return;
    }

    const sizes = this.toAsyncIterable<TTYSize>(resize);
    const iterator = sizes[Symbol.asyncIterator]();
    try {
      for (;;) {
        const next = await this.nextAsyncItem(iterator, stop.promise);
        if (!next || next.done || stop.isStopped()) {
          return;
        }
        const size = next.value;
        if (!Array.isArray(size) || size.length < 2) {
          continue;
        }
        const rows = Math.trunc(Number(size[0]));
        const cols = Math.trunc(Number(size[1]));
        if (!Number.isFinite(rows) || !Number.isFinite(cols) || rows <= 0 || cols <= 0) {
          continue;
        }
        await this.sendFireAndForget("exec_tty.resize", {
          id: reqID,
          rows,
          cols,
        }).catch(() => undefined);
      }
    } finally {
      this.closeAsyncIterator(iterator);
    }
  }

  private async nextAsyncItem<T>(
    iterator: AsyncIterator<T>,
    stopPromise: Promise<void>,
  ): Promise<IteratorResult<T> | undefined> {
    const nextPromise = iterator.next();
    const result = await Promise.race([
      nextPromise.then((next) => ({ kind: "next" as const, next })),
      stopPromise.then(() => ({ kind: "stopped" as const })),
    ]);
    if (result.kind === "stopped") {
      void nextPromise.catch(() => undefined);
      return undefined;
    }
    return result.next;
  }

  private closeAsyncIterator<T>(iterator: AsyncIterator<T>): void {
    const close = iterator.return;
    if (typeof close !== "function") {
      return;
    }
    void Promise.resolve(close.call(iterator)).catch(() => undefined);
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
}
