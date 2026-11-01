<script setup lang="ts">
/**
 * `@` 引用候选（设计 03 §2.3、设计 02 §4.1）：
 * 第一组**本画布的节点**（按最近创建，缩略图 + 标题；只在画布上下文出现），然后**素材库**、**角色资产**。
 *
 * - 键盘：↑↓ 选择、回车 / Tab 确认、Esc 关闭（由输入框转发：`move` / `confirm`）；
 * - ⚠️ 选中的引用只带 `{kind,id}` 出去（`label` / 缩略图只是展示），归属由服务端按 ID 校验（AG-03）；
 * - `only`（营销槽位，MKT-T-12）：`character` 只列**角色资产**一组、`video` 只列**视频** ——
 *   槽位只收这一类，列出别的选了也放不进去。不传时三组照旧。
 *   `node` 只列调用方传进来的**本画布节点**、不取素材与资产（3D 导演台的 `@` 参考，DRT-20：那里只收有产出的图片节点）。
 *   `video` 分两组：上传的视频（`/media?kind=video`）与**本工作区生成过的视频**（`/creations?resultType=video`，
 *   MKT-T-29：`/media` 只列参考上传，不含生成产物）。生成的视频以产物的媒体 ID（`kind=media`）引用，
 *   归属仍由服务端按工作区校验（MKT-T-28 R-8 §1）；一次出了几个视频的只给第一个（`cover`）并在副标题里说明。
 * - ⚠️ 缩略图是受保护媒体：一律 `useAuthenticatedMedia`（fetch → blob → objectURL，卸载时释放），
 *   请求缩略图档（`?w=160`），不直接 `<img src>` 受保护端点。
 */
import type { components } from "@tabtv/api-client";
import { Box, FileText, Image as ImageIcon, Music, UserRound, Video } from "@lucide/vue";
import { mediaThumbUrl } from "~/utils/authorizedMedia";
import { moveIndex } from "~/utils/composerKeys";
import { marketingCreationProjectLabel } from "~/utils/marketingLibrary";
import type { ComposerRef, NodeRefSource } from "~/utils/composerRefs";

type ScriptAsset = components["schemas"]["ScriptAsset"];
type MediaItem = { id: string; kind: string; mime?: string; createdAt?: string };
type Creation = components["schemas"]["Creation"];
type Candidate = Omit<ComposerRef, "auto"> & { group: "node" | "media" | "creation" | "asset"; key: string; note?: string };

const props = withDefaults(defineProps<{
  query?: string;
  /** 本画布节点候选（已按最近创建排好、按关键字过滤）；创造页不传。 */
  nodes?: NodeRefSource[];
  /** 只列一类：character = 角色资产、video = 本工作区上传或生成过的视频（营销槽位）；node = 只列 `nodes`（导演台）。 */
  only?: "character" | "video" | "node";
}>(), { query: "", nodes: () => [], only: undefined });
const emit = defineEmits<{ select: [ref: Omit<ComposerRef, "auto">]; close: [] }>();

const THUMB_WIDTH = 160;
const media = ref<MediaItem[]>([]);
const assets = ref<ScriptAsset[]>([]);
const creations = ref<Creation[]>([]);
const loadingMedia = ref(false);
const loadingCreations = ref(false);
const loadingAssets = ref(false);
const activeIndex = ref(0);
const root = ref<HTMLElement>();
const thumbs = useAuthenticatedMedia();
let assetTimer: ReturnType<typeof setTimeout> | undefined;
let assetSeq = 0;

const KIND_LABELS: Record<string, string> = { image: "图片", video: "视频", audio: "音频" };
const ASSET_LABELS: Record<string, string> = { character: "角色", scene: "场景", prop: "道具" };

function dateLabel(value?: string): string {
  if (!value) return "";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "" : `${date.getMonth() + 1}/${date.getDate()} ${String(date.getHours()).padStart(2, "0")}:${String(date.getMinutes()).padStart(2, "0")}`;
}

