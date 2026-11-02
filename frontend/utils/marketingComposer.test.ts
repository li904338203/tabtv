import { describe, expect, it } from "vitest";
import {
  applyCreative,
  applyPick,
  applySkill,
  conflictText,
  dropChip,
  initialComposerState,
  removeChip,
  removeSkill,
  marketingSendGate,
  setChip,
  type MarketingComposerState,
  type SkillForComposer,
  type SlotDecl
} from "./marketingComposer";
import { prefModel } from "./agentPrefs";

const product: SlotDecl = { key: "product", label: "商品", accept: ["image"], required: true, maxCount: 20 };
const character: SlotDecl = { key: "character", label: "角色", accept: ["image", "character_asset"], required: false, maxCount: 2 };
const sourceVideo: SlotDecl = { key: "source_video", label: "源视频", accept: ["video"], required: false, maxCount: 1 };
const defaults = [product, character];

function skill(code: string, extra: Partial<SkillForComposer> = {}): SkillForComposer {
  return { code, name: `技能${code}`, prefillText: "", inputSlots: [], modelBindings: {}, ...extra };
}

const skillA = skill("A", { prefillText: "为这款商品拍一条 15 秒带货短视频" });
const skillB = skill("B", { prefillText: "B 的预填", inputSlots: [{ ...product, maxCount: 4 }] });
const bound = skill("bound", { modelBindings: { video: { code: "kling-v2", name: "可灵 2" }, reasoning: { code: "claude", name: "Claude" } } });

const base = (patch: Partial<MarketingComposerState> = {}) => ({ ...initialComposerState(defaults, { video: { model: "seedance", ratio: "9:16" } }), ...patch });
const dims = (state: MarketingComposerState) => state.chips.map(chip => chip.dim);

describe("胶囊顺序与同维替换（MKT-AC-02）", () => {
  it("依次选 风格、Hook、创意、技能 → 技能 → 创意 → Hook → 风格", () => {
    let s = base();
    s = applyPick(s, "style", { code: "film", name: "胶片" });
    s = applyPick(s, "hook", { code: "ask", name: "反问开场" });
    s = applyCreative(s, { code: "unbox", name: "开箱" }, null).state;
    s = applySkill(s, skillA).state;
    expect(dims(s)).toEqual(["skill", "creative", "hook", "style"]);
  });

  it("顺序与先后无关；同一维再选只剩新的那个", () => {
    const a = setChip(setChip([], { dim: "hook", code: "h1", name: "h1" }), { dim: "skill", code: "k", name: "k" });
    const b = setChip(setChip([], { dim: "skill", code: "k", name: "k" }), { dim: "hook", code: "h1", name: "h1" });
    expect(a).toEqual(b);
    const replaced = setChip(a, { dim: "hook", code: "h2", name: "h2" });
    expect(replaced.filter(chip => chip.dim === "hook")).toEqual([{ dim: "hook", code: "h2", name: "h2" }]);
    expect(replaced.map(chip => chip.dim)).toEqual(["skill", "hook"]);
  });

  it("removeChip：删 auto 技能才 suppress", () => {
    const chips = setChip([{ dim: "creative", code: "c", name: "c" }], { dim: "skill", code: "k", name: "k", auto: true });
    expect(removeChip(chips, "skill")).toEqual({ chips: [{ dim: "creative", code: "c", name: "c" }], suppressAutoSkill: true });
    expect(removeChip(chips, "creative").suppressAutoSkill).toBe(false);
    expect(removeChip([{ dim: "skill", code: "k", name: "k" }], "skill").suppressAutoSkill).toBe(false);
  });
});

