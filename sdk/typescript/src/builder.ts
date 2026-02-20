import type {
  CreateOptions,
  ImageConfig,
  MountConfig,
  Secret,
  VFSInterceptionConfig,
} from "./types";

export function cloneCreateOptions(opts: CreateOptions): CreateOptions {
  return {
    ...opts,
    allowedHosts: opts.allowedHosts ? [...opts.allowedHosts] : undefined,
    addHosts: opts.addHosts ? opts.addHosts.map((x) => ({ ...x })) : undefined,
    mounts: opts.mounts
      ? Object.fromEntries(
          Object.entries(opts.mounts).map(([k, v]) => [k, { ...v }]),
        )
      : undefined,
    env: opts.env ? { ...opts.env } : undefined,
    secrets: opts.secrets
      ? opts.secrets.map((s) => ({ ...s, hosts: s.hosts ? [...s.hosts] : undefined }))
      : undefined,
    vfsInterception: opts.vfsInterception
      ? {
          emitEvents: opts.vfsInterception.emitEvents,
          rules: opts.vfsInterception.rules
            ? opts.vfsInterception.rules.map((r) => ({ ...r, ops: r.ops ? [...r.ops] : undefined }))
            : undefined,
        }
      : undefined,
    dnsServers: opts.dnsServers ? [...opts.dnsServers] : undefined,
    portForwards: opts.portForwards
      ? opts.portForwards.map((pf) => ({ ...pf }))
      : undefined,
    portForwardAddresses: opts.portForwardAddresses
      ? [...opts.portForwardAddresses]
      : undefined,
    imageConfig: opts.imageConfig
      ? {
          ...opts.imageConfig,
          entrypoint: opts.imageConfig.entrypoint
            ? [...opts.imageConfig.entrypoint]
            : undefined,
          cmd: opts.imageConfig.cmd ? [...opts.imageConfig.cmd] : undefined,
          env: opts.imageConfig.env ? { ...opts.imageConfig.env } : undefined,
        }
      : undefined,
  };
}

export class Sandbox {
  private readonly opts: CreateOptions;

  constructor(image: string) {
    this.opts = { image };
  }

  withPrivileged(): Sandbox {
    this.opts.privileged = true;
    return this;
  }

  withCPUs(cpus: number): Sandbox {
    this.opts.cpus = cpus;
    return this;
  }

  withMemory(mb: number): Sandbox {
    this.opts.memoryMb = mb;
    return this;
  }

  withDiskSize(mb: number): Sandbox {
    this.opts.diskSizeMb = mb;
    return this;
  }

  withTimeout(seconds: number): Sandbox {
    this.opts.timeoutSeconds = seconds;
    return this;
  }

  withWorkspace(path: string): Sandbox {
    this.opts.workspace = path;
    return this;
  }

  withVFSInterception(config: VFSInterceptionConfig): Sandbox {
    this.opts.vfsInterception = config;
    return this;
  }

  withEnv(name: string, value: string): Sandbox {
    this.opts.env = this.opts.env ?? {};
    this.opts.env[name] = value;
    return this;
  }

  withEnvMap(env: Record<string, string>): Sandbox {
    this.opts.env = this.opts.env ?? {};
    Object.assign(this.opts.env, env);
    return this;
  }

  allowHost(...hosts: string[]): Sandbox {
    this.opts.allowedHosts = this.opts.allowedHosts ?? [];
    this.opts.allowedHosts.push(...hosts);
    return this;
  }

  addHost(host: string, ip: string): Sandbox {
    this.opts.addHosts = this.opts.addHosts ?? [];
    this.opts.addHosts.push({ host, ip });
    return this;
  }

  blockPrivateIPs(): Sandbox {
    return this.withBlockPrivateIPs(true);
  }

  withBlockPrivateIPs(enabled: boolean): Sandbox {
    this.opts.blockPrivateIPs = enabled;
    this.opts.blockPrivateIPsSet = true;
    return this;
  }

  allowPrivateIPs(): Sandbox {
    return this.withBlockPrivateIPs(false);
  }

  unsetBlockPrivateIPs(): Sandbox {
    this.opts.blockPrivateIPs = false;
    this.opts.blockPrivateIPsSet = false;
    return this;
  }

  addSecret(name: string, value: string, ...hosts: string[]): Sandbox {
    this.opts.secrets = this.opts.secrets ?? [];
    const secret: Secret = { name, value, hosts };
    this.opts.secrets.push(secret);
    return this;
  }

  withDNSServers(...servers: string[]): Sandbox {
    this.opts.dnsServers = this.opts.dnsServers ?? [];
    this.opts.dnsServers.push(...servers);
    return this;
  }

  withHostname(hostname: string): Sandbox {
    this.opts.hostname = hostname;
    return this;
  }

  withNetworkMTU(mtu: number): Sandbox {
    this.opts.networkMtu = mtu;
    return this;
  }

  withPortForward(localPort: number, remotePort: number): Sandbox {
    this.opts.portForwards = this.opts.portForwards ?? [];
    this.opts.portForwards.push({ localPort, remotePort });
    return this;
  }

  withPortForwardAddresses(...addresses: string[]): Sandbox {
    this.opts.portForwardAddresses = this.opts.portForwardAddresses ?? [];
    this.opts.portForwardAddresses.push(...addresses);
    return this;
  }

  mount(guestPath: string, config: MountConfig): Sandbox {
    this.opts.mounts = this.opts.mounts ?? {};
    this.opts.mounts[guestPath] = { ...config };
    return this;
  }

  mountHostDir(guestPath: string, hostPath: string): Sandbox {
    return this.mount(guestPath, { type: "host_fs", hostPath });
  }

  mountHostDirReadonly(guestPath: string, hostPath: string): Sandbox {
    return this.mount(guestPath, { type: "host_fs", hostPath, readonly: true });
  }

  mountMemory(guestPath: string): Sandbox {
    return this.mount(guestPath, { type: "memory" });
  }

  mountOverlay(guestPath: string, hostPath: string): Sandbox {
    return this.mount(guestPath, { type: "overlay", hostPath });
  }

  withUser(user: string): Sandbox {
    this.opts.imageConfig = this.opts.imageConfig ?? {};
    this.opts.imageConfig.user = user;
    return this;
  }

  withEntrypoint(...entrypoint: string[]): Sandbox {
    this.opts.imageConfig = this.opts.imageConfig ?? {};
    this.opts.imageConfig.entrypoint = [...entrypoint];
    return this;
  }

  withImageConfig(config: ImageConfig | undefined | null): Sandbox {
    if (!config) {
      return this;
    }
    this.opts.imageConfig = this.opts.imageConfig ?? {};

    if (config.user) {
      this.opts.imageConfig.user = config.user;
    }
    if (config.workingDir) {
      this.opts.imageConfig.workingDir = config.workingDir;
    }
    if (config.entrypoint && config.entrypoint.length > 0) {
      this.opts.imageConfig.entrypoint = [...config.entrypoint];
    }
    if (config.cmd && config.cmd.length > 0) {
      this.opts.imageConfig.cmd = [...config.cmd];
    }
    if (config.env && Object.keys(config.env).length > 0) {
      this.opts.imageConfig.env = { ...config.env };
    }
    return this;
  }

  options(): CreateOptions {
    return cloneCreateOptions(this.opts);
  }
}

export function createSandbox(image: string): Sandbox {
  return new Sandbox(image);
}
