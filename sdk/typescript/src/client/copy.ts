import { isAbsolute, sep } from "node:path";
import { MatchlockError } from "../errors";
import type { CopyInOptions, CopyOutOptions } from "../types";
import type { JSONObject } from "./wire";

/**
 * Makes a host path absolute with a plain string join rather than
 * path.resolve, which would drop docker cp suffixes such as "/." and a
 * trailing "/".
 */
export function toAbsoluteHostPath(path: string): string {
  if (isAbsolute(path)) {
    return path;
  }
  return `${process.cwd()}${sep}${path}`;
}

export function buildCopyInParams(
  src: string,
  dest: string,
  options: CopyInOptions,
): JSONObject {
  validateCopyPaths(src, dest);
  validateSandboxPath(dest);
  if (options.owner && options.archive) {
    throw new MatchlockError("copy owner cannot be combined with archive");
  }

  const params: JSONObject = { src: toAbsoluteHostPath(src), dest };
  if (options.owner) {
    params.owner = options.owner;
  }
  if (options.archive) {
    params.archive = true;
  }
  addCommonCopyParams(params, options);
  return params;
}

export function buildCopyOutParams(
  src: string,
  dest: string,
  options: CopyOutOptions,
): JSONObject {
  validateCopyPaths(src, dest);
  validateSandboxPath(src);
  validateCopyLimit("maxSize", options.maxSize);
  validateCopyLimit("maxFiles", options.maxFiles);

  const params: JSONObject = { src, dest: toAbsoluteHostPath(dest) };
  addCommonCopyParams(params, options);
  if (options.maxSize !== undefined) {
    params.max_size = options.maxSize;
  }
  if (options.maxFiles !== undefined) {
    params.max_files = options.maxFiles;
  }
  return params;
}

function validateCopyPaths(src: string, dest: string): void {
  if (!src) {
    throw new MatchlockError("copy source path is required");
  }
  if (!dest) {
    throw new MatchlockError("copy destination path is required");
  }
}

function validateSandboxPath(path: string): void {
  if (!path.startsWith("/")) {
    throw new MatchlockError("copy sandbox path must be absolute");
  }
}

function validateCopyLimit(name: string, value: number | undefined): void {
  if (value !== undefined && (!Number.isSafeInteger(value) || value < 0)) {
    throw new MatchlockError(`copy ${name} must be a non-negative integer`);
  }
}

function addCommonCopyParams(
  params: JSONObject,
  options: { followLink?: boolean; exclude?: string[] },
): void {
  if (options.followLink) {
    params.follow_link = true;
  }
  if ((options.exclude?.length ?? 0) > 0) {
    params.exclude = [...(options.exclude ?? [])];
  }
}
