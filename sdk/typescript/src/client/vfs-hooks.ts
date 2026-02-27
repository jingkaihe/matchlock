import { minimatch } from "minimatch";
import { MatchlockError } from "../errors";
import {
  VFS_HOOK_ACTION_ALLOW,
  VFS_HOOK_ACTION_BLOCK,
  VFS_HOOK_OP_READ,
  VFS_HOOK_OP_READDIR,
  VFS_HOOK_OP_WRITE,
  VFS_HOOK_PHASE_AFTER,
  VFS_HOOK_PHASE_BEFORE,
  type BinaryLike,
  type VFSActionRequest,
  type VFSHookAction,
  type VFSHookEvent,
  type VFSHookRule,
  type VFSInterceptionConfig,
  type VFSMutateRequest,
} from "../types";
import { asNumber, asObject, asString, getGID, getUID, lowerSet, toBuffer } from "./utils";
import type { Client } from "../client";
import type {
  CompiledVFSActionHook,
  CompiledVFSHook,
  CompiledVFSMutateHook,
  JSONObject,
  WireVFSHookRule,
  WireVFSInterceptionConfig,
} from "./wire";

export class VFSHooks {
  private vfsHooks: CompiledVFSHook[] = [];
  private vfsMutateHooks: CompiledVFSMutateHook[] = [];
  private vfsActionHooks: CompiledVFSActionHook[] = [];
  private vfsHookActive = false;

  clearLocalHooks(): void {
    this.setLocalHooks([], [], []);
  }

  setLocalHooks(
    hooks: CompiledVFSHook[],
    mutateHooks: CompiledVFSMutateHook[],
    actionHooks: CompiledVFSActionHook[],
  ): void {
    this.vfsHooks = [...hooks];
    this.vfsMutateHooks = [...mutateHooks];
    this.vfsActionHooks = [...actionHooks];
    this.vfsHookActive = false;
  }

  compile(cfg: VFSInterceptionConfig | undefined): [
    WireVFSInterceptionConfig | undefined,
    CompiledVFSHook[],
    CompiledVFSMutateHook[],
    CompiledVFSActionHook[],
  ] {
    if (!cfg) {
      return [undefined, [], [], []];
    }

    const wire: WireVFSInterceptionConfig = {
      emit_events: cfg.emitEvents,
      rules: [],
    };

    const localHooks: CompiledVFSHook[] = [];
    const localMutateHooks: CompiledVFSMutateHook[] = [];
    const localActionHooks: CompiledVFSActionHook[] = [];

    for (const rule of cfg.rules ?? []) {
      const callbackCount =
        Number(Boolean(rule.hook)) +
        Number(Boolean(rule.dangerousHook)) +
        Number(Boolean(rule.mutateHook)) +
        Number(Boolean(rule.actionHook));

      if (callbackCount > 1) {
        throw new MatchlockError(
          `invalid vfs hook ${JSON.stringify(rule.name ?? "")}: cannot set more than one callback hook`,
        );
      }

      if (
        !rule.hook &&
        !rule.dangerousHook &&
        !rule.mutateHook &&
        !rule.actionHook
      ) {
        const action = String(rule.action ?? "allow").trim().toLowerCase();
        if (action === "mutate_write") {
          throw new MatchlockError(
            `invalid vfs hook ${JSON.stringify(rule.name ?? "")}: mutate_write requires mutate_hook callback`,
          );
        }

        wire.rules?.push(this.ruleToWire(rule, action));
        continue;
      }

      if (rule.hook) {
        this.validateLocalAfterRule(rule, "callback hooks");
        localHooks.push({
          name: rule.name ?? "",
          ops: lowerSet(rule.ops),
          path: rule.path ?? "",
          timeoutMs: rule.timeoutMs ?? 0,
          dangerous: false,
          callback: async (_client, event): Promise<void> => {
            await rule.hook?.(event);
          },
        });
        continue;
      }

      if (rule.dangerousHook) {
        this.validateLocalAfterRule(rule, "dangerous_hook");
        localHooks.push({
          name: rule.name ?? "",
          ops: lowerSet(rule.ops),
          path: rule.path ?? "",
          timeoutMs: rule.timeoutMs ?? 0,
          dangerous: true,
          callback: async (client, event): Promise<void> => {
            await rule.dangerousHook?.(client, event);
          },
        });
        continue;
      }

      if (rule.actionHook) {
        const action = String(rule.action ?? "").trim().toLowerCase();
        if (action && action !== VFS_HOOK_ACTION_ALLOW) {
          throw new MatchlockError(
            `invalid vfs hook ${JSON.stringify(rule.name ?? "")}: action_hook cannot set action=${JSON.stringify(rule.action)}`,
          );
        }
        if (
          rule.phase &&
          String(rule.phase).toLowerCase() !== VFS_HOOK_PHASE_BEFORE
        ) {
          throw new MatchlockError(
            `invalid vfs hook ${JSON.stringify(rule.name ?? "")}: action_hook must use phase=before`,
          );
        }

        localActionHooks.push({
          name: rule.name ?? "",
          ops: lowerSet(rule.ops),
          path: rule.path ?? "",
          callback: async (request): Promise<VFSHookAction> =>
            (await rule.actionHook?.(request)) ?? VFS_HOOK_ACTION_ALLOW,
        });
        continue;
      }

      const action = String(rule.action ?? "").trim().toLowerCase();
      if (action && action !== VFS_HOOK_ACTION_ALLOW) {
        throw new MatchlockError(
          `invalid vfs hook ${JSON.stringify(rule.name ?? "")}: mutate_hook cannot set action=${JSON.stringify(rule.action)}`,
        );
      }
      if (rule.phase && String(rule.phase).toLowerCase() !== VFS_HOOK_PHASE_BEFORE) {
        throw new MatchlockError(
          `invalid vfs hook ${JSON.stringify(rule.name ?? "")}: mutate_hook must use phase=before`,
        );
      }

      localMutateHooks.push({
        name: rule.name ?? "",
        ops: lowerSet(rule.ops),
        path: rule.path ?? "",
        callback: async (request): Promise<BinaryLike | null | undefined> =>
          rule.mutateHook?.(request),
      });
    }

    if (localHooks.length > 0) {
      wire.emit_events = true;
    }

    if ((wire.rules?.length ?? 0) === 0) {
      wire.rules = undefined;
    }

    if (!wire.emit_events && !wire.rules) {
      return [undefined, localHooks, localMutateHooks, localActionHooks];
    }

    return [wire, localHooks, localMutateHooks, localActionHooks];
  }

