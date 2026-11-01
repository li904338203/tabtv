<script setup lang="ts">
/**
 * 输入框正文（AGT-42 起；AGT-29 加技能胶囊与 `/`、`@` 触发）。
 *
 * - 自动增高，最多 8 行后滚动（设计 02 §4.1）；
 * - `Enter` 发送、`Shift+Enter` 换行；粘贴只取纯文本（textarea 天然如此）；
 * - ⚠️ **中文输入法组字时 `Enter` 不发送**：组字中的回车是"选字确认"，发出去的是半截拼音。
 *   判定在 `utils/composerKeys`（`isComposing` / keyCode 229 / 本组件记的组字状态，三者兜底）；
 * - 胶囊（MKT-T-11）：技能 / 创意 / Hook / 风格四个维度各至多一个，按 技能 → 创意 → Hook → 风格 固定顺序显示在正文
 *   **开头**（与选择先后无关）；光标在最前面时按退格删**最后一个**。创造页与画布面板只传技能一个胶囊，
 *   渲染、退格、`/` 触发与单技能时代逐字一致（判定都在 `utils/composerKeys`，有测试钉住）；
 * - `/`（开头）与 `@` 触发候选：上报 `trigger`，候选打开期间 ↑↓/Enter/Tab/Esc 交给候选（`picker-key`），
 *   **组字期间不交**（那时的回车与方向键属于输入法）。
 *
 * 用 textarea 而不是 contenteditable：引用以胶囊对象为准（不靠解析正文），正文只需要纯文本；
 * textarea 的 IME、撤销、粘贴纯文本在各浏览器上都是一致的，contenteditable 恰恰在这几处各不相同。
 */
import { X, Zap } from "@lucide/vue";
import {
  chipRemoveLabel,
  chipText,
  chipTitle,
  chipToRemoveOnBackspace,
  detectTrigger,
  enterAction,
  hasSkillChip,
  isComposingKey,
  orderChips,
  type ComposerChip,
  type ComposerChipDim,
  type Trigger
} from "~/utils/composerKeys";

const props = defineProps<{
  modelValue: string;
  placeholder?: string;
  disabled?: boolean;
  /** 开头的胶囊（每维至多一个）；没有为空数组 / undefined。 */
  chips?: ComposerChip[] | null;
  /** 候选（技能 / 引用）正在显示：方向键与回车交给候选。 */
  pickerOpen?: boolean;
  /** 是否识别 `/` 与 `@`（创造页直接生成模式不需要）。 */
  triggers?: boolean;
}>();
const emit = defineEmits<{
  "update:modelValue": [value: string];
  submit: [];
  focus: [];
  /** 光标前正在输入的 `/技能` 或 `@引用`；没有时为 null（候选应关闭）。 */
  trigger: [value: Trigger | null];
  "picker-key": [key: "up" | "down" | "confirm" | "close"];
  /** 删掉某一维的胶囊（× 按钮或正文开头退格）。 */
  "remove-chip": [dim: ComposerChipDim];
}>();

const area = ref<HTMLTextAreaElement>();
const composing = ref(false);
const MAX_ROWS = 8;

const orderedChips = computed(() => orderChips(props.chips ?? []));

function resize() {
  const el = area.value;
  if (!el) return;
  el.style.height = "auto";
  const line = Number.parseFloat(getComputedStyle(el).lineHeight) || 22;
  el.style.height = `${Math.min(el.scrollHeight, line * MAX_ROWS + 4)}px`;
  el.style.overflowY = el.scrollHeight > line * MAX_ROWS + 4 ? "auto" : "hidden";
}

function reportTrigger() {
  if (!props.triggers) return;
  const el = area.value;
  if (!el) return;
  const before = el.value.slice(0, el.selectionStart ?? el.value.length);
  emit("trigger", detectTrigger(before, { skillSelected: hasSkillChip(props.chips) }));
}

function onInput(event: Event) {
  emit("update:modelValue", (event.target as HTMLTextAreaElement).value);
  resize();
  // 组字中不报：拼音字母不是关键字，等上屏之后再判。
  if (!composing.value) reportTrigger();
}

function onKeydown(event: KeyboardEvent) {
  const composingNow = isComposingKey({ key: event.key, isComposing: event.isComposing, keyCode: event.keyCode, composing: composing.value });
  if (props.pickerOpen && !composingNow) {
    const map: Record<string, "up" | "down" | "confirm" | "close"> = {
      ArrowUp: "up",
      ArrowDown: "down",
      Enter: "confirm",
      Tab: "confirm",
      Escape: "close"
    };
    const key = map[event.key];
    if (key && !(event.key === "Enter" && event.shiftKey)) {
      event.preventDefault();
      event.stopPropagation();
      emit("picker-key", key);
      return;
    }
  }
  const el = area.value;
  const removeDim = el
    ? chipToRemoveOnBackspace(orderedChips.value, { key: event.key, selectionStart: el.selectionStart, selectionEnd: el.selectionEnd, composing: composingNow })
    : null;
  if (removeDim) {
    event.preventDefault();
    emit("remove-chip", removeDim);
    return;
  }
  const action = enterAction({ key: event.key, shiftKey: event.shiftKey, isComposing: event.isComposing, keyCode: event.keyCode, composing: composing.value });
  if (action === "none" && event.key === "Enter") return; // 组字中：交给输入法
  if (action !== "send") return;
  event.preventDefault();
  emit("submit");
}

