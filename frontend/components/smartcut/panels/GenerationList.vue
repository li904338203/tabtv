<script setup lang="ts">
/**
 * 生成记录（SCT-40，SC-F-139、SC-F-136）：这条时间线里所有片段级生成与识别（时间、类型、状态、花费），按时间倒序。
 * 产物没能自动放进时间线（位置已经不在了）的，可以拖进时间线，或者「放到播放头」；失败的可以重试（重新报价、新的一次确认）。
 */
import { ElMessage } from "element-plus";
import { SMARTCUT_GENERATION_DRAG, smartcutGenerationKindLabels, smartcutGenerationOutputKind, smartcutGenerationStatusText } from "~/utils/smartcutClipGen";
import { smartcutFormatTime } from "~/utils/smartcutTimeline";
import type { SmartcutStoreGeneration } from "~/stores/smartcut";

defineProps<{ editable: boolean; disabledReason: string }>();
const emit = defineEmits<{ close: []; retry: [generation: SmartcutStoreGeneration] }>();
const store = useSmartcutStore();

const loading = ref(false);
const failure = ref("");
const items = computed(() => Object.values(store.generations).sort((a, b) => b.createdAt.localeCompare(a.createdAt)));

async function load(more = false) {
  loading.value = true;
  failure.value = "";
  try {
    await store.refreshGenerations(more);
  } catch {
    failure.value = "生成记录取不到，稍后再试";
  } finally {
    loading.value = false;
  }
}
onMounted(() => { void load(); });

function placeable(g: SmartcutStoreGeneration): boolean {
  return g.status === "success" && !g.landed && Boolean(g.outputMediaId) && Boolean(smartcutGenerationOutputKind(g.kind));
}

function timeText(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? "" : d.toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit" });
}

function onDragStart(event: DragEvent, g: SmartcutStoreGeneration) {
  if (!event.dataTransfer) return;
  event.dataTransfer.setData(SMARTCUT_GENERATION_DRAG, g.generationId);
  event.dataTransfer.effectAllowed = "copy";
}

async function placeAtPlayhead(g: SmartcutStoreGeneration) {
  const reason = await store.placeGeneration(g.generationId, store.playheadMs);
  if (reason) ElMessage.warning(reason);
  else ElMessage.success(`已放到播放头 ${smartcutFormatTime(store.playheadMs)}`);
}
</script>

<template>
  <section class="sc-panel" aria-label="生成记录" data-testid="smartcut-generation-list">
    <div class="sc-panel-head">
      <h3>生成记录</h3>
      <button type="button" class="sc-btn" @click="emit('close')">关闭</button>
    </div>
    <p v-if="!items.length && !loading" class="sc-note">还没有片段级生成或识别。</p>
    <ul class="sc-gen-list">
      <li
        v-for="g in items"
        :key="g.generationId"
        :class="[`status-${g.status}`, { placeable: placeable(g) && editable }]"
        :draggable="placeable(g) && editable"
        :data-generation-id="g.generationId"
        @dragstart="onDragStart($event, g)"
      >
        <div class="sc-gen-row">
          <span class="sc-gen-kind">{{ smartcutGenerationKindLabels[g.kind] ?? g.kind }}</span>
          <span class="sc-gen-time">{{ timeText(g.createdAt) }}</span>
        </div>
        <p class="sc-note" :class="{ 'sc-error': g.status === 'failed' }">{{ smartcutGenerationStatusText(g) }}</p>
        <div v-if="placeable(g) || g.status === 'failed'" class="sc-seg">
          <button
            v-if="placeable(g)"
            type="button"
            class="sc-btn"
            :disabled="!editable"
            :title="editable ? '也可以直接拖到时间线上' : disabledReason"
            data-testid="smartcut-generation-place"
            @click="placeAtPlayhead(g)"
          >
            放到播放头
          </button>
          <button
            v-if="g.status === 'failed' && g.kind !== 'asr'"
            type="button"
            class="sc-btn"
            :disabled="!editable"
            :title="editable ? '重新报价、确认后再发起一次（新的一次扣费）' : disabledReason"
            data-testid="smartcut-generation-retry"
            @click="emit('retry', g)"
          >
            重试
          </button>
        </div>
      </li>
    </ul>
    <p v-if="failure" class="sc-error" role="alert">{{ failure }}</p>
    <button v-if="store.generationsCursor" type="button" class="sc-btn" :disabled="loading" @click="load(true)">{{ loading ? "载入中…" : "加载更多" }}</button>
  </section>
</template>

<style scoped>
.sc-panel-head { display: flex; align-items: center; justify-content: space-between; gap: 8px; }
.sc-panel-head h3 { margin: 0; font-size: 13px; }
.sc-gen-list { display: grid; gap: 8px; margin: 0; padding: 0; list-style: none; }
.sc-gen-list li { padding: 8px; border: 1px solid var(--portal-line); border-radius: 6px; background: var(--portal-surface-2); }
.sc-gen-list li.placeable { cursor: grab; border-style: dashed; }
.sc-gen-row { display: flex; align-items: center; gap: 8px; font-size: 12px; }
.sc-gen-kind { font-weight: 600; }
.sc-gen-time { color: var(--portal-muted); font-variant-numeric: tabular-nums; }
</style>
