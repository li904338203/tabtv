import { describe, expect, it } from "vitest";
import {
  marketingPlanItem,
  planApproveSubmission,
  planItemOverrides,
  planItemProductText,
  planNotices,
  planProductMedia,
  planFailedItemKey,
  planProgressView,
  planRetryMessage,
  planSelectionError,
  planVariantGroups,
  togglePlanSelection,
  type AgentPlanItemProgress,
  type AgentPlanProgress
} from "./planItems";

const marketingItem = {
  kind: "video",
  modelCode: "seedance",
  label: "主片",
  quantity: 10,
  unit: "second",
  credits: 60,
  key: "v1",
  variant: 2,
  variantLabel: "痛点开场",
  productIndexes: [1, 2, 3],
  productMediaIds: ["m1", "m2", "m3"],
  productTotal: 20,
  imageLimit: 3,
  picks: [{ kind: "creative", code: "cr_pain", version: 1, name: "痛点开场" }, { kind: "hook", code: "hk_q", version: 2 }]
};

describe("marketingPlanItem", () => {
  it("营销条目：变体、选项、选图字段", () => {
    expect(marketingPlanItem(marketingItem)).toEqual({
      key: "v1",
      variantText: "变体 2 · 痛点开场",
      picksText: "创意：痛点开场 · Hook：hk_q",
      productIndexes: [1, 2, 3],
      productMediaIds: ["m1", "m2", "m3"],
      productTotal: 20,
      imageLimit: 3,
      canChange: true
    });
  });
  it("通用条目（没有 key）返回 undefined —— 卡片渲染不变", () => {
    expect(marketingPlanItem({ kind: "video", label: "主片", credits: 60 })).toBeUndefined();
  });
  it("被拦下 / 不收图 / 没有商品图的不能换", () => {
    expect(marketingPlanItem({ ...marketingItem, blockedCode: "MARKETING_OPTION_UNAVAILABLE" })?.canChange).toBe(false);
    expect(marketingPlanItem({ ...marketingItem, imageLimit: 0 })?.canChange).toBe(false);
    expect(marketingPlanItem({ ...marketingItem, productTotal: 0 })?.canChange).toBe(false);
  });
});

describe("planItemProductText（MKT-AC-26 ①）", () => {
  it("「共 X 张，本项用 Y 张（模型上限 L）」", () => {
    expect(planItemProductText({ productTotal: 20, imageLimit: 3, selected: 3 })).toBe("共 20 张，本项用 3 张（模型上限 3）");
    expect(planItemProductText({ productTotal: 20, imageLimit: 4, selected: 2 })).toBe("共 20 张，本项用 2 张（模型上限 4）");
  });
  it("上限 0 说清不收图；没有商品图 / 不涉及时不显示", () => {
    expect(planItemProductText({ productTotal: 5, imageLimit: 0, selected: 0 })).toBe("共 5 张，本项不使用商品图（所选模型此操作不收图）");
    expect(planItemProductText({ productTotal: 0, imageLimit: 3, selected: 0 })).toBe("");
    expect(planItemProductText({ productTotal: 5, selected: 0 })).toBe("");
  });
});

describe("换图 ≤ Y 校验", () => {
  it("至少 1 张、不超上限、在范围内、不重复", () => {
    expect(planSelectionError([1, 4], 3, 20)).toBe("");
    expect(planSelectionError([], 3, 20)).toBe("至少选 1 张商品图");
    expect(planSelectionError([1, 2, 3, 4], 3, 20)).toBe("最多选 3 张（所选模型的上限）");
    expect(planSelectionError([21], 3, 20)).toBe("只能从这 20 张商品图里选");
    expect(planSelectionError([0], 3, 20)).toBe("只能从这 20 张商品图里选");
    expect(planSelectionError([2, 2], 3, 20)).toBe("同一张图不能选两次");
  });
  it("点选：追加到末尾、再点取消；到上限不挤掉已选的", () => {
    expect(togglePlanSelection([1, 2], 5, 3)).toEqual({ indexes: [1, 2, 5], reason: "" });
    expect(togglePlanSelection([1, 2, 5], 2, 3)).toEqual({ indexes: [1, 5], reason: "" });
    expect(togglePlanSelection([1, 2, 5], 7, 3)).toEqual({ indexes: [1, 2, 5], reason: "最多选 3 张，先取消一张再选" });
  });
});