describe("技能联动（MKT-AC-03）", () => {
  it("正文为空选 A → 正文等于 A 的预填；仍是预填原文时换 B → 换成 B 的预填", () => {
    const a = applySkill(base(), skillA).state;
    expect(a.text).toBe(skillA.prefillText);
    expect(applySkill(a, skillB).state.text).toBe("B 的预填");
  });

  it("改过一个字后选 B → 正文不变", () => {
    const a = applySkill(base(), skillA).state;
    const edited = { ...a, text: `${a.text}。` };
    const b = applySkill(edited, skillB).state;
    expect(b.text).toBe(edited.text);
    // 之后再换技能也不覆盖
    expect(applySkill(b, skillA).state.text).toBe(edited.text);
  });

  it("用户自己写的正文（从未预填）不被覆盖；空白正文算空", () => {
    expect(applySkill(base({ text: "我的想法" }), skillA).state.text).toBe("我的想法");
    expect(applySkill(base({ text: "  \n" }), skillA).state.text).toBe(skillA.prefillText);
  });

  it("技能没有预填：正文不动", () => {
    expect(applySkill(base({ text: "" }), skill("C")).state.text).toBe("");
  });

  it("B 声明仅商品槽 → 槽位无角色槽；没声明 → 场景默认", () => {
    const b = applySkill(base(), skillB).state;
    expect(b.slots.map(slot => slot.key)).toEqual(["product"]);
    expect(applySkill(b, skill("C")).state.slots).toEqual(defaults);
  });

  it("不兼容：已传角色而新技能没有角色槽 → needsConfirm 且状态不变；确认后清空", () => {
    const s = base({ filled: { product: 2, character: 1 } });
    const first = applySkill(s, skillB);
    expect(first.needsConfirm).toEqual([{ key: "character", count: 1, reason: "missing" }]);
    expect(first.state).toBe(s);
    expect(conflictText(first.needsConfirm!)).toContain("角色");
    const confirmed = applySkill(s, skillB, { confirmed: true });
    expect(confirmed.needsConfirm).toBeUndefined();
    expect(confirmed.clearSlots).toEqual(["character"]);
    expect(confirmed.state.filled).toEqual({ product: 2 });
    expect(dims(confirmed.state)).toEqual(["skill"]);
  });

  it("超出新上限也要确认", () => {
    const r = applySkill(base({ filled: { product: 6 } }), skillB);
    expect(r.needsConfirm).toEqual([{ key: "product", count: 6, reason: "over_limit", maxCount: 4 }]);
  });

  it("有 modelBindings → 切到自定义模型，其余字段保留", () => {
    const s = applySkill(base(), bound).state;
    expect(prefModel(s.prefs, "video")).toBe("kling-v2");
    expect(prefModel(s.prefs, "reasoning")).toBe("claude");
    expect((s.prefs.video as Record<string, unknown>).ratio).toBe("9:16");
    expect(s.skillPrefs).toEqual({ video: { original: "seedance", applied: "kling-v2" }, reasoning: { original: undefined, applied: "claude" } });
  });

  it("从绑定技能换到不绑定的技能 → 偏好回到用户原选择", () => {
    const s = applySkill(applySkill(base(), bound).state, skillA).state;
    expect(prefModel(s.prefs, "video")).toBe("seedance");
    expect(prefModel(s.prefs, "reasoning")).toBeUndefined();
    expect(s.skillPrefs).toBeNull();
  });

  it("两个绑定技能之间切换，原选择始终是用户自己的", () => {
    const other = skill("o", { modelBindings: { video: { code: "veo", name: "Veo" } } });
    const s = removeSkill(applySkill(applySkill(base(), bound).state, other).state).state;
    expect(prefModel(s.prefs, "video")).toBe("seedance");
    expect(prefModel(s.prefs, "reasoning")).toBeUndefined();
  });
});

