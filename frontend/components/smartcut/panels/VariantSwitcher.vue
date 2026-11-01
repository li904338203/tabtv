<script setup lang="ts">
/**
 * 多方案切换（SCT-31，SC-F-112）：A / B / C 标签，每套显示总时长、用到的段与摘要；失败的那一套置灰并显示原因。
 * 切换即请求 `/plan/{callId}/apply`（服务端按当前素材重新编译那一套、写入前存快照），这里只发 `select`。
 */
import type { components } from "@tabtv/api-client";
import { smartcutVariantLabel } from "~/utils/smartcutPlan";
import { smartcutFormatTime } from "~/utils/smartcutTimeline";

type Variant = components["schemas"]["SmartcutVariant"];

const props = defineProps<{ variants: Variant[]; appliedVariant: number | null; disabledReason?: string }>();
const emit = defineEmits<{ select: [index: number] }>();

const items = computed(() => props.variants.map(v => ({
  index: v.index,
  label: smartcutVariantLabel(v.index),
  failed: v.status === "failed",
  applied: v.index === props.appliedVariant,
  meta: v.status === "failed" ? "" : `${smartcutFormatTime(v.totalMs)} · ${v.usedClips.length} 段（${v.usedClips.join("、") || "无"}）`,
  summary: v.status === "failed" ? `这一套失败：${v.error?.trim() || "未知原因"}` : v.summary?.trim() || ""
})));
</script>

<template>
  <div class="sc-variants" role="radiogroup" aria-label="方案" data-testid="smartcut-variants">
    <button
      v-for="item in items"
      :key="item.index"
      type="button"
      role="radio"
      class="sc-variant"
      :class="{ applied: item.applied, failed: item.failed }"
      :aria-checked="item.applied"
      :disabled="item.failed || Boolean(disabledReason)"
      :title="item.failed ? item.summary : disabledReason || (item.applied ? '当前时间线用的是这一套' : '切到这一套（写入前存快照，可撤销）')"
      :data-variant="item.index"
      @click="emit('select', item.index)"
    >
      <strong>{{ item.label }}</strong>
      <span v-if="item.meta" class="meta">{{ item.meta }}</span>
      <span class="summary">{{ item.summary }}</span>
    </button>
  </div>
</template>

<style scoped>
.sc-variants { display: grid; grid-template-columns: repeat(auto-fit, minmax(120px, 1fr)); gap: 6px; }
.sc-variant {
  display: grid;
  align-content: start;
  gap: 2px;
  min-width: 0;
  padding: 6px 8px;
  border: 1px solid var(--portal-line);
  border-radius: 6px;
  color: var(--portal-text);
  background: transparent;
  font-size: 11px;
  text-align: left;
  cursor: pointer;
}
.sc-variant:hover:not(:disabled) { background: var(--portal-hover); }
.sc-variant:focus-visible { outline: 2px solid var(--portal-cyan); outline-offset: 1px; }
.sc-variant.applied { border-color: var(--portal-cyan-line); background: var(--portal-cyan-soft); }
.sc-variant.failed { opacity: 0.55; cursor: not-allowed; }
.sc-variant:disabled:not(.failed) { cursor: not-allowed; }
.sc-variant strong { font-size: 12px; }
.sc-variant .meta { color: var(--portal-text-soft); }
.sc-variant .summary { overflow: hidden; color: var(--portal-muted); text-overflow: ellipsis; white-space: nowrap; }
.sc-variant.failed .summary { color: var(--portal-danger); white-space: normal; }
</style>
