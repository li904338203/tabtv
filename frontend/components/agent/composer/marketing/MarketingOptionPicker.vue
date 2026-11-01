<script setup lang="ts">
/**
 * 创意 / Hook / 风格弹层（MKT-T-13；MKT-UX-01/03/06/07，dev/03 §3.2）。三类共用一个组件，`kind` 区分。
 *
 * 骨架与技能弹层一致：顶部搜索 +「全部 →」；左栏分类（第一项「全部」，创意另有「按模型」组）；右侧竖版封面卡三列网格。
 *
 * - **分类与搜索都交给服务端**（MKT-F-34，`GET /marketing/options`），游标翻页、滚到底取下一页。
 *   本地过滤的话只能筛已加载的那一页，选项一多「搜不到」就成了常态。
 *   「按模型」的筛选项只来自配置下发的 `creativeModelFilters`（MKT-AC-13），这里不自己派生。
 * - **不可用的照样列出**（MKT-F-19）：置灰 + 悬停写原因；语气按 `userFixable` 分类，不匹配文案（MKT-UX-07）。
 *   缺商品图的仍可选（发送键去拦，MKT-D-29），其余不可选。
 * - **悬停播放只在精确指针设备上**（MKT-UX-03）：有示例视频的卡悬停时挂一个静音 `<video>`，离开即卸载 ——
 *   同一时刻最多一个在播，卸载即停（不留一堆后台解码的视频）。
 * - **键盘**：↑↓ 按行、←→ 按格（焦点在搜索框时 ←→ 留给光标）、回车选中、Esc 关闭（详情开着先关详情）；
 *   点外部关闭，但输入框底栏的选择按钮（`data-picker-trigger`）与详情抽屉不算外部。
 * - **定位**：量一次所在输入框（`offsetParent`）的位置，`pickerPlacement` 决定向上还是向下弹 —— 与技能 / @ 弹层同一条规则。
 * - 片段（prompt）不出现在任何地方：契约里本来就没有这个字段（MKT-AC-06）。
 */
import { ArrowRight, Check, ChevronRight, Search } from "@lucide/vue";
import { canHoverPlay } from "~/utils/mediaGrid";
import { pickerPlacement, type PickerPlacement } from "~/utils/pickerPlacement";
import {
  ALL_FILTER,
  KIND_LABEL,
  gridMove,
  optionBlock,
  optionCoverUrl,
  optionListQuery,
  optionPreviewUrl,
  sameFilter,
  type MarketingCategory,
  type MarketingKind,
  type MarketingModelRef,
  type MarketingOption,
  type OptionBlock,
  type OptionBlockCtx,
  type OptionFilter
} from "~/utils/marketingOptions";
import OptionDetailDrawer from "./OptionDetailDrawer.vue";

const props = withDefaults(defineProps<{
  kind: MarketingKind;
  /** 该类启用的分类（`/marketing/config` 的 `categories[kind]`），按 sort。 */
  categories?: MarketingCategory[];
  /** 「按模型」筛选项（`/marketing/config` 的 `creativeModelFilters`）；只对创意生效。 */
  modelFilters?: MarketingModelRef[];
  /** 可用性判定的上下文（当前视频模型、商品图条数、产出、服务端结论）。 */
  ctx?: OptionBlockCtx;
  /** 当前已选的 code（打勾）。 */
  selectedCode?: string | null;
  /** 窄屏全屏面板里内嵌显示：不浮动、不量位置、撑满父容器。 */
  embedded?: boolean;
}>(), { categories: () => [], modelFilters: () => [], ctx: () => ({}), selectedCode: null, embedded: false });

const emit = defineEmits<{ select: [option: MarketingOption]; close: []; "view-all": [] }>();

/** 网格列数：与样式里的断点一致（宽屏 3 列，< 640px 2 列），键盘 ↑↓ 按它跨行。 */
const cols = ref(3);
let narrowQuery: MediaQueryList | undefined;
function syncCols() {
  cols.value = narrowQuery?.matches ? 2 : 3;
}
const PREFERRED_HEIGHT = 440;

const label = computed(() => KIND_LABEL[props.kind]);
const root = ref<HTMLElement>();
const gridBox = ref<HTMLElement>();
const searchInput = ref<HTMLInputElement>();

const filter = ref<OptionFilter>(ALL_FILTER);
const search = ref("");
const appliedSearch = ref("");
const items = ref<MarketingOption[]>([]);
const cursor = ref("");
const finished = ref(false);
const loading = ref(false);
const error = ref("");
const activeIndex = ref(-1);
const playing = ref<string | null>(null);
const detail = ref<MarketingOption | null>(null);
const canHover = ref(false);
const apiBase = useRuntimeConfig().public.apiBase as string;
/** 封面取不到的选项：回落占位，而不是显示裂图。 */
const brokenCovers = ref(new Set<string>());

