import { describe, expect, it } from "vitest";
import {
  currentVideoModel,
  gridMove,
  inputRequirementText,
  optionBlock,
  optionCoverUrl,
  optionListQuery,
  optionPreviewUrl,
  recommendedParamsText,
  sameFilter,
  serverOptionBlock,
  skillCoverUrl,
  type MarketingOption
} from "./marketingOptions";

function option(patch: Partial<MarketingOption> = {}): MarketingOption {
  return {
    kind: "hook",
    code: "sfx",
    name: "抓耳音效",
    categoryCode: "sound",
    description: "",
    lifecycle: "live",
    coverCount: 1,
    hasPreview: false,
    isAudioRequired: false,
    outputTypes: ["video"],
    inputRequirements: {},
    tags: [],
    recommendedModels: [],
    recommendedParams: {},
    defaultSkillCode: null,
    ...patch
  } as MarketingOption;
}

const mute = { code: "silent-v", hasNativeAudio: false };
const loud = { code: "loud-v", hasNativeAudio: true };

describe("optionBlock 四类原因（MKT-F-19、MKT-UX-07）", () => {
  it("依赖出声 ∧ 当前模型不出声 → needs_audio_model，用户能解决，不可选", () => {
    const block = optionBlock(option({ isAudioRequired: true }), { videoModel: mute });
    expect(block).toEqual({
      code: "needs_audio_model",
      userFixable: true,
      text: "当前模型不出声，这个 Hook 不生效 · 换一个能出声的模型",
      selectable: false
    });
  });

  it("智能匹配（没选模型）不判出声（MKT-AC-14）", () => {
    expect(optionBlock(option({ isAudioRequired: true }), { videoModel: null })).toBeNull();
    expect(optionBlock(option({ isAudioRequired: true }), {})).toBeNull();
  });

  it("出声标记未知（目录还没取到）不判，能出声的模型也不判", () => {
    expect(optionBlock(option({ isAudioRequired: true }), { videoModel: { code: "x" } })).toBeNull();
    expect(optionBlock(option({ isAudioRequired: true }), { videoModel: loud })).toBeNull();
  });

  it("不依赖出声的选项在不出声模型下照常可用", () => {
    expect(optionBlock(option(), { videoModel: mute })).toBeNull();
  });

  it("商品必填而没有商品图 → needs_product，仍可选（发送键去拦）", () => {
    const block = optionBlock(option({ kind: "creative", inputRequirements: { product: "required" } }), { productCount: 0 });
    expect(block).toMatchObject({ code: "needs_product", userFixable: true, text: "需要商品图", selectable: true });
    expect(optionBlock(option({ inputRequirements: { product: "required" } }), { productCount: 2 })).toBeNull();
    expect(optionBlock(option({ inputRequirements: { product: "optional" } }), {})).toBeNull();
  });

  it("产出对不上 → output_mismatch；产出未定不判", () => {
    const style = option({ kind: "style", outputTypes: ["video"] });
    expect(optionBlock(style, { outputType: "image" })).toEqual({
      code: "output_mismatch",
      userFixable: true,
      text: "这个风格只适用于视频",
      selectable: false
    });
    expect(optionBlock(style, { outputType: null })).toBeNull();
    expect(optionBlock(style, { outputType: "video" })).toBeNull();
  });

  it("服务端 42215 → unavailable，userFixable 照服务端分类，文案用服务端原话", () => {
    const ops = optionBlock(option(), { serverBlocks: { sfx: { message: "该 Hook 已下线", userFixable: false } } });
    expect(ops).toEqual({ code: "unavailable", userFixable: false, text: "该 Hook 已下线", selectable: false });
    const mine = optionBlock(option(), { serverBlocks: { sfx: { message: "", userFixable: true } } });
    expect(mine).toMatchObject({ code: "unavailable", userFixable: true, text: "这个 Hook 暂不可用" });
    expect(optionBlock(option(), { serverBlocks: { other: { message: "x", userFixable: false } } })).toBeNull();
  });

  it("多个原因同时成立给最根本的那一条：服务端 > 产出 > 出声 > 商品", () => {
    const all = option({ isAudioRequired: true, outputTypes: ["video"], inputRequirements: { product: "required" } });
    const ctx = { videoModel: mute, productCount: 0, outputType: "image" as const };
    expect(optionBlock(all, { ...ctx, serverBlocks: { sfx: { message: "下线", userFixable: false } } })?.code).toBe("unavailable");
    expect(optionBlock(all, ctx)?.code).toBe("output_mismatch");
    expect(optionBlock(all, { ...ctx, outputType: "video" })?.code).toBe("needs_audio_model");
    expect(optionBlock(all, { productCount: 0 })?.code).toBe("needs_product");
  });

  it("语气只看分类不看文案：改了服务端文案，userFixable 不变", () => {
    const a = optionBlock(option(), { serverBlocks: { sfx: { message: "需要商品图", userFixable: false } } });
    expect(a?.userFixable).toBe(false);
    expect(a?.code).toBe("unavailable");
  });
});

describe("serverOptionBlock", () => {
  it("从 42215 错误取出选项结论", () => {
    const error = { message: "所选 Hook 已结束", data: { code: 42215, details: { kind: "hook", code: "sfx", reason: "ended", userFixable: false } } };
    expect(serverOptionBlock(error)).toEqual({ kind: "hook", code: "sfx", block: { message: "所选 Hook 已结束", userFixable: false } });
    const fixable = { message: "x", data: { code: 42215, details: { kind: "style", code: "s", reason: "output_mismatch", userFixable: true } } };
    expect(serverOptionBlock(fixable)?.block.userFixable).toBe(true);
  });

  it("其他错误码 / 没有 details / 不是对象 → null", () => {
    expect(serverOptionBlock({ data: { code: 40001, details: { code: "x" } } })).toBeNull();
    expect(serverOptionBlock({ data: { code: 42215 } })).toBeNull();
    expect(serverOptionBlock(new Error("x"))).toBeNull();
    expect(serverOptionBlock(null)).toBeNull();
  });
});

