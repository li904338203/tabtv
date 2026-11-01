<script setup lang="ts">
/**
 * 营销页底部的模板墙（MKT-T-15，营销 dev/03 §5；MKT-F-30~35、MK-16）。
 *
 * - **模板墙 = 创意库**：与选项弹层同一个接口 `GET /marketing/options?kind=creative`（MKD-7），
 *   不另开接口 —— 两条接口迟早对「什么算在投」给出两个答案，而用户会在两处看到不一样的东西。
 * - 页签：「全部」+ `config.categories.creative`（服务端已按 sort）+ 分隔 + `config.creativeModelFilters`（按模型）。
 *   **服务端筛选**（MKT-F-34），切页签从头取；不在已取的 24 条里筛（那只能说明不在这 24 条里）。
 *   查询参数与弹层共用 `utils/marketingOptions.optionListQuery`。
 * - 分页：`useInfiniteList`（limit=24，游标）；翻页时已显示的卡片保持可见。
 * - 失败只在这一块报错并给重试，不影响上面的输入框。
 * - 点卡片：只**填入**输入框（`useMarketingFill`，MKT-T-14 消费）→ 平滑滚到输入框，由输入框聚焦正文。
 *   **不发送、不建会话、不扣费**（MK-16、MKT-AC-12）；未登录同样只填入（发送时才要求登录）。
 * - 网格断点与悬停播放走 `utils/mediaGrid`（与精选作品墙同一组）。
 */
import type { MarketingConfig } from "~/composables/useMarketingConfig";
import {
  ALL_FILTER,
  OPTION_PAGE_SIZE,
  optionListQuery,
  sameFilter,
  type MarketingOption,
  type OptionFilter
} from "~/utils/marketingOptions";
import { canHoverPlay } from "~/utils/mediaGrid";

const props = defineProps<{ config: MarketingConfig }>();

const apiBase = useRuntimeConfig().public.apiBase as string;
const fill = useMarketingFill();

type Tab = { key: string; label: string; filter: OptionFilter };

const categoryTabs = computed<Tab[]>(() => (props.config.categories?.creative ?? []).map(item => ({
  key: `c:${item.code}`,
  label: item.name,
  filter: { type: "category", code: item.code }
})));
const modelTabs = computed<Tab[]>(() => (props.config.creativeModelFilters ?? []).map(item => ({
  key: `m:${item.code}`,
  label: item.name,
  filter: { type: "model", code: item.code }
})));

const filter = ref<OptionFilter>(ALL_FILTER);
const error = ref("");
const hoverPlay = ref(false);

const list = useInfiniteList<MarketingOption>({
  pageSize: OPTION_PAGE_SIZE,
  fetchPage: async (_page, cursor) => {
    error.value = "";
    try {
      return await useApi()<{ items: MarketingOption[]; nextCursor?: string | null }>("/marketing/options", {
        query: optionListQuery({ kind: "creative", filter: filter.value, cursor, limit: OPTION_PAGE_SIZE })
      });
    } catch (cause) {
      error.value = wallErrorText(cause, import.meta.client ? navigator.onLine : true);
      return null;
    }
  }
});
const { items, loading, finished } = list;

/** ⚠️ 断网要说成断网：说「加载失败」的话用户会反复点重试，而该做的是看自己的网络。 */
function wallErrorText(cause: unknown, online: boolean): string {
  if (!online) return "网络已断开，恢复后点重试";
  const message = typeof cause === "object" && cause !== null && "message" in cause
    ? String((cause as { message?: unknown }).message ?? "")
    : "";
  return message ? `模板加载失败：${message}` : "模板加载失败";
}

/**
 * 切换页签：从头按新条件重新取。
 * ⚠️ 上一页还在路上时 `reset()` 会被 loading 挡掉 —— 等它回来再重置一次，
 *    否则列表停在旧页签的结果上，而高亮已经在新页签（同 ShowcaseGrid）。
 */