describe("删技能回退（MKT-UX-05）", () => {
  it("槽位回场景默认、模型回原偏好、正文不回退", () => {
    const withB = applySkill(applySkill(base(), bound).state, skillB).state;
    const chosen = applySkill(base(), bound).state;
    const s = removeSkill({ ...chosen, slots: [product], text: "改过的正文" }).state;
    expect(s.slots).toEqual(defaults);
    expect(prefModel(s.prefs, "video")).toBe("seedance");
    expect(s.text).toBe("改过的正文");
    expect(dims(s)).toEqual([]);
    // 预填原文也不回退
    expect(removeSkill(withB).state.text).toBe("B 的预填");
  });

  it("技能生效期间用户自己换了模型 → 删技能不抹掉他换的", () => {
    const chosen = applySkill(base(), bound).state;
    const changed = { ...chosen, prefs: { ...chosen.prefs, video: { model: "hailuo", ratio: "9:16" } } };
    expect(prefModel(removeSkill(changed).state.prefs, "video")).toBe("hailuo");
  });

  it("场景默认放不下已有内容 → 先问", () => {
    const withVideo = { ...applySkill(base(), skill("v", { inputSlots: [sourceVideo] })).state, filled: { source_video: 1 } };
    const r = removeSkill(withVideo);
    expect(r.needsConfirm?.[0]?.key).toBe("source_video");
    expect(r.state).toBe(withVideo);
    expect(removeSkill(withVideo, { confirmed: true }).clearSlots).toEqual(["source_video"]);
  });

  it("dropChip 统一入口：技能走 removeSkill，创意清掉推荐模型提示", () => {
    let s = applyCreative(base(), { code: "c", name: "开箱", recommendedModels: [{ code: "kling-v2", name: "可灵 2", available: true }] }, null).state;
    expect(s.smartMatchHint).toEqual(["kling-v2"]);
    s = dropChip(s, "creative").state;
    expect(s.smartMatchHint).toEqual([]);
    expect(dims(s)).toEqual([]);
  });
});

describe("选创意带入默认技能（MK-20）", () => {
  const creative = { code: "unbox", name: "开箱", defaultSkillCode: "A", recommendedModels: [{ code: "kling-v2", name: "可灵 2", available: true }] };

  it("当前无技能 → 带入默认技能（auto）并执行技能联动；推荐模型只进提示不切换", () => {
    const r = applyCreative(base(), creative, skillA);
    expect(r.autoSkill).toBe("applied");
    expect(r.state.chips).toEqual([
      { dim: "skill", code: "A", name: "技能A", auto: true },
      { dim: "creative", code: "unbox", name: "开箱" }
    ]);
    expect(r.state.text).toBe(skillA.prefillText);
    expect(r.state.smartMatchHint).toEqual(["kling-v2"]);
    expect(prefModel(r.state.prefs, "video")).toBe("seedance");
  });

  it("已手选技能 → 不替换", () => {
    const manual = applySkill(base(), skillB).state;
    const r = applyCreative(manual, creative, skillA);
    expect(r.autoSkill).toBe("kept_manual");
    expect(r.state.chips.find(chip => chip.dim === "skill")).toEqual({ dim: "skill", code: "B", name: "技能B" });
  });

  it("删掉 auto 技能后再选创意 → 不再补回", () => {
    const auto = applyCreative(base(), creative, skillA).state;
    const removed = dropChip(auto, "skill").state;
    expect(removed.suppressAutoSkill).toBe(true);
    expect(dims(removed)).toEqual(["creative"]);
    const again = applyCreative(removed, { ...creative, code: "unbox2" }, skillA);
    expect(again.autoSkill).toBe("suppressed");
    expect(dims(again.state)).toEqual(["creative"]);
  });

  it("删手选技能不 suppress：之后选创意仍会带入", () => {
    const removed = removeSkill(applySkill(base(), skillB).state).state;
    expect(removed.suppressAutoSkill).toBe(false);
    expect(applyCreative(removed, creative, skillA).autoSkill).toBe("applied");
  });

  it("当前技能是上一个创意自动带入的 → 换成这个创意的默认技能", () => {
    const first = applyCreative(base(), creative, skillA).state;
    const r = applyCreative(first, { code: "c2", name: "测评", defaultSkillCode: "B" }, skillB);
    expect(r.autoSkill).toBe("applied");
    expect(r.state.chips.find(chip => chip.dim === "skill")).toMatchObject({ code: "B", auto: true });
    expect(r.state.text).toBe("B 的预填");
  });

  it("没有默认技能 / 默认技能不可用 / 会清掉已有内容 → 不带入，创意照常插入", () => {
    expect(applyCreative(base(), { code: "x", name: "x" }, null).autoSkill).toBe("none");
    expect(applyCreative(base(), creative, null).autoSkill).toBe("unavailable");
    const r = applyCreative(base({ filled: { character: 1 } }), { ...creative, defaultSkillCode: "B" }, skillB);
    expect(r.autoSkill).toBe("skipped_conflict");
    expect(r.needsConfirm).toBeUndefined();
    expect(dims(r.state)).toEqual(["creative"]);
    expect(r.state.filled).toEqual({ character: 1 });
  });
});