const showModelGroup = computed(() => props.kind === "creative" && props.modelFilters.length > 0);
const blocks = computed(() => {
  const map = new Map<string, OptionBlock | null>();
  for (const option of items.value) map.set(option.code, optionBlock(option, props.ctx));
  return map;
});

function blockOf(option: MarketingOption): OptionBlock | null {
  return blocks.value.get(option.code) ?? null;
}

// ---- 取数：服务端筛选 + 游标；请求序号挡掉晚到的旧响应（快速切分类时后发先至会把列表换回旧分类） ----
let seq = 0;

async function loadPage(reset: boolean) {
  if (!reset && (loading.value || finished.value)) return;
  const mine = ++seq;
  loading.value = true;
  error.value = "";
  if (reset) {
    cursor.value = "";
    finished.value = false;
    playing.value = null;
  }
  try {
    const response = await useApi()<{ items?: MarketingOption[]; nextCursor?: string | null }>("/marketing/options", {
      query: optionListQuery({ kind: props.kind, filter: filter.value, q: appliedSearch.value, cursor: reset ? "" : cursor.value })
    });
    if (mine !== seq) return;
    const rows = response.items ?? [];
    items.value = reset ? rows : [...items.value, ...rows];
    cursor.value = response.nextCursor ?? "";
    finished.value = !cursor.value;
    if (reset) {
      activeIndex.value = items.value.length ? 0 : -1;
      gridBox.value?.scrollTo?.({ top: 0 });
    }
  } catch (cause) {
    if (mine !== seq) return;
    if (reset) items.value = [];
    const message = cause instanceof Error ? cause.message : "";
    error.value = message && message !== "请求失败" ? message : `${label.value}没有取到，稍后再试`;
  } finally {
    if (mine === seq) loading.value = false;
  }
  // 一页填不满可视区时接着取，否则永远滚不到底、也就永远不会取下一页
  await nextTick();
  if (mine === seq && !finished.value && !error.value && nearBottom()) void loadPage(false);
}

function nearBottom(): boolean {
  const box = gridBox.value;
  if (!box) return false;
  return box.scrollTop + box.clientHeight >= box.scrollHeight - 160;
}

function onScroll() {
  if (nearBottom()) void loadPage(false);
}

function setFilter(next: OptionFilter) {
  if (sameFilter(filter.value, next)) return;
  filter.value = next;
  void loadPage(true);
}

let searchTimer: ReturnType<typeof setTimeout> | undefined;
watch(search, (value) => {
  clearTimeout(searchTimer);
  searchTimer = setTimeout(() => {
    if (value.trim() === appliedSearch.value) return;
    appliedSearch.value = value.trim();
    void loadPage(true);
  }, 250);
});

watch(() => props.kind, () => {
  filter.value = ALL_FILTER;
  search.value = "";
  appliedSearch.value = "";
  detail.value = null;
  void loadPage(true);
});

/** 「全部 →」：回到全部分类、清掉搜索；同时通知外层（需要时可以跳到完整列表）。 */
function viewAll() {
  clearTimeout(searchTimer);
  const changed = filter.value.type !== "all" || appliedSearch.value !== "";
  filter.value = ALL_FILTER;
  search.value = "";
  appliedSearch.value = "";
  if (changed) void loadPage(true);
  emit("view-all");
}

// ---- 选择 ----
function choose(option: MarketingOption | undefined) {
  if (!option) return;
  const block = blockOf(option);
  if (block && !block.selectable) {
    // 不可选：把焦点挪到它身上，原因随之显示出来（而不是点了没反应）
    activeIndex.value = items.value.indexOf(option);
    return;
  }
  emit("select", option);
}

function openDetail(option: MarketingOption) {
  playing.value = null;
  detail.value = option;
}

// 详情关掉后焦点回到搜索框，键盘操作接着可用
watch(detail, (value) => {
  if (!value) nextTick(() => searchInput.value?.focus({ preventScroll: true }));
});

function onDetailSelect(option: MarketingOption) {
  detail.value = null;
  emit("select", option);
}

// ---- 悬停播放（只在精确指针设备上；离开即卸载） ----
function onEnter(option: MarketingOption, index: number) {
  activeIndex.value = index;
  if (canHover.value && option.hasPreview) playing.value = option.code;
}

function onLeave(option: MarketingOption) {
  if (playing.value === option.code) playing.value = null;
}