function onCompositionEnd() {
  composing.value = false;
  reportTrigger();
}

/**
 * 把光标前从 `start` 起的触发文字（`/故事`、`@sho`）换成 `replacement`（默认删掉）。
 * 选中候选之后调用：胶囊已经代表了它，正文里不该再留一截 `@sho`。
 */
function replaceTrigger(start: number, replacement = "") {
  const el = area.value;
  const value = props.modelValue;
  const caret = el?.selectionStart ?? value.length;
  if (start < 0 || start > caret) return;
  const next = value.slice(0, start) + replacement + value.slice(caret);
  emit("update:modelValue", next);
  emit("trigger", null);
  nextTick(() => {
    if (!el) return;
    const position = start + replacement.length;
    el.focus();
    el.setSelectionRange(position, position);
    resize();
  });
}

watch(() => props.modelValue, () => nextTick(resize));
onMounted(resize);

defineExpose({ focus: () => area.value?.focus(), replaceTrigger });
</script>

<template>
  <div class="composer-editor-wrap" :class="{ 'has-skill': hasSkillChip(orderedChips) }">
    <span v-for="chip in orderedChips" :key="chip.dim" class="composer-skill" :class="`dim-${chip.dim}`" :title="chipTitle(chip)">
      <Zap v-if="chip.dim === 'skill'" :size="12" />
      <span>{{ chipText(chip) }}</span>
      <button type="button" :aria-label="chipRemoveLabel(chip.dim)" :disabled="disabled" @click="emit('remove-chip', chip.dim)"><X :size="10" /></button>
    </span>
    <textarea
      ref="area"
      class="composer-editor"
      rows="2"
      :value="modelValue"
      :placeholder="placeholder"
      :disabled="disabled"
      aria-label="创作描述"
      @input="onInput"
      @keydown="onKeydown"
      @click="reportTrigger"
      @compositionstart="composing = true"
      @compositionend="onCompositionEnd"
      @focus="emit('focus')"
    />
  </div>
</template>

<style scoped>
/*
 * 胶囊单独占一行（多了自动换行），正文永远在胶囊**下方**另起一行（MKT-UX-04，2026-10-24 按用户要求修订：
 * 原先单个胶囊时正文接在同一行，正文一长就被挤成窄窄一条、换行处也对不齐）。没有胶囊时就只有正文。
 */
.composer-editor-wrap { display: flex; flex-wrap: wrap; align-items: center; gap: 8px 6px; }
/*
 * 胶囊是中性色：描边 + 正文色，不再用青色（2026-10-25 按用户要求，输入框统一成中性主题色）。
 * 删除用的 × 平时隐藏，悬浮（或键盘聚焦到胶囊）时出现在右上角；触屏没有悬浮，× 常驻 —— 否则删不掉。
 */
.composer-skill {
  position: relative;
  flex: none;
  height: 26px;
  display: inline-flex;
  align-items: center;
  gap: 4px;
  border: 1px solid var(--portal-line-strong);
  border-radius: 7px;
  padding: 0 9px;
  color: var(--portal-text);
  background: var(--portal-surface-2);
  font-size: 12px;
  white-space: nowrap;
}
.composer-skill > svg { color: var(--portal-muted); }
.composer-skill button {
  position: absolute;
  z-index: 1;
  top: -7px;
  right: -7px;
  width: 16px;
  height: 16px;
  display: grid;
  place-items: center;
  border: 1px solid var(--portal-line-strong);
  border-radius: 50%;
  padding: 0;
  color: var(--portal-text);
  background: var(--portal-bg-deep);
  cursor: pointer;
  opacity: 0;
  pointer-events: none;
  transition: opacity .12s ease;
}
.composer-skill:hover button:not(:disabled),
.composer-skill:focus-within button:not(:disabled) { opacity: 1; pointer-events: auto; }
.composer-skill button:hover:not(:disabled) { background: var(--portal-surface-3); }
@media (hover: none) {
  .composer-skill button:not(:disabled) { opacity: 1; pointer-events: auto; }
}
.composer-editor {
  flex: 1 1 100%;
  min-width: 0;
  width: 100%;
  min-height: 52px;
  resize: none;
  overflow-y: hidden;
  border: 0;
  outline: 0;
  padding: 0;
  color: var(--portal-text);
  background: transparent;
  font: inherit;
  font-size: 14px;
  line-height: 22px;
}
.composer-editor::placeholder { color: var(--portal-placeholder); }
.composer-editor:disabled { color: var(--portal-disabled-text); cursor: not-allowed; }
</style>
