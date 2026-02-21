import { describe, expect, it } from "vitest";

import { formatCreatedAt, shortDigest, statusTone } from "./utils";

describe("shortDigest", () => {
  it("returns dash when digest is empty", () => {
    expect(shortDigest("")).toBe("-");
  });

  it("keeps short digests as-is", () => {
    expect(shortDigest("sha256:abc123")).toBe("sha256:abc123");
  });

  it("truncates long digests", () => {
    expect(shortDigest("sha256:1234567890abcdef1234567890abcdef")).toBe("sha256:123456789...");
  });
});

describe("statusTone", () => {
  it("maps running", () => {
    expect(statusTone("running")).toBe("running");
  });

  it("maps crashed", () => {
    expect(statusTone("crashed")).toBe("crashed");
  });

  it("defaults unknown status to stopped", () => {
    expect(statusTone("unknown")).toBe("stopped");
  });
});

describe("formatCreatedAt", () => {
  it("returns dash for invalid timestamps", () => {
    expect(formatCreatedAt("not-a-date")).toBe("-");
  });

  it("formats valid timestamps", () => {
    const value = formatCreatedAt("2026-02-20T12:34:56Z");
    expect(value).not.toBe("-");
  });
});
