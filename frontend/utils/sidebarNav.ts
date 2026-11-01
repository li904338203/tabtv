/**
 * 侧栏「创造」入口的纯函数（AGT-40，设计 02 §1）。
 *
 * ⚠️ 「创造」**不是产品模块**，不进 `stores/products.ts` 的产品清单：它是跨产品的
 *    创作入口，发送后落到 AI 视频的画布项目里。进了产品清单的话「产品开关」就能把它
 *    关掉，而关掉之后 AI 视频本身还在 —— 语义不通。所以它在 `AppSidebar.vue` 的
 *    产品循环之前**写死**，判定放在这里以便单测（用户端 Vitest 没有 DOM 环境）。
 */

/** 「创造」入口的路由。 */
export const CREATE_PATH = "/create";

/**
 * NEW 角标显示到这一刻为止（不含）：上线日 2026-10-15 + 30 天。
 *
 * ⚠️ 前端常量，**不进产品配置** —— 它只是一个上线提示，不值得一张配置表与一个后台入口。
 */
export const CREATE_BADGE_UNTIL = new Date("2026-11-14T00:00:00+08:00");

/**
 * 当前路由是否落在「创造」下。
 *
 * ⚠️ 按路径段判，不按字符串前缀：`/created` 不是 `/create` 的子页。
 */
export function isCreateActive(path: string): boolean {
  return path === CREATE_PATH || path.startsWith(`${CREATE_PATH}/`);
}

/** 是否显示 NEW 角标；`now` 由调用方传入（不在函数里取当前时间，便于测边界）。 */
export function showCreateBadge(now: Date, until: Date = CREATE_BADGE_UNTIL): boolean {
  return now.getTime() < until.getTime();
}

/**
 * 侧栏「营销 Agent」入口（MKT-T-10，营销 dev/03 §1、MKT-F-01）。
 *
 * ⚠️ 与「创造」同理**不进产品清单**：营销是跨产品入口，发送后落到 AI 视频的画布项目里；
 *    进了 `stores/products.ts` 就能被「产品开关」关掉，而 AI 视频本身还在。
 *    不加 NEW 角标（是否要角标未定；要加时照 `CREATE_BADGE_UNTIL` 再加一个）。
 */
export const MARKETING_PATH = "/marketing";

/** 当前路由是否落在「营销 Agent」下；按路径段判，`/marketingx` 不算。 */
export function isMarketingActive(path: string): boolean {
  return path === MARKETING_PATH || path.startsWith(`${MARKETING_PATH}/`);
}

/**
 * `/marketing/config` 的加载状态（侧栏与营销页共用，`composables/useMarketingConfig.ts`）。
 *
 * - `disabled`：接口 404 = 营销总开关关着（`SEEAITV_MARKETING_ENABLED=false`）→ 侧栏隐藏、页面说「暂未开放」；
 * - `ready`：拿到配置。⚠️ `available:false`（Agent 关着，MKT-T-04 B-5）**仍是 ready** ——
 *   页面照常可浏览、只禁用发送，所以侧栏入口也照常显示；
 * - `error`：断网 / 5xx 等**不是定论**，侧栏照常显示，只在营销页里报错并给重试。
 */
export type MarketingConfigState = "idle" | "loading" | "ready" | "disabled" | "error";

/** 加载失败时按状态码归类：只有明确的 404 才算「总开关关闭」。 */
export function marketingFailureState(statusCode: number | undefined): "disabled" | "error" {
  return statusCode === 404 ? "disabled" : "error";
}

/** 侧栏是否显示「营销 Agent」：只有确认总开关关闭时才隐藏（拿不准时不藏 —— 藏了用户就找不到重试的地方）。 */
export function showMarketingEntry(state: MarketingConfigState): boolean {
  return state !== "disabled";
}
