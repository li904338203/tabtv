import { describe, expect, it } from "vitest";
import type { SlotDecl } from "./marketingComposer";
import { emptySlotsValue, type MarketingSlotsValue, type SlotItem } from "./marketingSlots";
import {
  MARKETING_LEGACY_SHAPE_TEXT,
  MARKETING_PROJECT_FALLBACK,
  buildMarketingSessionRequest,
  marketingPicksOf,
  marketingProjectName,
  marketingSendFailure,
  marketingSendFingerprint,
  type MarketingRequestInput
} from "./marketingRequest";

const product: SlotDecl = { key: "product", label: "商品", accept: ["marketing_product"], required: true, maxCount: 1 };
const character: SlotDecl = { key: "character", label: "角色", accept: ["marketing_character", "character_asset"], required: false, maxCount: 2 };
const sourceVideo: SlotDecl = { key: "source_video", label: "源视频", accept: ["video"], required: false, maxCount: 1 };

function productItem(id: string, patch: Partial<SlotItem> = {}): SlotItem {
  return { localId: `p${id}`, kind: "marketing_product", id, status: "done", label: "清透防晒霜", thumbMediaId: "701", imageCount: 3, hint: "image", ...patch };
}

function slots(patch: Partial<MarketingSlotsValue["items"]> = {}): MarketingSlotsValue {
  const value = emptySlotsValue();
  return { ...value, items: { ...value.items, ...patch } };
}

function input(patch: Partial<MarketingRequestInput> = {}): MarketingRequestInput {
  return {
    text: "  夏季防晒霜，突出清爽不油腻  ",
    chips: [
      { dim: "style", code: "st_clean", name: "清爽" },
      { dim: "skill", code: "mk_ugc", name: "UGC 种草" },
      { dim: "creative", code: "cr_pain", name: "痛点开场" }
    ],
    slots: slots({
      product: [productItem("901")],
      character: [
        { localId: "c1", kind: "marketing_character", id: "501", status: "done", label: "柚子君", thumbMediaId: "601" },
        { localId: "c2", kind: "asset", id: "201", status: "done", label: "唐僧" }
      ]
    }),
    decls: [product, character],
    refs: [{ kind: "media", id: "301" }, { kind: "node", id: "n1" }],
    prefs: { reasoning: { model: "gpt" } },
    ...patch
  };
}

describe("buildMarketingSessionRequest（dev/03 §3.5 + dev/05 §3.3）", () => {
  it("请求体形状：商品槽 marketing_product、角色槽 marketing_character / asset；没有 marketing.product", () => {
    expect(buildMarketingSessionRequest(input())).toEqual({
      createProject: { name: "夏季防晒霜，突出清爽不油腻" },
      scene: "marketing",
      message: {
        text: "夏季防晒霜，突出清爽不油腻",
        refs: [
          { kind: "marketing_product", id: "901", slot: "product", order: 1 },
          { kind: "marketing_character", id: "501", slot: "character", order: 1 },
          { kind: "asset", id: "201", slot: "character", order: 2 },
          { kind: "media", id: "301" },
          { kind: "node", id: "n1" }
        ]
      },
      marketing: { picks: { creative: "cr_pain", hook: null, style: "st_clean" } },
      skillCode: "mk_ugc",
      prefs: { reasoning: { model: "gpt" } }
    });
  });

  it("商品槽恰好 [{kind:marketing_product, id, slot:product, order:1}]；无 marketing.product、无任何 URL / 名称 / 缩略图（MKT-T-38 验收）", () => {
    const body = buildMarketingSessionRequest(input({ refs: [] }));
    expect(body.message.refs?.filter(ref => ref.slot === "product")).toEqual([{ kind: "marketing_product", id: "901", slot: "product", order: 1 }]);
    expect(body.marketing).not.toHaveProperty("product");
    const json = JSON.stringify(body);
    expect(json).not.toMatch(/blob:|https?:|thumb|sellingPoints|imageCount|701|601/);
    expect(json).not.toContain("清透防晒霜");
  });

  it("旧形状的槽位项（商品媒体图、上传人物图）不发；上传中的源视频不发", () => {
    const body = buildMarketingSessionRequest(input({
      decls: [product, character, sourceVideo],
      refs: [],
      slots: slots({
        product: [{ localId: "m", kind: "media", id: "101", status: "done", label: "1.png" }],
        character: [{ localId: "n", kind: "media", id: "102", status: "done", label: "人.png" }],
        source_video: [{ localId: "v", kind: "media", status: "uploading", label: "a.mp4", hint: "video" }]
      })
    }));
    expect(body.message.refs).toEqual([]);
  });

  it("与槽位重复的普通引用不重复发；不在当前声明里的槽不发", () => {
    const withDup = buildMarketingSessionRequest(input({ refs: [{ kind: "asset", id: "201" }, { kind: "media", id: "301" }] }));
    expect(withDup.message.refs?.filter(ref => ref.id === "201")).toHaveLength(1);
    const refs = buildMarketingSessionRequest(input({ decls: [product], refs: [] })).message.refs ?? [];
    expect(refs.map(ref => ref.slot)).toEqual(["product"]);
  });

  it("没选技能不带 skillCode；偏好为空不带 prefs", () => {
    const body = buildMarketingSessionRequest(input({ chips: [], prefs: {} }));
    expect(body).not.toHaveProperty("skillCode");
    expect(body).not.toHaveProperty("prefs");
    expect(body.marketing?.picks).toEqual({ creative: null, hook: null, style: null });
  });
});