describe("marketingSendGate（MKT-AC-04 前端部分）", () => {
  const baseGate = { disabled: true, reason: "推理模型暂时不可用" };

  it("商品必填未填 → 「请选择商品」", () => {
    expect(marketingSendGate(base(), { readOnly: false, uploading: false })).toMatchObject({ disabled: true, reason: "请选择商品", code: "slot_required", slot: "product" });
  });

  it("只读排最前，其次上传中，再次必填槽，再次选项要求，最后才是原有判定", () => {
    const s = applyPick(base(), "hook", { code: "h", name: "反问", inputRequirements: { character: "required" } });
    expect(marketingSendGate(s, { readOnly: true, uploading: true, base: baseGate }).code).toBe("read_only");
    expect(marketingSendGate(s, { readOnly: true, uploading: true }).reason).toBe("你在该工作区是只读，无法发起创作");
    expect(marketingSendGate(s, { readOnly: false, uploading: true, base: baseGate }).code).toBe("uploading");
    expect(marketingSendGate(s, { readOnly: false, uploading: false, base: baseGate }).reason).toBe("请选择商品");
    const withProduct = { ...s, filled: { product: 1 } };
    expect(marketingSendGate(withProduct, { readOnly: false, uploading: false, base: baseGate })).toMatchObject({
      code: "pick_requirement",
      slot: "character",
      reason: "「Hook：反问」需要角色，请选择角色"
    });
    const satisfied = { ...withProduct, filled: { product: 1, character: 1 } };
    expect(marketingSendGate(satisfied, { readOnly: false, uploading: false, base: baseGate })).toMatchObject({ code: "base", reason: baseGate.reason });
    expect(marketingSendGate(satisfied, { readOnly: false, uploading: false, base: { disabled: false, reason: "" } })).toEqual({ disabled: false, reason: "" });
  });

  // 04 文档 P-47：素材裂变只有源视频槽，部署数据里的选项又全部要求商品 —— 原先一个选项都用不了。
  it("素材裂变：源视频本身就是商品视频，「需要商品」的选项不再挡", () => {
    const remix = applySkill(base({ filled: {} }), skill("remix", { inputSlots: [{ ...sourceVideo, required: true }] })).state;
    const s = applyPick(remix, "hook", { code: "h", name: "结果前置", inputRequirements: { product: "required" } });
    expect(marketingSendGate({ ...s, filled: {} }, { readOnly: false, uploading: false }).code).toBe("slot_required");
    expect(marketingSendGate({ ...s, filled: { source_video: 1 } }, { readOnly: false, uploading: false }).disabled).toBe(false);
  });

  it("技能把商品槽改成可选 → 不再挡", () => {
    const optional = applySkill(base(), skill("consult", { inputSlots: [{ ...product, required: false }] })).state;
    expect(marketingSendGate(optional, { readOnly: false, uploading: false }).disabled).toBe(false);
  });

  it("选项要求的槽当前技能没有 → 说清是技能不支持", () => {
    const s = applyPick(applySkill(base({ filled: {} }), skillB).state, "style", { code: "s", name: "真人", inputRequirements: { character: "required" } });
    expect(marketingSendGate({ ...s, filled: { product: 1 } }, { readOnly: false, uploading: false }).reason).toBe("「风格：真人」需要角色，当前技能不支持添加角色");
  });

  it("删掉选项后它的要求不再生效", () => {
    const s = applyPick(base({ filled: { product: 1 } }), "hook", { code: "h", name: "反问", inputRequirements: { character: "required" } });
    expect(marketingSendGate(dropChip(s, "hook").state, { readOnly: false, uploading: false }).disabled).toBe(false);
  });
});
