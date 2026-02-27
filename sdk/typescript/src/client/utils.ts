import { isIP } from "node:net";
import { MatchlockError } from "../errors";
import type {
  BinaryLike,
  Config,
  HostIPMapping,
  NetworkInterceptionConfig,
} from "../types";
import type {
  JSONObject,
  JSONValue,
  WireNetworkHookRule,
  WireNetworkInterceptionConfig,
} from "./wire";

export const DEFAULT_CPUS = 1;
export const DEFAULT_MEMORY_MB = 512;
export const DEFAULT_DISK_SIZE_MB = 5120;
export const DEFAULT_TIMEOUT_SECONDS = 300;

export function defaultConfig(config: Config = {}): Required<Config> {
  return {
    binaryPath: config.binaryPath ?? process.env.MATCHLOCK_BIN ?? "matchlock",
    useSudo: config.useSudo ?? false,
  };
}

export function toError(value: unknown): Error {
  if (value instanceof Error) {
    return value;
  }
  return new Error(String(value));
}

export function toBuffer(content: BinaryLike): Buffer {
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

export function lowerSet(values: string[] | undefined): Set<string> {
  return new Set((values ?? []).map((value) => value.toLowerCase()));
}

export function buildWireNetworkInterception(
  config: NetworkInterceptionConfig | undefined,
): WireNetworkInterceptionConfig | undefined {
  if (!config) {
    return undefined;
  }

  const wire: WireNetworkInterceptionConfig = {};
  if ((config.rules?.length ?? 0) > 0) {
    wire.rules = (config.rules ?? []).map((rule) => {
      const out: WireNetworkHookRule = {};
      if (rule.name) {
        out.name = rule.name;
      }
      if (rule.phase) {
        out.phase = rule.phase;
      }
      if ((rule.hosts?.length ?? 0) > 0) {
        out.hosts = [...(rule.hosts ?? [])];
      }
      if ((rule.methods?.length ?? 0) > 0) {
        out.methods = [...(rule.methods ?? [])];
      }
      if (rule.path) {
        out.path = rule.path;
      }
      if (rule.action) {
        out.action = rule.action;
      }
      if (rule.setHeaders && Object.keys(rule.setHeaders).length > 0) {
        out.set_headers = { ...rule.setHeaders };
      }
      if ((rule.deleteHeaders?.length ?? 0) > 0) {
        out.delete_headers = [...(rule.deleteHeaders ?? [])];
      }
      if (rule.setQuery && Object.keys(rule.setQuery).length > 0) {
        out.set_query = { ...rule.setQuery };
      }
      if ((rule.deleteQuery?.length ?? 0) > 0) {
        out.delete_query = [...(rule.deleteQuery ?? [])];
      }
      if (rule.rewritePath) {
        out.rewrite_path = rule.rewritePath;
      }
      if (
        rule.setResponseHeaders &&
        Object.keys(rule.setResponseHeaders).length > 0
      ) {
        out.set_response_headers = { ...rule.setResponseHeaders };
      }
      if ((rule.deleteResponseHeaders?.length ?? 0) > 0) {
        out.delete_response_headers = [...(rule.deleteResponseHeaders ?? [])];
      }
      if ((rule.bodyReplacements?.length ?? 0) > 0) {
        out.body_replacements = (rule.bodyReplacements ?? []).map((x) => ({
          find: x.find,
          replace: x.replace,
        }));
      }
      if ((rule.timeoutMs ?? 0) > 0) {
        out.timeout_ms = rule.timeoutMs;
      }
      return out;
    });
  }
  return wire;
}

export function asObject(value: JSONValue | undefined): JSONObject {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    return {};
  }
  return value as JSONObject;
}

export function asNumber(value: JSONValue | undefined, fallback = 0): number {
  if (typeof value === "number" && Number.isFinite(value)) {
    return value;
  }
  return fallback;
}

export function asString(value: JSONValue | undefined, fallback = ""): string {
  return typeof value === "string" ? value : fallback;
}

export function getUID(): number {
  return typeof process.geteuid === "function" ? process.geteuid() : 0;
}

export function getGID(): number {
  return typeof process.getegid === "function" ? process.getegid() : 0;
}

export function validateAddHost(mapping: HostIPMapping): void {
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
  if (isIP(mapping.ip.trim()) === 0) {
    throw new MatchlockError(
      `invalid add-host mapping: invalid ip ${JSON.stringify(mapping.ip)}`,
    );
  }
}
