import { describe, expect, it } from "vitest";
import {
  SMARTCUT_RECREATED_NOTICE,
  smartcutInboundSignature,
  smartcutOpenPlan,
  smartcutOutputMediaIdOf,
  smartcutTimelineIdOf,
  smartcutUpstreamNotReadyText
} from "./smartcutCanvas";

const node = (id: string, params: Record<string, unknown>) => ({ id, data: { nodeType: "smartcut", title: "剪辑", params } });

describe("smartcutOpenPlan", () => {
  it("smartcutOpenPlan：四种结果", () => {
    expect(smartcutOpenPlan(node("n1", {}), null)).toEqual({ action: "create" });
    expect(smartcutOpenPlan(node("n1", { timelineId: "abc" }), null)).toEqual({ action: "create" });
    expect(smartcutOpenPlan(node("n1", { timelineId: "701" }), { status: "found", canvasNodeId: "n1" })).toEqual({ action: "open", timelineId: "701" });
    // 复制出来的节点还指向原节点的时间线
    expect(smartcutOpenPlan(node("n2", { timelineId: "701" }), { status: "found", canvasNodeId: "n1" })).toEqual({ action: "fork", sourceTimelineId: "701" });
    expect(smartcutOpenPlan(node("n1", { timelineId: "701" }), { status: "missing" })).toEqual({ action: "recreate", sourceTimelineId: "701", notice: SMARTCUT_RECREATED_NOTICE });
  });

  it("smartcutTimelineIdOf：只认雪花号", () => {
    expect(smartcutTimelineIdOf({ timelineId: " 701 " })).toBe("701");
    expect(smartcutTimelineIdOf({ timelineId: 701 })).toBe("701");
    expect(smartcutTimelineIdOf({ timelineId: "tmp-1" })).toBe("");
    expect(smartcutTimelineIdOf(null)).toBe("");
  });
});

describe("下游取输入", () => {
  it("smartcutOutputMediaIdOf：只认最近一次成功导出的成片", () => {
    expect(smartcutOutputMediaIdOf({ latestRender: { id: "1", status: "success", progress: 100, timelineVersion: 3, outputMediaId: "9001" } })).toBe("9001");
    expect(smartcutOutputMediaIdOf({ latestRender: { id: "1", status: "running", progress: 40, timelineVersion: 3 } })).toBe("");
    expect(smartcutOutputMediaIdOf({ latestRender: { id: "1", status: "failed", progress: 0, timelineVersion: 3, outputMediaId: "9001" } })).toBe("");
    expect(smartcutOutputMediaIdOf({})).toBe("");
    expect(smartcutOutputMediaIdOf(null)).toBe("");
  });

  it("smartcutUpstreamNotReadyText：说清要先导出", () => {
    expect(smartcutUpstreamNotReadyText("新品混剪")).toBe("请先在智能剪辑里导出成片：新品混剪");
  });
});

describe("smartcutInboundSignature", () => {
  it("smartcutInboundSignature：只看指向本节点的连线的来源与顺序，与先后无关", () => {
    const edges = [
      { source: "a", target: "sc", data: { order: 1 } },
      { source: "b", target: "sc", data: { order: 2 } },
      { source: "a", target: "other", data: { order: 1 } }
    ];
    expect(smartcutInboundSignature(edges, "sc")).toBe("a:1|b:2");
    expect(smartcutInboundSignature([...edges].reverse(), "sc")).toBe("a:1|b:2");
    expect(smartcutInboundSignature(edges.slice(0, 1), "sc")).not.toBe(smartcutInboundSignature(edges, "sc"));
  });
});

describe("smartcutConnectableNodes", () => {
  it("smartcutConnectableNodes：规则允许、还没连上、不是自己，最近建的在前", async () => {
    const { smartcutConnectableNodes } = await import("./smartcutCanvas");
    const n = (id: string, nodeType: string) => ({ id, data: { nodeType, params: {} } });
    const nodes = [n("v1", "video"), n("img", "image"), n("doc", "document"), n("sc", "smartcut"), n("v2", "video")];
    const rules = [
      { sourceType: "video", targetType: "smartcut", isAllowed: true },
      { sourceType: "image", targetType: "smartcut", isAllowed: true },
      { sourceType: "document", targetType: "smartcut", isAllowed: false }
    ];
    const picked = smartcutConnectableNodes(nodes, [{ source: "v1", target: "sc" }], "sc", rules);
    expect(picked.map(x => x.id)).toEqual(["v2", "img"]);
  });
});

describe("smartcutSendDisabledReason", () => {
  it("smartcutSendDisabledReason：给最根本的那一条", async () => {
    const { smartcutSendDisabledReason } = await import("./smartcutCanvas");
    const config = { available: true, usedThisHour: 1, perHour: 20 };
    expect(smartcutSendDisabledReason({ canEdit: false, readyInputs: 0, config: null })).toMatch(/只读/);
    expect(smartcutSendDisabledReason({ canEdit: true, readyInputs: 0, config })).toMatch(/先连入/);
    expect(smartcutSendDisabledReason({ canEdit: true, readyInputs: 2, config: null, configError: "暂时读不到" })).toBe("暂时读不到");
    expect(smartcutSendDisabledReason({ canEdit: true, readyInputs: 2, config: { ...config, available: false, reason: "没有绑定模型" } })).toBe("AI 剪辑暂不可用：没有绑定模型");
    expect(smartcutSendDisabledReason({ canEdit: true, readyInputs: 2, config: { ...config, usedThisHour: 20 } })).toMatch(/用完/);
    expect(smartcutSendDisabledReason({ canEdit: true, readyInputs: 2, config, mode: "narr", hasVoice: false })).toMatch(/配音/);
    expect(smartcutSendDisabledReason({ canEdit: true, readyInputs: 2, config, mode: "mix" })).toBe("");
  });

  it("smartcutSendDisabledReason：进行中排在只读之后；被限流时说多久后再试（SC-AC-35）", async () => {
    const { smartcutSendDisabledReason } = await import("./smartcutCanvas");
    const config = { available: true, usedThisHour: 1, perHour: 20 };
    expect(smartcutSendDisabledReason({ canEdit: false, readyInputs: 2, config, running: true })).toMatch(/只读/);
    expect(smartcutSendDisabledReason({ canEdit: true, readyInputs: 0, config, running: true })).toBe("AI 剪辑进行中，请等它完成");
    expect(smartcutSendDisabledReason({ canEdit: true, readyInputs: 2, config, retryAfterSeconds: 125 })).toBe("AI 剪辑太频繁，请约 3 分钟后再试");
    expect(smartcutSendDisabledReason({ canEdit: true, readyInputs: 2, config, retryAfterSeconds: 9 })).toBe("AI 剪辑太频繁，请 9 秒后再试");
    // 等到了就不再拦（倒数到 0）
    expect(smartcutSendDisabledReason({ canEdit: true, readyInputs: 2, config, retryAfterSeconds: 0 })).toBe("");
  });
});
