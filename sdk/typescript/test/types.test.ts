import { describe, expect, it } from "vitest";
import {
  defaultConfig,
  MatchlockError,
  NETWORK_HOOK_ACTION_ALLOW,
  NETWORK_HOOK_ACTION_BLOCK,
  NETWORK_HOOK_ACTION_MUTATE,
  NETWORK_HOOK_PHASE_AFTER,
  NETWORK_HOOK_PHASE_BEFORE,
  RPCError,
  VFS_HOOK_ACTION_ALLOW,
  VFS_HOOK_ACTION_BLOCK,
  VFS_HOOK_OP_CREATE,
  VFS_HOOK_OP_WRITE,
  VFS_HOOK_PHASE_AFTER,
  VFS_HOOK_PHASE_BEFORE,
} from "../src";

describe("types and errors", () => {
  it("exports vfs constants", () => {
    expect(VFS_HOOK_PHASE_BEFORE).toBe("before");
    expect(VFS_HOOK_PHASE_AFTER).toBe("after");
    expect(VFS_HOOK_OP_CREATE).toBe("create");
    expect(VFS_HOOK_OP_WRITE).toBe("write");
    expect(VFS_HOOK_ACTION_ALLOW).toBe("allow");
    expect(VFS_HOOK_ACTION_BLOCK).toBe("block");
  });

  it("exports network hook constants", () => {
    expect(NETWORK_HOOK_PHASE_BEFORE).toBe("before");
    expect(NETWORK_HOOK_PHASE_AFTER).toBe("after");
    expect(NETWORK_HOOK_ACTION_ALLOW).toBe("allow");
    expect(NETWORK_HOOK_ACTION_BLOCK).toBe("block");
    expect(NETWORK_HOOK_ACTION_MUTATE).toBe("mutate");
  });

  it("implements rpc error helpers", () => {
    const vm = new RPCError(RPCError.VM_FAILED, "vm failed");
    expect(vm).toBeInstanceOf(MatchlockError);
    expect(vm.message).toBe("[-32000] vm failed");
    expect(vm.isVMError()).toBe(true);
    expect(vm.isExecError()).toBe(false);
    expect(vm.isFileError()).toBe(false);

    const exec = new RPCError(RPCError.EXEC_FAILED, "exec failed");
    expect(exec.isExecError()).toBe(true);

    const file = new RPCError(RPCError.FILE_FAILED, "file failed");
    expect(file.isFileError()).toBe(true);
  });

  it("builds default config", () => {
    expect(defaultConfig()).toEqual({
      binaryPath: "matchlock",
      useSudo: false,
    });
  });
});
