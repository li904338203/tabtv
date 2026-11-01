/**
 * 页面左上角「返回」去哪（会员中心、通知、账号、我的作品、任务中心、积分中心等二级页）。
 *
 * - 是从站内别的页面点进来的：**退回上一页** —— 保留来源页的筛选、翻页与滚动位置，
 *   直接跳一个固定地址的话用户刚翻到第 3 页的列表又回到了第 1 页。
 * - 直接打开 / 刷新 / 新标签页：没有站内上一页，`history.back()` 会退出本站或什么都不做 ——
 *   去页面自己的上级（`fallback`）。
 * - 上一页是登录页（未登录打开、登录后被带回来）：退回去只会在登录页上再跳一次，同样去上级。
 *
 * `historyBack` 是 vue-router 写在 `history.state.back` 里的上一个站内地址（没有时为 null）。
 */
export type BackTarget = { mode: "history" } | { mode: "push"; to: string };

export function backTarget(historyBack: unknown, fallback: string): BackTarget {
  if (typeof historyBack === "string" && historyBack.startsWith("/") && !historyBack.startsWith("/login")) {
    return { mode: "history" };
  }
  return { mode: "push", to: fallback };
}
