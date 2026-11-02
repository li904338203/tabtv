/**
 * 设置页调用管理端点时的两件小事（TT-T-19~21）。
 *
 * - 错误：`useApi` 抛的是 Nuxt 的 `createError`，业务码与 `details` 在 `.data` 里。
 *   版本冲突要靠 `code` 进冲突态（给「重新加载」出路），字段级问题要靠 `details.issues` 列出来。
 * - 平台素材（封面、角色图）：经 `/admin/media/{id}` 读取。桌面版后端给每个请求补上本机身份，
 *   所以 `<img src>` 直接可用（来源系统那边要 fetch 成 blob，因为浏览器不带 Authorization 头）。
 */
export interface SettingsApiError {
  message: string;
  code: number;
  status: number;
  details: Record<string, unknown> | undefined;
}

export function settingsApiError(e: unknown): SettingsApiError {
  const err = (e ?? {}) as { message?: unknown; statusCode?: unknown; data?: { code?: unknown; details?: unknown } };
  const details = err.data?.details;
  return {
    message: typeof err.message === "string" && err.message ? err.message : "请求失败",
    code: typeof err.data?.code === "number" ? err.data.code : 0,
    status: typeof err.statusCode === "number" ? err.statusCode : 0,
    details: details && typeof details === "object" ? (details as Record<string, unknown>) : undefined
  };
}

/** 平台素材的读取地址；空 ID 返回空串。 */
export function settingsMediaUrl(id: string | null | undefined, apiBase = "/api/v1"): string {
  return id && id !== "0" ? `${apiBase}/admin/media/${encodeURIComponent(id)}` : "";
}
