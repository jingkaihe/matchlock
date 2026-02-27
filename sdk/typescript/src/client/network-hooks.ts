import { mkdtemp, rm } from "node:fs/promises";
import { createServer } from "node:net";
import type { Server, Socket } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { NETWORK_HOOK_ACTION_ALLOW, type NetworkHookRequest, type NetworkHookResult, type NetworkInterceptionConfig } from "../types";
import { MatchlockError } from "../errors";
import { asNumber, asObject, asString, buildWireNetworkInterception, toBuffer, toError } from "./utils";
import type {
  CompiledNetworkHook,
  JSONObject,
  JSONValue,
  WireNetworkInterceptionConfig,
} from "./wire";

export class NetworkHooks {
  private networkHooks = new Map<string, CompiledNetworkHook>();
  private networkHookServer: Server | undefined;
  private networkHookSocketPath = "";
  private networkHookTempDir = "";

  compile(cfg: NetworkInterceptionConfig | undefined): [
    WireNetworkInterceptionConfig | undefined,
    Map<string, CompiledNetworkHook>,
  ] {
    if (!cfg) {
      return [undefined, new Map()];
    }

    const wire = buildWireNetworkInterception(cfg);
    const local = new Map<string, CompiledNetworkHook>();
    for (const [index, rule] of (cfg.rules ?? []).entries()) {
      if (!rule.hook) {
        continue;
      }

      const action = String(rule.action ?? "").trim().toLowerCase();
      if (action && action !== NETWORK_HOOK_ACTION_ALLOW) {
        throw new MatchlockError(
          `invalid network hook ${JSON.stringify(rule.name ?? "")}: callback hooks cannot set action=${JSON.stringify(rule.action)}`,
        );
      }

      const callbackID = `network_hook_${index + 1}`;
      local.set(callbackID, {
        id: callbackID,
        name: rule.name ?? "",
        phase: String(rule.phase ?? "").trim().toLowerCase(),
        timeoutMs: rule.timeoutMs ?? 0,
        callback: rule.hook,
      });
      if (wire?.rules?.[index]) {
        wire.rules[index].callback_id = callbackID;
      }
    }

    return [wire, local];
  }

  async start(hooks: Map<string, CompiledNetworkHook>): Promise<string> {
    const tempDir = await mkdtemp(join(tmpdir(), "matchlock-network-hook-"));
    const socketPath = join(tempDir, "hook.sock");

    const hooksCopy = new Map(hooks);
    const server = createServer((socket) => {
      this.serveNetworkHookSocket(socket, hooksCopy);
    });

    try {
      await new Promise<void>((resolve, reject) => {
        server.once("error", reject);
        server.listen(socketPath, () => {
          server.off("error", reject);
          resolve();
        });
      });
    } catch (error) {
      server.close();
      await rm(tempDir, { recursive: true, force: true }).catch(() => undefined);
      throw error;
    }

    this.networkHooks = hooksCopy;
    this.networkHookServer = server;
    this.networkHookSocketPath = socketPath;
    this.networkHookTempDir = tempDir;
    return socketPath;
  }

  async stop(): Promise<void> {
    const server = this.networkHookServer;
    const tempDir = this.networkHookTempDir;

    this.networkHooks = new Map();
    this.networkHookServer = undefined;
    this.networkHookSocketPath = "";
    this.networkHookTempDir = "";

    if (server) {
      await new Promise<void>((resolve) => {
        server.close(() => resolve());
      });
    }
    if (tempDir) {
      await rm(tempDir, { recursive: true, force: true }).catch(() => undefined);
    }
  }

  private serveNetworkHookSocket(
    socket: Socket,
    hooks: Map<string, CompiledNetworkHook>,
  ): void {
    let buffer = "";
    const onData = (chunk: Buffer | string): void => {
      buffer += chunk.toString();
      const newline = buffer.indexOf("\n");
      if (newline === -1) {
        return;
      }
      socket.off("data", onData);
      const line = buffer.slice(0, newline).trim();
      void this.handleNetworkHookSocketLine(socket, line, hooks);
    };

    socket.on("data", onData);
    socket.on("error", () => {
      socket.off("data", onData);
    });
    socket.on("close", () => {
      socket.off("data", onData);
    });
  }

