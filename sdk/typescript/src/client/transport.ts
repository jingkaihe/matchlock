import { once } from "node:events";
import { execFile, spawn } from "node:child_process";
import type { ChildProcessWithoutNullStreams } from "node:child_process";
import { MatchlockError, RPCError } from "../errors";
import type { Config, RequestOptions } from "../types";
import { asNumber } from "./utils";
import type {
  JSONObject,
  JSONRPCNotification,
  JSONRPCRequest,
  JSONRPCResponse,
  JSONValue,
  PendingRequest,
} from "./wire";

const REQUEST_NOTIFICATION_METHODS = new Set<string>([
  "exec_stream.stdout",
  "exec_stream.stderr",
  "exec_pipe.ready",
  "exec_pipe.stdout",
  "exec_pipe.stderr",
  "exec_tty.ready",
  "exec_tty.stdout",
]);

export class RPCTransport {
  private readonly config: Required<Config>;
  private readonly onNotification: (method: string, params: JSONObject) => void;
  private readonly onClosed: (error?: Error) => void;

  private process: ChildProcessWithoutNullStreams | undefined;
  private requestID = 0;
  private pending = new Map<number, PendingRequest>();
  private writeLock: Promise<void> = Promise.resolve();
  private readBuffer = "";

  constructor(
    config: Required<Config>,
    onNotification: (method: string, params: JSONObject) => void,
    onClosed: (error?: Error) => void,
  ) {
    this.config = config;
    this.onNotification = onNotification;
    this.onClosed = onClosed;
  }

  async start(): Promise<void> {
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

  isRunning(): boolean {
    return !!this.process && this.process.exitCode === null && !this.process.killed;
  }

  async close(timeoutSeconds = 0): Promise<void> {
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
  }

  async execCLI(args: string[]): Promise<{ stdout: string; stderr: string }> {
    return new Promise((resolve, reject) => {
      execFile(
        this.config.binaryPath,
        args,
        { encoding: "utf8" },
        (error, stdout, stderr) => {
          if (error) {
            reject(error);
            return;
          }
          resolve({
            stdout,
            stderr,
          });
        },
      );
    });
  }

  async sendRequest(
    method: string,
    params?: JSONObject,
    options: RequestOptions = {},
    onNotification?: (method: string, params: JSONObject) => void,
  ): Promise<JSONValue> {
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

  async sendFireAndForget(method: string, params?: JSONObject): Promise<void> {
    const request: JSONRPCRequest = {
      jsonrpc: "2.0",
      method,
      id: ++this.requestID,
    };
    if (params && Object.keys(params).length > 0) {
      request.params = params;
    }
    await this.enqueueWrite(`${JSON.stringify(request)}\n`);
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
    if (!method) {
      return;
    }

    const params = msg.params ?? {};
    if (REQUEST_NOTIFICATION_METHODS.has(method)) {
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

    this.onNotification(method, params);
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
    this.onClosed(error);
  }
}
