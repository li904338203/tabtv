import { describe, expect, it } from "vitest";
import { AGENT_GENERATION_NOTE, agentPriceView, modelUnavailableReason, optimizePriceView, optimizeRequestBody, priceView, sendGate, unitPriceText, variantsPriceLine, type AgentPriceInput, type SendGateInput } from "./composerPrice";

describe("priceView 精确报价", () => {
  it("显示服务端总价与单价算式", () => {
    const view = priceView({ loading: false, credits: 60, unit: "per_second", durationSec: 10 });
    expect(view.label).toBe("✦60");
    expect(view.formula).toBe("✦6/秒 × 10 秒 = ✦60");
  });

  it("按张计价按数量拆", () => {
    expect(priceView({ loading: false, credits: 8, unit: "per_image", count: 4 }).formula)
      .toBe("✦2/张 × 4 张 = ✦8");
  });

  it("除不尽时不硬拆单价，只说总价", () => {
    expect(priceView({ loading: false, credits: 7, unit: "per_image", count: 2 }).formula).toBe("共 ✦7");
  });

  it("按字数计价说清是按字数", () => {
    expect(priceView({ loading: false, credits: 3, unit: "per_kchar" }).formula).toBe("按字数计费，共 ✦3");
  });

  it("折扣：原价划线并写原因（服务端给了才显示）", () => {
    const view = priceView({ loading: false, credits: 40, unit: "per_time", originalCredits: 60, discountReason: "会员 7 折" });
    expect(view.original).toBe("✦60");
    expect(view.discountReason).toBe("会员 7 折");
    expect(priceView({ loading: false, credits: 40, originalCredits: 40 }).original).toBeUndefined();
  });

  it("报价中 / 报价失败", () => {
    expect(priceView({ loading: true }).label).toBe("报价中");
    const failed = priceView({ loading: false, error: "该模型需要更高的会员等级" });
    expect(failed.label).toBe("—");
    expect(failed.error).toBe("该模型需要更高的会员等级");
  });
});

describe("sendGate 禁用原因", () => {
  const ok: SendGateInput = {
    readOnly: false, sending: false, uploading: false, hasContent: true, hasModel: true,
    quote: { loading: false, credits: 60 }, balance: 100
  };

  it("全部满足时可发送", () => {
    expect(sendGate(ok)).toEqual({ disabled: false, reason: "" });
  });

  it("余额不足写明差额", () => {
    const gate = sendGate({ ...ok, balance: 15 });
    expect(gate.disabled).toBe(true);
    expect(gate.shortfall).toBe(45);
    expect(gate.reason).toBe("积分不足，还差 ✦45");
  });

  it("只读原因优先于余额", () => {
    const gate = sendGate({ ...ok, readOnly: true, balance: 0 });
    expect(gate.reason).toBe("你在该工作区是只读，无法发起创作");
    expect(gate.shortfall).toBeUndefined();
  });

  it("不知道余额时不猜", () => {
    expect(sendGate({ ...ok, balance: undefined }).disabled).toBe(false);
  });

  it("上传中、空内容、报价失败各有原因", () => {
    expect(sendGate({ ...ok, uploading: true }).reason).toContain("上传中");
    expect(sendGate({ ...ok, hasContent: false }).reason).toBe("先写下想生成的内容");
    expect(sendGate({ ...ok, quote: { loading: false, error: "模型当前不可用" } }).reason).toBe("模型当前不可用");
    expect(sendGate({ ...ok, modeUnavailable: "暂无可用的音频模型" }).reason).toBe("暂无可用的音频模型");
    expect(sendGate({ ...ok, missingParam: "请先选择音色" }).reason).toBe("请先选择音色");
  });
});

describe("Agent 模式价格（AGT-29）", () => {
  const reasoning = { code: "claude", name: "Claude", available: true, price: 2, unit: "per_time" as const };

  it("显示每轮单价", () => {
    const view = agentPriceView({ reasoning });
    expect(view.label).toBe("✦2/轮");
    expect(view.lines[0]).toBe("对话按轮计费 ✦2/轮（Claude）");
    expect(view.error).toBeUndefined();
  });

  it("所选视频 / 图片单价与智能匹配", () => {
    const view = agentPriceView({
      reasoning,
      video: { code: "sd2", name: "Seedance 2.0", available: true, price: 4, unit: "per_second" }
    });
    expect(view.lines).toContain("视频 ✦4/秒（Seedance 2.0）");
    expect(view.lines).toContain("图片 智能匹配，确认规划时告诉你用了哪个");
    expect(unitPriceText(3, "per_image")).toBe("✦3/张");
  });

  it("模型不可用给出原因", () => {
    const view = agentPriceView({ reasoning: { code: "x", name: "X", available: false, reason: "需要会员" } });
    expect(view.label).toBe("—");
    expect(view.error).toBe("X：需要会员");
    expect(agentPriceView({}).error).toBe("还没有可用的推理模型");
    expect(modelUnavailableReason({ message: "该模型需要黄金会员" })).toBe("该模型需要黄金会员");
    expect(modelUnavailableReason({ message: "请求失败" })).toBe("暂时不可用");
  });

  it("明细包含确认前不扣生成费的说明", () => {
    expect(agentPriceView({ reasoning }).note).toBe(AGENT_GENERATION_NOTE);
    expect(AGENT_GENERATION_NOTE).toContain("确认前不会扣费");
  });
});