describe("currentVideoModel", () => {
  const catalog = [{ code: "a", hasNativeAudio: true }, { code: "b", hasNativeAudio: false }, { code: "c" }];
  it("没选 → null（智能匹配）；选了 → 带目录里的出声标记；目录里没有 → 未知", () => {
    expect(currentVideoModel(undefined, catalog)).toBeNull();
    expect(currentVideoModel("", catalog)).toBeNull();
    expect(currentVideoModel("b", catalog)).toEqual({ code: "b", hasNativeAudio: false });
    expect(currentVideoModel("c", catalog)).toEqual({ code: "c", hasNativeAudio: undefined });
    expect(currentVideoModel("zz", catalog)).toEqual({ code: "zz", hasNativeAudio: undefined });
  });
});

describe("optionListQuery：分类与搜索交给服务端", () => {
  it("全部：只有 kind 与 limit", () => {
    expect(optionListQuery({ kind: "hook", filter: { type: "all" } })).toEqual({ kind: "hook", limit: 24 });
  });
  it("分类 / 按模型 / 搜索 / 游标", () => {
    expect(optionListQuery({ kind: "creative", filter: { type: "category", code: "douyin" }, q: "  开箱 ", cursor: "c1" }))
      .toEqual({ kind: "creative", limit: 24, category: "douyin", q: "开箱", cursor: "c1" });
    expect(optionListQuery({ kind: "creative", filter: { type: "model", code: "seedance" } }))
      .toEqual({ kind: "creative", limit: 24, model: "seedance" });
  });
  it("按模型只对创意生效（Hook / 风格没有这一组）；空搜索不发", () => {
    expect(optionListQuery({ kind: "style", filter: { type: "model", code: "seedance" }, q: "   " })).toEqual({ kind: "style", limit: 24 });
  });
  it("sameFilter", () => {
    expect(sameFilter({ type: "all" }, { type: "all" })).toBe(true);
    expect(sameFilter({ type: "category", code: "a" }, { type: "category", code: "a" })).toBe(true);
    expect(sameFilter({ type: "category", code: "a" }, { type: "model", code: "a" })).toBe(false);
    expect(sameFilter({ type: "category", code: "a" }, { type: "category", code: "b" })).toBe(false);
  });
});

describe("公开地址", () => {
  it("封面 / 示例视频 / 技能封面", () => {
    expect(optionCoverUrl("/api/v1", "a b", 1)).toBe("/api/v1/marketing/options/a%20b/covers/1?w=320");
    expect(optionCoverUrl("/api/v1", "x", 0, 640)).toBe("/api/v1/marketing/options/x/covers/0?w=640");
    expect(optionPreviewUrl("/api/v1", "x")).toBe("/api/v1/marketing/options/x/preview");
    expect(skillCoverUrl("/api/v1", "s", 160)).toBe("/api/v1/agent/skills/s/cover?w=160");
  });
});

describe("gridMove：三列网格的键盘移动（MKT-UX-06）", () => {
  it("↑↓ 按行、←→ 按格", () => {
    expect(gridMove(0, "down", 9, 3)).toBe(3);
    expect(gridMove(4, "up", 9, 3)).toBe(1);
    expect(gridMove(4, "left", 9, 3)).toBe(3);
    expect(gridMove(4, "right", 9, 3)).toBe(5);
  });
  it("越界停在边上；最后一行不满时 ↓ 落到最后一项", () => {
    expect(gridMove(1, "up", 9, 3)).toBe(1);
    expect(gridMove(0, "left", 9, 3)).toBe(0);
    expect(gridMove(8, "right", 9, 3)).toBe(8);
    expect(gridMove(7, "down", 9, 3)).toBe(7);
    expect(gridMove(4, "down", 7, 3)).toBe(6);
    expect(gridMove(5, "down", 7, 3)).toBe(6);
  });
  it("没有当前项 → 第一项；空列表 → -1", () => {
    expect(gridMove(-1, "down", 5, 3)).toBe(0);
    expect(gridMove(9, "up", 5, 3)).toBe(0);
    expect(gridMove(0, "down", 0, 3)).toBe(-1);
  });
});

describe("详情抽屉文案", () => {
  it("适用输入：必填在前", () => {
    expect(inputRequirementText({ character: "optional", product: "required" })).toBe("商品图：必填 · 角色：可选");
    expect(inputRequirementText({})).toBe("不限");
    expect(inputRequirementText(null)).toBe("不限");
  });
  it("推荐画幅时长", () => {
    expect(recommendedParamsText({ aspectRatio: "9:16", durationSec: [10, 15] })).toBe("9:16 · 10~15 秒");
    expect(recommendedParamsText({ durationSec: 8 })).toBe("8 秒");
    expect(recommendedParamsText({ durationSec: [5, 5] })).toBe("5 秒");
    expect(recommendedParamsText({})).toBe("");
    expect(recommendedParamsText(null)).toBe("");
  });
});

describe("optionBlock：素材裂变的源视频（04 文档 P-47）", () => {
  it("有源视频时「需要商品图」不再提示", () => {
    const hook = option({ kind: "hook", code: "h", isAudioRequired: false, inputRequirements: { product: "required" } });
    expect(optionBlock(hook, { productCount: 0 })?.code).toBe("needs_product");
    expect(optionBlock(hook, { productCount: 0, sourceVideo: true })).toBeNull();
  });
});
