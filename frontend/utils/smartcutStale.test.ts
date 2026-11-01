import type { components } from "@tabtv/api-client";
import { describe, expect, it } from "vitest";
import { smartcutNodeBadges, smartcutNodeState, smartcutStageLabel } from "./smartcutStale";

type SmartcutSummary = components["schemas"]["SmartcutSummary"];
type RenderBrief = components["schemas"]["SmartcutRenderBrief"];

function summary(over: Partial<SmartcutSummary> = {}): SmartcutSummary {
  return {
    timelineId: "9", canvasNodeId: "sc1", version: 5, isArranged: false, totalMs: 12000, effectiveMs: 11000,
    clipCount: 3, inputsStale: false, renderStale: false, strip: [], ...over
  };
}
const render = (over: Partial<RenderBrief> = {}): RenderBrief => ({ id: "70", status: "success", progress: 100, timelineVersion: 5, ...over });

describe("节点卡的七种状态（SC-F-02）", () => {
  it("空：没有片段，或还没取到摘要", () => {
    expect(smartcutNodeState(summary({ clipCount: 0 }))).toMatchObject({ kind: "empty", label: "空" });
    expect(smartcutNodeState(undefined)).toEqual({ kind: "empty", label: "空", badges: [] });
  });

  it("已铺好（连线即入轨，还没剪过）与已剪好未导出", () => {
    expect(smartcutNodeState(summary())).toMatchObject({ kind: "laid", label: "已铺好" });
    expect(smartcutNodeState(summary({ isArranged: true }))).toMatchObject({ kind: "edited", label: "已剪好，未导出" });
  });

  it("AI 剪辑中带阶段：同步素材、理解素材、规划、装配", () => {
    const at = (stage: "sync" | "understand" | "plan" | "compile" | "apply") =>
      smartcutNodeState(summary({ activeCall: { id: "1", kind: "plan", stage, startedAt: "2026-10-27T00:00:00Z" } })).label;
    expect([at("sync"), at("understand"), at("plan"), at("compile"), at("apply")]).toEqual([
      "AI 剪辑中 · 同步素材", "AI 剪辑中 · 理解素材", "AI 剪辑中 · 规划", "AI 剪辑中 · 装配", "AI 剪辑中 · 装配"
    ]);
    expect(smartcutNodeState(summary({ activeCall: { kind: "translate" } }))).toMatchObject({ kind: "ai", label: "翻译字幕中" });
  });

  it("导出中带百分比；排队时说排队", () => {
    expect(smartcutNodeState(summary({ latestRender: render({ status: "running", progress: 42 }) })))
      .toMatchObject({ kind: "exporting", label: "导出中 42%", progress: 42 });
    expect(smartcutNodeState(summary({ latestRender: render({ status: "queued", progress: 0 }) })).label).toBe("导出排队中");
  });

  it("已导出；失败带原因", () => {
    expect(smartcutNodeState(summary({ isArranged: true, latestRender: render() }))).toMatchObject({ kind: "exported", label: "已导出" });
    expect(smartcutNodeState(summary({ latestRender: render({ status: "failed", errorMessage: "第 2 段素材已被删除" }) })))
      .toMatchObject({ kind: "failed", label: "导出失败：第 2 段素材已被删除", detail: "第 2 段素材已被删除" });
    expect(smartcutNodeState(summary({ latestRender: render({ status: "failed" }) })).label).toBe("导出失败：导出失败");
  });

  // 失败之后又改过时间线：那次失败已经不是最新的消息；取消也不算失败。
  it("针对旧版本的失败与取消的导出不占卡片，回到内容状态", () => {
    expect(smartcutNodeState(summary({ isArranged: true, latestRender: render({ status: "failed", timelineVersion: 4 }) })).kind).toBe("edited");
    expect(smartcutNodeState(summary({ latestRender: render({ status: "cancelled", progress: 30 }) })).kind).toBe("laid");
  });

  it("优先级：AI 剪辑中压过导出中，导出中压过失败与已导出", () => {
    const busy = summary({ activeCall: { kind: "edit", stage: "plan" }, latestRender: render({ status: "running", progress: 10 }) });
    expect(smartcutNodeState(busy).kind).toBe("ai");
    expect(smartcutNodeState(summary({ latestRender: render({ status: "running", progress: 10 }) })).kind).toBe("exporting");
  });
});

describe("两个角标（只看摘要，不自己判过期）", () => {
  it("素材有变化、剪辑已改成片未更新", () => {
    expect(smartcutNodeBadges(summary({ inputsStale: true, renderStale: true })).map(b => b.label))
      .toEqual(["素材有变化", "剪辑已改，成片未更新"]);
    expect(smartcutNodeBadges(summary())).toEqual([]);
  });

  // 已导出之后改了时间线：状态仍是已导出（成片还在、下游还能用），由角标说明成片旧了。
  it("与状态同时给出", () => {
    const state = smartcutNodeState(summary({ isArranged: true, version: 7, renderStale: true, latestRender: render() }));
    expect(state.kind).toBe("exported");
    expect(state.badges.map(b => b.key)).toEqual(["renderStale"]);
  });

  it("阶段名认不出时为空", () => {
    expect(smartcutStageLabel("whatever")).toBe("");
    expect(smartcutStageLabel(undefined)).toBe("");
  });
});