const candidates = computed<Candidate[]>(() => {
  const keyword = props.query.trim().toLowerCase();
  const out: Candidate[] = (props.only && props.only !== "node" ? [] : props.nodes).map(node => ({
    group: "node", key: `node:${node.id}`, kind: "node", id: node.id, label: node.label, thumbMediaId: node.thumbMediaId, hint: node.hint
  }));
  // 素材没有名字（只有类型与上传时间）：有关键字时不列，免得一屏与关键字无关的东西。
  if (props.only !== "character" && props.only !== "node" && (!keyword || props.only === "video")) {
    for (const item of media.value) {
      if (props.only === "video" && item.kind !== "video") continue;
      out.push({
        group: "media", key: `media:${item.id}`, kind: "media", id: item.id,
        label: `${KIND_LABELS[item.kind] ?? "素材"} ${dateLabel(item.createdAt)}`.trim(),
        thumbMediaId: item.kind === "image" ? item.id : undefined, hint: item.kind
      });
    }
  }
  for (const item of props.only === "video" ? creations.value : []) {
    const mediaId = item.cover?.type === "video" ? item.cover.mediaId : undefined;
    if (!mediaId) continue;
    out.push({
      group: "creation", key: `creation:${item.id}`, kind: "media", id: String(mediaId),
      label: `${marketingCreationProjectLabel(item, "项目已删除")} ${dateLabel(item.createdAt)}`.trim(), hint: "video",
      note: item.outputCount > 1 ? `取第 1 个（共 ${item.outputCount}）` : "生成"
    });
  }
  for (const asset of props.only === "video" || props.only === "node" ? [] : assets.value) {
    if (props.only === "character" && asset.type !== "character") continue;
    out.push({
      group: "asset", key: `asset:${asset.id}`, kind: "asset", id: String(asset.id), label: asset.name,
      thumbMediaId: asset.mediaId ? String(asset.mediaId) : undefined, hint: asset.type
    });
  }
  return out;
});

const groups = computed(() => [
  { code: "node" as const, title: "本画布节点", items: candidates.value.filter(item => item.group === "node") },
  { code: "media" as const, title: props.only === "video" ? "上传的视频" : "素材库", items: candidates.value.filter(item => item.group === "media") },
  { code: "creation" as const, title: "生成的视频", items: candidates.value.filter(item => item.group === "creation") },
  { code: "asset" as const, title: "角色资产", items: candidates.value.filter(item => item.group === "asset") }
].filter(group => group.items.length || (group.code === "node" && props.nodes.length)));

watch(candidates, list => {
  if (activeIndex.value >= list.length) activeIndex.value = list.length ? 0 : -1;
  void thumbs.appendAll(list
    .filter(item => item.thumbMediaId)
    .map(item => ({ key: item.thumbMediaId!, url: mediaThumbUrl(item.thumbMediaId, THUMB_WIDTH) })));
});

async function loadMedia() {
  if (props.only === "character" || props.only === "node") return;
  loadingMedia.value = true;
  try {
    const page = await useApi()<{ items?: MediaItem[] }>("/media", { query: { limit: 30, ...(props.only === "video" ? { kind: "video" } : {}) } });
    media.value = (page.items ?? []).map(item => ({ ...item, id: String(item.id) }));
  } catch {
    media.value = [];
  } finally {
    loadingMedia.value = false;
  }
}

/** 本工作区生成过的视频（只在 `only="video"` 时取）。取不到不影响上传的那一组。 */
async function loadCreations() {
  if (props.only !== "video") return;
  loadingCreations.value = true;
  try {
    const page = await useApi()<{ items?: Creation[] }>("/creations", { query: { limit: 30, resultType: "video" } });
    creations.value = page.items ?? [];
  } catch {
    creations.value = [];
  } finally {
    loadingCreations.value = false;
  }
}

async function loadAssets() {
  if (props.only === "video" || props.only === "node") return;
  const seq = ++assetSeq;
  loadingAssets.value = true;
  try {
    const keyword = props.query.trim();
    // only="character" 时让服务端按类型筛（B-20）：不带 type 的话场景 / 道具多时角色会被 30 条挤出去。
    const list = await useApi()<ScriptAsset[]>("/script-assets", { query: { limit: 30, ...(props.only === "character" ? { type: "character" } : {}), ...(keyword ? { keyword } : {}) } });
    if (seq === assetSeq) assets.value = list ?? [];
  } catch {
    if (seq === assetSeq) assets.value = [];
  } finally {
    if (seq === assetSeq) loadingAssets.value = false;
  }
}

watch(() => props.query, () => {
  clearTimeout(assetTimer);
  assetTimer = setTimeout(loadAssets, 250);
});

function iconOf(item: Candidate) {
  if (item.group === "asset") return UserRound;
  if (item.hint === "video") return Video;
  if (item.hint === "audio") return Music;
  if (item.hint === "text" || item.hint === "document") return FileText;
  if (item.group === "media" || item.hint === "image") return ImageIcon;
  return Box;
}

