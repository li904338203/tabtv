import { describe, expect, it } from "vitest";
import { CREATE_DRAFT_KEY, saveCreateDraft, takeCreateDraft } from "./createDraft";
import { initialComposerState, type SlotDecl } from "./marketingComposer";
import {
  MARKETING_DRAFT_KEY,
  MARKETING_LEGACY_DRAFT_TEXT,
  MARKETING_DRAFT_TTL_MS,
  clearMarketingDraft,
  composerStateFromDraft,
  decodeMarketingDraft,
  encodeMarketingDraft,
  isEmptyMarketingDraft,
  marketingDraftOf,
  pendingUploadsText,
  readMarketingDraft,
  saveMarketingDraft,
  slotsValueFromDraft
} from "./marketingDraft";
import { emptySlotsValue, type MarketingSlotsValue } from "./marketingSlots";

function memoryStorage(initial: Record<string, string> = {}) {
  const data = new Map(Object.entries(initial));
  return {
    data,
    getItem: (key: string) => data.get(key) ?? null,
    setItem: (key: string, value: string) => { data.set(key, value); },
    removeItem: (key: string) => { data.delete(key); }
  };
}

const NOW = 1_800_000_000_000;
const DEFAULT_SLOTS: SlotDecl[] = [
  { key: "product", label: "商品", accept: ["marketing_product"], required: true, maxCount: 1 },
  { key: "character", label: "角色", accept: ["marketing_character", "character_asset"], required: false, maxCount: 2 },
  { key: "source_video", label: "源视频", accept: ["video"], required: false, maxCount: 1 }
];

function filledSlots(): MarketingSlotsValue {
  const value = emptySlotsValue();
  value.items.product = [{ localId: "a", kind: "marketing_product", id: "901", status: "done", label: "保温杯", thumbMediaId: "301", hint: "image", imageCount: 4 }];
  value.items.character = [
    { localId: "e", kind: "marketing_character", id: "502", status: "done", label: "柚子君", thumbMediaId: "602" },
    { localId: "f", kind: "asset", id: "501", status: "done", label: "林澈", thumbMediaId: "601", hint: "character" }
  ];
  value.items.source_video = [{ localId: "v", kind: "media", status: "uploading", label: "上传中.mp4", hint: "video" }];
  return value;
}

function liveState() {
  return {
    ...initialComposerState(DEFAULT_SLOTS),
    chips: [
      { dim: "skill" as const, code: "mk_reversal_drama", name: "反转短剧", auto: true },
      { dim: "creative" as const, code: "cq_tower", name: "解放碑广场" },
      { dim: "hook" as const, code: "hk_fly", name: "巨物飞入" }
    ],
    lastPrefill: "基于商品特点…",
    suppressAutoSkill: false,
    pickRequirements: { creative: { product: "required" } },
    skillPrefs: { video: { original: undefined, applied: "seedance" } }
  };
}

