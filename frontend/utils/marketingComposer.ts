/**
 * 营销输入框的胶囊与联动（MKT-T-11，dev/03 §3.3；MKT-F-14~17、MK-20、MKT-UX-04/05）。纯函数。
 *
 * 组件只调用这里：选技能 / 选创意 / 选 Hook·风格 / 删胶囊 / 发送键可用性。每个函数吃一份状态、吐一份新状态，
 * 不碰网络、不碰 DOM —— 联动规则一旦散进组件，就只能靠手点去验，而它们每一条都直接决定用户写的字会不会被覆盖。
 *
 * ⚠️ 几条容易做反的：
 * - **预填只在正文没被改过时生效**：正文为空，或正文仍逐字等于上一个技能填进去的预填原文。改过一个字就不覆盖 ——
 *   用户写的东西被选技能这个动作悄悄换掉，他甚至不知道原文去哪了。
 * - **删技能正文不回退**（MKT-UX-05）：正文此时可能已经被他改过，回退等于删他写的字。
 * - **槽位不兼容先问再清**：新技能没有角色槽而他已经传了角色，直接清掉是不可逆的；返回 `needsConfirm`，
 *   组件提示一次，用户确认后带 `confirmed` 再调一次，这时才把冲突的槽位列进 `clearSlots`。
 * - **模型偏好只回退技能改过的那几节、且只在它仍是技能设的值时回退**：用户在技能生效期间自己换了模型，
 *   删技能不能把他换的那个也抹掉。
 * - **自动带入（MK-20）不替换手选、删过不补回、绝不弹确认**：自动带入是「顺手帮一把」，
 *   它若要清掉用户已传的内容就放弃带入（`autoSkill: "skipped_conflict"`），而不是拿一个确认框打断他。
 */
import type { components } from "@tabtv/api-client";
import { prefModel, withPrefFields, type AgentPrefsValue, type PrefKind } from "./agentPrefs";
import { chipText, orderChips, type ComposerChip, type ComposerChipDim } from "./composerKeys";
import type { SendGate } from "./composerPrice";

export type SlotDecl = components["schemas"]["MarketingSlotDecl"];
export type SlotKey = SlotDecl["key"];
export type AgentSkill = components["schemas"]["AgentSkill"];
/** 联动只用到技能的这几个字段（`GET /agent/skills?scene=marketing`）。 */
export type SkillForComposer = Pick<AgentSkill, "code" | "name" | "prefillText" | "inputSlots" | "modelBindings">;
export type PickDim = Exclude<ComposerChipDim, "skill">;

/**
 * 被选中的创意 / Hook / 风格用到的字段：契约 `MarketingOption`（`GET /marketing/options`）的子集。
 * 只要 code + name，其余可缺 —— 恢复草稿时手上只有胶囊（code + name），没有整份选项。
 * （MKT-T-11 时契约里还没有用户端选项 DTO，曾是手写的结构；MKT-T-13 换成从生成类型派生。）
 */
export type MarketingPick = Pick<components["schemas"]["MarketingOption"], "code" | "name">
  & Partial<Pick<components["schemas"]["MarketingOption"], "defaultSkillCode" | "recommendedModels" | "inputRequirements">>;

export type MarketingComposerState = {
  chips: ComposerChip[];
  text: string;
  /** 上一次填进正文的技能预填原文（"" = 没填过）。正文仍逐字等于它 → 视为没改过，可被下一个技能覆盖。 */
  lastPrefill: string;
  /** 当前槽位声明：技能声明了 `inputSlots` 用它，否则场景默认。 */
  slots: SlotDecl[];
  /** 场景默认槽位（`/marketing/config` 的 `defaultSlots`）。 */
  defaultSlots: SlotDecl[];
  /** 各槽已有内容的条数（内容本身在槽位组件里，这里只记数，用于兼容判定与必填判定）。 */
  filled: Partial<Record<SlotKey, number>>;
  /** 生效的模型偏好（形状见 utils/agentPrefs）。 */
  prefs: AgentPrefsValue;
  /**
   * 技能模型绑定改写过的那几节：`original` 是改写前的模型（undefined = 原来是智能匹配），`applied` 是技能设的模型。
   * null = 当前没有被技能改写。
   */
  skillPrefs: Partial<Record<PrefKind, { original?: string; applied: string }>> | null;
  /** 删过自动带入的技能之后为 true：本次输入内选创意不再补回（MK-20）。 */
  suppressAutoSkill: boolean;
  /** 已选创意 / Hook / 风格的输入要求（发送键判定用）。 */
  pickRequirements: Partial<Record<PickDim, Record<string, string>>>;
  /** 创意的推荐模型：只写进「智能匹配」的提示，不切换偏好。 */
  smartMatchHint: string[];
};

