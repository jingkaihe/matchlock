import type {
  BinaryLike,
  NetworkHookFunc,
  VFSActionRequest,
  VFSHookAction,
  VFSHookEvent,
  VFSMutateRequest,
} from "../types";
import type { Client } from "../client";

export type JSONValue = null | boolean | number | string | JSONObject | JSONArray;
export type JSONObject = { [key: string]: JSONValue };
export type JSONArray = JSONValue[];

export interface JSONRPCRequest {
  jsonrpc: "2.0";
  method: string;
  params?: JSONObject;
  id: number;
}

export interface JSONRPCError {
  code: number;
  message: string;
}

export interface JSONRPCResponse {
  jsonrpc?: string;
  id?: number;
  result?: JSONValue;
  error?: JSONRPCError;
}

export interface JSONRPCNotification {
  method?: string;
  params?: JSONObject;
}

export interface PendingRequest {
  resolve: (result: JSONValue) => void;
  reject: (error: unknown) => void;
  onNotification?: (method: string, params: JSONObject) => void;
}

export interface CompiledVFSHook {
  name: string;
  ops: Set<string>;
  path: string;
  timeoutMs: number;
  dangerous: boolean;
  callback: (client: Client, event: VFSHookEvent) => Promise<void>;
}

export interface CompiledVFSMutateHook {
  name: string;
  ops: Set<string>;
  path: string;
  callback: (request: VFSMutateRequest) => Promise<BinaryLike | null | undefined>;
}

export interface CompiledVFSActionHook {
  name: string;
  ops: Set<string>;
  path: string;
  callback: (request: VFSActionRequest) => Promise<VFSHookAction>;
}

export interface CompiledNetworkHook {
  id: string;
  name: string;
  phase: string;
  timeoutMs: number;
  callback: NetworkHookFunc;
}

export interface WireVFSHookRule {
  name?: string;
  phase?: string;
  ops?: string[];
  path?: string;
  action?: string;
  timeout_ms?: number;
}

export interface WireVFSInterceptionConfig {
  emit_events?: boolean;
  rules?: WireVFSHookRule[];
}

export interface WireNetworkBodyTransform {
  find: string;
  replace?: string;
}

export interface WireNetworkHookRule {
  name?: string;
  phase?: string;
  hosts?: string[];
  methods?: string[];
  path?: string;
  action?: string;
  callback_id?: string;
  set_headers?: Record<string, string>;
  delete_headers?: string[];
  set_query?: Record<string, string>;
  delete_query?: string[];
  rewrite_path?: string;
  set_response_headers?: Record<string, string>;
  delete_response_headers?: string[];
  body_replacements?: WireNetworkBodyTransform[];
  timeout_ms?: number;
}

export interface WireNetworkInterceptionConfig {
  callback_socket?: string;
  rules?: WireNetworkHookRule[];
}