  private async handleNetworkHookSocketLine(
    socket: Socket,
    line: string,
    hooks: Map<string, CompiledNetworkHook>,
  ): Promise<void> {
    const write = (payload: JSONObject): void => {
      if (socket.destroyed) {
        return;
      }
      socket.end(`${JSON.stringify(payload)}\n`);
    };

    let payload: JSONObject;
    try {
      payload = asObject(JSON.parse(line) as JSONValue);
    } catch (error) {
      write({ error: toError(error).message });
      return;
    }

    const callbackID = asString(payload.callback_id).trim();
    const hook = hooks.get(callbackID);
    if (!hook) {
      write({ error: "network hook callback not found" });
      return;
    }

    const phase = asString(payload.phase).trim().toLowerCase();
    if (hook.phase && hook.phase !== phase) {
      write({ error: "network hook phase mismatch" });
      return;
    }

    const request: NetworkHookRequest = {
      phase: phase as NetworkHookRequest["phase"],
      host: asString(payload.host),
      method: asString(payload.method),
      path: asString(payload.path),
      query: this.toStringMap(payload.query),
      requestHeaders: this.toStringSliceMap(payload.request_headers),
      statusCode: asNumber(payload.status_code),
      responseHeaders: this.toStringSliceMap(payload.response_headers),
      isSSE: Boolean(payload.is_sse),
    };

    try {
      const result = await this.invokeNetworkHook(hook, request);
      write(this.networkHookResultToWire(result));
    } catch (error) {
      write({ error: toError(error).message });
    }
  }

  private async invokeNetworkHook(
    hook: CompiledNetworkHook,
    request: NetworkHookRequest,
  ): Promise<NetworkHookResult | null | undefined> {
    if (hook.timeoutMs <= 0) {
      return hook.callback(request);
    }

    let timer: NodeJS.Timeout | undefined;
    try {
      return await Promise.race([
        Promise.resolve(hook.callback(request)),
        new Promise<never>((_resolve, reject) => {
          timer = setTimeout(() => {
            reject(new Error("network hook callback timed out"));
          }, hook.timeoutMs);
        }),
      ]);
    } finally {
      if (timer) {
        clearTimeout(timer);
      }
    }
  }

  private networkHookResultToWire(
    result: NetworkHookResult | null | undefined,
  ): JSONObject {
    const out: JSONObject = {};
    if (!result) {
      return out;
    }

    if (result.action) {
      out.action = result.action;
    }
    if (result.request) {
      const req: JSONObject = {};
      if (result.request.headers !== undefined) {
        req.headers = result.request.headers as unknown as JSONValue;
      }
      if (result.request.query !== undefined) {
        req.query = result.request.query as unknown as JSONValue;
      }
      if (result.request.path) {
        req.path = result.request.path;
      }
      if (Object.keys(req).length > 0) {
        out.request = req;
      }
    }
    if (result.response) {
      const resp: JSONObject = {};
      if (result.response.headers !== undefined) {
        resp.headers = result.response.headers as unknown as JSONValue;
      }
      if ((result.response.bodyReplacements?.length ?? 0) > 0) {
        resp.body_replacements = result.response.bodyReplacements?.map((x) => ({
          find: x.find,
          replace: x.replace,
        })) as unknown as JSONValue;
      }
      if (
        result.response.setBody !== undefined &&
        result.response.setBody !== null
      ) {
        resp.set_body_base64 = toBuffer(result.response.setBody).toString("base64");
      }
      if (Object.keys(resp).length > 0) {
        out.response = resp;
      }
    }

    return out;
  }

  private toStringMap(
    value: JSONValue | undefined,
  ): Record<string, string> | undefined {
    if (!value || typeof value !== "object" || Array.isArray(value)) {
      return undefined;
    }
    const out: Record<string, string> = {};
    for (const [key, item] of Object.entries(value)) {
      out[key] = String(item ?? "");
    }
    return out;
  }

  private toStringSliceMap(
    value: JSONValue | undefined,
  ): Record<string, string[]> | undefined {
    if (!value || typeof value !== "object" || Array.isArray(value)) {
      return undefined;
    }
    const out: Record<string, string[]> = {};
    for (const [key, item] of Object.entries(value)) {
      if (Array.isArray(item)) {
        out[key] = item.map((x) => String(x ?? ""));
      } else {
        out[key] = [String(item ?? "")];
      }
    }
    return out;
  }
}
