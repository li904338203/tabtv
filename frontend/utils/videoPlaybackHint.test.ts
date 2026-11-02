import { describe, expect, it } from "vitest";
import {
  MEDIA_ERR_ABORTED, MEDIA_ERR_DECODE, MEDIA_ERR_NETWORK, MEDIA_ERR_SRC_NOT_SUPPORTED,
  isLinuxDesktop, shouldHintVideoDecoder
} from "./videoPlaybackHint";

describe("videoPlaybackHint", () => {
  it("Linux 上解码失败或格式不支持时提示", () => {
    expect(shouldHintVideoDecoder(true, MEDIA_ERR_DECODE)).toBe(true);
    expect(shouldHintVideoDecoder(true, MEDIA_ERR_SRC_NOT_SUPPORTED)).toBe(true);
  });

  it("macOS / Windows 上同样的错误不提示", () => {
    expect(shouldHintVideoDecoder(false, MEDIA_ERR_DECODE)).toBe(false);
    expect(shouldHintVideoDecoder(false, MEDIA_ERR_SRC_NOT_SUPPORTED)).toBe(false);
  });

  it("Linux 上网络错误、中止与未知错误不提示", () => {
    expect(shouldHintVideoDecoder(true, MEDIA_ERR_NETWORK)).toBe(false);
    expect(shouldHintVideoDecoder(true, MEDIA_ERR_ABORTED)).toBe(false);
    expect(shouldHintVideoDecoder(true, undefined)).toBe(false);
  });

  it("按 userAgent 认 Linux 桌面，排除 Android", () => {
    expect(isLinuxDesktop("Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/605.1.15")).toBe(true);
    expect(isLinuxDesktop("Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36")).toBe(false);
    expect(isLinuxDesktop("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15")).toBe(false);
    expect(isLinuxDesktop("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Edg/120")).toBe(false);
  });
});
