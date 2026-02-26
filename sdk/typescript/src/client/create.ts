import { MatchlockError } from "../errors";
import type { CreateOptions } from "../types";
import {
  DEFAULT_CPUS,
  DEFAULT_DISK_SIZE_MB,
  DEFAULT_MEMORY_MB,
  DEFAULT_TIMEOUT_SECONDS,
} from "./utils";
import type {
  JSONObject,
  JSONValue,
  WireNetworkInterceptionConfig,
  WireVFSInterceptionConfig,
} from "./wire";

export function validateCreateOptions(options: CreateOptions): void {
  if (!options.image) {
    throw new MatchlockError("image is required (e.g., alpine:latest)");
  }
  if ((options.networkMtu ?? 0) < 0) {
    throw new MatchlockError("network mtu must be > 0");
  }
  if (
    options.noNetwork &&
    ((options.allowedHosts?.length ?? 0) > 0 ||
      (options.secrets?.length ?? 0) > 0 ||
      options.forceInterception === true ||
      options.networkInterception !== undefined)
  ) {
    throw new MatchlockError(
      "no network cannot be combined with allowed hosts, secrets, forced interception, or network interception rules",
    );
  }
}

export function buildCreateParams(
  options: CreateOptions,
  wireVFS: WireVFSInterceptionConfig | undefined,
  wireNetworkInterception: WireNetworkInterceptionConfig | undefined,
): JSONObject {
  const resources = {
    cpus: options.cpus || DEFAULT_CPUS,
    memory_mb: options.memoryMb || DEFAULT_MEMORY_MB,
    disk_size_mb: options.diskSizeMb || DEFAULT_DISK_SIZE_MB,
    timeout_seconds: options.timeoutSeconds || DEFAULT_TIMEOUT_SECONDS,
  };

  const params: JSONObject = {
    image: options.image ?? "",
    resources,
  };

  if (options.privileged) {
    params.privileged = true;
  }

  const network = buildCreateNetworkParams(options, wireNetworkInterception);
  if (network) {
    params.network = network;
  }

  if (
    (options.mounts && Object.keys(options.mounts).length > 0) ||
    options.workspace ||
    wireVFS
  ) {
    const vfs: JSONObject = {};
    if (options.mounts && Object.keys(options.mounts).length > 0) {
      const mounts: JSONObject = {};
      for (const [guestPath, config] of Object.entries(options.mounts)) {
        const mount: JSONObject = {
          type: config.type ?? "memory",
        };
        if (config.hostPath) {
          mount.host_path = config.hostPath;
        }
        if (config.readonly) {
          mount.readonly = true;
        }
        mounts[guestPath] = mount;
      }
      vfs.mounts = mounts;
    }
    if (options.workspace) {
      vfs.workspace = options.workspace;
    }
    if (wireVFS) {
      vfs.interception = wireVFS as unknown as JSONValue;
    }
    params.vfs = vfs;
  }

  if (options.env && Object.keys(options.env).length > 0) {
    params.env = options.env;
  }

  if (options.imageConfig) {
    const imageConfig: JSONObject = {};
    if (options.imageConfig.user) {
      imageConfig.user = options.imageConfig.user;
    }
    if (options.imageConfig.workingDir) {
      imageConfig.working_dir = options.imageConfig.workingDir;
    }
    if (options.imageConfig.entrypoint) {
      imageConfig.entrypoint = [...options.imageConfig.entrypoint];
    }
    if (options.imageConfig.cmd) {
      imageConfig.cmd = [...options.imageConfig.cmd];
    }
    if (options.imageConfig.env) {
      imageConfig.env = { ...options.imageConfig.env };
    }
    params.image_config = imageConfig;
  }

  return params;
}

function resolveCreateBlockPrivateIPs(opts: CreateOptions): {
  value: boolean;
  hasOverride: boolean;
} {
  if (opts.blockPrivateIPsSet) {
    return { value: !!opts.blockPrivateIPs, hasOverride: true };
  }
  if (opts.blockPrivateIPs) {
    return { value: true, hasOverride: true };
  }
  return { value: false, hasOverride: false };
}

function buildCreateNetworkParams(
  opts: CreateOptions,
  wireInterception: WireNetworkInterceptionConfig | undefined,
): JSONObject | undefined {
  const hasAllowedHosts = (opts.allowedHosts?.length ?? 0) > 0;
  const hasAddHosts = (opts.addHosts?.length ?? 0) > 0;
  const hasSecrets = (opts.secrets?.length ?? 0) > 0;
  const hasDNSServers = (opts.dnsServers?.length ?? 0) > 0;
  const hasHostname = (opts.hostname?.length ?? 0) > 0;
  const hasMTU = (opts.networkMtu ?? 0) > 0;
  const hasNoNetwork = opts.noNetwork === true;
  const hasForceInterception = opts.forceInterception === true;
  const hasNetworkInterception = wireInterception !== undefined;

  const blockPrivate = resolveCreateBlockPrivateIPs(opts);

  const includeNetwork =
    hasAllowedHosts ||
    hasAddHosts ||
    hasSecrets ||
    hasDNSServers ||
    hasHostname ||
    hasMTU ||
    hasNoNetwork ||
    blockPrivate.hasOverride ||
    hasForceInterception ||
    hasNetworkInterception;

  if (!includeNetwork) {
    return undefined;
  }

  if (hasNoNetwork) {
    const network: JSONObject = {
      no_network: true,
    };
    if (hasAddHosts) {
      network.add_hosts = (opts.addHosts ?? []).map((mapping) => ({
        host: mapping.host,
        ip: mapping.ip,
      }));
    }
    if (hasDNSServers) {
      network.dns_servers = opts.dnsServers ?? [];
    }
    if (hasHostname) {
      network.hostname = opts.hostname ?? "";
    }
    return network;
  }

  const network: JSONObject = {
    allowed_hosts: opts.allowedHosts ?? [],
    block_private_ips: blockPrivate.hasOverride ? blockPrivate.value : true,
  };
  if (hasForceInterception || hasNetworkInterception) {
    network.intercept = true;
  }
  if (hasNetworkInterception) {
    network.interception = wireInterception as unknown as JSONValue;
  }

  if (hasAddHosts) {
    network.add_hosts = (opts.addHosts ?? []).map((mapping) => ({
      host: mapping.host,
      ip: mapping.ip,
    }));
  }

  if (hasSecrets) {
    const secrets: JSONObject = {};
    for (const secret of opts.secrets ?? []) {
      secrets[secret.name] = {
        value: secret.value,
        hosts: secret.hosts ?? [],
      };
    }
    network.secrets = secrets;
  }

  if (hasDNSServers) {
    network.dns_servers = opts.dnsServers ?? [];
  }

  if (hasHostname) {
    network.hostname = opts.hostname ?? "";
  }

  if (hasMTU) {
    network.mtu = opts.networkMtu ?? 0;
  }

  return network;
}