export type SlotConflict = {
  key: SlotKey;
  /** 已有几条。 */
  count: number;
  /** missing = 新槽位里没有这个槽；over_limit = 有但上限比已有的少。 */
  reason: "missing" | "over_limit";
  /** over_limit 时新的上限。 */
  maxCount?: number;
};

export type SwitchResult = {
  state: MarketingComposerState;
  /** 非空 = 还没生效：要清掉这些槽位里已有的内容，先问用户，确认后带 `confirmed: true` 再调一次。 */
  needsConfirm?: SlotConflict[];
  /** 已生效且需要组件清空的槽位（只在确认之后出现）。 */
  clearSlots: SlotKey[];
};

const PICK_DIMS: readonly PickDim[] = ["creative", "hook", "style"];
const BOUND_KINDS: readonly PrefKind[] = ["video", "image", "reasoning"];

/** 槽位在提示里的叫法（固定，不用运营填的 label —— 验收要求逐字「请选择商品」，MKT-AC-42）。 */
export const SLOT_NOUN: Record<SlotKey, string> = { product: "商品", character: "角色", source_video: "源视频" };
/** 补齐槽位的动作：商品 / 角色从库里**选**（MK-27），源视频仍是上传或引用（**添加**）。 */
export const SLOT_VERB: Record<SlotKey, string> = { product: "选择", character: "选择", source_video: "添加" };
/** 槽位条数的量词（「已添加 2 个角色」）。 */
const SLOT_COUNT_UNIT: Record<SlotKey, string> = { product: "个", character: "个", source_video: "段" };

/** 新建一份空状态。`defaultSlots` 来自 `/marketing/config`。 */
export function initialComposerState(defaultSlots: SlotDecl[], prefs: AgentPrefsValue = {}): MarketingComposerState {
  return {
    chips: [],
    text: "",
    lastPrefill: "",
    slots: defaultSlots,
    defaultSlots,
    filled: {},
    prefs,
    skillPrefs: null,
    suppressAutoSkill: false,
    pickRequirements: {},
    smartMatchHint: []
  };
}

// ---------------------------------------------------------------- 胶囊数组

/** 放一个胶囊：同维度替换；结果按 技能 → 创意 → Hook → 风格 排序，与选择先后无关（MKT-AC-02）。 */
export function setChip(chips: readonly ComposerChip[], chip: ComposerChip): ComposerChip[] {
  return orderChips([...chips.filter(item => item.dim !== chip.dim), chip]);
}

/** 删一维的胶囊；删掉的若是自动带入的技能，`suppressAutoSkill=true`（本次输入内不再补回）。 */
export function removeChip(chips: readonly ComposerChip[], dim: ComposerChipDim): { chips: ComposerChip[]; suppressAutoSkill: boolean } {
  const removed = chips.find(item => item.dim === dim);
  return {
    chips: orderChips(chips.filter(item => item.dim !== dim)),
    suppressAutoSkill: Boolean(removed && removed.dim === "skill" && removed.auto)
  };
}

function chipOf(state: MarketingComposerState, dim: ComposerChipDim): ComposerChip | undefined {
  return state.chips.find(item => item.dim === dim);
}

// ---------------------------------------------------------------- 槽位

