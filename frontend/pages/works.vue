<script setup lang="ts">
import PageCrumb from "~/components/common/PageCrumb.vue";
import MarkdownView from "~/components/common/MarkdownView.vue";
import { AudioLines, Download, FileText, Image as ImageIcon, Play, Trash2, Video, X } from "@lucide/vue";
import { ElMessage } from "element-plus";
import { appConfirmDanger } from "~/utils/appDialog";
import { mediaThumbUrl } from "~/utils/authorizedMedia";
import { LIBRARY_KIND_LABELS, libraryThumbMediaId, libraryTime, toggleTag, type LibraryItem } from "~/utils/assetLibrary";
import { directorCanEdit } from "~/utils/directorRole";

definePageMeta({ layout: "platform" });

/**
 * 「我的作品 · 创作资产」= **保存过的**生成产物（我的资产，`GET /library/items`，迁移 00074）。
 *
 * ⚠️ 2026-10-30 用户要求：只有保存过的才展示（画布右键「保存到我的资产」）。此前这一页列出工作区里**全部**成功的生成
 *    （`/creations`），用户没法挑。文本也收了（保存的剧本、提示词同样是资产）。
 *    评级（1~5 星，整个工作区共用）可以在卡片上直接改，也可以按评级、标签、类型筛。
 */
const PAGE_SIZE = 24;
const TYPES = [
  { code: "", label: "全部" },
  { code: "text", label: "文本" },
  { code: "image", label: "图片" },
  { code: "video", label: "视频" },
  { code: "audio", label: "音频" }
];
const RATINGS = [
  { value: 0, label: "全部评级" },
  { value: 5, label: "★5" },
  { value: 4, label: "★4 及以上" },
  { value: 3, label: "★3 及以上" },
  { value: 1, label: "已评级" }
];

const route = useRoute();
const router = useRouter();
const workspace = useWorkspaceStore();
const canEdit = computed(() => directorCanEdit(workspace.current?.role));
const type = ref(typeof route.query.type === "string" ? route.query.type : "");
const minRating = ref(Number(route.query.rating ?? 0) || 0);
const tags = ref<string[]>(typeof route.query.tags === "string" && route.query.tags ? route.query.tags.split(",") : []);
const allTags = ref<string[]>([]);
const total = ref(-1);
const loadError = ref("");
const { urls: previews, appendAll: loadPreviews, release: releasePreviews } = useAuthenticatedMedia();

const list = useInfiniteList<LibraryItem>({
  pageSize: PAGE_SIZE,
  fetchPage: async (_next, cursor) => {
    const query: Record<string, string> = { limit: String(PAGE_SIZE) };
    if (cursor) query.cursor = cursor;
    if (type.value) query.kind = type.value;
    if (minRating.value) query.minRating = String(minRating.value);
    if (tags.value.length) query.tags = tags.value.join(",");
    try {
      loadError.value = "";
      const page = await useApi()<{ items: LibraryItem[]; nextCursor?: string | null; hasMore: boolean; total: number }>("/library/items", { query });
      total.value = page.total;
      return page;
    } catch (cause) {
      loadError.value = (cause as { message?: string })?.message || "资产加载失败";
      return null;
    }
  },
  onPage: items => loadPreviews(items.map(item => {
    const id = libraryThumbMediaId(item);
    return { key: item.id, url: id ? mediaThumbUrl(id) : undefined };
  }))
});

const headline = computed(() => {
  if (list.loading.value && !list.items.value.length) return "正在加载资产";
  if (total.value >= 0) return `${total.value} 项已保存的资产`;
  return `已加载 ${list.items.value.length} 项`;
});

function refresh() {
  void router.replace({ query: {
    ...(type.value ? { type: type.value } : {}),
    ...(minRating.value ? { rating: String(minRating.value) } : {}),
    ...(tags.value.length ? { tags: tags.value.join(",") } : {})
  } });
  releasePreviews();
  total.value = -1;
  void list.reset();
}
function selectType(code: string) {
  if (type.value === code) return;
  type.value = code;
  refresh();
}
function selectRating(value: number) {
  minRating.value = value;
  refresh();
}
function pickTag(tag: string) {
  tags.value = toggleTag(tags.value, tag);
  refresh();
}

