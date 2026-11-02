import { describe, expect, it } from "vitest";
import { modelUnavailableReason, optimizeRequestBody, reasoningUnavailable, sendGate, type SendGateInput } from "./composerPrice";

describe("sendGate 禁用原因（tabtv 不计价，TT-T-14）", () => {
  const ok: SendGateInput = {
    readOnly: false, sending: false, uploading: false, hasContent: true, hasModel: true,
    quote: { loading: false }
  };

  it("全部满足时可发送", () => {
    expect(sendGate(ok)).toEqual({ disabled: false, reason: "" });
  });

  it("只读原因排在最前", () => {
    expect(sendGate({ ...ok, readOnly: true, hasContent: false }).reason).toBe("你在该工作区是只读，无法发起创作");
  });

  it("上传中、空内容、能力校验失败各有原因", () => {
    expect(sendGate({ ...ok, uploading: true }).reason).toContain("上传中");
    expect(sendGate({ ...ok, hasContent: false }).reason).toBe("先写下想生成的内容");
    expect(sendGate({ ...ok, quote: { loading: false, error: "模型当前不可用" } }).reason).toBe("模型当前不可用");
    expect(sendGate({ ...ok, modeUnavailable: "暂无可用的音频模型" }).reason).toBe("暂无可用的音频模型");
    expect(sendGate({ ...ok, missingParam: "请先选择音色" }).reason).toBe("请先选择音色");
  });
});

describe("推理模型可用性", () => {
  it("没有 / 不可用给原因，可用为 undefined", () => {
    expect(reasoningUnavailable(undefined)).toBe("还没有可用的推理模型");
    expect(reasoningUnavailable({ code: "x", name: "X", available: false, reason: "未配置" })).toBe("X：未配置");
    expect(reasoningUnavailable({ code: "x", name: "X", available: true })).toBeUndefined();
    expect(modelUnavailableReason({ message: "该模型未启用" })).toBe("该模型未启用");
    expect(modelUnavailableReason({ message: "请求失败" })).toBe("暂时不可用");
  });
});

describe("AI 帮写与一键优化同链路（MK-26）", () => {
  it("不传 purpose 时请求体与改动前逐字一致；AI 帮写只多一个 purpose", () => {
    expect(JSON.stringify(optimizeRequestBody("原文", "claude"))).toBe(JSON.stringify({ text: "原文", modelCode: "claude" }));
    expect(JSON.stringify(optimizeRequestBody("原文"))).toBe(JSON.stringify({ text: "原文" }));
    expect(optimizeRequestBody("原文", "claude", "character_appearance")).toEqual({ text: "原文", modelCode: "claude", purpose: "character_appearance" });
  });
});

describe("模型目录的视频报价（2026-10-30：报 1 秒会让所有视频模型显示不可用）", () => {
  it("报价时长落在各模型允许的范围里", async () => {
    const { QUOTE_VIDEO_SECONDS } = await import("./composerPrice");
    expect(QUOTE_VIDEO_SECONDS).toBeGreaterThanOrEqual(4);
    expect(QUOTE_VIDEO_SECONDS).toBeLessThanOrEqual(15);
  });
});