/** 已有内容放进新槽位声明会不会丢东西。 */
export function slotConflicts(filled: MarketingComposerState["filled"], target: readonly SlotDecl[]): SlotConflict[] {
  const out: SlotConflict[] = [];
  for (const [key, count] of Object.entries(filled) as Array<[SlotKey, number | undefined]>) {
    if (!count || count <= 0) continue;
    const decl = target.find(slot => slot.key === key);
    if (!decl) out.push({ key, count, reason: "missing" });
    else if (decl.maxCount > 0 && count > decl.maxCount) out.push({ key, count, reason: "over_limit", maxCount: decl.maxCount });
  }
  return out;
}

/** 冲突的提示文案（组件弹一次确认用）。 */
export function conflictText(conflicts: readonly SlotConflict[]): string {
  return conflicts
    .map(item => (item.reason === "missing"
      ? `已添加的${SLOT_NOUN[item.key]}将被移除（新技能不需要${SLOT_NOUN[item.key]}）`
      : `已添加 ${item.count} ${SLOT_COUNT_UNIT[item.key]}${SLOT_NOUN[item.key]}，新技能最多 ${item.maxCount} ${SLOT_COUNT_UNIT[item.key]}，将被清空`))
    .join("；");
}

function slotsForSkill(state: MarketingComposerState, skill: SkillForComposer | null): SlotDecl[] {
  return skill && skill.inputSlots.length > 0 ? skill.inputSlots : state.defaultSlots;
}

function clearFilled(filled: MarketingComposerState["filled"], keys: readonly SlotKey[]): MarketingComposerState["filled"] {
  return Object.fromEntries(Object.entries(filled).filter(([key]) => !keys.includes(key as SlotKey))) as MarketingComposerState["filled"];
}

// ---------------------------------------------------------------- 模型偏好

/** 撤掉技能对偏好的改写：只回退仍是技能设的值的那几节（用户自己又换过的不动）。 */
function restorePrefs(prefs: AgentPrefsValue, skillPrefs: MarketingComposerState["skillPrefs"]): AgentPrefsValue {
  let out = prefs;
  for (const kind of BOUND_KINDS) {
    const entry = skillPrefs?.[kind];
    if (!entry || prefModel(out, kind) !== entry.applied) continue;
    out = withPrefFields(out, kind, { model: entry.original });
  }
  return out;
}

function boundModels(skill: SkillForComposer): Array<[PrefKind, string]> {
  const bindings = skill.modelBindings ?? {};
  return BOUND_KINDS.flatMap(kind => {
    const code = bindings[kind]?.code;
    return code ? [[kind, code] as [PrefKind, string]] : [];
  });
}

// ---------------------------------------------------------------- 技能

/**
 * 选技能（手选或 MK-20 自动带入）：
 * ① 预填：正文为空或仍是上一个预填原文时填入 `prefillText`；改过一个字就不动（MKT-AC-03）。
 * ② 槽位：切到技能的 `inputSlots`（没声明用场景默认）；已有内容放不下 → `needsConfirm`，状态不变。
 * ③ 模型：技能有 `modelBindings` 时把对应节切到「自定义模型」，记下原选择供删技能时回退。
 */
export function applySkill(
  state: MarketingComposerState,
  skill: SkillForComposer,
  options: { auto?: boolean; confirmed?: boolean } = {}
): SwitchResult {
  const slots = slotsForSkill(state, skill);
  const conflicts = slotConflicts(state.filled, slots);
  if (conflicts.length > 0 && !options.confirmed) return { state, needsConfirm: conflicts, clearSlots: [] };
  const clearSlots = conflicts.map(item => item.key);

  const pristine = state.text.trim() === "" || (state.lastPrefill !== "" && state.text === state.lastPrefill);
  const fill = pristine && skill.prefillText !== "";

  // 先撤掉上一个技能的改写，再套这个技能的，换技能时「原选择」始终是用户自己的那个。
  const base = restorePrefs(state.prefs, state.skillPrefs);
  const bound = boundModels(skill);
  let prefs = base;
  const skillPrefs: NonNullable<MarketingComposerState["skillPrefs"]> = {};
  for (const [kind, code] of bound) {
    skillPrefs[kind] = { original: prefModel(base, kind), applied: code };
    prefs = withPrefFields(prefs, kind, { model: code });
  }

  return {
    state: {
      ...state,
      chips: setChip(state.chips, { dim: "skill", code: skill.code, name: skill.name, ...(options.auto ? { auto: true } : {}) }),
      text: fill ? skill.prefillText : state.text,
      lastPrefill: fill ? skill.prefillText : state.lastPrefill,
      slots,
      filled: clearFilled(state.filled, clearSlots),
      prefs,
      skillPrefs: bound.length > 0 ? skillPrefs : null
    },
    clearSlots
  };
}

