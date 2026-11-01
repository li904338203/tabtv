<script setup lang="ts">
/**
 * 时间线的一行（SCT-20）：左侧固定的轨道头（名称、用途 / 语言、隐藏、静音、音量）+ 右侧的轨道（片段、字幕、文字层）。
 *
 * 轨道头的改动直接经 `smartcutPatchTrack` 记一步撤销；音量在失焦或回车时才提交（逐字提交会压几十步撤销）。
 * 拖动与键盘导航交给 TimelineView（它持有拖动状态与指针捕获）。
 */
import { Eye, EyeOff, Volume2, VolumeX } from "@lucide/vue";
import type { components } from "@tabtv/api-client";
import type { SmartcutTrackPatch } from "~/utils/smartcutEdit";
import ClipBlock from "./ClipBlock.vue";
import type { SmartcutLaneItem } from "./context";

type TimelineTrack = components["schemas"]["TimelineTrack"];

const props = defineProps<{
  track: TimelineTrack;
  label: string;
  items: SmartcutLaneItem[];
  selectedIds: ReadonlySet<string>;
  focusId: string;
  dragId: string;
  dragDx: number;
  invalidId: string;
  /** 画面轨拖动排序时插入线的位置（像素，相对轨道左边缘）；没有为 null。 */
  insertPx: number | null;
  laneWidth: number;
  visibleFromPx: number;
  visibleToPx: number;
  editable: boolean;
  disabledReason: string;
  headWidth: number;
  height: number;
}>();
const emit = defineEmits<{
  "item-pointerdown": [event: PointerEvent, item: SmartcutLaneItem, part: "body" | "start" | "end"];
  "item-keydown": [event: KeyboardEvent, item: SmartcutLaneItem];
  "lane-pointerdown": [event: PointerEvent];
  patch: [patch: SmartcutTrackPatch];
  seam: [clipId: string];
}>();

const hasSound = computed(() => props.track.kind === "video" || props.track.kind === "audio");
const hideable = computed(() => props.track.kind === "subtitle" || props.track.kind === "text");
const volumeDraft = ref(props.track.volume);
watch(() => props.track.volume, (v) => { volumeDraft.value = v; });

function commitVolume() {
  const v = Math.max(0, Math.min(200, Math.round(Number(volumeDraft.value))));
  if (!Number.isFinite(v)) {
    volumeDraft.value = props.track.volume;
    return;
  }
  volumeDraft.value = v;
  if (v !== props.track.volume) emit("patch", { volume: v });
}

function setRole(value: string) {
  emit("patch", { role: value ? value as NonNullable<TimelineTrack["role"]> : null });
}

const dimTitle = (base: string) => (props.editable ? base : props.disabledReason);
</script>

