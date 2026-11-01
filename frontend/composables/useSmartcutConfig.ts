/**
 * 智能剪辑的可用性与上限 `GET /smartcut/config`（SCT-23 / SCT-27）：AI 剪辑能不能用、本小时用了几次、并发、各种上限、
 * 剪辑台各操作可选的模型。
 *
 * 节点上的指令面板与剪辑台**共用一份、同时只请求一次**（单飞，同 useDirectorBuildConfig）：
 * - 画布页加载时取一次（`load`）；剪辑台打开时、每次 AI 剪辑返回后再刷新（`refresh`）—— `usedThisHour` 会变；
 * - 取失败**不是定论**：面板只说「暂时读不到」，不当成「不可用」把按钮永久置灰。
 * - 被限流（42901 / 42902）时记下「多少秒后可以再试」（`markLimited`）：面板与剪辑台的按钮都据此置灰并说出时间
 *   （SC-AC-35）。配置里只有本小时的次数，没有「几点可以再试」，这个数只有 429 的响应里有。
 */
import type { components } from "@tabtv/api-client";

export type SmartcutConfigValue = components["schemas"]["SmartcutConfig"];
export type SmartcutConfigState = "idle" | "loading" | "ready" | "error";

let inflight: Promise<void> | null = null;

export function useSmartcutConfig() {
  const config = useState<SmartcutConfigValue | null>("smartcut-config", () => null);
  const state = useState<SmartcutConfigState>("smartcut-config-state", () => "idle");
  const error = useState<string>("smartcut-config-error", () => "");
  /** 被限流时最早可以再试的时刻（毫秒时间戳）；0 = 没有被限流。 */
  const retryAt = useState<number>("smartcut-plan-retry-at", () => 0);

  async function fetchOnce() {
    state.value = "loading";
    error.value = "";
    try {
      config.value = await useApi()<SmartcutConfigValue>("/smartcut/config");
      state.value = "ready";
    } catch (cause) {
      state.value = "error";
      const message = (cause as { message?: unknown })?.message;
      error.value = typeof message === "string" && message && message !== "请求失败"
        ? message
        : "暂时读不到 AI 剪辑的可用性，请检查网络后重试";
    }
  }

  function start(): Promise<void> {
    if (inflight) return inflight;
    inflight = fetchOnce().finally(() => { inflight = null; });
    return inflight;
  }

  function load(): Promise<void> {
    if (!import.meta.client) return Promise.resolve();
    if (state.value === "ready" || state.value === "loading") return inflight ?? Promise.resolve();
    return start();
  }

  function refresh(): Promise<void> {
    if (!import.meta.client) return Promise.resolve();
    return start();
  }

  /** 记下被限流：`seconds` 来自 429 的 `retry_after_seconds`。 */
  function markLimited(seconds: number) {
    if (Number.isFinite(seconds) && seconds > 0) retryAt.value = Date.now() + Math.ceil(seconds) * 1000;
  }

  /** 还要等多少秒（0 = 不用等）。调用方传 `now`（每秒刷新的时钟），倒计时才会跟着变。 */
  function retryAfterSeconds(now: number = Date.now()): number {
    return retryAt.value > now ? Math.ceil((retryAt.value - now) / 1000) : 0;
  }

  return { config, state, error, retryAt, load, refresh, markLimited, retryAfterSeconds };
}
