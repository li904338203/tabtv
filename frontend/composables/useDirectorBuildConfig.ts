/**
 * AI 搭景的可用性 `GET /director/build-config`（DRT-20，DR-F-108）：能不能用、能带几张参考图、本小时用了几次。
 *
 * 节点上的提示词面板与编辑器里的 AI 条**共用一份、同时只请求一次**（单飞，同 useMarketingConfig）：
 * 各请求一次的话两边对「能不能用」可能得出不同结论（一个成功一个失败）。
 * - 画布页加载时取一次（`load`）；编辑器打开时、每次搭景返回后再刷新（`refresh`）—— `usedThisHour` 会变；
 * - 取失败**不是定论**：面板照常显示、只说「暂时读不到」，不当成「不可用」把按钮永久置灰。
 * 只读、不占额度：「能不能用」与搭景时是同一个服务端判断（`SceneBuildChoice`）。
 */
import type { components } from "@tabtv/api-client";

export type DirectorBuildConfig = components["schemas"]["DirectorBuildConfig"];
export type DirectorBuildConfigState = "idle" | "loading" | "ready" | "error";

let inflight: Promise<void> | null = null;

export function useDirectorBuildConfig() {
  const config = useState<DirectorBuildConfig | null>("director-build-config", () => null);
  const state = useState<DirectorBuildConfigState>("director-build-config-state", () => "idle");
  const error = useState<string>("director-build-config-error", () => "");

  async function fetchOnce() {
    state.value = "loading";
    error.value = "";
    try {
      config.value = await useApi()<DirectorBuildConfig>("/director/build-config");
      state.value = "ready";
    } catch (cause) {
      state.value = "error";
      const message = (cause as { message?: unknown })?.message;
      error.value = typeof message === "string" && message && message !== "请求失败"
        ? message
        : "暂时读不到 AI 搭景的可用性，请检查网络后重试";
    }
  }

  function start(): Promise<void> {
    if (inflight) return inflight;
    inflight = fetchOnce().finally(() => { inflight = null; });
    return inflight;
  }

  /** 首次加载：已有结论时不再请求。 */
  function load(): Promise<void> {
    if (!import.meta.client) return Promise.resolve();
    if (state.value === "ready" || state.value === "loading") return inflight ?? Promise.resolve();
    return start();
  }

  /** 刷新（编辑器打开、搭景返回后）：正在取的话复用那一次。 */
  function refresh(): Promise<void> {
    if (!import.meta.client) return Promise.resolve();
    return start();
  }

  return { config, state, error, load, refresh };
}