// ---- 键盘 ----
function scrollActive() {
  nextTick(() => root.value?.querySelector(".option-card.active")?.scrollIntoView({ block: "nearest" }));
}

function onKey(event: KeyboardEvent) {
  if (event.isComposing) return;
  const inInput = event.target instanceof HTMLInputElement;
  const dirs: Record<string, "up" | "down" | "left" | "right"> = { ArrowUp: "up", ArrowDown: "down", ArrowLeft: "left", ArrowRight: "right" };
  const dir = dirs[event.key];
  if (dir) {
    if (inInput && (dir === "left" || dir === "right")) return;
    event.preventDefault();
    playing.value = null;
    activeIndex.value = gridMove(activeIndex.value, dir, items.value.length, cols.value);
    scrollActive();
    if (activeIndex.value >= items.value.length - cols.value) void loadPage(false);
    return;
  }
  if (event.key === "Enter") {
    event.preventDefault();
    choose(items.value[activeIndex.value]);
    return;
  }
  if (event.key === "Escape") {
    event.preventDefault();
    event.stopPropagation();
    if (detail.value) detail.value = null;
    else emit("close");
  }
}

// ---- 点外部关闭 ----
function onOutside(event: PointerEvent) {
  const target = event.target;
  if (!(target instanceof Element) || !root.value) return;
  if (root.value.contains(target)) return;
  if (target.closest("[data-option-detail]") || target.closest("[data-picker-trigger]")) return;
  emit("close");
}

// ---- 定位：向上放不下就向下（utils/pickerPlacement） ----
const place = ref<PickerPlacement>({ side: "top", maxHeight: PREFERRED_HEIGHT });
function measure() {
  if (props.embedded || typeof window === "undefined") return;
  const anchor = root.value?.offsetParent;
  if (!(anchor instanceof HTMLElement)) return;
  const rect = anchor.getBoundingClientRect();
  // 上界取顶栏下沿：只按视口算的话弹层会钻到固定顶栏下面被裁掉
  const topbar = document.querySelector(".portal-topbar")?.getBoundingClientRect().bottom ?? 0;
  place.value = pickerPlacement({ above: rect.top - Math.max(0, topbar), below: window.innerHeight - rect.bottom, preferred: PREFERRED_HEIGHT });
}
const placeClass = computed(() => (props.embedded ? "embedded" : `picker-${place.value.side}`));
const placeStyle = computed(() => (props.embedded ? undefined : { "--picker-max": `${place.value.maxHeight}px` }));

onMounted(() => {
  canHover.value = canHoverPlay(typeof window !== "undefined" && window.matchMedia ? query => window.matchMedia(query) : undefined);
  narrowQuery = window.matchMedia?.("(max-width: 639px)");
  syncCols();
  narrowQuery?.addEventListener?.("change", syncCols);
  measure();
  window.addEventListener("resize", measure);
  window.addEventListener("scroll", measure, true);
  document.addEventListener("pointerdown", onOutside, true);
  void loadPage(true);
  nextTick(() => searchInput.value?.focus({ preventScroll: true }));
});

onBeforeUnmount(() => {
  clearTimeout(searchTimer);
  seq++;
  narrowQuery?.removeEventListener?.("change", syncCols);
  window.removeEventListener("resize", measure);
  window.removeEventListener("scroll", measure, true);
  document.removeEventListener("pointerdown", onOutside, true);
});

function coverOf(option: MarketingOption): string {
  return option.coverCount > 0 && !brokenCovers.value.has(option.code) ? optionCoverUrl(apiBase, option.code, 0, 320) : "";
}

function markBroken(code: string) {
  brokenCovers.value = new Set([...brokenCovers.value, code]);
}

function reasonText(block: OptionBlock): string {
  return block.userFixable ? block.text : `暂不可用：${block.text}`;
}
</script>

