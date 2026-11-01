import { describe, expect, it } from "vitest";
import {
  smartcutCloseBlocker,
  smartcutHistoryDisabledReason,
  smartcutMissingText,
  smartcutSnapshotLabel,
  smartcutToolDisabledReason,
  smartcutTrackLabel
} from "./smartcutStudio";

describe("smartcutStudio", () => {
  it("smartcutToolDisabledReason：只读排在「没有选中」之前", () => {
    expect(smartcutToolDisabledReason("delete", { readonlyReason: "role", hasSelection: false, playheadInTarget: false })).toMatch(/只读/);
    expect(smartcutToolDisabledReason("delete", { readonlyReason: null, hasSelection: false, playheadInTarget: false })).toBe("先选中一个片段");
    expect(smartcutToolDisabledReason("split", { readonlyReason: null, hasSelection: true, playheadInTarget: false })).toBe("把播放头移到要处理的片段中间");
    expect(smartcutToolDisabledReason("split", { readonlyReason: null, hasSelection: false, playheadInTarget: true })).toBe("");
  });

  it("smartcutHistoryDisabledReason / smartcutCloseBlocker", () => {
    expect(smartcutHistoryDisabledReason({ readonlyReason: null, available: false, kind: "undo" })).toBe("没有可以撤销的操作");
    expect(smartcutCloseBlocker("conflict")).toMatch(/冲突/);
    expect(smartcutCloseBlocker("idle")).toBeNull();
  });

  it("smartcutTrackLabel：起过名用名字，否则按类型与顺序", () => {
    expect(smartcutTrackLabel({ kind: "video" }, 1)).toBe("画面");
    expect(smartcutTrackLabel({ kind: "audio", role: "music" }, 2)).toBe("音频 2 · 配乐");
    expect(smartcutTrackLabel({ kind: "subtitle", lang: "en" }, 1)).toBe("字幕 · 英文");
    expect(smartcutTrackLabel({ kind: "subtitle", lang: "fr-FR" }, 2)).toBe("字幕 · fr-FR");
    expect(smartcutTrackLabel({ kind: "text", name: " 片头 " }, 1)).toBe("片头");
  });

  it("smartcutMissingText：画面轨说第几段，其它轨带轨道名", () => {
    const timeline = { tracks: [
      { kind: "video" as const, clips: [{ id: "a" }, { id: "b" }, { id: "c" }] },
      { kind: "audio" as const, role: "music" as const, clips: [{ id: "x" }] }
    ] };
    expect(smartcutMissingText(timeline, [{ clipId: "c" }, { clipId: "x" }])).toBe("第 3 段、音频 1 · 配乐第 1 段缺素材（素材已被删除），导出前请替换或删除");
    expect(smartcutMissingText(timeline, [])).toBe("");
    expect(smartcutSnapshotLabel("ai_plan")).toBe("AI 剪辑之前");
    expect(smartcutSnapshotLabel("whatever")).toBe("自动存档");
  });
});
