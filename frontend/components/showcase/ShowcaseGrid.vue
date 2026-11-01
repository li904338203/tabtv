<script setup lang="ts">
/**
 * 精选作品墙 —— 首页「灵感与作品」、AI 视频首页、创造页**共用这一个**（AGT-40，设计 02 §6）。
 *
 * ⚠️ 此前首页与 AI 视频首页各抄了一份，且两份都按老契约读 `type/url/authorName/cover`，
 *    而 `GET /showcase` 只回 `{id,title,hasMedia,type,createdAt}` —— 卡片永远落到占位块。
 *    抄两份的结果就是「一处修了另一处没修」。
 *
 * - 取数：公开接口 `/showcase`，游标分页 + 滚动加载；分类是服务端的 `type` 参数（AGT-41），
 *   **不做客户端筛选**（在已取的 24 条里筛会得出「没有音频作品」的错误结论）。
 * - 取图：公开端点 `/showcase/{id}/media`，直接给 `<img>/<video>` 地址、不走 blob ——
 *   浏览器能吃服务端的 public 缓存头，而 blob 会把每张卡的完整媒体读进内存。
 * - 视频卡（2026-11-01）：封面是第一帧（`/showcase/{id}/cover`，十几 KB），视频 `preload="none"`、悬停才取并盖在
 *   封面上播。此前用 `<video preload=metadata>` 取封面，而公开媒体端点不支持 Range，每张卡都把整段视频（10~20MB）
 *   下完 —— 几段一起下占满浏览器对同一主机的连接，页面上别的接口跟着排队好几秒。封面帧还没截好的退回老办法。
 * - 失败只在本块报错并给重试，不影响页面其他部分（设计 02 §8）。
 */
import { Music, Play } from "@lucide/vue";
import {
  SHOWCASE_FILTERS,
  buildShowcaseQuery,
  showcaseCardKind,
  showcaseCoverUrl,
  showcaseErrorText,
  showcaseMediaUrl,
  type ShowcaseFilter,
  type ShowcaseItem
} from "~/utils/showcase";
import { LIST_THUMB_WIDTH } from "~/utils/authorizedMedia";
import { startHoverPreview, stopHoverPreview } from "~/utils/mediaGrid";

const props = withDefaults(defineProps<{
  pageSize?: number;
  /** 是否显示分类按钮（全部 / 视频 / 图片 / 音频）。 */
  showCategories?: boolean;
}>(), {
  pageSize: 24,
  showCategories: false
});

/** 预览大图用的宽度：服务端只认 160/320/640，不给 w 时默认是 320 的缩略图。 */
const PREVIEW_WIDTH = 640;

const apiBase = useRuntimeConfig().public.apiBase as string;
const filter = ref<ShowcaseFilter>("");
const error = ref("");
const preview = ref<ShowcaseItem | null>(null);
/** `<img>/<video>` 取失败的作品：回落占位，而不是显示裂图。 */
const broken = ref(new Set<string>());
/** 封面帧还没截好的视频（`/cover` 回 404）：退回 `<video preload=metadata>` 取首帧当封面。 */
const noCover = ref(new Set<string>());
/** 正在悬停播放的视频卡。 */
const playingId = ref("");

const list = useInfiniteList<ShowcaseItem>({
  pageSize: props.pageSize,
  fetchPage: async (_page, cursor) => {
    error.value = "";
    try {
      return await useApi()<{ items?: ShowcaseItem[]; nextCursor?: string }>(
        `/showcase?${buildShowcaseQuery({ limit: props.pageSize, cursor, type: filter.value })}`
      );
    } catch (cause) {
      error.value = showcaseErrorText(cause, import.meta.client ? navigator.onLine : true);
      return null;
    }
  }
});
const { items, loading, finished } = list;

function kindOf(item: ShowcaseItem) {
  const kind = showcaseCardKind(item);
  return broken.value.has(item.id) ? "none" : kind;
}

function markBroken(id: string) {
  broken.value = new Set(broken.value).add(id);
}

function markNoCover(id: string) {
  noCover.value = new Set(noCover.value).add(id);
}

function thumbUrl(item: ShowcaseItem) {
  return showcaseMediaUrl(item.id, apiBase, LIST_THUMB_WIDTH);
}

function coverUrl(item: ShowcaseItem) {
  return showcaseCoverUrl(item.id, apiBase, LIST_THUMB_WIDTH);
}

/**
 * 视频地址：有封面图时就是视频本身（`preload=none`，悬停才取）；没有封面帧时退回老办法 ——
 * `#t=0.1` 配 `preload=metadata`，取首帧当封面。
 */
function videoUrl(item: ShowcaseItem) {
  const url = showcaseMediaUrl(item.id, apiBase);
  return noCover.value.has(item.id) ? `${url}#t=0.1` : url;
}

const previewSrc = computed(() => {
  const item = preview.value;
  if (!item) return undefined;
  return kindOf(item) === "image"
    ? showcaseMediaUrl(item.id, apiBase, PREVIEW_WIDTH)
    : showcaseMediaUrl(item.id, apiBase);
});

/**
 * 切换分类：从头按新的 `type` 重新取。
 *
 * ⚠️ 上一页还在路上时 `reset()` 里的取数会被 loading 挡掉 —— 等它回来再重置一次，
 *    否则列表停在旧分类的结果上，而按钮已经高亮在新分类。
 */
let resetPending = false;
function selectFilter(value: ShowcaseFilter) {
  if (filter.value === value) return;
  filter.value = value;
  if (loading.value) {
    resetPending = true;
    return;
  }
  void list.reset();
}
watch(loading, (busy) => {
  if (!busy && resetPending) {
    resetPending = false;
    void list.reset();
  }
});