<template>
  <div
    ref="root"
    class="option-picker"
    :class="placeClass"
    :style="placeStyle"
    role="dialog"
    :aria-label="`选择${label}`"
    @keydown="onKey"
  >
    <header class="option-search">
      <Search :size="14" />
      <input ref="searchInput" v-model="search" type="search" :placeholder="`搜索${label}`" :aria-label="`搜索${label}`">
      <button type="button" class="option-all" @click="viewAll">全部 <ArrowRight :size="13" /></button>
    </header>
    <div class="option-columns">
      <nav class="option-filters" aria-label="分类">
        <ul>
          <li>
            <button type="button" :class="{ active: filter.type === 'all' }" @click="setFilter(ALL_FILTER)">全部</button>
          </li>
          <li v-for="category in categories" :key="category.code">
            <button
              type="button"
              :class="{ active: filter.type === 'category' && filter.code === category.code }"
              @click="setFilter({ type: 'category', code: category.code })"
            >
              {{ category.name }}
            </button>
          </li>
        </ul>
        <template v-if="showModelGroup">
          <p class="option-group">按模型</p>
          <ul>
            <li v-for="model in modelFilters" :key="model.code">
              <button
                type="button"
                :class="{ active: filter.type === 'model' && filter.code === model.code }"
                @click="setFilter({ type: 'model', code: model.code })"
              >
                {{ model.name || model.code }}
              </button>
            </li>
          </ul>
        </template>
      </nav>
      <div ref="gridBox" class="option-grid-box" @scroll.passive="onScroll">
        <p v-if="loading && !items.length" class="option-empty">加载中…</p>
        <p v-else-if="error && !items.length" class="option-empty error">
          {{ error }}
          <button type="button" class="option-retry" @click="loadPage(true)">重试</button>
        </p>
        <p v-else-if="!items.length" class="option-empty">
          {{ appliedSearch || filter.type !== 'all' ? `没有符合条件的${label}` : `还没有可用的${label}` }}
        </p>
        <ul v-else class="option-grid" role="listbox" :aria-label="label">
          <li
            v-for="(option, index) in items"
            :key="option.code"
            class="option-card"
            :class="{
              active: index === activeIndex,
              selected: option.code === selectedCode,
              blocked: !!blockOf(option),
              locked: blockOf(option) && !blockOf(option)!.selectable,
              fixable: blockOf(option)?.userFixable,
            }"
            role="option"
            :aria-selected="option.code === selectedCode"
            :aria-disabled="blockOf(option) && !blockOf(option)!.selectable ? 'true' : undefined"
            :title="blockOf(option) ? reasonText(blockOf(option)!) : option.description || option.name"
            @mouseenter="onEnter(option, index)"
            @mouseleave="onLeave(option)"
            @mousedown.prevent
            @click="choose(option)"
          >
            <div class="option-cover">
              <img v-if="coverOf(option)" :src="coverOf(option)" :alt="option.name" loading="lazy" @error="markBroken(option.code)">
              <span v-else class="option-placeholder">{{ option.name.slice(0, 1) }}</span>
              <video
                v-if="playing === option.code"
                class="option-preview"
                :src="optionPreviewUrl(apiBase, option.code)"
                muted
                autoplay
                loop
                playsinline
                preload="auto"
              />
              <span v-if="option.code === selectedCode" class="option-check" aria-hidden="true"><Check :size="12" /></span>
              <span v-if="blockOf(option)" class="option-reason">{{ reasonText(blockOf(option)!) }}</span>
            </div>
            <div class="option-foot">
              <span class="option-name">{{ option.name }}</span>
              <button type="button" class="option-more" :aria-label="`${option.name}详情`" @click.stop="openDetail(option)">
                <ChevronRight :size="14" />
              </button>
            </div>
          </li>
        </ul>
        <p v-if="items.length && loading" class="option-tail">加载中…</p>
        <p v-else-if="items.length && error" class="option-tail error">
          {{ error }} <button type="button" class="option-retry" @click="loadPage(false)">重试</button>
        </p>
      </div>
    </div>
    <OptionDetailDrawer
      v-if="detail"
      :option="detail"
      :block="blockOf(detail)"
      :selected="detail.code === selectedCode"
      @select="onDetailSelect"
      @close="detail = null"
    />
  </div>
</template>

