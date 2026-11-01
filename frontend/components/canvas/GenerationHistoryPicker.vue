<script setup lang="ts">
/**
 * 「从生成历史选择」（画布加节点菜单 → 添加资源）：列出本工作区成功的图片 / 视频 / 音频产物，选中的建成「已有产出」节点。
 *
 * - 每个产物一项（一次出四张图就是四项，GAP-12）；可按类型筛、可只看本项目；游标分页「加载更多」；
 * - 缩略图都是受保护媒体：经 useAuthenticatedMedia 取成 blob（关窗 / 卸载时释放）。图片取 320 宽缩略图；
 *   视频取 `/media/{id}/info` 的封面帧（不下载整段视频），还没截出来时显示图标；音频显示图标；
 * - 多选，确认后一次建成「已有产出」节点（useCanvasResourceNodes，一次撤销撤掉整批），选中与对焦由画布做。
 */
import { AudioLines, Check, LoaderCircle, Video, X } from "@lucide/vue";
import type { components } from "@tabtv/api-client";
import { historyNodeTitle, historyOutputs, type HistoryOutput, type ResourceMediaType } from "~/utils/canvasResources";

type Generation = components["schemas"]["Generation"];

const props = defineProps<{ modelValue: boolean; projectId: string }>();
const emit = defineEmits<{ "update:modelValue": [open: boolean]; created: [ids: string[]] }>();
const { addResourceNodes } = useCanvasResourceNodes();

const TABS: Array<{ value: ResourceMediaType | "all"; label: string }> = [
  { value: "all", label: "全部" }, { value: "image", label: "图片" }, { value: "video", label: "视频" }, { value: "audio", label: "音频" }
];
const tab = ref<ResourceMediaType | "all">("all");
const onlyProject = ref(false);
const generations = ref<Generation[]>([]);
const cursor = ref<string | null>(null);
const hasMore = ref(false);
const loading = ref(false);
const error = ref("");
const selected = ref<string[]>([]);
const thumbs = useAuthenticatedMedia();
let seq = 0;

const items = computed(() => historyOutputs(generations.value, tab.value));

async function load(reset: boolean) {
  if (loading.value && !reset) return;
  const my = ++seq;
  loading.value = true;
  error.value = "";
  try {
    const query: Record<string, string> = { limit: "30" };
    if (!reset && cursor.value) query.cursor = cursor.value;
    if (onlyProject.value && props.projectId) query.projectId = props.projectId;
    const res = await useApi()<{ items: Generation[]; nextCursor?: string | null; hasMore?: boolean }>("/generations", { query });
    if (my !== seq) return;
    generations.value = reset ? res.items ?? [] : [...generations.value, ...(res.items ?? [])];
    cursor.value = res.nextCursor ?? null;
    hasMore.value = Boolean(res.hasMore && res.nextCursor);
  } catch (cause) {
    if (my === seq) error.value = cause instanceof Error ? cause.message : "生成历史取不到，请稍后再试";
  } finally {
    if (my === seq) loading.value = false;
  }
}

/** 缩略图：图片直接取缩略图；视频先问封面帧。已经取过的不再取。 */
const posterRequested = new Set<string>();
watch(items, (list) => {
  const images = list.filter(i => i.type === "image" && !thumbs.urls.value[i.mediaId]).map(i => ({ key: i.mediaId, url: `/api/v1/media/${i.mediaId}?w=320` }));
  if (images.length) void thumbs.appendAll(images);
  for (const item of list.filter(i => i.type === "video" && !posterRequested.has(i.mediaId))) {
    posterRequested.add(item.mediaId);
    void useApi()<{ posterMediaId?: string }>(`/media/${item.mediaId}/info`).then((info) => {
      if (info.posterMediaId) void thumbs.appendAll([{ key: item.mediaId, url: `/api/v1/media/${info.posterMediaId}?w=320` }]);
    }).catch(() => undefined);
  }
});

