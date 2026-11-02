import { describe, expect, it } from "vitest";
import type { HostUpdateInfo } from "./desktopBridge";
import { updateAction, updateSummary } from "./updateStatus";

const base: HostUpdateInfo = { current: "0.1.0", commit: "", buildTime: "", latest: "v0.2.0", notes: "", pageUrl: "https://example.invalid/r", available: true, mode: "auto", message: "" };

describe("updateStatus", () => {
  it("有新版：auto 给安装，manual 给打开下载页", () => {
    expect(updateAction(base)).toBe("install");
    expect(updateAction({ ...base, mode: "manual" })).toBe("open-page");
    expect(updateAction({ ...base, mode: "manual", pageUrl: "" })).toBe("none");
    expect(updateSummary(base)).toContain("v0.2.0");
  });

  it("没有新版或检查不了时不给按钮，原样说明原因", () => {
    const none = { ...base, available: false, message: "无法检查更新（仓库未公开或尚无发布）" };
    expect(updateAction(none)).toBe("none");
    expect(updateSummary(none)).toBe(none.message);
    expect(updateSummary({ ...base, available: false, message: "" })).toBe("已是最新版本");
    expect(updateAction(null)).toBe("none");
  });
});
