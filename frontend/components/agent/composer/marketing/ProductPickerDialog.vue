<script setup lang="ts">
/**
 * 商品选择弹窗（MKT-T-38，营销 dev/05 §4.4、§5；MKT-F-92/96/97、MKT-UX-14/16/17/18，对标截图 58）。
 *
 * 搜索（服务端，300ms 防抖）· 排序（最近更新 / 最近创建 / 名称）· 「＋ 新增」· 网格（主图、名称、张数，游标分页滚动加载）·
 * 单选（打开时保留当前已选；再点一次取消）· 悬停「编辑 / 删除」· 空态「暂无商品」/ 无结果「没有匹配的商品」·
 * 底部「取消 / 添加」（选择有变化才可点）。
 *
 * **选择即放入，确认才写库**（dev/05 §5）：「添加」只把选择交回输入框（不写库）；新增 / 编辑 / 删除才写库。
 * 所以只读成员能浏览、能选（输入框本身禁用，选了也发不出去），写库按钮全禁用并说明（MKT-UX-17）。
 *
 * ⚠️ 打开只发 GET（列表 + 缩略图）。缩略图走 `useAuthenticatedMedia`（160 档），弹窗关闭即全部 revoke。
 * ⚠️ 被删的商品若正被输入框选中，交回 `deleted` 由输入框从卡片移除并提示（MKT-UX-18）；被编辑的交回 `updated` 同步卡片。
 */
import { ElMessage, ElMessageBox } from "element-plus";
import { Check, LoaderCircle, Pencil, Plus, Search, Trash2, X } from "@lucide/vue";
import ProductEditorDialog from "./ProductEditorDialog.vue";
import { mediaThumbUrl } from "~/utils/authorizedMedia";
import { apiFailure } from "~/utils/agentPanelBridge";
import {
  MARKETING_LIBRARY_PAGE_SIZE,
  MARKETING_LIBRARY_SORTS,
  MARKETING_READ_ONLY_TEXT,
  MARKETING_SEARCH_DEBOUNCE_MS,
  marketingLibraryQuery,
  marketingPickChanged,
  marketingProductCountText,
  marketingProductDeleteConfirm,
  marketingProductEmptyText,
  marketingProductQuota,
  type MarketingLibrarySort,
  type MarketingProduct,
  type MarketingProductLimits,
  type MarketingProductSummary
} from "~/utils/marketingLibrary";
import type { MarketingPickedProduct } from "~/utils/marketingSlots";

const props = withDefaults(defineProps<{
  /** 输入框里当前选中的商品 ID；没选为空串。 */
  selectedId?: string;
  limits: MarketingProductLimits;
  readOnly?: boolean;
}>(), { selectedId: "", readOnly: false });

const emit = defineEmits<{
  close: [];
  /** 「添加」：交回选中的商品；取消选中后点添加交回 null（从卡片移除）。 */
  pick: [product: MarketingPickedProduct | null];
  /** 某个商品被删除了（输入框若正选着它就移除）。 */
  deleted: [id: string];
  /** 某个商品被编辑了（输入框若正选着它就同步名称 / 主图 / 张数）。 */
  updated: [product: MarketingPickedProduct];
}>();

const thumbs = useAuthenticatedMedia();
const q = ref("");
const appliedQ = ref("");
const sort = ref<MarketingLibrarySort>("updated");
const loadError = ref("");
const selected = ref(props.selectedId);
/** 选中项的摘要（翻页 / 搜索后它可能不在当前列表里，交回时仍要有名称与张数）。 */
const selectedProduct = ref<MarketingPickedProduct | null>(null);
const serverQuotaFull = ref(false);
const editor = ref<{ open: boolean; productId: string }>({ open: false, productId: "" });
const busyDelete = ref("");

const list = useInfiniteList<MarketingProductSummary>({
  pageSize: MARKETING_LIBRARY_PAGE_SIZE,
  async fetchPage(_page, cursor) {
    loadError.value = "";
    try {
      return await useApi()<{ items: MarketingProductSummary[]; nextCursor: string | null }>("/marketing/products", {
        query: marketingLibraryQuery({ q: appliedQ.value, sort: sort.value, cursor })
      });
    } catch (cause) {
      loadError.value = !navigator.onLine ? "网络已断开，恢复后点重试" : apiFailure(cause).message || "商品加载失败";
      return null;
    }
  },
  onPage(items) {
    const wanted = items.filter(item => item.mainMediaId).map(item => ({ key: item.mainMediaId!, url: mediaThumbUrl(item.mainMediaId, 160) }));
    if (wanted.length) void thumbs.appendAll(wanted);
    if (!selectedProduct.value && selected.value) {
      const hit = items.find(item => item.id === selected.value);
      if (hit) selectedProduct.value = pickedOf(hit);
    }
  }
});