/** 重试：从上一次成功的游标接着取，不清掉已经显示出来的作品。 */
function retry() {
  error.value = "";
  finished.value = false;
  void list.loadNext();
}

/**
 * 悬停播放 / 离开即停：规则在 `utils/mediaGrid`（与营销模板墙共用）。悬停在整张卡上（视频平时藏在封面图下面）；
 * 离开时有封面图的回到 0（封面图会盖回来），自己当封面的回到 0.1 秒 = `#t=0.1` 的那一帧。
 */
function playPreview(event: Event, item: ShowcaseItem) {
  const video = (event.currentTarget as HTMLElement).querySelector("video");
  if (!video) return;
  playingId.value = item.id;
  startHoverPreview(video);
}

function stopPreview(event: Event, item: ShowcaseItem) {
  if (playingId.value === item.id) playingId.value = "";
  const video = (event.currentTarget as HTMLElement).querySelector("video");
  if (video) stopHoverPreview(video, noCover.value.has(item.id) ? 0.1 : 0);
}

onMounted(() => void list.loadNext());
</script>

<template>
  <div class="showcase-block">
    <div v-if="showCategories" class="showcase-bar">
      <div class="category-row" role="tablist" aria-label="作品类型">
        <button
          v-for="item in SHOWCASE_FILTERS"
          :key="item.value || 'all'"
          type="button"
          role="tab"
          class="filter-chip"
          :class="{ active: filter === item.value }"
          :aria-selected="filter === item.value"
          @click="selectFilter(item.value)"
        >{{ item.label }}</button>
      </div>
    </div>

    <section class="showcase-grid">
      <article v-for="item in items" :key="item.id" class="show-card">
        <div v-if="kindOf(item) === 'none'" class="show-image">
          <span class="show-image-placeholder" />
        </div>
        <button
          v-else
          type="button"
          class="show-image"
          :title="item.title"
          @click="preview = item"
          @mouseenter="playPreview($event, item)"
          @mouseleave="stopPreview($event, item)"
        >
          <!-- 视频卡：封面是第一帧（几十 KB 以内）；视频 preload=none，悬停才取、盖在封面上静音播，离开即停。
               封面帧还没截好（/cover 404）就退回老办法：视频自己 preload=metadata 取首帧当封面 -->
          <template v-if="kindOf(item) === 'video'">
            <img v-if="!noCover.has(item.id)" :src="coverUrl(item)" :alt="item.title" loading="lazy" @error="markNoCover(item.id)">
            <video
              class="show-video"
              :class="{ cover: noCover.has(item.id), on: playingId === item.id }"
              :src="videoUrl(item)"
              muted
              loop
              playsinline
              :preload="noCover.has(item.id) ? 'metadata' : 'none'"
              @error="markBroken(item.id)"
            />
          </template>
          <span v-else-if="kindOf(item) === 'audio'" class="show-image-placeholder show-audio">
            <Music :size="28" />
          </span>
          <img
            v-else
            :src="thumbUrl(item)"
            :alt="item.title"
            loading="lazy"
            @error="markBroken(item.id)"
          >
          <!-- 只有可播放的产出才给播放角标：图片作品挂一个播放键是在承诺不存在的行为 -->
          <span v-if="kindOf(item) === 'video' || kindOf(item) === 'audio'" class="show-play">
            <Play :size="15" fill="currentColor" />
          </span>
        </button>
        <p :title="item.title">{{ item.title || '未命名作品' }}</p>
      </article>
      <div :ref="el => list.sentinel.value = (el as HTMLElement)" class="showcase-sentinel" />
    </section>

    <p v-if="error" class="library-end showcase-error">
      {{ error }}
      <button type="button" class="portal-link-button" @click="retry">重试</button>
    </p>
    <p v-else-if="loading" class="library-end">正在加载</p>
    <p v-else-if="finished && items.length" class="library-end">没有更多了</p>
    <p v-else-if="!items.length" class="library-end">{{ filter ? '这个分类下还没有精选作品' : '还没有精选作品' }}</p>

    <!-- 预览浮层与「创作资产」共用同一个组件 -->
    <MediaPreviewDialog
      v-if="preview"
      :type="kindOf(preview)"
      :src="previewSrc"
      :title="preview.title"
      subtitle="AITv 精选作品"
      @close="preview = null"
    />
  </div>
</template>

<style scoped>
/* 设计 02 §6：≥1440 4 列 / ≥1024 3 列 / ≥640 2 列 / 手机 1 列，封面统一 4:5。
   断点与 utils/mediaGrid.ts 的 MEDIA_GRID_BREAKPOINTS 同一组（mediaGrid.test.ts 核对源码，改一边另一边会红） */
.showcase-grid { grid-template-columns: minmax(0, 1fr); }
@media (min-width: 640px) { .showcase-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
@media (min-width: 1024px) { .showcase-grid { grid-template-columns: repeat(3, minmax(0, 1fr)); } }
@media (min-width: 1440px) { .showcase-grid { grid-template-columns: repeat(4, minmax(0, 1fr)); } }
.show-image { display: block; aspect-ratio: 4 / 5; }
.show-image video { display: block; }
/* 视频叠在封面图上面，悬停播放时才显出来；没有封面帧的（.cover）视频自己当封面，一直显示 */
.show-video { position: absolute; inset: 0; opacity: 0; transition: opacity 150ms ease; }
.show-video.on, .show-video.cover { opacity: 1; }
.show-audio { display: grid; place-items: center; color: var(--portal-muted); }
/* 作者信息后端不下发（公开作者信息属于单独决定），标题不再为头像让出缩进 */
.show-card p { margin: 9px 0 0; }
.showcase-error { color: var(--portal-danger); }
</style>
