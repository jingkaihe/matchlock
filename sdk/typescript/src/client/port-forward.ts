import { MatchlockError } from "../errors";
import type { PortForward } from "../types";

export function parsePortForwards(specs: string[]): PortForward[] {
  return specs.map((spec) => parsePortForward(spec));
}

function parsePortForward(spec: string): PortForward {
  const trimmed = spec.trim();
  if (!trimmed) {
    throw new MatchlockError("invalid port-forward spec: empty spec");
  }

  const parts = trimmed.split(":");
  if (parts.length === 1) {
    const remotePort = parsePort(parts[0], "remote");
    return { localPort: remotePort, remotePort };
  }
  if (parts.length === 2) {
    const localPort = parsePort(parts[0], "local");
    const remotePort = parsePort(parts[1], "remote");
    return { localPort, remotePort };
  }

  throw new MatchlockError(
    `invalid port-forward spec: ${JSON.stringify(trimmed)} (expected [LOCAL_PORT:]REMOTE_PORT)`,
  );
}

function parsePort(raw: string, role: "local" | "remote"): number {
  const value = raw.trim();
  if (!value) {
    throw new MatchlockError(`invalid port-forward spec: empty ${role} port`);
  }

  const port = Number.parseInt(value, 10);
  if (!Number.isFinite(port)) {
    throw new MatchlockError(`invalid port value ${JSON.stringify(value)}`);
  }
  if (port < 1 || port > 65535) {
    throw new MatchlockError(`invalid port value ${port}: must be in range 1-65535`);
  }

  return port;
}
