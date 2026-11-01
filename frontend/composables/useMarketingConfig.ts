/**
 * 营销页配置 `GET /marketing/config`（MKT-T-10，营销 dev/03 §1）。
 *
 * 侧栏与营销页**共用一份状态、只请求一次**：侧栏要靠它决定入口显不显示，
 * 营销页要靠它拿槽位 / 上限 / 分类；各请求一次的话进营销页就是两个一样的请求，
 * 且两边对「开关关没关」可能得出不同结论（一个成功一个失败时）。
 *
 * 三种结局（判定在 `utils/sidebarNav.ts`，有单测）：
 * - 404 → `disabled`：营销总开关关着，侧栏隐藏、页面说「暂未开放」；
 * - 200 → `ready`：⚠️ `available:false`（Agent 关着，MKT-T-04 B-5）也是 ready ——
 *   页面照常浏览、只禁用发送；
 * - 其余 → `error`：不是定论，侧栏照常显示，营销页报错并给重试（`reload`）。
 *
 * ⚠️ 只在客户端请求：接口是公开的（不需要登录），但服务端渲染时带不上会话与工作区，
 *    且侧栏在服务端渲染出的「显示 / 隐藏」与客户端不一致会造成水合闪动。
 */
import type { components } from "@tabtv/api-client";
import { marketingFailureState, type MarketingConfigState } from "~/utils/sidebarNav";

export type MarketingConfig = components["schemas"]["MarketingConfig"];

/** 进行中的那一次请求（单飞）：侧栏与页面同时挂载时只发一个。 */
let inflight: Promise<void> | null = null;

export function useMarketingConfig() {
  const state = useState<MarketingConfigState>("marketing-config-state", () => "idle");
  const config = useState<MarketingConfig | null>("marketing-config", () => null);
  const error = useState<string>("marketing-config-error", () => "");

  async function fetchOnce() {
    state.value = "loading";
    error.value = "";
    try {
      config.value = await useApi()<MarketingConfig>("/marketing/config");
      state.value = "ready";
    } catch (cause) {
      const failure = cause as { statusCode?: number; message?: string };
      state.value = marketingFailureState(failure.statusCode);
      // 断网时 useApi 给不出状态码（落成 500），message 是 $fetch 的英文原文，不直接给用户看。
      error.value = failure.statusCode && failure.statusCode !== 500 && failure.message
        ? failure.message
        : "营销配置加载失败，请检查网络后重试";
    }
  }

  /**
   * 首次加载：只在 `idle` 时真正请求，正在加载时复用那一次。
   * ⚠️ 出错之后也不自动再取 —— 侧栏与页面各挂载一次就会变成两个请求；重试只走页面上的按钮（`reload`）。
   */
  function load(): Promise<void> {
    if (!import.meta.client) return Promise.resolve();
    if (inflight) return inflight;
    if (state.value !== "idle") return Promise.resolve();
    inflight = fetchOnce().finally(() => { inflight = null; });
    return inflight;
  }

  /** 页面上的「重试」：出错之后再取一次（已有结论时不重复）。 */
  function reload(): Promise<void> {
    if (state.value === "error") state.value = "idle";
    return load();
  }

  return { state, config, error, load, reload };
}