const changed = computed(() => marketingPickChanged(props.selectedId, selected.value));
const quota = computed(() => marketingProductQuota({
  loaded: list.items.value.length,
  finished: list.finished.value,
  max: props.limits.quota,
  serverFull: serverQuotaFull.value
}));
const addReason = computed(() => (props.readOnly ? MARKETING_READ_ONLY_TEXT : quota.value.reason));
const firstLoad = computed(() => list.loading.value && list.items.value.length === 0);

function pickedOf(item: Pick<MarketingProductSummary, "id" | "name" | "mainMediaId" | "imageCount">): MarketingPickedProduct {
  return { id: item.id, name: item.name, mainMediaId: item.mainMediaId, imageCount: item.imageCount };
}

let searchTimer: ReturnType<typeof setTimeout> | undefined;
watch(q, value => {
  clearTimeout(searchTimer);
  searchTimer = setTimeout(() => {
    if (value.trim() === appliedQ.value) return;
    appliedQ.value = value.trim();
    void list.reset();
  }, MARKETING_SEARCH_DEBOUNCE_MS);
});
watch(sort, () => void list.reset());

function toggle(item: MarketingProductSummary) {
  if (selected.value === item.id) {
    selected.value = "";
    selectedProduct.value = null;
  } else {
    selected.value = item.id;
    selectedProduct.value = pickedOf(item);
  }
}

function confirm() {
  if (!changed.value) return;
  emit("pick", selected.value ? selectedProduct.value : null);
}

// ---------------------------------------------------------------- 新增 / 编辑 / 删除

function openEditor(productId = "") {
  if (props.readOnly) return;
  if (!productId && quota.value.full) return;
  editor.value = { open: true, productId };
}

function onSaved(product: MarketingProduct, created: boolean) {
  editor.value = { open: false, productId: "" };
  const picked = pickedOf(product);
  if (product.mainMediaId) void thumbs.appendAll([{ key: product.mainMediaId, url: mediaThumbUrl(product.mainMediaId, 160) }]);
  if (created) {
    // 新建的那一个自动选中并回到列表（MKT-UX-14）；列表从第一页重取，它会按排序出现在该在的位置。
    selected.value = product.id;
    selectedProduct.value = picked;
    void list.reset();
    return;
  }
  list.items.value = list.items.value.map(item => (item.id === product.id
    ? { ...item, name: product.name, imageCount: product.imageCount, mainMediaId: product.mainMediaId, version: product.version, updatedAt: product.updatedAt }
    : item));
  if (selected.value === product.id) selectedProduct.value = picked;
  emit("updated", picked);
}

async function remove(item: MarketingProductSummary) {
  if (props.readOnly || busyDelete.value) return;
  const text = marketingProductDeleteConfirm(item.name);
  try {
    await ElMessageBox.confirm(text.message, text.title, { confirmButtonText: "删除", cancelButtonText: "取消", type: "warning" });
  } catch {
    return;
  }
  busyDelete.value = item.id;
  try {
    await useApi()(`/marketing/products/${encodeURIComponent(item.id)}`, { method: "DELETE" });
    list.items.value = list.items.value.filter(entry => entry.id !== item.id);
    serverQuotaFull.value = false;
    if (selected.value === item.id) {
      selected.value = "";
      selectedProduct.value = null;
    }
    emit("deleted", item.id);
  } catch (cause) {
    const failure = apiFailure(cause);
    // 已经被别人删掉了：结果与想要的一致，从列表拿掉即可。
    if ((cause as { statusCode?: number }).statusCode === 404) {
      list.items.value = list.items.value.filter(entry => entry.id !== item.id);
      emit("deleted", item.id);
      return;
    }
    ElMessage.error(!navigator.onLine ? "网络已断开，恢复后再试" : failure.message);
  } finally {
    busyDelete.value = "";
  }
}

// ---------------------------------------------------------------- 关闭

function onKeydown(event: KeyboardEvent) {
  // 编辑弹窗开着时 Esc 归它（它自己会确认是否放弃修改）。
  if (event.key !== "Escape" || editor.value.open) return;
  event.stopPropagation();
  emit("close");
}

onMounted(() => {
  window.addEventListener("keydown", onKeydown, true);
  void list.reset();
});
onBeforeUnmount(() => {
  window.removeEventListener("keydown", onKeydown, true);
  clearTimeout(searchTimer);
});
</script>

