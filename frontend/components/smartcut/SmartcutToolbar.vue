<script setup lang="ts">
/**
 * 时间线上方的工具条（SCT-20）：撤销 / 重做、分割、删左 / 删右、删除、变速档位、吸附开关、缩放与适配宽度，
 * 以及播放头的帧级时码与总长。
 *
 * 只发动作，不改数据：所有改动由外壳经纯函数算出新时间线再 `store.commit`（一次操作一步撤销）。
 * 禁用原因来自 `smartcutToolDisabledReason`：只读排在「没有选中」之前（给最根本的那一条）。
 */
import { ArrowLeftToLine, ArrowRightToLine, AudioLines, Clapperboard, ListChecks, Magnet, Maximize2, Pause, Play, Redo2, Scissors, Trash2, Type, Undo2 } from "@lucide/vue";
import type { SmartcutTool } from "~/utils/smartcutStudio";
import { smartcutHistoryDisabledReason, smartcutToolDisabledReason } from "~/utils/smartcutStudio";
import { smartcutSpeedLevels } from "~/utils/smartcutContent";
import { smartcutFormatTimecode } from "~/utils/smartcutTimecode";
import { smartcutDescribeDuration, smartcutZoomLevels } from "~/utils/smartcutTimeline";

const props = defineProps<{
  zoom: number;
  snap: boolean;
  /** 这一下要作用的片段（选中的，或播放头下画面轨的那一段）的当前变速；没有为 null。 */
  targetSpeed: number | null;
  hasSelection: boolean;
  playheadInTarget: boolean;
  /** 屏幕太窄时只读查看（SC-UX-05）。 */
  viewOnly: boolean;
  /** 片段级生成的入口（SCT-40）：补镜在播放头处、生成音频、生成记录；不可用时的原因（空串 = 可用）。 */
  fillReason?: string;
  audioReason?: string;
  sidePanel?: string | null;
  /** 要调用模型的入口（补镜、生成音频、生成记录）出不出现；AI 关着时（这一版只保留手动剪辑）为 false。缺省出现。 */
  aiTools?: boolean;
}>();
const emit = defineEmits<{
  tool: [tool: SmartcutTool | "undo" | "redo"];
  speed: [speed: number];
  play: [];
  fit: [];
  "add-text": [];
  fill: [];
  "audio-gen": [];
  generations: [];
  "update:zoom": [zoom: number];
  "update:snap": [snap: boolean];
}>();
const store = useSmartcutStore();

function reason(tool: SmartcutTool): string {
  if (props.viewOnly && !store.readonlyReason) return "屏幕太窄，只能查看";
  // 变速只对片段有意义（选中的是字幕或文字时说「先选中一个片段」）；删除对字幕、文字也成立
  const hasSelection = tool === "speed" ? props.targetSpeed !== null : props.hasSelection;
  return smartcutToolDisabledReason(tool, { readonlyReason: store.readonlyReason, hasSelection, playheadInTarget: props.playheadInTarget });
}
const textReason = computed(() => {
  if (store.readonlyReason) return smartcutToolDisabledReason("split", { readonlyReason: store.readonlyReason, hasSelection: true, playheadInTarget: true });
  return props.viewOnly ? "屏幕太窄，只能查看" : "";
});
const undoReason = computed(() => smartcutHistoryDisabledReason({ readonlyReason: store.readonlyReason, available: store.undoStack.length > 0, kind: "undo" }));
const redoReason = computed(() => smartcutHistoryDisabledReason({ readonlyReason: store.readonlyReason, available: store.redoStack.length > 0, kind: "redo" }));
const fps = computed(() => store.timeline?.fps ?? 30);
const timecode = computed(() => smartcutFormatTimecode(store.playheadMs, fps.value));
const duration = computed(() => (store.timeline ? smartcutDescribeDuration(store.timeline.totalMs, store.timeline.effectiveMs) : ""));
const speedLabel = (s: number) => `${s / 100}×`;
</script>

