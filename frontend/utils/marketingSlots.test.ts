import { describe, expect, it } from "vitest";
import { initialComposerState, applySkill, marketingSendGate, type SlotDecl } from "./marketingComposer";
import {
  addSlotItems,
  clearSlotItems,
  emptySlotsValue,
  marketingProductSlotItem,
  marketingSelectedProductId,
  marketingSetProduct,
  marketingSlotRefs,
  marketingSyncProduct,
  moveSlotItem,
  orphanPreviewIds,
  patchSlotItem,
  removeSlotItem,
  requiredSlotText,
  slotAcceptsFile,
  slotAddGate,
  slotFilled,
  slotLimit,
  slotsCompatible,
  slotsFailedText,
  slotsUploading,
  type MarketingSlotsValue,
  type SlotItem
} from "./marketingSlots";

// 新形状的声明（dev/05 MKD-26：商品槽 accept=marketing_product、maxCount=1）。
const product: SlotDecl = { key: "product", label: "商品", accept: ["marketing_product"], required: true, maxCount: 1 };
const character: SlotDecl = { key: "character", label: "角色", accept: ["marketing_character", "character_asset"], required: false, maxCount: 2 };
const sourceVideo: SlotDecl = { key: "source_video", label: "源视频", accept: ["video"], required: true, maxCount: 1 };
const limits = { maxProductsPerSession: 1, maxCharacterRefs: 2 };

function prod(n: number, patch: Partial<SlotItem> = {}): SlotItem {
  return { localId: `p${n}`, kind: "marketing_product", id: `${900 + n}`, status: "done", label: `商品${n}`, thumbMediaId: `${700 + n}`, imageCount: 3, ...patch };
}

function role(n: number, patch: Partial<SlotItem> = {}): SlotItem {
  return { localId: `c${n}`, kind: "marketing_character", id: `${500 + n}`, status: "done", label: `角色${n}`, thumbMediaId: `${600 + n}`, ...patch };
}

function video(n: number, patch: Partial<SlotItem> = {}): SlotItem {
  return { localId: `v${n}`, kind: "media", id: `${100 + n}`, status: "done", label: `${n}.mp4`, hint: "video", ...patch };
}

function withItems(items: Partial<MarketingSlotsValue["items"]>): MarketingSlotsValue {
  const value = emptySlotsValue();
  return { ...value, items: { ...value.items, ...items } };
}

describe("新值结构（dev/05 §4.4）", () => {
  it("空值只有 items，不再有名称 / 卖点", () => {
    expect(emptySlotsValue()).toEqual({ items: { product: [], character: [], source_video: [] } });
  });

  it("选商品：替换原来那个（一次只选 1 个）；null 即移除", () => {
    const a = marketingSetProduct(emptySlotsValue(), { id: "901", name: "榨汁杯", mainMediaId: "701", imageCount: 4 }, "l1");
    expect(a.items.product).toEqual([{ localId: "l1", kind: "marketing_product", id: "901", status: "done", label: "榨汁杯", hint: "image", imageCount: 4, thumbMediaId: "701" }]);
    const b = marketingSetProduct(a, { id: "902", name: "保温杯", mainMediaId: null, imageCount: 1 }, "l2");
    expect(b.items.product).toHaveLength(1);
    expect(b.items.product[0]).not.toHaveProperty("thumbMediaId");
    expect(marketingSelectedProductId(b)).toBe("902");
    expect(marketingSetProduct(b, null, "x").items.product).toEqual([]);
    expect(marketingSelectedProductId(emptySlotsValue())).toBe("");
  });

  it("商品被编辑：卡片同步名称 / 主图 / 张数；被删除：从卡片移除（MKT-UX-18）；别的商品不动", () => {
    const value = withItems({ product: [prod(1)] });
    const edited = marketingSyncProduct(value, { kind: "updated", product: { id: "901", name: "新名字", mainMediaId: "799", imageCount: 7 } });
    expect(edited.changed).toBe(true);
    expect(edited.value.items.product[0]).toMatchObject({ localId: "p1", label: "新名字", thumbMediaId: "799", imageCount: 7 });
    expect(marketingSyncProduct(value, { kind: "deleted", id: "999" })).toEqual({ value, changed: false, removed: false });
    const deleted = marketingSyncProduct(value, { kind: "deleted", id: "901" });
    expect(deleted).toMatchObject({ changed: true, removed: true });
    expect(deleted.value.items.product).toEqual([]);
  });

  it("marketingProductSlotItem 不带任何 URL", () => {
    expect(JSON.stringify(marketingProductSlotItem({ id: "1", name: "a", mainMediaId: "2", imageCount: 1 }, "l"))).not.toMatch(/url|http/i);
  });
});

