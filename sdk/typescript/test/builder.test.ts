import { describe, expect, it } from "vitest";
import { Sandbox, VFS_HOOK_ACTION_BLOCK, VFS_HOOK_OP_CREATE, VFS_HOOK_PHASE_BEFORE } from "../src";

describe("Sandbox builder", () => {
  it("sets image on construction", () => {
    const opts = new Sandbox("alpine:latest").options();
    expect(opts.image).toBe("alpine:latest");
  });

  it("chains resource options", () => {
    const opts = new Sandbox("img")
      .withCPUs(4)
      .withMemory(1024)
      .withDiskSize(2048)
      .withTimeout(300)
      .options();

    expect(opts.cpus).toBe(4);
    expect(opts.memoryMb).toBe(1024);
    expect(opts.diskSizeMb).toBe(2048);
    expect(opts.timeoutSeconds).toBe(300);
  });

  it("builds network options", () => {
    const opts = new Sandbox("img")
      .allowHost("api.openai.com", "pypi.org")
      .addHost("api.internal", "10.0.0.10")
      .addSecret("API_KEY", "secret", "api.openai.com")
      .withDNSServers("1.1.1.1", "8.8.8.8")
      .withHostname("sandbox-host")
      .withNetworkMTU(1200)
      .blockPrivateIPs()
      .options();

    expect(opts.allowedHosts).toEqual(["api.openai.com", "pypi.org"]);
    expect(opts.addHosts).toEqual([{ host: "api.internal", ip: "10.0.0.10" }]);
    expect(opts.secrets).toEqual([
      { name: "API_KEY", value: "secret", hosts: ["api.openai.com"] },
    ]);
    expect(opts.dnsServers).toEqual(["1.1.1.1", "8.8.8.8"]);
    expect(opts.hostname).toBe("sandbox-host");
    expect(opts.networkMtu).toBe(1200);
    expect(opts.blockPrivateIPs).toBe(true);
    expect(opts.blockPrivateIPsSet).toBe(true);
  });

  it("supports private ip override helpers", () => {
    const allow = new Sandbox("img").allowPrivateIPs().options();
    expect(allow.blockPrivateIPs).toBe(false);
    expect(allow.blockPrivateIPsSet).toBe(true);

    const unset = new Sandbox("img")
      .blockPrivateIPs()
      .unsetBlockPrivateIPs()
      .options();
    expect(unset.blockPrivateIPs).toBe(false);
    expect(unset.blockPrivateIPsSet).toBe(false);
  });

  it("builds mount and env config", () => {
    const opts = new Sandbox("img")
      .withEnv("FOO", "bar")
      .withEnvMap({ BAZ: "qux" })
      .mountHostDir("/data", "/host/data")
      .mountHostDirReadonly("/config", "/host/config")
      .mountMemory("/tmp")
      .mountOverlay("/workspace", "/host/workspace")
      .options();

    expect(opts.env).toEqual({ FOO: "bar", BAZ: "qux" });
    expect(opts.mounts).toEqual({
      "/data": { type: "host_fs", hostPath: "/host/data" },
      "/config": { type: "host_fs", hostPath: "/host/config", readonly: true },
      "/tmp": { type: "memory" },
      "/workspace": { type: "overlay", hostPath: "/host/workspace" },
    });
  });

  it("builds vfs interception and image config", () => {
    const opts = new Sandbox("img")
      .withVFSInterception({
        rules: [
          {
            phase: VFS_HOOK_PHASE_BEFORE,
            ops: [VFS_HOOK_OP_CREATE],
            path: "/workspace/blocked.txt",
            action: VFS_HOOK_ACTION_BLOCK,
          },
        ],
      })
      .withUser("nobody")
      .withEntrypoint("python", "main.py")
      .withImageConfig({ workingDir: "/workspace" })
      .options();

    expect(opts.vfsInterception?.rules?.[0].action).toBe("block");
    expect(opts.imageConfig).toEqual({
      user: "nobody",
      entrypoint: ["python", "main.py"],
      workingDir: "/workspace",
    });
  });

  it("supports port forward builder methods", () => {
    const opts = new Sandbox("img")
      .withPortForward(18080, 8080)
      .withPortForward(18443, 8443)
      .withPortForwardAddresses("127.0.0.1", "0.0.0.0")
      .options();

    expect(opts.portForwards).toEqual([
      { localPort: 18080, remotePort: 8080 },
      { localPort: 18443, remotePort: 8443 },
    ]);
    expect(opts.portForwardAddresses).toEqual(["127.0.0.1", "0.0.0.0"]);
  });
});
