import { describe, expect, it } from "vitest";
import {
  NETWORK_HOOK_ACTION_MUTATE,
  NETWORK_HOOK_PHASE_AFTER,
  Sandbox,
} from "../src";

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

  it("builds kernel config", () => {
    const opts = new Sandbox("img").withKernel("file:///tmp/vmlinux").options();

    expect(opts.kernelRef).toBe("file:///tmp/vmlinux");
  });

  it("builds network options", () => {
    const opts = new Sandbox("img")
      .allowHost("api.openai.com", "pypi.org")
      .addHost("api.internal", "10.0.0.10")
      .addSecret("API_KEY", "secret", "api.openai.com")
      .addSecretWithPlaceholder(
        "GH_TOKEN",
        "gho_real_token",
        "gho_sandbox_placeholder",
        "github.com",
      )
      .withDNSServers("1.1.1.1", "8.8.8.8")
      .withHostname("sandbox-host")
      .withNetworkMTU(1200)
      .blockPrivateIPs()
      .options();

    expect(opts.allowedHosts).toEqual(["api.openai.com", "pypi.org"]);
    expect(opts.addHosts).toEqual([{ host: "api.internal", ip: "10.0.0.10" }]);
    expect(opts.secrets).toEqual([
      { name: "API_KEY", value: "secret", hosts: ["api.openai.com"] },
      {
        name: "GH_TOKEN",
        value: "gho_real_token",
        placeholder: "gho_sandbox_placeholder",
        hosts: ["github.com"],
      },
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

  it("supports no-network option", () => {
    const opts = new Sandbox("img").withNoNetwork().options();
    expect(opts.noNetwork).toBe(true);
  });

  it("builds network interception options", () => {
    const opts = new Sandbox("img")
      .withNetworkInterception({
        rules: [
          {
            phase: NETWORK_HOOK_PHASE_AFTER,
            hosts: ["api.example.com"],
            action: NETWORK_HOOK_ACTION_MUTATE,
            bodyReplacements: [{ find: "foo", replace: "bar" }],
          },
        ],
      })
      .options();

    expect(opts.forceInterception).toBe(true);
    expect(opts.networkInterception?.rules?.[0]).toEqual({
      phase: "after",
      hosts: ["api.example.com"],
      action: "mutate",
      bodyReplacements: [{ find: "foo", replace: "bar" }],
    });
  });

  it("builds env config", () => {
    const opts = new Sandbox("img")
      .withEnv("FOO", "bar")
      .withEnvMap({ BAZ: "qux" })
      .options();

    expect(opts.env).toEqual({ FOO: "bar", BAZ: "qux" });
  });

  it("builds image config", () => {
    const opts = new Sandbox("img")
      .withUser("nobody")
      .withEntrypoint("python", "main.py")
      .withImageConfig({ workingDir: "/workspace" })
      .options();

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