<style scoped>
.option-picker {
  position: absolute;
  left: 0;
  right: 0;
  bottom: calc(100% + 8px);
  z-index: 90;
  display: flex;
  flex-direction: column;
  max-height: var(--picker-max, 440px);
  border: 1px solid var(--portal-line-strong);
  border-radius: 12px;
  overflow: hidden;
  color: var(--portal-text);
  background: var(--portal-elevated);
  box-shadow: 0 18px 48px var(--portal-shadow-strong);
  font-size: 12px;
}
/* 上方放不下时向下弹 */
.option-picker.picker-bottom { bottom: auto; top: calc(100% + 8px); }
.option-picker.embedded { position: static; height: 100%; max-height: none; border: 0; border-radius: 0; box-shadow: none; }
.option-search { display: flex; align-items: center; gap: 6px; padding: 9px 12px; border-bottom: 1px solid var(--portal-line-soft); color: var(--portal-muted); }
.option-search input { flex: 1; min-width: 0; border: 0; outline: 0; color: var(--portal-text); background: transparent; font: inherit; }
.option-all { display: inline-flex; align-items: center; gap: 4px; border: 0; color: var(--portal-text-soft); background: transparent; font: inherit; cursor: pointer; }
.option-all:hover { color: var(--portal-text); }
.option-columns { flex: 1 1 auto; min-height: 0; display: grid; grid-template-columns: 112px minmax(0, 1fr); }
.option-filters { overflow-y: auto; padding: 6px; border-right: 1px solid var(--portal-line-soft); }
.option-filters ul { margin: 0; padding: 0; list-style: none; }
.option-filters button {
  width: 100%;
  border: 0;
  border-radius: 6px;
  padding: 7px 8px;
  overflow: hidden;
  color: var(--portal-text-soft);
  background: transparent;
  font: inherit;
  text-align: left;
  text-overflow: ellipsis;
  white-space: nowrap;
  cursor: pointer;
}
.option-filters button.active, .option-filters button:hover { color: var(--portal-text); background: var(--portal-hover); }
.option-group { margin: 10px 8px 4px; color: var(--portal-muted); font-size: 11px; }
.option-grid-box { min-height: 160px; overflow-y: auto; padding: 10px; }
.option-grid { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 10px; margin: 0; padding: 0; list-style: none; }
.option-card {
  display: grid;
  gap: 6px;
  border: 1px solid transparent;
  border-radius: 10px;
  padding: 6px;
  background: var(--portal-panel);
  cursor: pointer;
}
/* 亮色主题下白底封面会「消失」：卡片加 1px 描边（dev/03 §9） */
:root[data-theme="light"] .option-card { border-color: var(--portal-line); }
.option-card.active { border-color: var(--portal-line-strong); background: var(--portal-hover); }
.option-card.selected { border-color: var(--portal-text-soft); }
.option-card.locked { cursor: not-allowed; }
.option-cover { position: relative; aspect-ratio: 3 / 4; border-radius: 8px; overflow: hidden; background: var(--portal-media-letterbox); }
.option-cover img, .option-preview { position: absolute; inset: 0; width: 100%; height: 100%; object-fit: cover; }
.option-card.blocked .option-cover img { opacity: .45; filter: grayscale(.6); }
.option-placeholder { position: absolute; inset: 0; display: grid; place-items: center; color: var(--portal-faint); font-size: 28px; }
.option-check { position: absolute; top: 6px; right: 6px; display: grid; place-items: center; width: 20px; height: 20px; border-radius: 50%; color: var(--portal-mark-text); background: var(--portal-mark-bg); }
.option-reason {
  position: absolute;
  left: 6px;
  right: 6px;
  bottom: 6px;
  display: none;
  border-radius: 6px;
  padding: 4px 6px;
  color: var(--portal-on-media);
  background: var(--portal-on-media-bg);
  font-size: 11px;
  line-height: 15px;
}
.option-card.blocked:hover .option-reason, .option-card.blocked.active .option-reason { display: block; }
/* 用户自己能解决的（换模型 / 传图）用提示色；运营要解决的用中性灰 —— 只看分类，不看文案 */
.option-card.fixable .option-reason { box-shadow: inset 2px 0 0 var(--portal-gold); }
.option-foot { display: flex; align-items: center; gap: 4px; min-width: 0; }
.option-name { flex: 1; min-width: 0; overflow: hidden; color: var(--portal-text-soft); text-align: center; text-overflow: ellipsis; white-space: nowrap; }
.option-card.blocked .option-name { color: var(--portal-muted); }
.option-more { flex: none; display: grid; place-items: center; width: 22px; height: 22px; border: 0; border-radius: 6px; color: var(--portal-muted); background: transparent; cursor: pointer; }
.option-more:hover { color: var(--portal-text); background: var(--portal-hover); }
.option-empty, .option-tail { margin: 0; padding: 12px 4px; color: var(--portal-muted); text-align: center; }
.option-empty.error, .option-tail.error { color: var(--portal-danger); }
.option-retry { margin-left: 6px; border: 1px solid var(--portal-line); border-radius: 6px; padding: 2px 8px; color: var(--portal-text-soft); background: transparent; font: inherit; cursor: pointer; }
/* 窄屏：内嵌在全屏面板里，左栏收成顶部横排、网格两列 */
@media (max-width: 639px) {
  .option-columns { grid-template-columns: minmax(0, 1fr); grid-template-rows: auto minmax(0, 1fr); }
  .option-filters { display: flex; gap: 4px; overflow-x: auto; border-right: 0; border-bottom: 1px solid var(--portal-line-soft); }
  .option-filters ul { display: flex; gap: 4px; }
  .option-filters button { width: auto; }
  .option-group { margin: 0 4px; align-self: center; white-space: nowrap; }
  .option-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); }
}
</style>
