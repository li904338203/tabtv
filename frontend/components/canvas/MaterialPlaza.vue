<script setup lang="ts">
/**
 * 素材广场：风格 / 特效 / 运镜共用一个弹窗（版式照对标产品的「运镜广场」：页签 · 搜索 · 四列卡片 · 悬停收藏）。
 *
 * - 「广场」列全部启用的条目，「我的收藏」只列收藏过的；搜索按名称（停顿 300ms 再查，不是每敲一个字查一次）。
 * - 卡片：有封面显示封面（受保护媒体，按 320px 缩略图取）；运镜没有封面时是示意动画（CameraMovePreview，悬停才动）。
 * - 点卡片就选中并关闭；右上角星标收藏 / 取消收藏，已收藏的星标常亮。
 *
 * ⚠️ 对接的是新后端：列表是裸数组、按 code 收藏（utils/materialPlaza 的注释）。此前的素材库是照老后端写的，
 *    对着新后端列表恒空、选了也不生效。
 * ⚠️ 提示词不在这里：拼接在服务端（列表本来就不下发 prompt）。
 */
import type { components } from "@tabtv/api-client";
import { Image as ImageIcon, LoaderCircle, Search, Sparkles, Star, X } from "@lucide/vue";
import { ElMessage } from "element-plus";
import CameraMovePreview from "./CameraMovePreview.vue";
import {
  MATERIAL_PLAZA_TEXT,
  materialPlazaCoverUrl,
  materialPlazaEmptyText,
  materialPlazaQuery,
  type MaterialPlazaType
} from "~/utils/materialPlaza";

type MaterialItem = components["schemas"]["MaterialItem"];

const props = defineProps<{ modelValue: boolean; type: MaterialPlazaType; selectedCode?: string }>();
const emit = defineEmits<{ "update:modelValue": [value: boolean]; select: [item: MaterialItem] }>();

const tab = ref<"all" | "favorites">("all");
const keyword = ref("");
const appliedKeyword = ref("");
const items = ref<MaterialItem[]>([]);
const loading = ref(false);
const loadError = ref("");
const favoriteBusy = ref("");
const hovered = ref("");
const covers = useAuthenticatedMedia();
const text = computed(() => MATERIAL_PLAZA_TEXT[props.type]);
const emptyText = computed(() => materialPlazaEmptyText(props.type, { keyword: appliedKeyword.value, favorites: tab.value === "favorites" }));

let seq = 0;
async function load() {
  const mine = ++seq;
  loading.value = true;
  loadError.value = "";
  try {
    const list = await useApi()<MaterialItem[]>(`/canvas/${text.value.path}`, {
      query: materialPlazaQuery(appliedKeyword.value, tab.value === "favorites")
    });
    if (mine !== seq) return;
    items.value = Array.isArray(list) ? list : [];
    const wanted = items.value
      .map(item => ({ key: item.code, url: materialPlazaCoverUrl(item.coverUrl) }))
      .filter(item => item.url);
    if (wanted.length) void covers.appendAll(wanted);
  } catch (cause) {
    if (mine !== seq) return;
    items.value = [];
    loadError.value = !navigator.onLine ? "网络已断开，恢复后点重试" : cause instanceof Error ? cause.message : "加载失败";
  } finally {
    if (mine === seq) loading.value = false;
  }
}

let searchTimer: ReturnType<typeof setTimeout> | undefined;
watch(keyword, (value) => {
  clearTimeout(searchTimer);
  searchTimer = setTimeout(() => {
    if (value.trim() === appliedKeyword.value) return;
    appliedKeyword.value = value.trim();
    void load();
  }, 300);
});
watch(tab, () => void load());
watch(() => [props.modelValue, props.type] as const, ([open]) => {
  if (!open) return;
  tab.value = "all";
  keyword.value = "";
  appliedKeyword.value = "";
  void load();
}, { immediate: true });
onBeforeUnmount(() => clearTimeout(searchTimer));

function close() {
  emit("update:modelValue", false);
}

function choose(item: MaterialItem) {
  emit("select", item);
}