  handleFileEventNotification(params: JSONObject, client: Client): void {
    const file = asObject(params.file);
    if (Object.keys(file).length === 0) {
      return;
    }

    const op = asString(file.op).toLowerCase();
    if (!op) {
      return;
    }

    const event: VFSHookEvent = {
      op,
      path: asString(file.path),
      size: asNumber(file.size),
      mode: asNumber(file.mode),
      uid: asNumber(file.uid),
      gid: asNumber(file.gid),
    };

    this.handleVFSFileEvent(event, client);
  }

  async applyLocalWriteMutations(
    path: string,
    content: Buffer,
    mode: number,
  ): Promise<Buffer> {
    const hooks = [...this.vfsMutateHooks];
    if (hooks.length === 0) {
      return content;
    }

    let current = content;
    for (const hook of hooks) {
      if (!this.matchesVFSHook(hook.ops, hook.path, VFS_HOOK_OP_WRITE, path)) {
        continue;
      }
      const request: VFSMutateRequest = {
        path,
        size: current.length,
        mode,
        uid: getUID(),
        gid: getGID(),
      };
      const mutated = await hook.callback(request);
      if (mutated === null || mutated === undefined) {
        continue;
      }
      if (typeof mutated === "string") {
        current = Buffer.from(mutated, "utf8");
        continue;
      }
      if (
        Buffer.isBuffer(mutated) ||
        mutated instanceof Uint8Array ||
        mutated instanceof ArrayBuffer
      ) {
        current = toBuffer(mutated);
        continue;
      }
      throw new MatchlockError(
        `invalid mutate_hook return type for ${JSON.stringify(hook.name)}: expected bytes|string|undefined`,
      );
    }

    return current;
  }

