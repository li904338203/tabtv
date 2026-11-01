/**
 * 营销页上「模板墙 → 输入框」的填入通道（MKT-T-15，营销 dev/03 §5；MK-16）。
 *
 * 模板墙（`components/marketing/TemplateWall.vue`）与营销输入框（MKT-T-14）是页面上的兄弟组件，
 * 不互相持有实例 —— 页面级共享一个「待填入」请求：
 *
 * - 墙：`fillCreative(option)` 发出请求，再 `scrollToComposer()` 把输入框平滑滚进视口；
 * - 输入框：在 setup 里调一次 `onFill(handler)`，handler 拿到选项后自己做
 *   `applyCreative(state, option, defaultSkill)`（`utils/marketingComposer`）并**聚焦正文**。
 *   `onFill` 用 `immediate` 监听：输入框晚于点击挂载（配置还在加载）时，挂载那一刻补上那次填入。
 *
 * ⚠️ 只填入：这里没有任何发送 / 建会话 / 报价的入口（MK-16、MKT-AC-12）。
 * ⚠️ 请求被消费后立即清空：不清的话离开营销页再回来，输入框一挂载就把上一次点过的创意又填一遍。
 *    墙卸载时也清（没人消费的请求不该留到下一次进页）。
 */
import { isFillableOption, nextFillRequest, type MarketingFillRequest } from "~/utils/marketingFill";
import type { MarketingOption } from "~/utils/marketingOptions";

/** 输入框挂载位（`pages/marketing.vue`，MKT-T-10 留的）。滚动目标。 */
export const MARKETING_COMPOSER_SELECTOR = "[data-mount=\"marketing-composer\"]";

export function useMarketingFill() {
  const request = useState<MarketingFillRequest | null>("marketing-fill-request", () => null);
  const lastSeq = useState<number>("marketing-fill-seq", () => 0);

  /** 发出填入请求。非创意直接忽略（模板墙 = 创意库）。 */
  function fillCreative(option: MarketingOption): boolean {
    if (!isFillableOption(option)) return false;
    const next = nextFillRequest(request.value, option, lastSeq.value);
    lastSeq.value = next.seq;
    request.value = next;
    return true;
  }

  /**
   * 输入框注册消费者（MKT-T-14 在 setup 里调用一次）。返回取消函数；组件卸载时自动取消。
   * handler 里做 `applyCreative` + 聚焦正文；**不要**在这里发送。
   */
  function onFill(handler: (option: MarketingOption) => void): () => void {
    const stop = watch(request, (req) => {
      if (!req) return;
      request.value = null;
      handler(req.option);
    }, { immediate: true });
    if (getCurrentScope()) onScopeDispose(stop);
    return stop;
  }

  /** 丢弃没人消费的请求（墙卸载时调用）。 */
  function clear() {
    request.value = null;
  }

  /** 把输入框平滑滚进视口（减少动态效果时直接跳）。聚焦由输入框在 handler 里做（只有它知道正文编辑器在哪）。 */
  function scrollToComposer() {
    if (!import.meta.client) return;
    const target = document.querySelector<HTMLElement>(MARKETING_COMPOSER_SELECTOR);
    if (!target) return;
    const reduced = window.matchMedia?.("(prefers-reduced-motion: reduce)").matches ?? false;
    target.scrollIntoView({ behavior: reduced ? "auto" : "smooth", block: "center" });
  }

  return { request, fillCreative, onFill, clear, scrollToComposer };
}