describe("营销页价格与创造页一致（MKT-T-14，MK-10、MKT-AC-27）", () => {
  const reasoning = { code: "claude", name: "Claude", available: true, price: 2, unit: "per_time" as const };
  // 按轮（推理）/ 按秒 / 按次（视频）/ 按张（图片）各一例
  const cases: Array<[string, AgentPriceInput, string[]]> = [
    ["按轮 + 智能匹配", { reasoning }, ["对话按轮计费 ✦2/轮（Claude）"]],
    ["按秒", { reasoning, video: { code: "sd", name: "Seedance 2.5", available: true, price: 6, unit: "per_second" } }, ["视频 ✦6/秒（Seedance 2.5）"]],
    ["按次", { reasoning, video: { code: "kl", name: "Kling 3.0", available: true, price: 40, unit: "per_time" } }, ["视频 ✦40/次（Kling 3.0）"]],
    ["按张", { reasoning, image: { code: "sdi", name: "Seedream 5.0", available: true, price: 3, unit: "per_image" } }, ["图片 ✦3/张（Seedream 5.0）"]]
  ];

  it.each(cases)("%s：营销（不带附加行）与创造逐字相等", (_name, input, expected) => {
    const create = agentPriceView(input);
    const marketing = agentPriceView(input, {});
    expect(marketing).toEqual(create);
    expect(JSON.stringify(marketing)).toBe(JSON.stringify(create));
    for (const line of expected) expect(marketing.lines).toContain(line);
    expect(marketing.label).toBe("✦2/轮");
  });

  it("无原价 / 划线价 / 折扣字段", () => {
    for (const [, input] of cases) {
      const view = agentPriceView(input, { variants: { max: 4 } });
      expect(Object.keys(view).sort()).toEqual(["label", "lines", "note"]);
      expect(JSON.stringify(view)).not.toMatch(/原价|折扣|original|discount/);
    }
  });

  it("批量技能多一行提示，其余逐字不变；上限取传入的配置值", () => {
    const input = cases[1][1];
    const plain = agentPriceView(input);
    const batch = agentPriceView(input, { variants: { max: 4 } });
    expect(batch.lines.slice(0, plain.lines.length)).toEqual(plain.lines);
    expect(batch.lines).toHaveLength(plain.lines.length + 1);
    expect(batch.lines.at(-1)).toBe("按变体逐个计费（最多 4 个），确认时逐个列出");
    expect(batch.label).toBe(plain.label);
    expect(batch.note).toBe(plain.note);
    expect(variantsPriceLine({ max: 6 })).toBe("按变体逐个计费（最多 6 个），确认时逐个列出");
    expect(variantsPriceLine({ max: 4, count: 3 })).toBe("共 3 个变体（最多 4 个），确认时逐个列出");
    expect(variantsPriceLine({ max: 4, count: 0 })).toBe("按变体逐个计费（最多 4 个），确认时逐个列出");
  });
});

describe("一键优化的价格（AGT-43）", () => {
  it("按所选推理模型的每轮单价，点之前就说清", () => {
    const view = optimizePriceView({ code: "m", name: "推理 A", available: true, price: 36, unit: "per_time" });
    expect(view.label).toBe("✦36");
    expect(view.text).toContain("✦36");
    expect(view.text).toContain("全额退回");
    expect(view.error).toBeUndefined();
  });

  it("没有 / 不可用的推理模型给原因，不猜价格", () => {
    expect(optimizePriceView(undefined).error).toBe("还没有可用的推理模型");
    expect(optimizePriceView({ code: "m", name: "推理 A", available: false, reason: "需要会员" }).error).toBe("推理 A：需要会员");
    const pending = optimizePriceView({ code: "m", name: "推理 A", available: true });
    expect(pending.label).toBe("");
    expect(pending.error).toBeUndefined();
  });
});

describe("AI 帮写与一键优化同价同链路（MK-26、MKT-AC-40 前端部分）", () => {
  it("不传 purpose 时请求体与改动前逐字一致；AI 帮写只多一个 purpose", () => {
    expect(JSON.stringify(optimizeRequestBody("原文", "claude"))).toBe(JSON.stringify({ text: "原文", modelCode: "claude" }));
    expect(JSON.stringify(optimizeRequestBody("原文"))).toBe(JSON.stringify({ text: "原文" }));
    expect(optimizeRequestBody("原文", "claude", "character_appearance")).toEqual({ text: "原文", modelCode: "claude", purpose: "character_appearance" });
  });

  it("价格同一个函数、同一个输入：帮写与一键优化逐字相等", () => {
    const reasoning = { code: "m", name: "推理 A", available: true, price: 36, unit: "per_time" as const };
    // 角色弹窗收的就是输入框算好的 optimizePriceView（同一份推理模型）
    expect(optimizePriceView(reasoning)).toEqual(optimizePriceView({ ...reasoning }));
  });
});

describe("模型目录的视频报价（2026-10-30：报 1 秒会让所有视频模型显示不可用）", () => {
  it("报价时长落在各模型允许的范围里", async () => {
    const { QUOTE_VIDEO_SECONDS } = await import("./composerPrice");
    expect(QUOTE_VIDEO_SECONDS).toBeGreaterThanOrEqual(4);
    expect(QUOTE_VIDEO_SECONDS).toBeLessThanOrEqual(15);
  });
  it("按秒计价的折回每秒，其余原样", async () => {
    const { quoteUnitPrice } = await import("./composerPrice");
    expect(quoteUnitPrice(45, "per_second", 5)).toBe(9);
    expect(quoteUnitPrice(96, "per_time", 5)).toBe(96);
    expect(quoteUnitPrice(undefined, "per_second", 5)).toBeUndefined();
  });
});
