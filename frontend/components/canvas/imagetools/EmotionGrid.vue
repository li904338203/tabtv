<script setup lang="ts">
/**
 * 情绪坐标盘 5×5（docs/agent/imagetools IMG-F-14）：纵轴上「激动」下「平静」，横轴左「亲近」右「疏离」；
 * 点格子选中、选中的格子放大成大白点。
 *
 * 无障碍：整盘是一个 radiogroup，25 格各是一个 radio（读屏读情绪名 + 两个轴上的位置）；
 * 用 roving tabindex —— Tab 只停在选中那一格，方向键在格子之间移动并**直接选中**（与原生单选组一致），Home 回中心。
 */
import { imageToolEmotionGrid, type ImageToolsResolvedConfig } from "~/utils/imageTools";
import { imageToolEmotionMove } from "~/utils/imageToolEmotion";

type Emotion = ImageToolsResolvedConfig["emotions"][number];

const props = withDefaults(defineProps<{
  emotions: readonly Emotion[];
  arousal: number;
  warmth: number;
  disabled?: boolean;
  /** 读屏用的整盘名（如「角色1 的情绪」）。 */
  label?: string;
}>(), { disabled: false, label: "情绪坐标" });
const emit = defineEmits<{ select: [value: { arousal: number; warmth: number }] }>();

const AXIS = [2, 1, 0, -1, -2];
const grid = computed(() => imageToolEmotionGrid(props.emotions));
const cells = ref<Record<string, HTMLButtonElement | null>>({});

function keyOf(arousal: number, warmth: number) {
  return `${arousal}_${warmth}`;
}

function axisText(arousal: number, warmth: number) {
  const a = arousal === 0 ? "激动程度居中" : arousal > 0 ? `偏激动 ${arousal}` : `偏平静 ${-arousal}`;
  const w = warmth === 0 ? "亲疏居中" : warmth > 0 ? `偏亲近 ${warmth}` : `偏疏离 ${-warmth}`;
  return `${a}，${w}`;
}

function pick(arousal: number, warmth: number) {
  if (props.disabled) return;
  emit("select", { arousal, warmth });
}

function onKeydown(event: KeyboardEvent) {
  if (props.disabled) return;
  const next = imageToolEmotionMove({ arousal: props.arousal, warmth: props.warmth }, event.key);
  if (!next) return;
  event.preventDefault();
  event.stopPropagation();
  emit("select", next);
  void nextTick(() => cells.value[keyOf(next.arousal, next.warmth)]?.focus());
}
</script>

<template>
  <div class="emotion-grid" :class="{ 'is-disabled': disabled }">
    <span class="emotion-axis is-top" aria-hidden="true">激动</span>
    <span class="emotion-axis is-bottom" aria-hidden="true">平静</span>
    <span class="emotion-axis is-left" aria-hidden="true">亲近</span>
    <span class="emotion-axis is-right" aria-hidden="true">疏离</span>
    <div
      class="emotion-grid-board"
      role="radiogroup"
      :aria-label="`${label}（上激动、下平静、左亲近、右疏离，方向键移动）`"
      :aria-disabled="disabled || undefined"
      @keydown="onKeydown"
    >
      <span class="emotion-grid-cross is-h" aria-hidden="true" />
      <span class="emotion-grid-cross is-v" aria-hidden="true" />
      <template v-for="(row, r) in grid" :key="r">
        <button
          v-for="(cell, c) in row"
          :key="keyOf(AXIS[r]!, AXIS[c]!)"
          :ref="(el) => { cells[keyOf(AXIS[r]!, AXIS[c]!)] = el as HTMLButtonElement | null }"
          type="button"
          role="radio"
          class="emotion-grid-cell"
          :class="{ 'is-selected': AXIS[r] === arousal && AXIS[c] === warmth }"
          :aria-checked="AXIS[r] === arousal && AXIS[c] === warmth"
          :aria-label="`${cell?.name ?? '未配置'}（${axisText(AXIS[r]!, AXIS[c]!)}）`"
          :title="cell?.name"
          :tabindex="!disabled && AXIS[r] === arousal && AXIS[c] === warmth ? 0 : -1"
          :disabled="disabled || !cell"
          :data-testid="`emotion-cell-${keyOf(AXIS[r]!, AXIS[c]!)}`"
          @click="pick(AXIS[r]!, AXIS[c]!)"
        >
          <span class="emotion-grid-dot" />
        </button>
      </template>
    </div>
  </div>
</template>

<style scoped>
.emotion-grid { position: relative; display: grid; place-items: center; width: 100%; height: 100%; padding: 26px 36px; border-radius: 14px; background: var(--cv-n10); }
.emotion-grid.is-disabled { opacity: .45; }
.emotion-axis { position: absolute; color: var(--cv-n63); font-size: 11px; line-height: 1; white-space: nowrap; }
.emotion-axis.is-top { top: 9px; left: 50%; transform: translateX(-50%); }
.emotion-axis.is-bottom { bottom: 9px; left: 50%; transform: translateX(-50%); }
.emotion-axis.is-left { top: 50%; left: 10px; transform: translateY(-50%); }
.emotion-axis.is-right { top: 50%; right: 10px; transform: translateY(-50%); }
.emotion-grid-board { position: relative; display: grid; grid-template-columns: repeat(5, 1fr); grid-template-rows: repeat(5, 1fr); width: 100%; max-width: 220px; aspect-ratio: 1; }
.emotion-grid-cross { position: absolute; background: var(--cv-n24); pointer-events: none; }
.emotion-grid-cross.is-h { top: 50%; left: 10%; right: 10%; height: 1px; }
.emotion-grid-cross.is-v { left: 50%; top: 10%; bottom: 10%; width: 1px; }
.emotion-grid-cell { position: relative; display: grid; place-items: center; border: 0; border-radius: 50%; padding: 0; background: transparent; cursor: pointer; }
.emotion-grid-cell:disabled { cursor: not-allowed; }
.emotion-grid-dot { width: 7px; height: 7px; border-radius: 50%; background: var(--cv-n45); transition: width .12s ease, height .12s ease, background .12s ease; }
.emotion-grid-cell:not(:disabled):hover .emotion-grid-dot { width: 11px; height: 11px; background: var(--cv-n70); }
.emotion-grid-cell.is-selected .emotion-grid-dot { width: 22px; height: 22px; background: var(--cv-n100); box-shadow: 0 0 0 4px var(--cv-n24); }
.emotion-grid-cell:focus-visible { outline: none; }
.emotion-grid-cell:focus-visible .emotion-grid-dot { box-shadow: 0 0 0 2px var(--cv-n15), 0 0 0 4px var(--cv-n88); }
</style>