function subtitle(item: Candidate): string {
  if (item.group === "asset") return ASSET_LABELS[item.hint ?? ""] ?? "资产";
  if (item.group === "node") return "画布节点";
  if (item.note) return item.note;
  return props.only === "video" ? "上传" : "素材";
}

function choose(item: Candidate | undefined) {
  if (!item) {
    emit("close");
    return;
  }
  emit("select", { kind: item.kind, id: item.id, label: item.label, thumbMediaId: item.thumbMediaId, hint: item.hint });
}

function move(delta: number) {
  activeIndex.value = moveIndex(activeIndex.value, delta, candidates.value.length);
  nextTick(() => root.value?.querySelector(".mention-item.active")?.scrollIntoView({ block: "nearest" }));
}

function confirm() {
  choose(candidates.value[activeIndex.value]);
}

function indexOf(item: Candidate) {
  return candidates.value.findIndex(entry => entry.key === item.key);
}

function onOutside(event: PointerEvent) {
  if (root.value && event.target instanceof Node && !root.value.contains(event.target)) emit("close");
}

onMounted(() => {
  void loadMedia();
  void loadCreations();
  void loadAssets();
  document.addEventListener("pointerdown", onOutside, true);
});
onBeforeUnmount(() => {
  clearTimeout(assetTimer);
  document.removeEventListener("pointerdown", onOutside, true);
});

defineExpose({ move, confirm });
</script>

<template>
  <div ref="root" class="mention-picker" role="listbox" aria-label="引用候选">
    <template v-for="group in groups" :key="group.code">
      <p class="mention-group">{{ group.title }}</p>
      <p v-if="!group.items.length" class="mention-empty">没有匹配的节点</p>
      <button
        v-for="item in group.items"
        :key="item.key"
        type="button"
        class="mention-item"
        :class="{ active: indexOf(item) === activeIndex }"
        role="option"
        :aria-selected="indexOf(item) === activeIndex"
        @mouseenter="activeIndex = indexOf(item)"
        @mousedown.prevent
        @click="choose(item)"
      >
        <span class="mention-thumb">
          <img v-if="item.thumbMediaId && thumbs.urls.value[item.thumbMediaId]" :src="thumbs.urls.value[item.thumbMediaId]" :alt="item.label">
          <component :is="iconOf(item)" v-else :size="15" />
        </span>
        <span class="mention-label">{{ item.label }}</span>
        <small>{{ subtitle(item) }}</small>
      </button>
    </template>
    <p v-if="!candidates.length" class="mention-empty">
      {{ loadingMedia || loadingCreations || loadingAssets ? '加载中…' : (query ? '没有匹配的引用' : '还没有可引用的内容') }}
    </p>
  </div>
</template>

<style scoped>
.mention-picker {
  position: absolute;
  left: 0;
  right: 0;
  bottom: calc(100% + 8px);
  z-index: 90;
  /* 由输入框按可用空间给出（见 utils/pickerPlacement），再不超过 280px */
  max-height: min(280px, var(--picker-max, 280px));
  overflow-y: auto;
  padding: 6px;
  border: 1px solid var(--portal-line-strong);
  border-radius: 10px;
  color: var(--portal-text);
  background: var(--portal-elevated);
  box-shadow: 0 18px 48px var(--portal-shadow-strong);
  font-size: 12px;
}
/* 上方放不下时向下弹 */
.mention-picker.picker-bottom { bottom: auto; top: calc(100% + 8px); }
.mention-group { margin: 6px 6px 4px; color: var(--portal-muted); font-size: 11px; }
.mention-empty { margin: 4px 8px 8px; color: var(--portal-muted); }
.mention-item {
  width: 100%;
  display: flex;
  align-items: center;
  gap: 8px;
  border: 0;
  border-radius: 6px;
  padding: 5px 8px;
  color: var(--portal-text-soft);
  background: transparent;
  text-align: left;
  font: inherit;
  cursor: pointer;
}
.mention-item.active { color: var(--portal-text); background: var(--portal-hover); }
.mention-thumb {
  width: 28px;
  height: 28px;
  flex: none;
  display: grid;
  place-items: center;
  border-radius: 5px;
  overflow: hidden;
  background: var(--portal-surface-2);
}
.mention-thumb img { width: 100%; height: 100%; object-fit: cover; }
.mention-label { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.mention-item small { color: var(--portal-faint); }
</style>
