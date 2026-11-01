// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { hostApp } from "./desktopBridge";

describe("desktopBridge", () => {
  afterEach(() => {
    delete (window as unknown as { go?: unknown }).go;
  });

  it("没有 Wails 宿主时返回 null（浏览器直接打开开发服务器）", () => {
    expect(hostApp()).toBeNull();
  });

  it("有宿主时取 window.go.app.App", async () => {
    const failure = { reason: "启动失败", detail: "x", dataDir: "/d", logDir: "/d/logs" };
    (window as unknown as { go: unknown }).go = {
      app: { App: { GetStartupError: async () => failure, RetryStartup: async () => null } }
    };
    const host = hostApp();
    expect(host).not.toBeNull();
    expect(await host!.GetStartupError()).toEqual(failure);
    expect(await host!.RetryStartup()).toBeNull();
  });
});
