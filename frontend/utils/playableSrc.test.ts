import { describe, expect, it } from "vitest";
import { needsBlobPlayback } from "./playableSrc";

describe("needsBlobPlayback", () => {
  it("只有 Linux 桌面上的直接地址要取成 blob", () => {
    expect(needsBlobPlayback(true, "/api/v1/marketing/options/a/preview")).toBe(true);
    expect(needsBlobPlayback(true, "https://example.invalid/v.mp4")).toBe(true);
    expect(needsBlobPlayback(false, "/api/v1/marketing/options/a/preview")).toBe(false);
  });

  it("已经是 blob / data 地址或为空时不动", () => {
    expect(needsBlobPlayback(true, "blob:tabtv/123")).toBe(false);
    expect(needsBlobPlayback(true, "data:video/mp4;base64,AAAA")).toBe(false);
    expect(needsBlobPlayback(true, "")).toBe(false);
    expect(needsBlobPlayback(true, undefined)).toBe(false);
  });
});
