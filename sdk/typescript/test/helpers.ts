import { EventEmitter } from "node:events";
import { PassThrough, Writable } from "node:stream";

interface RPCRequest {
  jsonrpc: string;
  method: string;
  params?: Record<string, unknown>;
  id: number;
}

export class FakeProcess extends EventEmitter {
  readonly stdin: Writable;
  readonly stdout: PassThrough;
  readonly stderr: PassThrough;
  killed = false;
  exitCode: number | null = null;

  readonly requests: RPCRequest[] = [];

  private stdinBuffer = "";

  constructor() {
    super();
    this.stdout = new PassThrough();
    this.stderr = new PassThrough();

    this.stdin = new Writable({
      write: (chunk, _encoding, callback): void => {
        this.stdinBuffer += chunk.toString("utf8");
        this.consumeStdin();
        callback();
      },
    });
  }

  waitForRequest(method?: string, timeoutMs = 1000): Promise<RPCRequest> {
    const found = this.requests.find((request) => !method || request.method === method);
    if (found) {
      return Promise.resolve(found);
    }

    return new Promise<RPCRequest>((resolve, reject) => {
      const onRequest = (request: RPCRequest): void => {
        if (method && request.method !== method) {
          return;
        }
        cleanup();
        resolve(request);
      };

      const timer = setTimeout(() => {
        cleanup();
        reject(new Error(`timed out waiting for request${method ? ` ${method}` : ""}`));
      }, timeoutMs);

      const cleanup = (): void => {
        clearTimeout(timer);
        this.off("request", onRequest);
      };

      this.on("request", onRequest);
    });
  }

  pushResponse(response: Record<string, unknown>): void {
    this.stdout.write(`${JSON.stringify(response)}\n`);
  }

  pushNotification(method: string, params: Record<string, unknown>): void {
    this.pushResponse({
      jsonrpc: "2.0",
      method,
      params,
    });
  }

  kill(): boolean {
    this.killed = true;
    this.close(-9);
    return true;
  }

  close(code = 0): void {
    if (this.exitCode !== null) {
      return;
    }
    this.exitCode = code;
    this.stdout.end();
    this.stderr.end();
    this.emit("exit", code, null);
    this.emit("close", code, null);
  }

  private consumeStdin(): void {
    for (;;) {
      const index = this.stdinBuffer.indexOf("\n");
      if (index === -1) {
        return;
      }
      const line = this.stdinBuffer.slice(0, index).trim();
      this.stdinBuffer = this.stdinBuffer.slice(index + 1);
      if (!line) {
        continue;
      }

      const request = JSON.parse(line) as RPCRequest;
      this.requests.push(request);
      this.emit("request", request);
    }
  }
}