<template>
  <Teleport to="body">
    <div class="portal-overlay product-picker-overlay" @click.self="emit('close')">
      <section class="product-picker-dialog" role="dialog" aria-modal="true" aria-label="选择商品">
        <header class="pp-head">
          <h2>商品</h2>
          <label class="pp-search">
            <Search :size="14" />
            <input v-model="q" type="search" placeholder="搜索商品" aria-label="按名称搜索商品">
          </label>
          <button type="button" class="pp-close" title="关闭（Esc）" aria-label="关闭" @click="emit('close')"><X :size="15" /></button>
        </header>
        <div class="pp-tools">
          <span v-if="readOnly" class="pp-note">{{ MARKETING_READ_ONLY_TEXT }}</span>
          <span v-else-if="quota.full" class="pp-note">{{ quota.reason }}</span>
          <select v-model="sort" class="pp-pill" aria-label="排序">
            <option v-for="option in MARKETING_LIBRARY_SORTS" :key="option.value" :value="option.value">{{ option.label }}</option>
          </select>
          <i class="pp-sep" aria-hidden="true" />
          <button type="button" class="pp-pill" :disabled="Boolean(addReason)" :title="addReason || '新增商品'" @click="openEditor()">
            <Plus :size="13" />新增
          </button>
        </div>

        <div class="pp-body">
          <div v-if="firstLoad" class="pp-empty"><LoaderCircle :size="18" class="spin" />加载中…</div>
          <div v-else-if="loadError && !list.items.value.length" class="pp-empty">
            <span>{{ loadError }}</span>
            <button type="button" class="pp-link" @click="list.reset()">重试</button>
          </div>
          <div v-else-if="!list.items.value.length" class="pp-empty">{{ marketingProductEmptyText(appliedQ) }}</div>
          <ul v-else class="pp-grid">
            <li v-for="item in list.items.value" :key="item.id" class="pp-item" :class="{ on: selected === item.id }">
              <button type="button" class="pp-card" :aria-pressed="selected === item.id" :title="item.name" @click="toggle(item)">
                <span class="pp-thumb">
                  <img v-if="item.mainMediaId && thumbs.urls.value[item.mainMediaId]" :src="thumbs.urls.value[item.mainMediaId]" :alt="item.name" draggable="false">
                  <span v-if="selected === item.id" class="pp-check"><Check :size="13" /></span>
                </span>
                <strong>{{ item.name }}</strong>
                <small>{{ marketingProductCountText(item.imageCount) }}</small>
              </button>
              <span class="pp-actions">
                <button type="button" :disabled="readOnly" :title="readOnly ? MARKETING_READ_ONLY_TEXT : '编辑'" :aria-label="`编辑「${item.name}」`" @click="openEditor(item.id)"><Pencil :size="12" /></button>
                <button type="button" :disabled="readOnly || busyDelete === item.id" :title="readOnly ? MARKETING_READ_ONLY_TEXT : '删除'" :aria-label="`删除「${item.name}」`" @click="remove(item)"><Trash2 :size="12" /></button>
              </span>
            </li>
            <li v-if="!list.finished.value" class="pp-sentinel">
              <span :ref="el => { list.sentinel.value = (el as HTMLElement) || undefined }"><LoaderCircle v-if="list.loading.value" :size="14" class="spin" /></span>
            </li>
          </ul>
          <p v-if="loadError && list.items.value.length" class="pp-note warn">{{ loadError }} <button type="button" class="pp-link" @click="list.loadNext()">重试</button></p>
        </div>

        <footer class="pp-foot">
          <button type="button" class="pp-btn" @click="emit('close')">取消</button>
          <button type="button" class="pp-btn primary" :disabled="!changed" :title="changed ? '' : '选择没有变化'" @click="confirm">添加</button>
        </footer>
      </section>
    </div>

    <ProductEditorDialog
      v-if="editor.open"
      :product-id="editor.productId"
      :limits="limits"
      :read-only="readOnly"
      @close="editor = { open: false, productId: '' }"
      @saved="onSaved"
      @quota-full="serverQuotaFull = true"
    />
  </Teleport>
</template>