describe("项目名回落顺序：正文前 20 字 → 所选商品名称 → 「营销创作」", () => {
  it("正文优先，按码点截 20 字", () => {
    expect(marketingProjectName("一二三四五六七八九十一二三四五六七八九十多出来的", "商品")).toBe("一二三四五六七八九十一二三四五六七八九十");
    expect(Array.from(marketingProjectName("😀".repeat(25), "")).length).toBe(20);
  });
  it("正文为空用所选商品的名称，再空用「营销创作」", () => {
    expect(marketingProjectName("   ", " 清透防晒霜 ")).toBe("清透防晒霜");
    expect(marketingProjectName("", "")).toBe(MARKETING_PROJECT_FALLBACK);
    expect(buildMarketingSessionRequest(input({ text: "" })).createProject).toEqual({ name: "清透防晒霜" });
    expect(buildMarketingSessionRequest(input({ text: "", slots: slots() })).createProject).toEqual({ name: MARKETING_PROJECT_FALLBACK });
  });
});

describe("picks 与幂等指纹", () => {
  it("每维取 code，未选为 null", () => {
    expect(marketingPicksOf([{ dim: "hook", code: "hk_1", name: "x" }])).toEqual({ creative: null, hook: "hk_1", style: null });
  });
  it("内容不变指纹不变（连点 / 重试复用同一个键）；换一个商品就换", () => {
    expect(marketingSendFingerprint(input())).toBe(marketingSendFingerprint(input()));
    expect(marketingSendFingerprint(input())).not.toBe(marketingSendFingerprint(input({ slots: slots({ product: [productItem("902")] }) })));
  });
});

describe("旧形状被拒（MKD-27 / MKR-15）", () => {
  it("40001 legacy_shape →「页面已更新，请刷新后重新选择商品」，带槽位时标那个槽位", () => {
    const cause = Object.assign(new Error("x"), { data: { code: 40001, message: "旧形状", details: { field: "marketing.product", code: "legacy_shape" } } });
    expect(marketingSendFailure(cause, true)).toEqual({ text: MARKETING_LEGACY_SHAPE_TEXT });
    expect(MARKETING_LEGACY_SHAPE_TEXT).toBe("页面已更新，请刷新后重新选择商品");
    const slotted = Object.assign(new Error("x"), { data: { code: 40001, details: { slot: "product", code: "legacy_shape" } } });
    expect(marketingSendFailure(slotted, true)).toEqual({ text: MARKETING_LEGACY_SHAPE_TEXT, slot: "product" });
  });
});

function apiError(code: number, message: string, details: Record<string, unknown> = {}) {
  return Object.assign(new Error(message), { data: { code, details } });
}

describe("marketingSendFailure（MKT-AC-04 前端部分）", () => {
  it("断网说断网，不标红", () => {
    expect(marketingSendFailure(apiError(40001, "x", { slot: "product" }), false)).toEqual({ text: "网络已断开，恢复后再发送" });
  });
  it("源视频引用他人工作区的视频（MKT-T-28 details.slot=source_video）→ 标源视频槽，原话照说", () => {
    expect(marketingSendFailure(apiError(40001, "源视频：引用的视频不存在或不属于当前工作区", { slot: "source_video", field: "source_video", code: "not_found" }), true))
      .toEqual({ text: "源视频：引用的视频不存在或不属于当前工作区", slot: "source_video" });
  });
  it("40001 带 details.slot → 标那个槽位，原话照说", () => {
    expect(marketingSendFailure(apiError(40001, "请添加商品图", { slot: "product", field: "refs", code: "required" }), true))
      .toEqual({ text: "请添加商品图", slot: "product" });
    // 不认识的槽位不标
    expect(marketingSendFailure(apiError(40001, "x", { slot: "weird" }), true)).toEqual({ text: "x" });
  });
  it("42215 → 标对应维度的胶囊；语气按 userFixable 分类", () => {
    const ops = marketingSendFailure(apiError(42215, "Hook「抓耳」已下线，请换一个", { kind: "hook", code: "hk_ear", reason: "disabled", userFixable: false }), true);
    expect(ops).toEqual({
      text: "暂不可用：Hook「抓耳」已下线，请换一个",
      option: { dim: "hook", code: "hk_ear", block: { message: "Hook「抓耳」已下线，请换一个", userFixable: false } }
    });
    const fixable = marketingSendFailure(apiError(42215, "风格「x」不适用当前产出，请换一个技能或选项", { kind: "style", code: "st_x", reason: "output_mismatch", userFixable: true }), true);
    expect(fixable.text).toBe("风格「x」不适用当前产出，请换一个技能或选项");
    expect(fixable.option?.dim).toBe("style");
  });
  it("其余原话照说", () => {
    expect(marketingSendFailure(apiError(40201, "余额不足，还差 ✦20"), true)).toEqual({ text: "余额不足，还差 ✦20" });
  });
});
