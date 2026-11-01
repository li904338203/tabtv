import { describe, expect, it } from "vitest";
import { creditSummary, creditTransactionDetails, creditTransactionTitle } from "./creditsPage";

const time = (iso: string) => `T(${iso})`;

describe("积分中心页（2026-11-01 重接新后端）", () => {
  it("余额按到期与否拆开：有到期时间的是将过期（给最早的到期日），没有的是永久积分", () => {
    expect(creditSummary([
      { source: "purchase", remaining: 500, expireAt: null },
      { source: "membership", remaining: 300, expireAt: "2026-12-31T16:00:00Z" },
      { source: "promotion", remaining: 200, expireAt: "2026-11-30T16:00:00Z" },
      { source: "register", remaining: 0, expireAt: "2026-11-02T16:00:00Z" }
    ])).toEqual({ expiring: 500, nearestExpireAt: "2026-11-30T16:00:00Z", permanent: 500 });
  });

  it("没有批次信息时两项都是 0、没有到期日", () => {
    expect(creditSummary(undefined)).toEqual({ expiring: 0, nearestExpireAt: null, permanent: 0 });
    expect(creditSummary([])).toEqual({ expiring: 0, nearestExpireAt: null, permanent: 0 });
  });

  it("流水标题：类型说方向，按账本里实际的 refType 细分，生成带上操作名", () => {
    expect(creditTransactionTitle({ type: "consume", refType: "generation", operationName: "文生图" })).toBe("生成消耗 · 文生图");
    expect(creditTransactionTitle({ type: "refund", refType: "generation", operationName: "图生视频" })).toBe("失败退还 · 图生视频");
    expect(creditTransactionTitle({ type: "consume", refType: "script_llm" })).toBe("提示词合成");
    expect(creditTransactionTitle({ type: "consume", refType: "agent_turn" })).toBe("Agent 对话");
    expect(creditTransactionTitle({ type: "consume", refType: "render" })).toBe("成片渲染");
    expect(creditTransactionTitle({ type: "consume", refType: "something_new" })).toBe("积分消耗");
    expect(creditTransactionTitle({ type: "grant", refType: "purchase" })).toBe("积分购买");
    expect(creditTransactionTitle({ type: "grant", refType: "membership" })).toBe("会员赠送");
    expect(creditTransactionTitle({ type: "expire", refType: "grant" })).toBe("到期清零");
    expect(creditTransactionTitle({ type: "adjust", refType: "order_refund" })).toBe("订单退款回收");
    expect(creditTransactionTitle({ type: "adjust", refType: "admin_adjust" })).toBe("人工调整");
  });

  it("不认识的类型原样显示，没有类型时说「积分变动」", () => {
    expect(creditTransactionTitle({ type: "bonus" })).toBe("bonus");
    expect(creditTransactionTitle({})).toBe("积分变动");
  });

  it("明细：项目（能打开就给链接）· 模型 · 时间；生成消耗的原因就是操作编码，不再重复", () => {
    expect(creditTransactionDetails({
      type: "consume", refType: "generation", reason: "text2img", createdAt: "2026-11-01T02:21:40Z",
      projectId: "9", projectName: "演唱会舞台", operationCode: "text2img", operationName: "文生图", modelName: "Seedream 4.0"
    }, time)).toEqual([
      { key: "project", text: "项目「演唱会舞台」", to: "/video/canvas/9" },
      { key: "model", text: "Seedream 4.0" },
      { key: "time", text: "T(2026-11-01T02:21:40Z)" }
    ]);
  });

  it("明细：失败退还带上失败原因；已删除的项目注明且不给链接；会话与时间线各自说清", () => {
    const refund = creditTransactionDetails({
      type: "refund", refType: "generation", reason: "模型服务暂时不可用，请稍后重试", projectId: "9", projectName: "未命名",
      projectDeleted: true, operationCode: "img2video", operationName: "图生视频"
    }, time);
    expect(refund).toEqual([
      { key: "project", text: "项目「未命名」（已删除）" },
      { key: "reason", text: "模型服务暂时不可用，请稍后重试" }
    ]);
    expect(creditTransactionDetails({ type: "consume", refType: "agent_turn", reason: "Agent 对话", refTitle: "做一条宣传片" }, time))
      .toEqual([{ key: "title", text: "会话「做一条宣传片」" }]);
    expect(creditTransactionDetails({ type: "consume", refType: "render", refTitle: "成片 A" }, time))
      .toEqual([{ key: "title", text: "时间线「成片 A」" }]);
    expect(creditTransactionDetails({ type: "grant", refType: "purchase", reason: "订单 123" }, time))
      .toEqual([{ key: "reason", text: "订单 123" }]);
  });
});
