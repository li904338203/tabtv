/**
 * Agent 输入框的按键判定（AGT-29，设计 02 §4.1）。纯函数。
 *
 * ⚠️ **中文输入法组字时 `Enter` 不发送**：组字中的回车是「选字确认」，发出去的是半截拼音。
 *    三种判断都要兜住：`event.isComposing`（部分浏览器在最后一次 keydown 上已经是 false）、
 *    `keyCode === 229`（IME 处理中的 keydown）、组件自己记的 `compositionstart/end` 状态。
 */
export type KeyInput = {
  key: string;
  shiftKey?: boolean;
  isComposing?: boolean;
  keyCode?: number;
  /** 组件自己记的组字状态（compositionstart → true，compositionend → false）。 */
  composing?: boolean;
};

export type SendContext = {
  /** 正文（去掉首尾空白后）有没有内容。 */
  hasText: boolean;
  /** 有没有引用 / 附件胶囊。 */
  hasRefs: boolean;
  /** 附件还在上传。 */
  uploading: boolean;
  /** 输入框整体不可用（只读、发送中…）。 */
  disabled?: boolean;
};

/** 正在组字（任一判断成立即算）。 */
export function isComposingKey(event: KeyInput): boolean {
  return Boolean(event.composing || event.isComposing || event.keyCode === 229);
}

/** 这一下回车是「发送」还是「换行」还是什么都不做。 */
export function enterAction(event: KeyInput): "send" | "newline" | "none" {
  if (event.key !== "Enter") return "none";
  if (isComposingKey(event)) return "none";
  if (event.shiftKey) return "newline";
  return "send";
}

/** 内容是否够发：空正文且没有任何胶囊不发；附件上传中不发（发出去的引用会缺那一张）。 */
export function canSubmitContent(ctx: SendContext): boolean {
  if (ctx.disabled) return false;
  if (ctx.uploading) return false;
  return ctx.hasText || ctx.hasRefs;
}

/** 回车是否应触发发送（按键 + 内容两层）。 */
export function shouldSend(event: KeyInput, ctx: SendContext): boolean {
  return enterAction(event) === "send" && canSubmitContent(ctx);
}

export type Trigger = { kind: "/" | "@"; query: string; start: number };

const MAX_TRIGGER_QUERY = 24;

/**
 * 光标前的文字里有没有正在输入的 `/技能` 或 `@引用`。
 *
 * - `/` 只在**正文开头**生效（技能胶囊只能在开头，同一时间只有一个；正文中间的 `/` 多半是「和/或」、网址）；
 *   已经选了技能时不再触发；
 * - `@` 在开头或空白之后生效（`a@b.com` 这类邮箱不触发）；
 * - 关键字里出现空白即结束（用户已经在写别的了）。
 */
export function detectTrigger(textBeforeCaret: string, options: { skillSelected?: boolean } = {}): Trigger | null {
  const at = textBeforeCaret.lastIndexOf("@");
  if (at >= 0) {
    const before = at === 0 ? "" : textBeforeCaret[at - 1]!;
    const query = textBeforeCaret.slice(at + 1);
    if ((at === 0 || /\s/.test(before)) && !/\s/.test(query) && query.length <= MAX_TRIGGER_QUERY) {
      return { kind: "@", query, start: at };
    }
  }
  if (!options.skillSelected) {
    const leading = textBeforeCaret.length - textBeforeCaret.trimStart().length;
    if (textBeforeCaret[leading] === "/") {
      const query = textBeforeCaret.slice(leading + 1);
      if (!/\s/.test(query) && query.length <= MAX_TRIGGER_QUERY) return { kind: "/", query, start: leading };
    }
  }
  return null;
}

/** 候选列表里的上下移动（循环）。 */
export function moveIndex(current: number, delta: number, length: number): number {
  if (length <= 0) return -1;
  if (current < 0) return delta > 0 ? 0 : length - 1;
  return (current + delta + length) % length;
}

/**
 * 输入框开头的胶囊（MKT-T-11，MKT-F-14 / MKT-UX-04）。
 *
 * 四个维度各至多一个，顺序固定 **技能 → 创意 → Hook → 风格**（与选择先后无关）。
 * 创造页与画布面板只传技能一个胶囊 —— 那时的渲染、退格、`/` 触发与单技能时代逐字一致。
 * `auto` = 选创意时自动带入的默认技能（MK-20），删掉它要记下「本次不再补回」，见 utils/marketingComposer。
 */
export type ComposerChipDim = "skill" | "creative" | "hook" | "style";
export type ComposerChip = { dim: ComposerChipDim; code: string; name: string; auto?: boolean };

export const CHIP_DIM_ORDER: readonly ComposerChipDim[] = ["skill", "creative", "hook", "style"];

const CHIP_PREFIX: Record<ComposerChipDim, string> = { skill: "", creative: "创意：", hook: "Hook：", style: "风格：" };
const CHIP_NOUN: Record<ComposerChipDim, string> = { skill: "技能", creative: "创意", hook: "Hook", style: "风格" };

/** 按固定维度顺序排好（稳定排序；调用方保证每维至多一个）。 */
export function orderChips(chips: readonly ComposerChip[]): ComposerChip[] {
  return [...chips].sort((a, b) => CHIP_DIM_ORDER.indexOf(a.dim) - CHIP_DIM_ORDER.indexOf(b.dim));
}

/** 胶囊上显示的文字：技能只显示名，其余带前缀「创意：」「Hook：」「风格：」。 */
export function chipText(chip: ComposerChip): string {
  return `${CHIP_PREFIX[chip.dim]}${chip.name}`;
}

/** 悬停提示（技能沿用旧的「技能：xxx」）。 */
export function chipTitle(chip: ComposerChip): string {
  return `${CHIP_NOUN[chip.dim]}：${chip.name}`;
}

/** 删除按钮的无障碍名（技能沿用旧的「移除技能」）。 */
export function chipRemoveLabel(dim: ComposerChipDim): string {
  return `移除${CHIP_NOUN[dim]}`;
}

/**
 * 正文开头按退格要删哪个胶囊：光标在最前面（且没有选区）、不在组字中时删**最后一个**（顺序上的最后一个）；
 * 否则 null（按正常退格处理）。
 */
export function chipToRemoveOnBackspace(
  chips: readonly ComposerChip[],
  caret: { key: string; selectionStart: number | null; selectionEnd: number | null; composing: boolean }
): ComposerChipDim | null {
  if (caret.key !== "Backspace" || caret.composing || chips.length === 0) return null;
  if (caret.selectionStart !== 0 || caret.selectionEnd !== 0) return null;
  return orderChips(chips).at(-1)!.dim;
}

/** `detectTrigger` 的 `skillSelected`：由「有没有技能胶囊」推出。 */
export function hasSkillChip(chips: readonly ComposerChip[] | null | undefined): boolean {
  return Boolean(chips?.some(chip => chip.dim === "skill"));
}
