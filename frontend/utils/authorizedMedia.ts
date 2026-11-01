/**
 * 受保护媒体的取回方式 —— 抽成纯函数以便单测（AC-22 / FE-06、GAP-18）。
 *
 * ⚠️ 产出地址形如 `/api/v1/generations/{id}/outputs/0`，端点按归属鉴权，
 *    而 **`<img src>` 与 `<a href>` 都不会带 Authorization 头** —— 直接引用一律 401。
 *    老系统那条路只有把整个桶开成公开才走得通，而那等于取消归属校验。
 *    所以必须 fetch 成 blob 再转 object URL。
 */

export type MediaRequest = {
  url: string;
  headers: Record<string, string> | undefined;
};

/** 列表卡片用的缩略图宽度。原图单张动辄几 MB，一屏十几张会把标签页撑爆。 */
export const LIST_THUMB_WIDTH = 320;

/**
 * 项目卡片的封面地址（相对 API 前缀），没有封面时返回 undefined（显示占位）。
 *
 * ⚠️ 读的是 **`displayCoverMediaId`**（服务端算好的「显式封面 ?? 最近一次产出」；视频与成片给的是
 *    Worker 截好的封面帧，所以这里永远是一张图，CD-83），不是 `coverMediaId`（那只是用户手动设的，绝大多数项目是空的）。
 * ⚠️ 它是**媒体 ID 不是 URL**：此前三个页面都把 ID 直接当 URL 传给取图函数，
 *    拼出 `/api/v1/96467992686100480` → 404。所以就算有封面也显示不出来，
 *    而那只是一张占位图，不报错。
 */
export function projectCoverUrl(
  project: { displayCoverMediaId?: string | null } | null | undefined,
  width: number = LIST_THUMB_WIDTH
): string | undefined {
  return mediaThumbUrl(project?.displayCoverMediaId, width);
}

/**
 * 媒体 ID → 列表缩略图地址（相对 API 前缀）。没有 ID 时返回 undefined（显示占位）。
 *
 * ⚠️ 它是**媒体 ID 不是 URL** —— 直接当 URL 传给取图函数会拼出 `/api/v1/<ID>` → 404。
 */
export function mediaThumbUrl(
  mediaId: string | null | undefined,
  width: number = LIST_THUMB_WIDTH
): string | undefined {
  if (!mediaId) return undefined;
  return `/media/${mediaId}?w=${width}`;
}

/** 相对地址补上 API 前缀；已经是绝对地址或已带 `/api/` 前缀的原样返回。 */
export function absoluteMediaUrl(url: string, apiBase: string): string {
  if (/^https?:\/\//.test(url) || url.startsWith("/api/")) return url;
  return `${apiBase}${url.startsWith("/") ? url : `/${url}`}`;
}

/**
 * 取媒体的请求参数。
 *
 * ⚠️ **有 token 就必须带上**：漏了的表现是 401，而界面上只是一张裂图 ——
 *    看起来像"这张图没了"，不像"没带鉴权头"。
 * ⚠️ 没有 token 时**不发空的 Authorization 头**（`Bearer undefined` 会被服务端
 *    当成一个坏 token 拒掉，而不是当成匿名请求）。
 *
 * ⚠️ **`X-Workspace-Id` 同样必须带**：新后端按工作区分租户，`/api/v1/media/{id}`
 *    这条受保护端点在中间件里就要这个头 —— 漏了回的是
 *    **400「缺少 X-Workspace-Id 请求头」**，不是 401。
 *    老系统没有工作区概念，所以移植过来的**每一处**取图都漏了它，
 *    而表现只是"图片节点生成成功却不显示"。
 *
 * ⚠️ 这个判定**只许有这一处**：应用里有六处在取受保护媒体
 *    （生成产出、组子项预览、脚本资产、参考图 token、素材库、作品墙），
 *    各写一份的话漏掉哪一处都**不会有编译错误**，而漏掉的那一处就是一片裂图。
 */
export function authorizedMediaRequest(
  url: string,
  token: string | undefined | null,
  apiBase: string,
  workspaceId?: string | null
): MediaRequest {
  const headers: Record<string, string> = {};
  if (token) headers.Authorization = `Bearer ${token}`;
  if (workspaceId) headers["X-Workspace-Id"] = workspaceId;
  return {
    url: absoluteMediaUrl(url, apiBase),
    headers: Object.keys(headers).length ? headers : undefined
  };
}