describe("上限取配置（MKT-AC-42）", () => {
  it("商品最多 1 个、角色最多 2 个（数字取配置），第 3 个角色说「最多 2 个角色」", () => {
    expect(slotLimit(product, limits)).toBe(1);
    expect(slotLimit(character, limits)).toBe(2);
    expect(slotAddGate([role(1), role(2)], slotLimit(character, limits), "character")).toEqual({ ok: false, remaining: 0, reason: "最多 2 个角色" });
    expect(slotAddGate([prod(1)], 1, "product").reason).toBe("最多 1 个商品");
    expect(slotAddGate([], 1, "source_video")).toEqual({ ok: true, remaining: 1, reason: "" });
  });

  it("配置变化跟着变；声明只能收紧；配置缺失回落代码上限", () => {
    expect(slotLimit(character, { maxCharacterRefs: 1 })).toBe(1);
    expect(slotLimit({ key: "character", maxCount: 0 }, { maxCharacterRefs: 3 })).toBe(3);
    expect(slotLimit({ key: "character", maxCount: 5 }, null)).toBe(2);
    expect(slotLimit({ key: "product", maxCount: 20 }, undefined)).toBe(1);
    expect(slotLimit(sourceVideo, limits)).toBe(1);
  });

  it("一次加多了只加到上限并说清；同一个 ID 不重复加；种类不对的不加", () => {
    const out = addSlotItems([role(1)], [role(2), role(3)], 2, "character");
    expect(out.items.map(item => item.id)).toEqual(["501", "502"]);
    expect(out.notice).toBe("最多 2 个角色，有 1 个未添加");
    expect(addSlotItems([role(1)], [role(1, { localId: "x" })], 2, "character").items).toHaveLength(1);
    // 上传的人物图（media）不再是角色槽认的种类
    expect(addSlotItems([], [video(1, { hint: "image" })], 2, "character").items).toEqual([]);
    // 脚本角色资产仍可
    expect(addSlotItems([], [{ localId: "a", kind: "asset", id: "5", status: "done", label: "唐僧" }], 2, "character").items).toHaveLength(1);
  });
});

describe("序列化（MKT-T-37 新形状）", () => {
  it("商品槽 [{kind:marketing_product, id, slot:product, order:1}]；角色槽 marketing_character / asset；无 URL / 名称 / 缩略图", () => {
    const value = withItems({
      product: [prod(1)],
      character: [role(1), { localId: "a", kind: "asset", id: "5", status: "done", label: "唐僧", thumbMediaId: "77" }]
    });
    const refs = marketingSlotRefs(value, [product, character]);
    expect(refs).toEqual([
      { kind: "marketing_product", id: "901", slot: "product", order: 1 },
      { kind: "marketing_character", id: "501", slot: "character", order: 1 },
      { kind: "asset", id: "5", slot: "character", order: 2 }
    ]);
    expect(JSON.stringify(refs)).not.toMatch(/url|label|thumb|商品1|77|701/i);
  });

  it("旧形状的项（商品槽里的媒体图、角色槽里的上传图）一律不发", () => {
    const value = withItems({
      product: [{ localId: "m", kind: "media", id: "101", status: "done", label: "1.png", hint: "image" }],
      character: [{ localId: "n", kind: "media", id: "102", status: "done", label: "人.png", hint: "image" }]
    });
    expect(marketingSlotRefs(value, [product, character])).toEqual([]);
    expect(slotFilled(value)).toEqual({});
  });

  it("只发当前声明里有的槽；源视频上传完成才发", () => {
    const value = withItems({ product: [prod(1)], source_video: [video(1, { status: "uploading", id: undefined })] });
    expect(marketingSlotRefs(value, [product]).every(ref => ref.slot === "product")).toBe(true);
    expect(marketingSlotRefs(value, [product, sourceVideo])).toHaveLength(1);
  });
});