describe("itemOverrides", () => {
  const items = [marketingItem, { ...marketingItem, key: "v2", productIndexes: [4] }, { kind: "image", label: "通用", credits: 3 }];
  it("只带改过的营销条目（顺序变了也算），键不在卡片上的丢弃", () => {
    expect(planItemOverrides(items, { v1: [1, 2, 3], v2: [5], ghost: [1] })).toEqual([{ key: "v2", productIndexes: [5] }]);
    expect(planItemOverrides(items, { v1: [3, 2, 1] })).toEqual([{ key: "v1", productIndexes: [3, 2, 1] }]);
    expect(planItemOverrides(items, {})).toEqual([]);
  });
  it("没有换图时提交体与通用卡片逐字一致", () => {
    expect(planApproveSubmission([])).toEqual({ decision: "approve" });
    expect(planApproveSubmission([{ key: "v2", productIndexes: [5] }])).toEqual({ decision: "approve", itemOverrides: [{ key: "v2", productIndexes: [5] }] });
  });
  it("换后的序号 → 会话商品图的媒体 ID（界面显示的就是要发出去的）", () => {
    expect(planProductMedia(["a", "b", "c"], [3, 1])).toEqual(["c", "a"]);
    expect(planProductMedia(["a"], [2])).toEqual([]);
  });
});

describe("notices 置顶（MKT-AC-29 界面）", () => {
  it("去空、去重、保持顺序；没有为空数组", () => {
    expect(planNotices({ notices: ["画面为人物对镜讲解，不保证口型与语音逐字同步", " ", "画面为人物对镜讲解，不保证口型与语音逐字同步", "b"] }))
      .toEqual(["画面为人物对镜讲解，不保证口型与语音逐字同步", "b"]);
    expect(planNotices({ items: [] })).toEqual([]);
    expect(planNotices(null)).toEqual([]);
  });
});

describe("素材裂变条目的说明（MKT-T-28 fissionNote）", () => {
  it("有服务端说明照原话；没写时按路线兜底，只说走哪条不编金额；非裂变条目没有这个字段", () => {
    expect(marketingPlanItem({ ...marketingItem, fissionRoute: "video2video", fissionNote: "视频生视频：以源视频为输入，本项 ✦50" })?.fissionNote)
      .toBe("视频生视频：以源视频为输入，本项 ✦50");
    expect(marketingPlanItem({ ...marketingItem, fissionRoute: "regenerate" })?.fissionNote).toContain("不参考源视频画面");
    expect(marketingPlanItem({ ...marketingItem, fissionRoute: "regenerate" })?.fissionNote).not.toContain("✦");
    expect(marketingPlanItem(marketingItem)).not.toHaveProperty("fissionNote");
  });
});

describe("确认卡按变体分组（MKT-AC-17 界面）", () => {
  const v = (variant: number, credits: number, extra: Record<string, unknown> = {}) => ({ kind: "video", label: "主片", credits, key: `k${variant}${credits}`, variant, ...extra });
  it("3 个变体 3 组、组内小计之和 = 各行之和；按变体序排、0 组殿后", () => {
    const items = [v(2, 60, { variantLabel: "反问开场" }), v(1, 50), v(3, 40), v(0, 5), v(1, 3)];
    const groups = planVariantGroups(items);
    expect(groups.map(g => g.title)).toEqual(["变体 1", "变体 2 · 反问开场", "变体 3", "其他"]);
    expect(groups.map(g => g.indexes)).toEqual([[1, 4], [0], [2], [3]]);
    expect(groups.map(g => g.subtotal)).toEqual([53, 60, 40, 5]);
    expect(groups.reduce((sum, g) => sum + g.subtotal, 0)).toBe(158);
  });
  it("被拦下的条目不计小计；只有一个变体（或没有变体）不分组", () => {
    expect(planVariantGroups([v(1, 50), v(2, 30, { blockedCode: "MARKETING_OPTION_UNAVAILABLE" })]).map(g => g.subtotal)).toEqual([50, 0]);
    expect(planVariantGroups([v(1, 50), v(1, 30)])).toEqual([]);
    expect(planVariantGroups([{ kind: "image", label: "x", credits: 3 }])).toEqual([]);
  });
});

describe("批准失败标出具体项（42215 details.key）", () => {
  it("只认 42215 的 key；其余错误不指向条目", () => {
    expect(planFailedItemKey({ code: 42215, details: { key: "v2", reason: "x" } })).toBe("v2");
    expect(planFailedItemKey({ code: 40001, details: { key: "v2" } })).toBe("");
    expect(planFailedItemKey({ code: 42215, details: {} })).toBe("");
  });
});