  async applyLocalActionHooks(
    op: string,
    path: string,
    size: number,
    mode: number,
  ): Promise<void> {
    const hooks = [...this.vfsActionHooks];
    if (hooks.length === 0) {
      return;
    }

    const request: VFSActionRequest = {
      op,
      path,
      size,
      mode,
      uid: getUID(),
      gid: getGID(),
    };

    for (const hook of hooks) {
      if (!this.matchesVFSHook(hook.ops, hook.path, op, path)) {
        continue;
      }

      const decision = String(await hook.callback(request)).trim().toLowerCase();
      if (decision === "" || decision === VFS_HOOK_ACTION_ALLOW) {
        continue;
      }
      if (decision === VFS_HOOK_ACTION_BLOCK) {
        throw new MatchlockError(
          `vfs action hook blocked operation: op=${op} path=${path} hook=${JSON.stringify(hook.name)}`,
        );
      }
      throw new MatchlockError(
        `invalid action_hook return value for ${JSON.stringify(hook.name)}: expected ${JSON.stringify(VFS_HOOK_ACTION_ALLOW)}|${JSON.stringify(VFS_HOOK_ACTION_BLOCK)}, got ${JSON.stringify(decision)}`,
      );
    }
  }

  private handleVFSFileEvent(event: VFSHookEvent, client: Client): void {
    const hooks = [...this.vfsHooks];
    if (hooks.length === 0) {
      return;
    }

    const safeHooks: CompiledVFSHook[] = [];
    for (const hook of hooks) {
      if (!this.matchesVFSHook(hook.ops, hook.path, event.op, event.path)) {
        continue;
      }

      if (hook.dangerous) {
        void this.runSingleVFSHook(hook, event, client);
        continue;
      }
      safeHooks.push(hook);
    }

    if (safeHooks.length === 0) {
      return;
    }
    if (this.vfsHookActive) {
      return;
    }

    void this.runVFSSafeHooksForEvent(safeHooks, event, client);
  }

  private async runVFSSafeHooksForEvent(
    hooks: CompiledVFSHook[],
    event: VFSHookEvent,
    client: Client,
  ): Promise<void> {
    if (this.vfsHookActive) {
      return;
    }
    this.vfsHookActive = true;

    try {
      for (const hook of hooks) {
        await this.runSingleVFSHook(hook, event, client);
      }
    } finally {
      this.vfsHookActive = false;
    }
  }

  private async runSingleVFSHook(
    hook: CompiledVFSHook,
    event: VFSHookEvent,
    client: Client,
  ): Promise<void> {
    try {
      const run = hook.callback(client, event);
      if (hook.timeoutMs > 0) {
        await Promise.race([
          run,
          new Promise<void>((resolve) => {
            setTimeout(resolve, hook.timeoutMs);
          }),
        ]);
      } else {
        await run;
      }
    } catch {
      // Hooks are intentionally best effort.
    }
  }

  private matchesVFSHook(
    ops: Set<string>,
    path: string,
    op: string,
    actualPath: string,
  ): boolean {
    if (ops.size > 0 && !ops.has(op.toLowerCase())) {
      return false;
    }
    if (!path) {
      return true;
    }
    try {
      return minimatch(actualPath, path, { dot: true });
    } catch {
      return false;
    }
  }

  private validateLocalAfterRule(rule: VFSHookRule, label: string): void {
    const action = String(rule.action ?? "").trim().toLowerCase();
    if (action && action !== VFS_HOOK_ACTION_ALLOW) {
      throw new MatchlockError(
        `invalid vfs hook ${JSON.stringify(rule.name ?? "")}: ${label} cannot set action=${JSON.stringify(rule.action)}`,
      );
    }
    if (String(rule.phase ?? "").toLowerCase() !== VFS_HOOK_PHASE_AFTER) {
      throw new MatchlockError(
        `invalid vfs hook ${JSON.stringify(rule.name ?? "")}: ${label} must use phase=after`,
      );
    }
  }

  private ruleToWire(rule: VFSHookRule, normalizedAction: string): WireVFSHookRule {
    const wire: WireVFSHookRule = {
      action: normalizedAction,
    };

    if (rule.name) {
      wire.name = rule.name;
    }
    if (rule.phase) {
      wire.phase = rule.phase;
    }
    if (rule.ops && rule.ops.length > 0) {
      wire.ops = [...rule.ops];
    }
    if (rule.path) {
      wire.path = rule.path;
    }
    if ((rule.timeoutMs ?? 0) > 0) {
      wire.timeout_ms = rule.timeoutMs;
    }

    return wire;
  }

  readonly readOp = VFS_HOOK_OP_READ;
  readonly readdirOp = VFS_HOOK_OP_READDIR;
  readonly writeOp = VFS_HOOK_OP_WRITE;
}