describe("必填与上报", () => {
  it("未选商品 →「请选择商品」，与 marketingSendGate 同一句（MKT-AC-42）", () => {
    const empty = emptySlotsValue();
    expect(requiredSlotText([product, character], empty)).toBe("请选择商品");
    expect(requiredSlotText([product], withItems({ product: [prod(1)] }))).toBe("");
    expect(requiredSlotText([sourceVideo], empty)).toBe("请添加源视频");
    const state = { ...initialComposerState([product, character]), filled: slotFilled(empty) };
    expect(marketingSendGate(state, { readOnly: false, uploading: false }).reason).toBe("请选择商品");
  });

  it("源视频三态：filled 计上传中与已完成、不计失败；晚到的上传结果不把删掉的带回来", () => {
    const value = withItems({ source_video: [video(1, { status: "failed", id: undefined, error: "网络已断开" })] });
    expect(slotFilled(value)).toEqual({});
    expect(slotsFailedText(value)).toBe("有 1 项上传失败，请重试或移除");
    expect(slotsUploading(withItems({ source_video: [video(1, { status: "uploading" })] }))).toBe(true);
    const items = removeSlotItem([video(1, { status: "uploading", id: undefined })], "v1");
    expect(patchSlotItem(items, "v1", { status: "done", id: "9" })).toEqual([]);
  });

  it("slotsCompatible 供 applySkill 的 needsConfirm", () => {
    const value = withItems({ product: [prod(1)], character: [role(1), role(2)] });
    expect(slotsCompatible(value, [product, character]).ok).toBe(true);
    const out = slotsCompatible(value, [product, { ...character, maxCount: 1 }]);
    expect(out.conflicts).toEqual([{ key: "character", count: 2, reason: "over_limit", maxCount: 1 }]);
    const state = { ...initialComposerState([product, character]), filled: slotFilled(value) };
    const switched = applySkill(state, { code: "s", name: "只要商品", prefillText: "", inputSlots: [product], modelBindings: {} });
    expect(switched.needsConfirm).toEqual([{ key: "character", count: 2, reason: "missing" }]);
  });

  it("清空槽位返回被清掉的项；orphanPreviewIds 找出要释放的", () => {
    const value = withItems({ product: [prod(1)], character: [role(1)] });
    const out = clearSlotItems(value, ["character"]);
    expect(out.removed.map(item => item.localId)).toEqual(["c1"]);
    expect(out.value.items.product).toHaveLength(1);
    expect(orphanPreviewIds(["p1", "v9"], value)).toEqual(["v9"]);
  });

  it("文件类型：源视频只收视频", () => {
    expect(slotAcceptsFile(sourceVideo, "video/mp4")).toBe(true);
    expect(slotAcceptsFile(sourceVideo, "image/png")).toBe(false);
    expect(slotAcceptsFile(sourceVideo, "audio/mpeg")).toBe(false);
  });

  it("moveSlotItem 是通用的（商品编辑弹窗的图片条复用）；越界不变", () => {
    expect(moveSlotItem(["a", "b", "c"], 2, 0)).toEqual(["c", "a", "b"]);
    expect(moveSlotItem(["a", "b"], 0, 5)).toEqual(["a", "b"]);
  });
});