describe("执行进度：部分成功不摊平、停后续与失败分开、重试可用性（MKT-AC-18 界面）", () => {
  const item = (key: string, variant: number, state: AgentPlanItemProgress["state"], errorCode?: string): AgentPlanItemProgress =>
    ({ key, label: "主片", variant, variantLabel: `说明${variant}`, state, attempts: state === "pending" ? 0 : 1, ...(errorCode ? { errorCode } : {}) });
  const progress = (status: AgentPlanProgress["status"], items: AgentPlanItemProgress[]): AgentPlanProgress => ({
    cardId: "c1",
    status,
    total: items.length,
    succeeded: items.filter(i => i.state === "success").length,
    failed: items.filter(i => i.state === "failed").length,
    stopped: items.filter(i => i.state === "stopped").length,
    running: items.filter(i => i.state === "submitted").length,
    pending: items.filter(i => i.state === "pending").length,
    items
  });

  it("1 个上游失败：「部分成功（2/3）」—— 不说成功也不说失败，失败那条可重试", () => {
    const view = planProgressView(progress("partial_success", [item("v1", 1, "success"), item("v2", 2, "failed"), item("v3", 3, "success")]))!;
    expect(view.summary).toBe("部分成功（2/3）");
    expect(view.tone).toBe("partial");
    expect(view.summary).not.toMatch(/全部|失败/);
    expect(view.detail).toBe("失败 1");
    expect(view.items.map(i => i.canRetry)).toEqual([false, true, false]);
    expect(view.items[1]).toMatchObject({ title: "变体 2 · 说明2", stateText: "失败" });
    expect(view.items[1]!.reason).toContain("生成没有成功");
  });

  it("余额不足那一项与被停掉的后续文案分开；被停掉的要等余额不足那项解决才可单独重试", () => {
    const view = planProgressView(progress("partial_success", [
      item("v1", 1, "success"), item("v2", 2, "failed", "INSUFFICIENT_CREDITS"), item("v3", 3, "stopped", "plan_stopped_insufficient_credits")
    ]))!;
    expect(view.detail).toBe("余额不足 1 · 余额不足停止 1");
    expect(view.items[1]).toMatchObject({ stateText: "余额不足", canRetry: true });
    expect(view.items[1]!.reason).toContain("余额不足，没有提交，也没有扣费");
    expect(view.items[2]).toMatchObject({ stateText: "已停止", canRetry: false, retryBlocked: "先充值并重试「变体 2 · 说明2」" });
    expect(view.items[2]!.reason).toContain("前面有一项余额不足");
    expect(view.items[2]!.reason).not.toBe(view.items[1]!.reason);
    // 余额不足那一项重试成功之后：被停掉的可以单独重试了
    const after = planProgressView(progress("partial_success", [item("v1", 1, "success"), item("v2", 2, "success"), item("v3", 3, "stopped", "plan_stopped_insufficient_credits")]))!;
    expect(after.items[2]!.canRetry).toBe(true);
  });

  it("汇总文案直接用服务端计数；运行中 / 全成 / 全败", () => {
    expect(planProgressView(progress("running", [item("v1", 1, "success"), item("v2", 2, "submitted"), item("v3", 3, "pending")]))!.summary).toBe("执行中，已完成 1/3");
    expect(planProgressView(progress("success", [item("v1", 1, "success"), item("v2", 2, "success")]))!.summary).toBe("全部成功（2/2）");
    expect(planProgressView(progress("failed", [item("v1", 1, "failed"), item("v2", 2, "failed")]))!.summary).toBe("全部未成功（0/2）");
  });

  it("不能发消息（只读、Agent 正忙）时重试全部不可点并说原因", () => {
    const view = planProgressView(progress("partial_success", [item("v1", 1, "success"), item("v2", 2, "failed")]), "Agent 正在执行，完成后再试")!;
    expect(view.items[1]).toMatchObject({ canRetry: false, retryBlocked: "Agent 正在执行，完成后再试" });
  });

  it("不显示：没有进度、单项计划、还都没开始", () => {
    expect(planProgressView(null)).toBeUndefined();
    expect(planProgressView(progress("failed", [item("v1", 1, "failed")]))).toBeUndefined();
    expect(planProgressView(progress("pending", [item("v1", 1, "pending"), item("v2", 2, "pending")]))).toBeUndefined();
  });

  it("重试的话点名条目键，交给 Agent（新步骤、新幂等键由服务端定）", () => {
    expect(planRetryMessage({ key: "v2", title: "变体 2 · 说明2" })).toBe("请只重试计划里没有成功的「变体 2 · 说明2」（条目 v2），已经成功的变体不要重做");
  });
});