async function rate(item: LibraryItem, star: number) {
  if (!canEdit.value) return;
  const next = item.rating === star ? 0 : star;
  try {
    await useApi()(`/library/ratings/${item.generationId}`, { method: "PUT", body: { rating: next } });
    for (const it of list.items.value) if (it.generationId === item.generationId) it.rating = next;
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "评级失败");
  }
}
async function remove(item: LibraryItem) {
  try {
    await appConfirmDanger(`把「${item.title}」移出我的资产？画布上的节点和生成结果不受影响。`, "移出资产", "移出");
    await useApi()("/library/items/batch", { method: "POST", body: { action: "remove", ids: [item.id] } });
    list.items.value = list.items.value.filter(it => it.id !== item.id);
    if (total.value > 0) total.value -= 1;
  } catch (cause) {
    if (cause !== "cancel" && cause !== "close") ElMessage.error(cause instanceof Error ? cause.message : "移出失败");
  }
}

/** 媒体资产点开走共享的预览（逐项取原图、下载）；文本资产在页面里展开看全文。 */
const preview = useOutputPreview();
const textOpen = ref<LibraryItem | null>(null);
function open(item: LibraryItem) {
  if (item.kind === "text") {
    textOpen.value = item;
    return;
  }
  void preview.open({
    generationId: item.generationId,
    type: item.kind,
    title: item.title,
    subtitle: [item.modelName || item.modelCode, libraryTime(item.generatedAt)].filter(Boolean).join(" · "),
    filename: item.title
  });
}
const downloadCurrent = () => preview.download();

onMounted(async () => {
  void list.reset();
  try {
    const res = await useApi()<{ preset: string[]; custom: string[] }>("/library/tags");
    allTags.value = [...res.preset, ...res.custom];
  } catch {
    // 取不到标签：只是没有标签筛选
  }
});
</script>

<template>
  <main class="portal-page works-page">
    <header class="portal-page-head compact">
      <div>
        <PageCrumb><span class="page-kicker"><ImageIcon :size="15" />我的作品</span></PageCrumb>
        <h1>创作资产</h1>
        <p>{{ headline }}</p>
      </div>
      <div class="work-filter">
        <button v-for="item in TYPES" :key="item.code" type="button" :class="{ active: type === item.code }" @click="selectType(item.code)">{{ item.label }}</button>
      </div>
    </header>
    <div class="works-subfilter">
      <div class="work-filter">
        <button v-for="r in RATINGS" :key="r.value" type="button" :class="{ active: minRating === r.value }" @click="selectRating(r.value)">{{ r.label }}</button>
      </div>
      <div v-if="allTags.length" class="works-tags">
        <button v-for="tag in allTags" :key="tag" type="button" :class="{ active: tags.includes(tag) }" @click="pickTag(tag)">{{ tag }}</button>
      </div>
    </div>

    <section class="works-grid">
      <article v-for="item in list.items.value" :key="item.id" class="work-card" data-testid="work-card">
        <button type="button" class="work-preview" :class="{ text: item.kind === 'text' }" :title="`预览 ${item.title}`" @click="open(item)">
          <img v-if="previews[item.id]" :src="previews[item.id]" :alt="item.title">
          <div v-else-if="item.kind === 'text'" class="work-text"><MarkdownView :source="item.text ?? ''" :max-lines="7" /></div>
          <Video v-else-if="item.kind === 'video'" :size="34" />
          <AudioLines v-else-if="item.kind === 'audio'" :size="34" />
          <ImageIcon v-else :size="34" />
          <span v-if="item.kind === 'video'" class="work-play"><Play :size="16" fill="currentColor" /></span>
        </button>
        <header>
          <div>
            <strong>{{ item.title }}</strong>
            <span>{{ item.modelName || item.modelCode || LIBRARY_KIND_LABELS[item.kind] }} · {{ libraryTime(item.generatedAt).slice(0, 10) }}</span>
          </div>
          <span class="media-kind">{{ LIBRARY_KIND_LABELS[item.kind] }}</span>
        </header>
        <div class="work-meta">
          <span class="work-stars" role="radiogroup" aria-label="评级">
            <button v-for="star in 5" :key="star" type="button" :class="{ on: star <= item.rating }" :disabled="!canEdit" :aria-label="`${star} 星`" :title="canEdit ? '再点同一颗星清除' : '需要编辑权限'" @click="rate(item, star)">★</button>
          </span>
          <span v-if="item.tags.length" class="work-tags"><span v-for="tag in item.tags" :key="tag">{{ tag }}</span></span>
        </div>
        <footer>
          <button type="button" :disabled="!canEdit" :title="canEdit ? '移出我的资产（生成结果本身不受影响）' : '需要编辑权限'" @click="remove(item)"><Trash2 :size="16" /></button>
        </footer>
      </article>

      <div :ref="el => list.sentinel.value = (el as HTMLElement)" class="showcase-sentinel" />
      <div v-if="loadError && !list.items.value.length" class="portal-empty wide">
        资产加载失败：{{ loadError }}
        <button type="button" class="portal-link-button" @click="list.reset()">重试</button>
      </div>
      <div v-else-if="list.loading.value && !list.items.value.length" class="portal-empty wide">正在加载资产</div>
      <div v-else-if="list.finished.value && !list.items.value.length" class="portal-empty wide">
        {{ type || minRating || tags.length ? '没有符合条件的资产' : '还没有保存的资产。在画布上右键生成结果，选「保存到我的资产」。' }}
      </div>
    </section>
    <p v-if="list.loading.value && list.items.value.length" class="library-end">正在加载</p>

    <MediaPreviewDialog
      v-if="preview.opened.value"
      :type="preview.type.value"
      :src="preview.currentSrc.value"
      :title="preview.request.value?.title"
      :subtitle="preview.request.value?.subtitle"
      :loading="preview.loading.value"
      :error="preview.error.value"
      :index="preview.index.value"
      :count="preview.outputs.value.length"
      @close="preview.close"
      @prev="preview.step(-1)"
      @next="preview.step(1)"
      @retry="preview.retry"
    >
      <template #actions>
        <button type="button" class="product-primary-command" :disabled="!preview.currentSrc.value || preview.loading.value" @click="downloadCurrent">
          <Download :size="15" />下载
        </button>
      </template>
    </MediaPreviewDialog>
    <p v-else-if="list.finished.value && list.items.value.length > PAGE_SIZE" class="library-end">没有更多了</p>

    <Teleport to="body">
      <div v-if="textOpen" class="works-text-overlay" @click.self="textOpen = null">
        <section role="dialog" :aria-label="textOpen.title">
          <header><FileText :size="16" /><strong>{{ textOpen.title }}</strong><button type="button" aria-label="关闭" @click="textOpen = null"><X :size="16" /></button></header>
          <div class="works-text-body"><MarkdownView :source="textOpen.text ?? ''" /></div>
        </section>
      </div>
    </Teleport>
  </main>
