import type { Client } from "./client";

export const VFS_HOOK_PHASE_BEFORE = "before";
export const VFS_HOOK_PHASE_AFTER = "after";
export const NETWORK_HOOK_PHASE_BEFORE = "before";
export const NETWORK_HOOK_PHASE_AFTER = "after";

export const VFS_HOOK_ACTION_ALLOW = "allow";
export const VFS_HOOK_ACTION_BLOCK = "block";
export const NETWORK_HOOK_ACTION_ALLOW = "allow";
export const NETWORK_HOOK_ACTION_BLOCK = "block";
export const NETWORK_HOOK_ACTION_MUTATE = "mutate";

export const VFS_HOOK_OP_STAT = "stat";
export const VFS_HOOK_OP_READDIR = "readdir";
export const VFS_HOOK_OP_OPEN = "open";
export const VFS_HOOK_OP_CREATE = "create";
export const VFS_HOOK_OP_MKDIR = "mkdir";
export const VFS_HOOK_OP_CHMOD = "chmod";
export const VFS_HOOK_OP_REMOVE = "remove";
export const VFS_HOOK_OP_REMOVE_ALL = "remove_all";
export const VFS_HOOK_OP_RENAME = "rename";
export const VFS_HOOK_OP_SYMLINK = "symlink";
export const VFS_HOOK_OP_READLINK = "readlink";
export const VFS_HOOK_OP_READ = "read";
export const VFS_HOOK_OP_WRITE = "write";
export const VFS_HOOK_OP_CLOSE = "close";
export const VFS_HOOK_OP_SYNC = "sync";
export const VFS_HOOK_OP_TRUNCATE = "truncate";

export type VFSHookPhase = "" | "before" | "after";

export type VFSHookOp =
  | "stat"
  | "readdir"
  | "open"
  | "create"
  | "mkdir"
  | "chmod"
  | "remove"
  | "remove_all"
  | "rename"
  | "symlink"
  | "readlink"
  | "read"
  | "write"
  | "close"
  | "sync"
  | "truncate";

export type VFSHookAction = "allow" | "block" | (string & {});
export type NetworkHookPhase = "" | "before" | "after";
export type NetworkHookAction = "allow" | "block" | "mutate";

export interface Config {
  binaryPath?: string;
  useSudo?: boolean;
}

export interface HostIPMapping {
  host: string;
  ip: string;
}

export interface MountConfig {
  type?: string;
  hostPath?: string;
  readonly?: boolean;
}

export interface Secret {
  name: string;
  value: string;
  hosts?: string[];
}

export interface ImageConfig {
  user?: string;
  workingDir?: string;
  entrypoint?: string[];
  cmd?: string[];
  env?: Record<string, string>;
}

export interface PortForward {
  localPort: number;
  remotePort: number;
}

export interface PortForwardBinding {
  address: string;
  localPort: number;
  remotePort: number;
}

export interface VFSHookEvent {
  op: string;
  path: string;
  size: number;
  mode: number;
  uid: number;
  gid: number;
}

export interface VFSMutateRequest {
  path: string;
  size: number;
  mode: number;
  uid: number;
  gid: number;
}

export interface VFSActionRequest {
  op: string;
  path: string;
  size: number;
  mode: number;
  uid: number;
  gid: number;
}

export type BinaryLike = string | Buffer | Uint8Array | ArrayBuffer;

export type VFSHookFunc = (event: VFSHookEvent) => void | Promise<void>;
export type VFSDangerousHookFunc = (
  client: Client,
  event: VFSHookEvent,
) => void | Promise<void>;
export type VFSMutateHookFunc = (
  request: VFSMutateRequest,
) => BinaryLike | null | undefined | Promise<BinaryLike | null | undefined>;
export type VFSActionHookFunc = (
  request: VFSActionRequest,
) => VFSHookAction | Promise<VFSHookAction>;

export interface VFSHookRule {
  name?: string;
  phase?: VFSHookPhase;
  ops?: VFSHookOp[];
  path?: string;
  action?: VFSHookAction;
  timeoutMs?: number;
  hook?: VFSHookFunc;
  dangerousHook?: VFSDangerousHookFunc;
  mutateHook?: VFSMutateHookFunc;
  actionHook?: VFSActionHookFunc;
}