<style scoped>
.product-picker-dialog {
  width: min(960px, 100%);
  height: min(740px, calc(100vh - 48px));
  display: grid;
  grid-template-rows: auto auto minmax(0, 1fr) auto;
  gap: 12px;
  border: 1px solid var(--portal-line-strong);
  border-radius: 20px;
  padding: 20px 22px;
  color: var(--portal-text);
  background: var(--portal-elevated);
  box-shadow: 0 28px 70px var(--portal-shadow-strong);
}
.pp-head { display: flex; align-items: center; gap: 12px; }
.pp-head h2 { margin: 0 auto 0 0; font-size: 16px; }
.pp-search {
  width: min(280px, 50%);
  height: 34px;
  display: flex;
  align-items: center;
  gap: 6px;
  border-radius: 999px;
  padding: 0 12px;
  color: var(--portal-muted);
  background: var(--portal-surface-2);
}
.pp-search input { min-width: 0; flex: 1; border: 0; color: var(--portal-text); background: transparent; font: inherit; font-size: 13px; outline: none; }
.pp-search input::placeholder { color: var(--portal-placeholder); }
.pp-close {
  width: 28px;
  height: 28px;
  display: grid;
  place-items: center;
  border: 0;
  border-radius: 50%;
  color: var(--portal-muted);
  background: var(--portal-surface-2);
  cursor: pointer;
}
.pp-close:hover { color: var(--portal-text); background: var(--portal-hover); }
.pp-tools { display: flex; align-items: center; justify-content: flex-end; gap: 10px; }
.pp-note { margin: 0 auto 0 0; color: var(--portal-muted); font-size: 12px; }
.pp-note.warn { color: var(--portal-danger); }
.pp-sep { width: 1px; height: 18px; background: var(--portal-line); }
.pp-pill {
  height: 32px;
  display: inline-flex;
  align-items: center;
  gap: 4px;
  border: 1px solid var(--portal-line-strong);
  border-radius: 999px;
  padding: 0 14px;
  color: var(--portal-text);
  background: transparent;
  font: inherit;
  font-size: 12px;
  font-weight: 600;
  cursor: pointer;
}
select.pp-pill { padding-right: 10px; }
.pp-pill:hover:not(:disabled) { background: var(--portal-hover); }
.pp-pill:disabled { cursor: not-allowed; opacity: .5; }
.pp-body { min-height: 0; overflow-y: auto; }
.pp-empty {
  height: 100%;
  min-height: 200px;
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 8px;
  color: var(--portal-muted);
  font-size: 13px;
}
.pp-link { border: 0; padding: 0; color: var(--portal-cyan); background: transparent; font: inherit; cursor: pointer; }
.pp-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(150px, 1fr));
  gap: 14px;
  margin: 0;
  padding: 2px;
  list-style: none;
}
.pp-item { position: relative; min-width: 0; }
.pp-card {
  width: 100%;
  display: grid;
  gap: 4px;
  border: 0;
  padding: 0;
  color: var(--portal-text);
  background: transparent;
  font: inherit;
  text-align: left;
  cursor: pointer;
}
.pp-thumb {
  position: relative;
  aspect-ratio: 1;
  display: block;
  overflow: hidden;
  border: 2px solid transparent;
  border-radius: 12px;
  background: var(--portal-surface-2);
}
.pp-item.on .pp-thumb { border-color: var(--portal-cyan); }
.pp-card:hover .pp-thumb { background: var(--portal-hover); }
.pp-thumb img { width: 100%; height: 100%; object-fit: cover; }
.pp-check {
  position: absolute;
  top: 6px;
  left: 6px;
  width: 20px;
  height: 20px;
  display: grid;
  place-items: center;
  border-radius: 50%;
  color: var(--portal-on-accent);
  background: var(--portal-cyan);
}
.pp-card strong { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 12px; font-weight: 600; }
.pp-card small { color: var(--portal-muted); font-size: 11px; }
.pp-actions { position: absolute; top: 6px; right: 6px; display: flex; gap: 4px; opacity: 0; transition: opacity .12s; }
.pp-item:hover .pp-actions,
.pp-item:focus-within .pp-actions { opacity: 1; }
@media (hover: none) { .pp-actions { opacity: 1; } }
.pp-actions button {
  width: 24px;
  height: 24px;
  display: grid;
  place-items: center;
  border: 0;
  border-radius: 6px;
  color: var(--portal-on-media);
  background: var(--portal-on-media-bg);
  cursor: pointer;
}
.pp-actions button:hover:not(:disabled) { background: var(--portal-on-media-bg-hover); }
.pp-actions button:disabled { cursor: not-allowed; opacity: .5; }
.pp-sentinel { grid-column: 1 / -1; display: flex; justify-content: center; min-height: 20px; color: var(--portal-muted); }
.pp-foot { display: flex; justify-content: flex-end; gap: 10px; }
.pp-btn {
  min-width: 72px;
  height: 34px;
  border: 1px solid var(--portal-line-strong);
  border-radius: 999px;
  padding: 0 16px;
  color: var(--portal-text);
  background: transparent;
  font: inherit;
  font-size: 13px;
  font-weight: 600;
  cursor: pointer;
}
.pp-btn:hover:not(:disabled) { background: var(--portal-hover); }
.pp-btn.primary { border-color: transparent; color: var(--portal-mark-text); background: var(--portal-mark-bg); }
.pp-btn.primary:hover:not(:disabled) { opacity: .9; }
.pp-btn:disabled { cursor: not-allowed; color: var(--portal-disabled-text); background: var(--portal-disabled-bg); border-color: transparent; }
.spin { animation: pp-spin 1s linear infinite; }
@keyframes pp-spin { to { transform: rotate(360deg); } }
@media (max-width: 639px) {
  .product-picker-dialog { padding: 16px; border-radius: 16px; }
  .pp-grid { grid-template-columns: repeat(auto-fill, minmax(110px, 1fr)); gap: 10px; }
}
</style>