</template>

<style scoped>
.works-subfilter { display: flex; flex-wrap: wrap; align-items: center; gap: 12px; margin-top: 14px; }
.works-tags { display: flex; flex-wrap: wrap; gap: 6px; }
.works-tags button { height: 28px; padding: 0 12px; border: 1px solid var(--portal-line); border-radius: 999px; color: var(--portal-muted); background: transparent; font-size: 12px; cursor: pointer; }
.works-tags button.active { border-color: var(--portal-text); color: var(--portal-text); }
.work-preview.text { align-items: stretch; justify-content: stretch; }
.work-text { width: 100%; padding: 16px 18px; overflow: hidden; text-align: left; }
.work-text :deep(.markdown-view) { font-size: 13px; }
.work-meta { display: flex; align-items: center; justify-content: space-between; gap: 8px; padding: 0 16px 4px; }
.work-stars button { border: 0; padding: 0 1px; color: var(--portal-line-strong, #c5c7cb); background: transparent; font-size: 16px; cursor: pointer; }
.work-stars button.on { color: #f5b301; }
.work-stars button:disabled { cursor: default; }
.work-tags { display: flex; flex-wrap: wrap; justify-content: flex-end; gap: 4px; }
.work-tags span { padding: 1px 8px; border-radius: 999px; color: var(--portal-muted); background: var(--portal-surface-3); font-size: 11px; }
.works-text-overlay { position: fixed; inset: 0; z-index: 90; display: grid; place-items: center; padding: 24px; background: rgba(8, 9, 11, .55); }
.works-text-overlay section { width: min(760px, 100%); max-height: 86vh; display: flex; flex-direction: column; overflow: hidden; border-radius: 16px; background: var(--portal-elevated); box-shadow: 0 28px 70px rgba(0, 0, 0, .35); }
.works-text-overlay header { display: flex; align-items: center; gap: 8px; padding: 14px 18px; border-bottom: 1px solid var(--portal-line); }
.works-text-overlay header strong { flex: 1; }
.works-text-overlay header button { display: grid; place-items: center; border: 0; color: var(--portal-muted); background: transparent; cursor: pointer; }
.works-text-body { overflow-y: auto; padding: 18px 22px; }
</style>