let resetPending = false;
function select(next: OptionFilter) {
  if (sameFilter(filter.value, next)) return;
  filter.value = next;
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

/** 选中的分类 / 模型从配置里消失（停用、模型下线）时退回「全部」，不留一个高亮着却不存在的页签。 */
watch([categoryTabs, modelTabs], () => {
  if (filter.value.type === "all") return;
  const exists = [...categoryTabs.value, ...modelTabs.value].some(tab => sameFilter(tab.filter, filter.value));
  if (!exists) select(ALL_FILTER);
});

/** 重试：从上一次成功的游标接着取，不清掉已经显示出来的卡片。 */
function retry() {
  error.value = "";
  finished.value = false;
  void list.loadNext();
}

function pick(option: MarketingOption) {
  if (!fill.fillCreative(option)) return;
  fill.scrollToComposer();
}

const emptyText = computed(() => (filter.value.type === "all" ? "还没有模板" : "这个分类下还没有模板"));

onMounted(() => {
  hoverPlay.value = canHoverPlay(typeof window.matchMedia === "function" ? window.matchMedia.bind(window) : undefined);
  void list.loadNext();
});
onBeforeUnmount(() => fill.clear());
</script>

<template>
  <div class="tpl-wall">
    <div class="tpl-tabs" role="tablist" aria-label="模板分类">
      <button
        type="button"
        role="tab"
        class="tpl-tab"
        :class="{ active: filter.type === 'all' }"
        :aria-selected="filter.type === 'all'"
        @click="select(ALL_FILTER)"
      >全部</button>
      <button
        v-for="tab in categoryTabs"
        :key="tab.key"
        type="button"
        role="tab"
        class="tpl-tab"
        :class="{ active: sameFilter(filter, tab.filter) }"
        :aria-selected="sameFilter(filter, tab.filter)"
        @click="select(tab.filter)"
      >{{ tab.label }}</button>
      <span v-if="modelTabs.length" class="tpl-tab-divider" aria-hidden="true" />
      <button
        v-for="tab in modelTabs"
        :key="tab.key"
        type="button"
        role="tab"
        class="tpl-tab"
        :class="{ active: sameFilter(filter, tab.filter) }"
        :aria-selected="sameFilter(filter, tab.filter)"
        :title="`推荐使用 ${tab.label} 的模板`"
        @click="select(tab.filter)"
      >{{ tab.label }}</button>
    </div>

    <section class="tpl-grid" aria-label="模板列表">
      <MarketingTemplateCard
        v-for="item in items"
        :key="item.code"
        :option="item"
        :api-base="apiBase"
        :hover-play="hoverPlay"
        @pick="pick"
      />
      <div :ref="el => list.sentinel.value = (el as HTMLElement)" class="tpl-sentinel" />
    </section>

    <p v-if="error" class="tpl-end tpl-error" role="alert">
      {{ error }}
      <button type="button" class="portal-link-button" @click="retry">重试</button>
    </p>
    <p v-else-if="loading" class="tpl-end">正在加载</p>
    <p v-else-if="finished && items.length" class="tpl-end">没有更多了</p>
    <p v-else-if="!items.length" class="tpl-end">{{ emptyText }}</p>
  </div>
</template>

<style scoped>
.tpl-tabs { display: flex; flex-wrap: wrap; align-items: center; gap: 4px 2px; margin-bottom: 18px; }
.tpl-tab {
  min-height: 30px;
  border: 0;
  border-radius: 7px;
  padding: 0 10px;
  color: var(--portal-muted);
  background: transparent;
  font-size: 14px;
  white-space: nowrap;
  cursor: pointer;
}
.tpl-tab:hover { color: var(--portal-text); }
.tpl-tab.active { color: var(--portal-text); font-weight: 650; }
.tpl-tab:focus-visible { outline: 2px solid var(--portal-cyan); outline-offset: -2px; }
.tpl-tab-divider { width: 1px; height: 14px; margin: 0 8px; background: var(--portal-line-strong); }

/* 断点与 utils/mediaGrid.ts 的 MEDIA_GRID_BREAKPOINTS 同一组（mediaGrid.test.ts 核对源码，改一边另一边会红） */
.tpl-grid { display: grid; grid-template-columns: minmax(0, 1fr); gap: 16px; }
@media (min-width: 640px) { .tpl-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
@media (min-width: 1024px) { .tpl-grid { grid-template-columns: repeat(3, minmax(0, 1fr)); } }
@media (min-width: 1440px) { .tpl-grid { grid-template-columns: repeat(4, minmax(0, 1fr)); } }
.tpl-sentinel { grid-column: 1 / -1; height: 1px; }

.tpl-end { margin: 28px 0 0; color: var(--portal-faint); font-size: 12px; text-align: center; }
.tpl-error { color: var(--portal-danger); }

@media (max-width: 639px) {
  .tpl-tabs { flex-wrap: nowrap; overflow-x: auto; }
}
</style>