function toggle(item: HistoryOutput) {
  selected.value = selected.value.includes(item.key) ? selected.value.filter(k => k !== item.key) : [...selected.value, item.key];
}
async function confirm() {
  const byKey = new Map(items.value.map(i => [i.key, i]));
  const picked = selected.value.map(k => byKey.get(k)).filter((i): i is HistoryOutput => Boolean(i));
  if (!picked.length) return;
  const ids = await addResourceNodes(picked.map(item => ({ type: item.type, mediaId: item.mediaId, title: historyNodeTitle(item) })));
  emit("created", ids);
  close();
}
function close() {
  emit("update:modelValue", false);
}
function formatTime(iso: string) {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? "" : `${d.getMonth() + 1}-${d.getDate()} ${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
}

watch(() => props.modelValue, (open) => {
  if (open) {
    selected.value = [];
    void load(true);
  } else {
    thumbs.release();
    posterRequested.clear();
    generations.value = [];
  }
}, { immediate: true });
watch(onlyProject, () => { selected.value = []; void load(true); });
watch(tab, () => { selected.value = []; });
function onKey(event: KeyboardEvent) {
  if (props.modelValue && event.key === "Escape") close();
}
onMounted(() => window.addEventListener("keydown", onKey));
onBeforeUnmount(() => {
  window.removeEventListener("keydown", onKey);
  thumbs.release();
});
</script>

<template>
  <Teleport to="body">
    <div v-if="modelValue" class="history-picker-overlay" data-testid="history-picker" @mousedown.self="close">
      <section class="history-picker" role="dialog" aria-modal="true" aria-label="从生成历史选择">
        <header class="history-picker-head">
          <strong>从生成历史选择</strong>
          <nav class="history-picker-tabs" aria-label="类型">
            <button v-for="t in TABS" :key="t.value" type="button" :class="{ active: tab === t.value }" @click="tab = t.value">{{ t.label }}</button>
          </nav>
          <label class="history-picker-scope"><input v-model="onlyProject" type="checkbox">只看本项目</label>
          <button type="button" class="history-picker-close" aria-label="关闭" @click="close"><X :size="17" /></button>
        </header>
        <div class="history-picker-body">
          <p v-if="error" class="history-picker-state" role="alert">{{ error }} <button type="button" @click="load(true)">重试</button></p>
          <p v-else-if="!loading && !items.length" class="history-picker-state">
            {{ hasMore ? "这一页里没有这类产物，加载更多看看" : (onlyProject ? "本项目还没有生成过图片、视频或音频" : "还没有生成过图片、视频或音频") }}
          </p>
          <ul class="history-picker-grid">
            <li v-for="item in items" :key="item.key">
              <button
                type="button"
                :class="{ active: selected.includes(item.key) }"
                :aria-pressed="selected.includes(item.key)"
                :title="[item.modelCode, item.operation].filter(Boolean).join(' · ')"
                data-testid="history-item"
                @click="toggle(item)"
              >
                <img v-if="thumbs.urls.value[item.mediaId]" :src="thumbs.urls.value[item.mediaId]" alt="" :draggable="false">
                <span v-else class="history-picker-icon"><Video v-if="item.type === 'video'" :size="26" /><AudioLines v-else-if="item.type === 'audio'" :size="26" /><LoaderCircle v-else :size="20" class="spin" /></span>
                <small class="history-picker-kind">{{ item.type === "image" ? "图片" : item.type === "video" ? "视频" : "音频" }}</small>
                <small class="history-picker-time">{{ formatTime(item.createdAt) }}</small>
                <i v-if="selected.includes(item.key)" class="history-picker-check"><Check :size="13" /></i>
              </button>
            </li>
          </ul>
          <button v-if="hasMore" type="button" class="history-picker-more" :disabled="loading" @click="load(false)">
            <LoaderCircle v-if="loading" :size="14" class="spin" />加载更多
          </button>
          <p v-else-if="loading" class="history-picker-state"><LoaderCircle :size="14" class="spin" /> 加载中</p>
        </div>
        <footer class="history-picker-foot">
          <span>已选 {{ selected.length }} 项</span>
          <button type="button" class="ghost" @click="close">取消</button>
          <button type="button" class="primary" :disabled="!selected.length" data-testid="history-confirm" @click="confirm">添加到画布</button>
        </footer>
      </section>
    </div>
  </Teleport>
</template>

<style scoped>
.history-picker-overlay { position: fixed; inset: 0; z-index: 130; display: grid; place-items: center; padding: 24px; background: rgba(15, 16, 18, .42); }
.history-picker {
  width: min(860px, 100%); height: min(640px, calc(100vh - 48px)); display: grid; grid-template-rows: auto minmax(0, 1fr) auto; overflow: hidden;
  border: 1px solid var(--canvas-line); border-radius: 14px; color: var(--canvas-text); background: var(--cv-n15); box-shadow: 0 28px 70px var(--cv-shade-42);
}
.history-picker-head { display: flex; align-items: center; gap: 18px; padding: 14px 16px 14px 18px; border-bottom: 1px solid var(--canvas-line); }
.history-picker-head strong { font-size: 14px; font-weight: 600; }
.history-picker-tabs { display: flex; gap: 4px; }
.history-picker-tabs button { height: 30px; border: 0; border-radius: 8px; padding: 0 12px; color: var(--cv-n70); background: transparent; font-size: 13px; cursor: pointer; }
.history-picker-tabs button:hover { color: var(--cv-n92); }
.history-picker-tabs button.active { color: var(--cv-n94); background: var(--cv-n22); font-weight: 560; }
.history-picker-scope { display: inline-flex; align-items: center; gap: 6px; margin-left: auto; color: var(--cv-n70); font-size: 12px; cursor: pointer; }
.history-picker-close { width: 32px; height: 32px; display: grid; place-items: center; border: 0; border-radius: 8px; color: var(--cv-n70); background: transparent; cursor: pointer; }
.history-picker-close:hover { color: var(--cv-n94); background: var(--cv-n22); }
.history-picker-body { overflow-y: auto; padding: 14px 18px; }
.history-picker-state { display: flex; align-items: center; justify-content: center; gap: 6px; margin: 40px 0; color: var(--canvas-muted); font-size: 13px; }
.history-picker-state button { border: 1px solid var(--canvas-line); border-radius: 6px; padding: 2px 10px; color: var(--cv-n80); background: transparent; cursor: pointer; }
.history-picker-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(140px, 1fr)); gap: 10px; margin: 0; padding: 0; list-style: none; }
.history-picker-grid button {
  position: relative; width: 100%; aspect-ratio: 1; display: grid; place-items: center; overflow: hidden; padding: 0;
  border: 2px solid transparent; border-radius: 10px; color: var(--cv-n70); background: var(--cv-n22); cursor: pointer;
}
.history-picker-grid button:hover { border-color: var(--cv-tint-22, var(--canvas-line)); }
.history-picker-grid button.active { border-color: var(--canvas-cyan); }
.history-picker-grid img { width: 100%; height: 100%; object-fit: cover; }
.history-picker-kind, .history-picker-time {
  position: absolute; bottom: 6px; padding: 1px 6px; border-radius: 999px; color: var(--portal-on-media, #fff); background: var(--portal-on-media-bg, rgba(0, 0, 0, .55)); font-size: 10px;
}
.history-picker-kind { left: 6px; }
.history-picker-time { right: 6px; }
.history-picker-check { position: absolute; top: 6px; right: 6px; display: grid; place-items: center; width: 20px; height: 20px; border-radius: 50%; color: #fff; background: var(--canvas-cyan); }
.history-picker-more { display: flex; align-items: center; gap: 6px; margin: 16px auto 4px; border: 1px solid var(--canvas-line); border-radius: 8px; padding: 6px 16px; color: var(--cv-n80); background: transparent; font-size: 12px; cursor: pointer; }
.history-picker-foot { display: flex; align-items: center; gap: 10px; padding: 12px 18px; border-top: 1px solid var(--canvas-line); font-size: 12px; color: var(--canvas-muted); }
.history-picker-foot span { margin-right: auto; }
.history-picker-foot button { height: 32px; border-radius: 8px; padding: 0 16px; font-size: 13px; cursor: pointer; }
.history-picker-foot .ghost { border: 1px solid var(--canvas-line); color: var(--cv-n80); background: transparent; }
.history-picker-foot .primary { border: 0; color: var(--cv-n08, #111); background: var(--cv-n94, #eee); }
.history-picker-foot .primary:disabled { opacity: .45; cursor: not-allowed; }
</style>
