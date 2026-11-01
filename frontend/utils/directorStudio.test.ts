import { describe, expect, it } from "vitest";
import { directorCloseBlocker, directorHistoryDisabledReason, directorReadonlyText, directorSaveStateView } from "./directorStudio";

describe("编辑器界面", () => {
  it("六种保存状态各有文案，冲突 / 离线 / 失败醒目且能点开提示条（DR-UX-08）", () => {
    expect((["idle", "dirty", "saving", "error", "conflict", "offline"] as const).map(state => directorSaveStateView(state).label))
      .toEqual(["已保存", "未保存", "保存中", "保存失败", "有冲突", "离线"]);
    expect(directorSaveStateView("conflict")).toEqual({ label: "有冲突", tone: "alert", actionable: true });
    expect(directorSaveStateView("saving").actionable).toBe(false);
  });

  it("撤销按钮置灰的理由：只读 → 提案中 → 没有可撤销的", () => {
    expect(directorHistoryDisabledReason({ readonlyReason: "role", proposal: true, available: false, kind: "undo" })).toBe("你在该工作区是只读");
    expect(directorHistoryDisabledReason({ readonlyReason: null, proposal: true, available: true, kind: "undo" })).toBe("请先应用或放弃 AI 提案");
    expect(directorHistoryDisabledReason({ readonlyReason: null, proposal: false, available: false, kind: "redo" })).toBe("没有可以重做的操作");
    expect(directorHistoryDisabledReason({ readonlyReason: null, proposal: false, available: true, kind: "undo" })).toBe("");
    expect(directorReadonlyText("newer")).toContain("刷新页面");
    expect(directorReadonlyText(null)).toBe("");
  });

  it("冲突、离线、保存失败时不能直接返回画布（DR-F-154）", () => {
    expect(directorCloseBlocker("conflict")).toBeTruthy();
    expect(directorCloseBlocker("offline")).toBeTruthy();
    expect(directorCloseBlocker("error")).toBeTruthy();
    for (const state of ["idle", "dirty", "saving"] as const) expect(directorCloseBlocker(state)).toBeNull();
  });
});
