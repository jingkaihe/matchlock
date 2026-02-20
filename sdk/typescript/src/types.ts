import type { Client } from "./client";

export const VFS_HOOK_PHASE_BEFORE = "before";
export const VFS_HOOK_PHASE_AFTER = "after";

export const VFS_HOOK_ACTION_ALLOW = "allow";
export const VFS_HOOK_ACTION_BLOCK = "block";

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

export interface FileInfo {
  name: string;
  size: number;
  mode: number;
  isDir: boolean;
}

export type StreamWriter =
  | NodeJS.WritableStream
  | ((chunk: Buffer) => void | Promise<void>);

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