export interface VFSInterceptionConfig {
  emitEvents?: boolean;
  rules?: VFSHookRule[];
}

export interface NetworkBodyTransform {
  find: string;
  replace?: string;
}

export interface NetworkHookRequest {
  phase: NetworkHookPhase;
  host: string;
  method: string;
  path: string;
  query?: Record<string, string>;
  requestHeaders?: Record<string, string[]>;
  statusCode: number;
  responseHeaders?: Record<string, string[]>;
  isSSE: boolean;
}

export interface NetworkHookRequestMutation {
  headers?: Record<string, string[]>;
  query?: Record<string, string>;
  path?: string;
}

export interface NetworkHookResponseMutation {
  headers?: Record<string, string[]>;
  bodyReplacements?: NetworkBodyTransform[];
  setBody?: BinaryLike | null;
}

export interface NetworkHookResult {
  action?: NetworkHookAction;
  request?: NetworkHookRequestMutation;
  response?: NetworkHookResponseMutation;
}

export type NetworkHookFunc = (
  request: NetworkHookRequest,
) =>
  | NetworkHookResult
  | null
  | undefined
  | Promise<NetworkHookResult | null | undefined>;

export interface NetworkHookRule {
  name?: string;
  phase?: NetworkHookPhase;
  hosts?: string[];
  methods?: string[];
  path?: string;
  action?: NetworkHookAction;
  setHeaders?: Record<string, string>;
  deleteHeaders?: string[];
  setQuery?: Record<string, string>;
  deleteQuery?: string[];
  rewritePath?: string;
  setResponseHeaders?: Record<string, string>;
  deleteResponseHeaders?: string[];
  bodyReplacements?: NetworkBodyTransform[];
  timeoutMs?: number;
  hook?: NetworkHookFunc;
}

export interface NetworkInterceptionConfig {
  rules?: NetworkHookRule[];
}

export interface CreateOptions {
  image?: string;
  privileged?: boolean;
  cpus?: number;
  memoryMb?: number;
  diskSizeMb?: number;
  timeoutSeconds?: number;
  allowedHosts?: string[];
  addHosts?: HostIPMapping[];
  blockPrivateIPs?: boolean;
  blockPrivateIPsSet?: boolean;
  noNetwork?: boolean;
  forceInterception?: boolean;
  networkInterception?: NetworkInterceptionConfig;
  mounts?: Record<string, MountConfig>;
  env?: Record<string, string>;
  secrets?: Secret[];
  workspace?: string;
  vfsInterception?: VFSInterceptionConfig;
  dnsServers?: string[];
  hostname?: string;
  networkMtu?: number;
  portForwards?: PortForward[];
  portForwardAddresses?: string[];
  imageConfig?: ImageConfig;
}

export interface ExecResult {
  exitCode: number;
  stdout: string;
  stderr: string;
  durationMs: number;
}

export interface ExecStreamResult {
  exitCode: number;
  durationMs: number;
}

export interface ExecPipeResult {
  exitCode: number;
  durationMs: number;
}

export interface ExecInteractiveResult {
  exitCode: number;
  durationMs: number;
}

export interface FileInfo {
  name: string;
  size: number;
  mode: number;
  isDir: boolean;
}

export interface VolumeInfo {
  name: string;
  size: string;
  path: string;
}

export type StreamWriter =
  | NodeJS.WritableStream
  | ((chunk: Buffer) => void | Promise<void>);
export type StreamReader =
  | NodeJS.ReadableStream
  | AsyncIterable<BinaryLike>
  | Iterable<BinaryLike>;

export type TTYSize = [number, number];

export interface RequestOptions {
  signal?: AbortSignal;
  timeoutMs?: number;
}

export interface ExecOptions extends RequestOptions {
  workingDir?: string;
}

export interface ExecStreamOptions extends ExecOptions {
  stdout?: StreamWriter;
  stderr?: StreamWriter;
}

export interface ExecPipeOptions extends ExecOptions {
  stdin?: StreamReader;
  stdout?: StreamWriter;
  stderr?: StreamWriter;
}

export interface ExecInteractiveOptions extends ExecOptions {
  stdin?: StreamReader;
  stdout?: StreamWriter;
  rows?: number;
  cols?: number;
  resize?: AsyncIterable<TTYSize> | Iterable<TTYSize>;
}