/**
 * 删技能（MKT-UX-05）：槽位回场景默认、模型偏好回原选择、**正文不回退**；删的是自动带入的 → 本次不再补回。
 * 场景默认放不下已有内容时同样先问（`needsConfirm`，状态不变）。
 */
export function removeSkill(state: MarketingComposerState, options: { confirmed?: boolean } = {}): SwitchResult {
  if (!chipOf(state, "skill")) return { state, clearSlots: [] };
  const conflicts = slotConflicts(state.filled, state.defaultSlots);
  if (conflicts.length > 0 && !options.confirmed) return { state, needsConfirm: conflicts, clearSlots: [] };
  const clearSlots = conflicts.map(item => item.key);
  const removed = removeChip(state.chips, "skill");
  return {
    state: {
      ...state,
      chips: removed.chips,
      suppressAutoSkill: state.suppressAutoSkill || removed.suppressAutoSkill,
      slots: state.defaultSlots,
      filled: clearFilled(state.filled, clearSlots),
      prefs: restorePrefs(state.prefs, state.skillPrefs),
      skillPrefs: null
    },
    clearSlots
  };
}

// ---------------------------------------------------------------- 创意 / Hook / 风格

export type AutoSkillOutcome =
  /** 创意没有默认技能。 */
  | "none"
  /** 带入了（或换成了这个创意的默认技能）。 */
  | "applied"
  /** 已有手选技能，不替换。 */
  | "kept_manual"
  /** 本次输入里删过自动带入的技能，不再补回。 */
  | "suppressed"
  /** 默认技能不在当前可选列表里（停用 / 不属于营销场景）。 */
  | "unavailable"
  /** 带入会清掉已有内容，放弃带入（自动带入不弹确认）。 */
  | "skipped_conflict";

/**
 * 选创意（MKT-F-16 / MK-20）：插创意胶囊；当前没有手选技能 ∧ 没删过自动带入 → 带入默认技能（`auto`）。
 * `defaultSkill` 由调用方按 `option.defaultSkillCode` 从技能列表里找；找不到传 null。
 * 当前技能若也是自动带入的（上一个创意带的），换成这个创意的默认技能 —— 它不是用户选的。
 */
export function applyCreative(
  state: MarketingComposerState,
  option: MarketingPick,
  defaultSkill: SkillForComposer | null | undefined
): SwitchResult & { autoSkill: AutoSkillOutcome } {
  const picked = applyPick(state, "creative", option);
  const next: MarketingComposerState = { ...picked, smartMatchHint: (option.recommendedModels ?? []).map(model => model.code) };
  const result = (autoSkill: AutoSkillOutcome, s: MarketingComposerState = next) => ({ state: s, clearSlots: [], autoSkill });

  const code = option.defaultSkillCode;
  if (!code) return result("none");
  const current = chipOf(next, "skill");
  if (current && !current.auto) return result("kept_manual");
  if (current?.code === code) return result("applied");
  if (next.suppressAutoSkill) return result("suppressed");
  if (!defaultSkill || defaultSkill.code !== code) return result("unavailable");
  const applied = applySkill(next, defaultSkill, { auto: true });
  if (applied.needsConfirm) return result("skipped_conflict");
  return result("applied", applied.state);
}