<template>
  <div class="sc-toolbar" role="toolbar" aria-label="剪辑工具">
    <button type="button" class="sc-btn icon" :title="store.playing ? '暂停（空格）' : '播放（空格）'" :aria-label="store.playing ? '暂停' : '播放'" @click="emit('play')">
      <Pause v-if="store.playing" :size="15" /><Play v-else :size="15" />
    </button>
    <span class="sc-toolbar-time" aria-live="off" :title="`播放头 ${timecode}`" data-testid="smartcut-timecode">{{ timecode }}</span>
    <span class="sc-toolbar-duration" :title="`成片长度 ${duration}`">/ {{ duration }}</span>
    <span class="sc-toolbar-sep" />
    <button type="button" class="sc-btn icon" :disabled="Boolean(undoReason)" :title="undoReason || '撤销（⌘Z）'" :aria-label="undoReason || '撤销'" @click="emit('tool', 'undo')"><Undo2 :size="15" /></button>
    <button type="button" class="sc-btn icon" :disabled="Boolean(redoReason)" :title="redoReason || '重做（⇧⌘Z）'" :aria-label="redoReason || '重做'" @click="emit('tool', 'redo')"><Redo2 :size="15" /></button>
    <span class="sc-toolbar-sep" />
    <button type="button" class="sc-btn" :disabled="Boolean(reason('split'))" :title="reason('split') || '在播放头处分割（S）'" :aria-label="reason('split') || '分割'" data-testid="tool-split" @click="emit('tool', 'split')">
      <Scissors :size="14" />分割
    </button>
    <button type="button" class="sc-btn" :disabled="Boolean(reason('delete-left'))" :title="reason('delete-left') || '删除播放头左侧'" :aria-label="reason('delete-left') || '删左'" @click="emit('tool', 'delete-left')">
      <ArrowLeftToLine :size="14" />删左
    </button>
    <button type="button" class="sc-btn" :disabled="Boolean(reason('delete-right'))" :title="reason('delete-right') || '删除播放头右侧'" :aria-label="reason('delete-right') || '删右'" @click="emit('tool', 'delete-right')">
      <ArrowRightToLine :size="14" />删右
    </button>
    <button type="button" class="sc-btn" :disabled="Boolean(reason('delete'))" :title="reason('delete') || '删除选中的片段（Delete）'" :aria-label="reason('delete') || '删除'" @click="emit('tool', 'delete')">
      <Trash2 :size="14" />删除
    </button>
    <button type="button" class="sc-btn" :disabled="Boolean(textReason)" :title="textReason || '在播放头处加一条文字（2 秒）'" :aria-label="textReason || '加文字'" data-testid="tool-add-text" @click="emit('add-text')">
      <Type :size="14" />文字
    </button>
    <button v-if="aiTools !== false" type="button" class="sc-btn" :disabled="Boolean(fillReason)" :title="fillReason || '在播放头所在的那一段之后插一段新生成的视频'" :aria-label="fillReason || '补镜'" data-testid="tool-fill" @click="emit('fill')">
      <Clapperboard :size="14" />补镜
    </button>
    <button
      v-if="aiTools !== false"
      type="button"
      class="sc-btn"
      :class="{ active: sidePanel === 'audio' }"
      :aria-pressed="sidePanel === 'audio'"
      :title="audioReason || '生成配乐或音效（配音请在画布上用音频节点生成）'"
      data-testid="tool-audio-gen"
      @click="emit('audio-gen')"
    >
      <AudioLines :size="14" />生成音频
    </button>
    <button
      v-if="aiTools !== false"
      type="button"
      class="sc-btn"
      :class="{ active: sidePanel === 'generations' }"
      :aria-pressed="sidePanel === 'generations'"
      title="这条时间线里的片段级生成与识别"
      data-testid="tool-generations"
      @click="emit('generations')"
    >
      <ListChecks :size="14" />生成记录
    </button>
    <span class="sc-toolbar-sep" />
    <div class="sc-seg" role="radiogroup" aria-label="变速">
      <button
        v-for="s in smartcutSpeedLevels"
        :key="s"
        type="button"
        role="radio"
        class="sc-btn"
        :class="{ active: targetSpeed === s }"
        :aria-checked="targetSpeed === s"
        :disabled="Boolean(reason('speed'))"
        :title="reason('speed') || `变速 ${speedLabel(s)}`"
        :aria-label="reason('speed') || `变速 ${speedLabel(s)}`"
        @click="emit('speed', s)"
      >
        {{ speedLabel(s) }}
      </button>
    </div>
    <span class="sc-toolbar-sep" />
    <button
      type="button"
      class="sc-btn"
      :class="{ active: snap }"
      :aria-pressed="snap"
      :title="snap ? '吸附已开：拖动时贴住片段边缘、播放头与节拍' : '吸附已关'"
      aria-label="吸附"
      @click="emit('update:snap', !snap)"
    >
      <Magnet :size="14" />吸附
    </button>
    <label class="sc-toolbar-zoom" title="缩放（固定档位）">
      <span>缩放</span>
      <input
        type="range"
        :min="0"
        :max="smartcutZoomLevels.length - 1"
        step="1"
        :value="zoom"
        aria-label="缩放"
        :aria-valuetext="`${smartcutZoomLevels[zoom]} 像素每秒`"
        @input="emit('update:zoom', Number(($event.target as HTMLInputElement).value))"
      >
    </label>
    <button type="button" class="sc-btn icon" title="适配宽度：整条成片放进时间线" aria-label="适配宽度" @click="emit('fit')"><Maximize2 :size="14" /></button>
  </div>
</template>

<style scoped>
.sc-toolbar {
  display: flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
  padding: 6px 10px;
  border-top: 1px solid var(--portal-line);
  border-bottom: 1px solid var(--portal-line);
  overflow-x: auto;
  color: var(--portal-text);
  background: var(--portal-panel);
  font-size: 12px;
}
.sc-toolbar-time { font-variant-numeric: tabular-nums; font-weight: 600; }
.sc-toolbar-duration { color: var(--portal-muted); white-space: nowrap; }
.sc-toolbar-sep { width: 1px; height: 18px; margin: 0 4px; background: var(--portal-line); }
.sc-toolbar-zoom { display: inline-flex; align-items: center; gap: 6px; margin-left: auto; color: var(--portal-muted); }
.sc-toolbar-zoom input { width: 110px; }
.sc-seg .sc-btn { padding: 0 7px; }
</style>