<template>
  <div class="sc-row" :class="`kind-${track.kind}`" :style="{ height: `${height}px` }" :data-track-id="track.id" :data-track-kind="track.kind">
    <div class="sc-row-head" :style="{ width: `${headWidth}px` }">
      <span class="sc-row-name" :title="label">{{ label }}</span>
      <div class="sc-row-tools">
        <select
          v-if="track.kind === 'audio'"
          class="sc-row-role"
          :value="track.role ?? ''"
          :disabled="!editable"
          :title="dimTitle('这条音频轨的用途：人声闪避按它找「谁压谁」')"
          aria-label="音频用途"
          @change="setRole(($event.target as HTMLSelectElement).value)"
        >
          <option value="">未指定</option>
          <option value="music">配乐</option>
          <option value="voice">人声</option>
          <option value="sfx">音效</option>
        </select>
        <button
          v-if="hideable"
          type="button"
          class="sc-btn icon"
          :class="{ active: track.isHidden }"
          :disabled="!editable"
          :title="dimTitle(track.isHidden ? '已隐藏：不出现在预览与成片里' : '隐藏这条轨')"
          :aria-label="track.isHidden ? '显示这条轨' : '隐藏这条轨'"
          :aria-pressed="Boolean(track.isHidden)"
          @click="emit('patch', { isHidden: !track.isHidden })"
        >
          <EyeOff v-if="track.isHidden" :size="13" /><Eye v-else :size="13" />
        </button>
        <template v-if="hasSound">
          <button
            type="button"
            class="sc-btn icon"
            :class="{ active: track.isMuted }"
            :disabled="!editable"
            :title="dimTitle(track.isMuted ? '已静音' : track.kind === 'video' ? '静音画面里的原声' : '静音这条轨')"
            :aria-label="track.isMuted ? '取消静音' : '静音'"
            :aria-pressed="track.isMuted"
            @click="emit('patch', { isMuted: !track.isMuted })"
          >
            <VolumeX v-if="track.isMuted" :size="13" /><Volume2 v-else :size="13" />
          </button>
          <input
            v-model.number="volumeDraft"
            class="sc-row-volume"
            type="number"
            min="0"
            max="200"
            step="5"
            :disabled="!editable"
            :title="dimTitle('轨道音量（0~200，100 = 原音量）')"
            aria-label="轨道音量"
            @change="commitVolume"
            @keydown.enter.prevent="commitVolume"
          >
        </template>
      </div>
    </div>
    <div class="sc-row-lane" :style="{ width: `${laneWidth}px` }" @pointerdown.self="emit('lane-pointerdown', $event)">
      <ClipBlock
        v-for="item in items"
        :key="item.id"
        :item="dragId === item.id && dragDx ? { ...item, left: item.left + dragDx } : item"
        :selected="selectedIds.has(item.id)"
        :focusable="focusId === item.id"
        :dragging="dragId === item.id && dragDx !== 0"
        :invalid="invalidId === item.id"
        :editable="editable"
        :visible-from-px="visibleFromPx"
        :visible-to-px="visibleToPx"
        :height="height - 8"
        @item-pointerdown="(event, part) => emit('item-pointerdown', event, item, part)"
        @item-keydown="(event) => emit('item-keydown', event, item)"
      />
      <button
        v-for="item in items.filter(i => i.overlapPx && i.marks.transition)"
        :key="`seam-${item.id}`"
        type="button"
        class="sc-row-seam"
        :style="{ left: `${item.left}px`, width: `${Math.max(10, item.overlapPx ?? 0)}px` }"
        title="转场：点击设置"
        aria-label="设置这一处转场"
        data-testid="smartcut-seam"
        @pointerdown.stop
        @click="emit('seam', item.id)"
      />
      <span v-if="insertPx !== null" class="sc-row-insert" :style="{ left: `${insertPx}px` }" aria-hidden="true" data-testid="smartcut-insert-mark" />
    </div>
  </div>
</template>

<style scoped>
.sc-row { display: flex; border-bottom: 1px solid var(--portal-line-soft); }
.sc-row-head {
  position: sticky;
  left: 0;
  z-index: 6;
  display: flex;
  flex-shrink: 0;
  flex-direction: column;
  justify-content: center;
  gap: 4px;
  padding: 0 8px;
  border-right: 1px solid var(--portal-line);
  background: var(--portal-panel);
  font-size: 12px;
}
.sc-row-name { overflow: hidden; color: var(--portal-text); font-weight: 600; text-overflow: ellipsis; white-space: nowrap; }
.sc-row-tools { display: flex; align-items: center; gap: 4px; }
.sc-row-tools .sc-btn.icon { width: 24px; height: 22px; }
.sc-row-role { height: 22px; border: 1px solid var(--portal-line); border-radius: 4px; color: var(--portal-text); background: var(--portal-input-bg); font-size: 11px; }
.sc-row-volume { width: 52px; height: 22px; padding: 0 4px; border: 1px solid var(--portal-line); border-radius: 4px; color: var(--portal-text); background: var(--portal-input-bg); font-size: 11px; }
.sc-row-lane { position: relative; flex-shrink: 0; padding: 4px 0; box-sizing: border-box; }
.sc-row-lane > :deep(.sc-clip) { top: 4px; }
.sc-row-seam { position: absolute; top: 4px; bottom: 24px; z-index: 6; padding: 0; border: 0; border-radius: 4px; background: repeating-linear-gradient(135deg, var(--portal-cyan-soft) 0 4px, transparent 4px 8px); cursor: pointer; }
.sc-row-seam:hover, .sc-row-seam:focus-visible { outline: 1px solid var(--portal-cyan); }
.sc-row-insert { position: absolute; top: 0; bottom: 0; z-index: 7; width: 2px; margin-left: -1px; background: var(--portal-cyan); pointer-events: none; }
</style>