async function toggleFavorite(item: MaterialItem) {
  if (favoriteBusy.value) return;
  favoriteBusy.value = item.code;
  try {
    const result = await useApi()<{ isFavorited: boolean }>(
      `/canvas/materials/${props.type}/${encodeURIComponent(item.code)}/favorite`,
      { method: "POST", body: { favorite: !item.isFavorited } }
    );
    item.isFavorited = Boolean(result?.isFavorited);
    if (tab.value === "favorites" && !item.isFavorited) items.value = items.value.filter(entry => entry.code !== item.code);
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "收藏没有成功");
  } finally {
    favoriteBusy.value = "";
  }
}

function onKeydown(event: KeyboardEvent) {
  if (event.key === "Escape" && props.modelValue) close();
}
onMounted(() => document.addEventListener("keydown", onKeydown));
onBeforeUnmount(() => document.removeEventListener("keydown", onKeydown));
</script>

<template>
  <Teleport to="body">
    <div v-if="modelValue" class="material-plaza-overlay" @mousedown.self="close">
      <section class="material-plaza" role="dialog" aria-modal="true" :aria-label="text.title">
        <header class="material-plaza-head">
          <nav class="material-plaza-tabs" :aria-label="text.title">
            <button type="button" :class="{ active: tab === 'all' }" @click="tab = 'all'">{{ text.title }}</button>
            <button type="button" :class="{ active: tab === 'favorites' }" @click="tab = 'favorites'">我的收藏</button>
          </nav>
          <label class="material-plaza-search">
            <input v-model="keyword" type="search" :placeholder="text.search" :aria-label="text.search">
            <Search :size="16" />
          </label>
          <button class="material-plaza-close" type="button" title="关闭" aria-label="关闭" @click="close"><X :size="20" /></button>
        </header>

        <div class="material-plaza-body">
          <div v-if="loading && !items.length" class="material-plaza-state"><LoaderCircle :size="22" class="spin" /><span>正在加载</span></div>
          <div v-else-if="loadError" class="material-plaza-state">
            <span>{{ loadError }}</span>
            <button type="button" @click="load">重试</button>
          </div>
          <div v-else-if="!items.length" class="material-plaza-state"><span>{{ emptyText }}</span></div>
          <div v-else class="material-plaza-grid">
            <article
              v-for="item in items"
              :key="item.code"
              class="material-plaza-card"
              :class="{ selected: item.code === selectedCode, favorited: item.isFavorited }"
              @mouseenter="hovered = item.code"
              @mouseleave="hovered = ''"
            >
              <button class="material-plaza-cover" type="button" :title="`选择「${item.name}」`" @click="choose(item)">
                <img v-if="covers.urls.value[item.code]" :src="covers.urls.value[item.code]" :alt="item.name">
                <CameraMovePreview v-else-if="type === 'camera'" :code="item.code" :playing="hovered === item.code" />
                <span v-else class="material-plaza-placeholder"><component :is="type === 'style' ? ImageIcon : Sparkles" :size="26" /></span>
              </button>
              <button
                class="material-plaza-star"
                type="button"
                :title="item.isFavorited ? '取消收藏' : '收藏'"
                :aria-label="item.isFavorited ? `取消收藏「${item.name}」` : `收藏「${item.name}」`"
                :aria-pressed="item.isFavorited"
                :disabled="favoriteBusy === item.code"
                @click.stop="toggleFavorite(item)"
              ><Star :size="15" :fill="item.isFavorited ? 'currentColor' : 'none'" /></button>
              <strong>{{ item.name }}</strong>
            </article>
          </div>
        </div>
      </section>
    </div>
  </Teleport>
</template>