describe("营销草稿（MKT-UX-11、MKT-F-03）", () => {
  it("往返：正文、胶囊（code + name + auto）、预填原文、输入要求、商品 / 角色 / 资产引用与顺序、偏好", () => {
    const draft = marketingDraftOf({ state: liveState(), text: "基于商品特点…", slots: filledSlots(), prefs: { video: { model: "seedance" } } as never });
    const back = decodeMarketingDraft(encodeMarketingDraft(draft, NOW), NOW + 1000);
    expect(back).toEqual(draft);
    expect(back?.chips.map(chip => `${chip.dim}:${chip.code}:${chip.auto ? "auto" : ""}`))
      .toEqual(["skill:mk_reversal_drama:auto", "creative:cq_tower:", "hook:hk_fly:"]);
    expect(back?.slots.product).toEqual([{ kind: "marketing_product", id: "901", label: "保温杯", thumbMediaId: "301", hint: "image", imageCount: 4 }]);
    expect(back?.slots.character.map(item => `${item.kind}:${item.id}`)).toEqual(["marketing_character:502", "asset:501"]);
    expect(back?.droppedLegacy).toBeUndefined();
    expect(JSON.parse(encodeMarketingDraft(draft, NOW)).v).toBe(2);
    expect(back?.skillPrefs).toEqual({ video: { applied: "seedance" } });
  });

  it("上传中 / 失败的不进草稿，只计数；恢复时说清", () => {
    const draft = marketingDraftOf({ state: liveState(), text: "", slots: filledSlots() });
    expect(draft.pendingUploads).toBe(1);
    expect(JSON.stringify(draft)).not.toContain("上传中.mp4");
    expect(pendingUploadsText(draft.pendingUploads)).toBe("有 1 个文件未上传完成，请重新添加");
    expect(pendingUploadsText(0)).toBe("");
  });

  it("只存 ID：非数字 ID、URL 丢弃；不存本地预览", () => {
    const raw = JSON.stringify({
      v: 2,
      savedAt: NOW,
      text: "x",
      slots: { product: [{ kind: "marketing_product", id: "https://evil.com/a", label: "a" }, { kind: "marketing_product", id: "12", label: "b", thumbMediaId: "blob:x" }] }
    });
    const draft = decodeMarketingDraft(raw, NOW);
    expect(draft?.slots.product).toEqual([{ kind: "marketing_product", id: "12", label: "b" }]);
  });

  it("胶囊：未知维度、重复维度、缺 code 的丢弃，并按固定顺序排", () => {
    const raw = JSON.stringify({
      savedAt: NOW,
      text: "",
      chips: [
        { dim: "style", code: "s1", name: "极简" },
        { dim: "writing", code: "w", name: "写作" },
        { dim: "skill", code: "k", name: "技能" },
        { dim: "skill", code: "k2", name: "第二个技能" },
        { dim: "hook", code: "", name: "空" }
      ]
    });
    expect(decodeMarketingDraft(raw, NOW)?.chips).toEqual([
      { dim: "skill", code: "k", name: "技能" },
      { dim: "style", code: "s1", name: "极简" }
    ]);
  });

  it("超过 24 小时、损坏、形状不对一律当没有", () => {
    const raw = encodeMarketingDraft(marketingDraftOf({ state: liveState(), text: "x", slots: emptySlotsValue() }), NOW);
    expect(decodeMarketingDraft(raw, NOW + MARKETING_DRAFT_TTL_MS - 1)).toBeDefined();
    expect(decodeMarketingDraft(raw, NOW + MARKETING_DRAFT_TTL_MS + 1)).toBeUndefined();
    expect(decodeMarketingDraft("{bad", NOW)).toBeUndefined();
    expect(decodeMarketingDraft("[]", NOW)).toBeUndefined();
    expect(decodeMarketingDraft(JSON.stringify({ savedAt: NOW, text: 1 }), NOW)).toBeUndefined();
    expect(decodeMarketingDraft(null, NOW)).toBeUndefined();
  });

  it("读不清（刷新两次都在）；过期的读时删掉；发送成功后 clear", () => {
    const storage = memoryStorage();
    const draft = marketingDraftOf({ state: liveState(), text: "你好", slots: filledSlots() });
    saveMarketingDraft(storage, draft, NOW);
    expect(readMarketingDraft(storage, NOW)?.text).toBe("你好");
    expect(readMarketingDraft(storage, NOW)?.text).toBe("你好");
    clearMarketingDraft(storage);
    expect(storage.data.has(MARKETING_DRAFT_KEY)).toBe(false);

    saveMarketingDraft(storage, draft, NOW);
    expect(readMarketingDraft(storage, NOW + MARKETING_DRAFT_TTL_MS + 1)).toBeUndefined();
    expect(storage.data.has(MARKETING_DRAFT_KEY)).toBe(false);
  });

  it("什么都没填时删掉而不是存一份空草稿", () => {
    const storage = memoryStorage({ [MARKETING_DRAFT_KEY]: "old" });
    const empty = marketingDraftOf({ state: initialComposerState(DEFAULT_SLOTS), text: "  ", slots: emptySlotsValue() });
    expect(isEmptyMarketingDraft(empty)).toBe(true);
    saveMarketingDraft(storage, empty, NOW);
    expect(storage.data.has(MARKETING_DRAFT_KEY)).toBe(false);
    // 只有一张没传完的图也算有内容（回来要提示他重新添加）
    const pendingOnly = emptySlotsValue();
    pendingOnly.items.source_video = [{ localId: "x", kind: "media", status: "uploading", label: "a" }];
    expect(isEmptyMarketingDraft(marketingDraftOf({ state: initialComposerState(DEFAULT_SLOTS), text: "", slots: pendingOnly }))).toBe(false);
  });

  it("与创造页草稿键隔离：互不读到、互不清掉", () => {
    expect(MARKETING_DRAFT_KEY).not.toBe(CREATE_DRAFT_KEY);
    const storage = memoryStorage();
    saveCreateDraft(storage, { mode: "agent", text: "创造页", mediaIds: [] }, NOW);
    expect(readMarketingDraft(storage, NOW)).toBeUndefined();
    saveMarketingDraft(storage, marketingDraftOf({ state: liveState(), text: "营销页", slots: emptySlotsValue() }), NOW);
    expect(takeCreateDraft(storage, NOW)?.text).toBe("创造页");
    expect(readMarketingDraft(storage, NOW)?.text).toBe("营销页");
    expect(readMarketingDraft(storage, NOW)?.chips).toHaveLength(3);
  });

  it("恢复：槽位全是已完成项、顺序不变；输入框状态带回胶囊与联动标记，槽位声明不动", () => {
    const draft = marketingDraftOf({ state: { ...liveState(), suppressAutoSkill: true }, text: "正文", slots: filledSlots() });
    let n = 0;
    const slots = slotsValueFromDraft(draft, () => `r${++n}`);
    expect(slots.items.product.map(item => [item.localId, item.kind, item.id, item.status, item.imageCount])).toEqual([["r1", "marketing_product", "901", "done", 4]]);
    expect(slots.items.character.map(item => item.id)).toEqual(["502", "501"]);
    expect(slots).not.toHaveProperty("productName");

    const base = initialComposerState(DEFAULT_SLOTS);
    const state = composerStateFromDraft(base, draft);
    expect(state.chips).toEqual(draft.chips);
    expect(state.text).toBe("正文");
    expect(state.suppressAutoSkill).toBe(true);
    expect(state.pickRequirements).toEqual({ creative: { product: "required" } });
    expect(state.slots).toBe(base.slots);
    expect(state.filled).toEqual({});
  });

  it("旧草稿（v1）：丢弃商品图、名称卖点与上传的人物图并提示一次；正文、胶囊、脚本资产角色与源视频保留（MKT-F-133）", () => {
    const raw = JSON.stringify({
      v: 1,
      savedAt: NOW,
      text: "旧正文",
      chips: [{ dim: "hook", code: "hk_fly", name: "巨物飞入" }],
      slots: {
        product: [{ kind: "media", id: "301", label: "主图.png", hint: "image" }, { kind: "media", id: "302", label: "侧面.png" }],
        character: [{ kind: "media", id: "401", label: "人.png" }, { kind: "asset", id: "501", label: "林澈", thumbMediaId: "601" }],
        source_video: [{ kind: "media", id: "801", label: "a.mp4", hint: "video" }]
      },
      productName: "保温杯",
      sellingPoints: "24 小时保温",
      pendingUploads: 2
    });
    const draft = decodeMarketingDraft(raw, NOW);
    expect(draft?.droppedLegacy).toBe(true);
    expect(draft?.text).toBe("旧正文");
    expect(draft?.chips).toEqual([{ dim: "hook", code: "hk_fly", name: "巨物飞入" }]);
    expect(draft?.slots.product).toEqual([]);
    expect(draft?.slots.character).toEqual([{ kind: "asset", id: "501", label: "林澈", thumbMediaId: "601" }]);
    expect(draft?.slots.source_video.map(item => item.id)).toEqual(["801"]);
    // v1 的「未上传完成」说的是商品图，迁移后不再提示
    expect(draft?.pendingUploads).toBe(0);
    expect(draft).not.toHaveProperty("productName");
    expect(MARKETING_LEGACY_DRAFT_TEXT).toBe("输入框已改为从商品库 / 角色库选择，请重新选择");
    // 迁移后再存一次就是新结构，下次读不再提示（只提示一次）
    const again = decodeMarketingDraft(encodeMarketingDraft(draft!, NOW), NOW);
    expect(again?.droppedLegacy).toBeUndefined();
    expect(again?.slots.character).toHaveLength(1);
  });

  it("旧草稿里没有可丢的东西（只有正文）时不打扰用户", () => {
    const raw = JSON.stringify({ v: 1, savedAt: NOW, text: "只有正文", slots: { product: [] }, productName: "", sellingPoints: " " });
    expect(decodeMarketingDraft(raw, NOW)?.droppedLegacy).toBeUndefined();
  });
});
