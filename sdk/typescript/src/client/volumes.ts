import { MatchlockError } from "../errors";
import type { VolumeInfo } from "../types";
import { asObject, asString, toError } from "./utils";
import type { JSONValue } from "./wire";

export type ExecCLIFunc = (
  args: string[],
) => Promise<{ stdout: string; stderr: string }>;

export async function volumeCreate(
  execCLI: ExecCLIFunc,
  name: string,
  sizeMb: number,
): Promise<VolumeInfo> {
  const trimmed = name.trim();
  if (!trimmed) {
    throw new MatchlockError("volume name is required");
  }
  if (!Number.isFinite(sizeMb) || sizeMb <= 0) {
    throw new MatchlockError("volume size must be > 0");
  }

  let stdout = "";
  try {
    ({ stdout } = await execCLI([
      "volume",
      "create",
      trimmed,
      "--size",
      String(sizeMb),
      "--json",
    ]));
  } catch (error) {
    const err = toError(error);
    throw new MatchlockError(`matchlock volume create ${trimmed}: ${err.message}`);
  }

  let decoded: JSONValue;
  try {
    decoded = JSON.parse(stdout) as JSONValue;
  } catch (error) {
    const err = toError(error);
    throw new MatchlockError(`failed to parse volume create output: ${err.message}`);
  }

  const data = asObject(decoded);
  const path = asString(data.path).trim();
  if (!path) {
    throw new MatchlockError("failed to parse volume create output: missing Path");
  }

  return {
    name: asString(data.name) || trimmed,
    size: asString(data.size) || `${sizeMb.toFixed(1)} MB`,
    path,
  };
}

export async function volumeList(execCLI: ExecCLIFunc): Promise<VolumeInfo[]> {
  let stdout = "";
  try {
    ({ stdout } = await execCLI(["volume", "ls", "--json"]));
  } catch (error) {
    const err = toError(error);
    throw new MatchlockError(`matchlock volume ls: ${err.message}`);
  }

  let decoded: JSONValue;
  try {
    decoded = JSON.parse(stdout) as JSONValue;
  } catch (error) {
    const err = toError(error);
    throw new MatchlockError(`failed to parse volume list output: ${err.message}`);
  }
  if (!Array.isArray(decoded)) {
    throw new MatchlockError("failed to parse volume list output: expected array");
  }

  const volumes: VolumeInfo[] = [];
  for (const entry of decoded) {
    const data = asObject(entry);
    const name = asString(data.name).trim();
    const size = asString(data.size).trim();
    const path = asString(data.path).trim();
    if (!name || !path) {
      throw new MatchlockError("failed to parse volume list output: missing required fields");
    }
    volumes.push({ name, size, path });
  }
  return volumes;
}

export async function volumeRemove(execCLI: ExecCLIFunc, name: string): Promise<void> {
  const trimmed = name.trim();
  if (!trimmed) {
    throw new MatchlockError("volume name is required");
  }

  try {
    await execCLI(["volume", "rm", trimmed]);
  } catch (error) {
    const err = toError(error);
    throw new MatchlockError(`matchlock volume rm ${trimmed}: ${err.message}`);
  }
}
