import type { Ref } from "vue";

/**
 * 分页接口的统一形状。
 *
 * ⚠️ **新后端是游标分页**（`{items, nextCursor, hasMore}`），老后端是
 * 页码分页（`{list, total}` + `page`/`size`）—— 这不是字段改名，是两种
 * 翻页方式：游标分页没有"总数"，也没有"第几页"。
 *
 * ⚠️ 新后端选游标是有理由的：**订单/任务一直在新增，offset 翻页会漏掉或重复**
 * （CD-56 记过）。所以这里适配成游标，而不是让后端退回 offset。
 *
 * 两种形状都认：`normalize` 统一到一处，**12 个调用点一个都不用改** ——
 * 各自适配的话，"这个列表用哪种分页"会散成十二份，而漏改一处的表现是
 * 那一个列表只显示第一页，且**不报错**。
 */
export type PagedResponse<T> = {
  /** 新后端。⚠️ 最后一页时 `nextCursor` 是 **null**（不是空串、也不是缺省）。 */
  items?: T[]
  nextCursor?: string | null
  hasMore?: boolean
  /** 老后端（保留以兼容尚未重接的端点） */
  list?: T[]
  total?: number
};

/** 把两种形状归一。游标为空串即"没有下一页"。 */
function normalize<T>(r: PagedResponse<T>): { rows: T[]; cursor: string; more: boolean } {
  const rows = r.items ?? r.list ?? [];
  const cursor = r.nextCursor ?? "";
  // ⚠️ hasMore 缺省时**不能默认 true**：那会让列表在到底之后继续空转请求。
  // 有游标就还有下一页；没有游标则看老后端的 total。
  const more = r.hasMore ?? (cursor !== "" ? true : false);
  return { rows, cursor, more };
}

/**
 * 下拉分页（滚动加载）。
 *
 * <p>用 IntersectionObserver 盯住列表末尾的哨兵元素，进入视口就取下一页 ——
 * 比监听 scroll 事件好在：不用自己算滚动位置，也不会在页面缩放或容器变化时算错。
 *
 * <p>只负责翻页与累加，不碰渲染。封面之类的副作用由调用方在 onPage 里做，
 * 因为不同列表要加载的东西不一样。
 */
export function useInfiniteList<T>(options: {
  pageSize: number;
  /**
   * 取某一页；返回 null 视为失败，会停在当前页而不是无限重试。
   *
   * ⚠️ 两个参数都给：新后端用 `cursor`（第一页是空串），
   * 尚未重接的老端点仍用 `page`。调用方用哪个都行，**不用改签名**。
   */
  fetchPage: (page: number, cursor: string) => Promise<PagedResponse<T> | null>;
  /** 每页到手后的副作用，比如预加载封面 */
  onPage?: (items: T[], page: number) => Promise<void> | void;
}) {
  const items = ref([]) as Ref<T[]>;
  const total = ref(0);
  const page = ref(0);
  const cursor = ref("");
  const loading = ref(false);
  const finished = ref(false);
  const sentinel = ref<HTMLElement>();
  let observer: IntersectionObserver | undefined;

  async function loadNext() {
    // finished 之后不再请求：否则用户停在底部时会持续空转
    if (loading.value || finished.value) return;
    loading.value = true;
    const next = page.value + 1;
    try {
      const response = await options.fetchPage(next, cursor.value);
      if (!response) {
        finished.value = true;
        return;
      }
      const { rows, cursor: nextCursor, more } = normalize(response);
      items.value = next === 1 ? rows : [...items.value, ...rows];
      // 游标分页没有总数：用累计条数兜底，界面上的"共 N 条"因此是
      // **已加载条数**而不是总数 —— 这是游标分页的固有取舍，不是 bug。
      total.value = response.total ?? items.value.length;
      page.value = next;
      cursor.value = nextCursor;
      // 终止条件按分页方式分开判：
      //  - 游标分页：服务端说没有更多（hasMore=false 或游标为空）；
      //  - 页码分页：返回不足一页，或累计数已达 total（最后一页正好满页的情况）。
      const cursorPaged = response.items !== undefined || response.nextCursor !== undefined;
      if (cursorPaged) {
        if (!more || nextCursor === "") finished.value = true;
      } else if (rows.length < options.pageSize || items.value.length >= total.value) {
        finished.value = true;
      }
      await options.onPage?.(rows, next);
    } finally {
      loading.value = false;
    }
    // 一页填不满视口时要接着取，否则哨兵一直可见却不会再触发 observer
    await nextTick();
    if (!finished.value && sentinel.value && isVisible(sentinel.value)) await loadNext();
  }

  function isVisible(element: HTMLElement) {
    const rect = element.getBoundingClientRect();
    return rect.top < window.innerHeight && rect.bottom > 0;
  }

  /** 换筛选条件时从头再来。 */
  async function reset() {
    items.value = [];
    total.value = 0;
    page.value = 0;
    cursor.value = "";
    finished.value = false;
    await loadNext();
  }

  onMounted(() => {
    observer = new IntersectionObserver((entries) => {
      if (entries.some(entry => entry.isIntersecting)) void loadNext();
      // rootMargin 提前 300px 触发：等哨兵真正进入视口再请求，用户会看到明显的空白等待
    }, { rootMargin: "300px" });
    if (sentinel.value) observer.observe(sentinel.value);
  });

  // 哨兵是 v-if 渲染的，挂载时可能还不存在，出现后要补上观察
  watch(sentinel, (element) => {
    if (element && observer) observer.observe(element);
  });

  onBeforeUnmount(() => observer?.disconnect());

  return { items, total, page, loading, finished, sentinel, loadNext, reset };
}
