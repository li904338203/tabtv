<script setup lang="ts">
/**
 * 节拍标记（SCT-20，SC-F-27）：配乐的拍点画在刻度尺下沿，强拍更长。节拍来自预览素材的 `beats`（素材时间），
 * 由 TimelineView 换算成成片时间（片段起点 + (拍点 − 取用起点) × 100 / 变速）。
 *
 * ⚠️ 强拍的判断是近似的（`downbeatApprox`）：标题里说出来，别让用户以为它是乐谱级的准确。
 * 只画可见范围里的拍点（5 分钟、120 BPM 就是 600 根）。
 */
import { smartcutMsToPx } from "~/utils/smartcutTimeline";

const props = defineProps<{
  beats: ReadonlyArray<{ ms: number; strong: boolean }>;
  zoom: number;
  fromMs: number;
  toMs: number;
  approx: boolean;
}>();

const visible = computed(() => props.beats.filter(b => b.ms >= props.fromMs && b.ms <= props.toMs).slice(0, 2000));
</script>

<template>
  <div class="sc-beats" :title="approx ? '配乐节拍（强拍为近似判断）' : '配乐节拍'" aria-hidden="true" data-testid="smartcut-beats">
    <span
      v-for="beat in visible"
      :key="beat.ms"
      class="sc-beat"
      :class="{ strong: beat.strong }"
      :style="{ left: `${smartcutMsToPx(beat.ms, zoom)}px` }"
    />
  </div>
</template>

<style scoped>
.sc-beats { position: absolute; right: 0; bottom: 0; left: 0; height: 8px; pointer-events: none; }
.sc-beat { position: absolute; bottom: 0; width: 1px; height: 4px; background: var(--portal-gold); }
.sc-beat.strong { width: 2px; height: 8px; margin-left: -0.5px; }
</style>