<style scoped>
.material-plaza-overlay {
  position: fixed;
  inset: 0;
  z-index: 130;
  display: grid;
  place-items: center;
  padding: 24px;
  background: rgba(15, 16, 18, .42);
}
.material-plaza {
  width: min(860px, 100%);
  height: min(640px, calc(100vh - 48px));
  display: grid;
  grid-template-rows: auto minmax(0, 1fr);
  overflow: hidden;
  border: 1px solid var(--canvas-line);
  border-radius: 14px;
  color: var(--canvas-text);
  background: var(--cv-n15);
  box-shadow: 0 28px 70px var(--cv-shade-42);
}
.material-plaza-head {
  display: grid;
  grid-template-columns: auto minmax(0, 340px) 1fr;
  align-items: center;
  gap: 24px;
  padding: 14px 16px 14px 18px;
  border-bottom: 1px solid var(--canvas-line);
}
.material-plaza-tabs { display: flex; gap: 6px; }
.material-plaza-tabs button {
  height: 34px;
  border: 0;
  border-radius: 8px;
  padding: 0 13px;
  color: var(--cv-n70);
  background: transparent;
  font-size: 13px;
  cursor: pointer;
}
.material-plaza-tabs button:hover { color: var(--cv-n92); }
.material-plaza-tabs button.active { color: var(--cv-n94); background: var(--cv-n22); font-weight: 560; }
.material-plaza-search {
  height: 34px;
  display: flex;
  align-items: center;
  gap: 8px;
  border-radius: 8px;
  padding: 0 12px;
  color: var(--canvas-muted);
  background: var(--cv-n22);
}
.material-plaza-search input {
  min-width: 0;
  flex: 1;
  border: 0;
  outline: 0;
  color: var(--canvas-text);
  background: transparent;
  font-size: 13px;
}
.material-plaza-search input::-webkit-search-cancel-button { display: none; }
.material-plaza-close {
  width: 34px;
  height: 34px;
  justify-self: end;
  display: grid;
  place-items: center;
  border: 0;
  border-radius: 8px;
  color: var(--cv-n70);
  background: transparent;
  cursor: pointer;
}
.material-plaza-close:hover { color: var(--cv-n94); background: var(--cv-n22); }
.material-plaza-body { min-height: 0; overflow-y: auto; padding: 16px 18px 20px; }
.material-plaza-grid { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 14px 10px; }
.material-plaza-card { min-width: 0; position: relative; display: grid; gap: 7px; }
.material-plaza-cover {
  width: 100%;
  aspect-ratio: 1;
  display: block;
  overflow: hidden;
  border: 0;
  border-radius: 6px;
  padding: 0;
  background: var(--portal-media-letterbox);
  cursor: pointer;
}
.material-plaza-cover img { width: 100%; height: 100%; display: block; object-fit: cover; }
.material-plaza-card.selected .material-plaza-cover { outline: 2px solid var(--cv-n94); outline-offset: 2px; }
.material-plaza-placeholder { width: 100%; height: 100%; display: grid; place-items: center; color: var(--cv-n58); background: var(--cv-n20); }
.material-plaza-card > strong {
  overflow: hidden;
  color: var(--cv-n92);
  font-size: 12px;
  font-weight: 520;
  text-align: center;
  text-overflow: ellipsis;
  white-space: nowrap;
}
/* 星标叠在画面上：用媒体控件色（两套主题同值）。悬停才出现，已收藏的常亮。 */
.material-plaza-star {
  width: 26px;
  height: 26px;
  position: absolute;
  top: 6px;
  right: 6px;
  display: grid;
  place-items: center;
  border: 0;
  border-radius: 7px;
  padding: 0;
  color: var(--portal-on-media);
  background: var(--portal-on-media-bg);
  opacity: 0;
  cursor: pointer;
  transition: opacity .12s ease;
}
.material-plaza-card:hover .material-plaza-star,
.material-plaza-star:focus-visible,
.material-plaza-card.favorited .material-plaza-star { opacity: 1; }
.material-plaza-card.favorited .material-plaza-star { color: #ffd45c; }
.material-plaza-star:hover { background: var(--portal-on-media-bg-hover); }
.material-plaza-state {
  min-height: 100%;
  display: grid;
  place-items: center;
  align-content: center;
  gap: 10px;
  color: var(--canvas-muted);
  font-size: 13px;
  text-align: center;
}
.material-plaza-state button {
  height: 30px;
  border: 1px solid var(--canvas-line-strong);
  border-radius: 7px;
  padding: 0 14px;
  color: var(--cv-n92);
  background: var(--cv-n20);
  cursor: pointer;
}
@media (max-width: 720px) {
  .material-plaza-grid { grid-template-columns: repeat(3, minmax(0, 1fr)); }
  .material-plaza-head { grid-template-columns: 1fr auto; }
  .material-plaza-search { grid-column: 1 / -1; grid-row: 2; }
}
</style>