/** 选 Hook / 风格（创意请用 `applyCreative`，它还有联动）：同维替换并记下输入要求。 */
export function applyPick(state: MarketingComposerState, dim: PickDim, option: MarketingPick): MarketingComposerState {
  return {
    ...state,
    chips: setChip(state.chips, { dim, code: option.code, name: option.name }),
    pickRequirements: { ...state.pickRequirements, [dim]: { ...(option.inputRequirements ?? {}) } }
  };
}

/**
 * 删任意一维的胶囊（× 按钮与正文开头退格都走这里）：技能走 `removeSkill`；创意连带清掉推荐模型提示；
 * 删创意**不连带删**它带入的技能（技能已经在正文里起了作用，悄悄消失他会以为点错了）。
 */
export function dropChip(state: MarketingComposerState, dim: ComposerChipDim, options: { confirmed?: boolean } = {}): SwitchResult {
  if (dim === "skill") return removeSkill(state, options);
  const pickRequirements = Object.fromEntries(Object.entries(state.pickRequirements).filter(([key]) => key !== dim)) as MarketingComposerState["pickRequirements"];
  return {
    state: {
      ...state,
      chips: removeChip(state.chips, dim).chips,
      pickRequirements,
      smartMatchHint: dim === "creative" ? [] : state.smartMatchHint
    },
    clearSlots: []
  };
}

// ---------------------------------------------------------------- 发送键

export type MarketingGateCode = "read_only" | "uploading" | "slot_required" | "pick_requirement" | "base";
export type MarketingSendGate = SendGate & { code?: MarketingGateCode; slot?: SlotKey };

/**
 * 发送键能不能点、不能点为什么（MKT-F-17、MKT-AC-04 前端部分）。
 *
 * 顺序（给**最根本的那一条**，CD-59 / CD-60）：只读权限 > 上传中 > 必填槽缺失（「请选择商品」）
 * > 所选创意 / Hook / 风格的输入要求未满足 > 其余交给 Agent 模式原有的判定 `base`（发送中、空内容、报价、余额）。
 * 只读排最前：否则只读成员会去传图、去充值，而真正的原因从头到尾是他没有权限。
 */
export function marketingSendGate(
  state: MarketingComposerState,
  ctx: { readOnly: boolean; uploading: boolean; base?: SendGate }
): MarketingSendGate {
  if (ctx.readOnly) return { disabled: true, reason: "你在该工作区是只读，无法发起创作", code: "read_only" };
  if (ctx.uploading) return { disabled: true, reason: "附件上传中，完成后再发送", code: "uploading" };
  const has = (key: SlotKey) => (state.filled[key] ?? 0) > 0;
  // 素材裂变的源视频本身就是商品视频：「需要商品图」的选项有源视频也算满足（04 文档 P-47，与服务端同一条）。
  const satisfies = (key: SlotKey) => has(key) || (key === "product" && has("source_video"));
  for (const slot of state.slots) {
    if (slot.required && !has(slot.key)) {
      return { disabled: true, reason: `请${SLOT_VERB[slot.key]}${SLOT_NOUN[slot.key]}`, code: "slot_required", slot: slot.key };
    }
  }
  for (const dim of PICK_DIMS) {
    const chip = chipOf(state, dim);
    const requirements = state.pickRequirements[dim];
    if (!chip || !requirements) continue;
    for (const [key, level] of Object.entries(requirements) as Array<[SlotKey, string]>) {
      if (level !== "required" || satisfies(key) || !(key in SLOT_NOUN)) continue;
      const offered = state.slots.some(slot => slot.key === key);
      const reason = offered
        ? `「${chipText(chip)}」需要${SLOT_NOUN[key]}，请${SLOT_VERB[key]}${SLOT_NOUN[key]}`
        : `「${chipText(chip)}」需要${SLOT_NOUN[key]}，当前技能不支持添加${SLOT_NOUN[key]}`;
      return { disabled: true, reason, code: "pick_requirement", slot: key };
    }
  }
  if (ctx.base?.disabled) return { ...ctx.base, code: "base" };
  return { disabled: false, reason: "" };
}
