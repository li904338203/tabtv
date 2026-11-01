/**
 * 精选作品（公开墙）的纯函数 —— 首页、AI 视频首页、创造页共用的 ShowcaseGrid 取数与渲染判定
 * （AGT-40 / AGT-41，设计 02 §6）。
 *
 * ⚠️ 契约以生成类型 `ShowcaseItem` 为准：`{id,title,hasMedia,type,createdAt}`。
 *    此前两处页面都读老契约的 `type/url/authorName/cover`，而新后端从来不发 `url` ——
 *    卡片**永远**落到占位块，且不报错。
 * ⚠️ 取图只走公开端点 `/showcase/{id}/media`：受保护端点按工作区卡归属，访客一律 401。
 * ⚠️ **不做客户端筛选**：按类型筛选是服务端的 `type` 参数。在已取的 24 条里筛，
 *    「点音频筛不出东西」只代表不在这 24 条里，而不是没有音频作品。
 */
import type { components } from "@tabtv/api-client";
import { absoluteMediaUrl } from "./authorizedMedia";

export type ShowcaseItem = components["schemas"]["ShowcaseItem"];
/** 服务端可筛的类型；空串 = 全部。 */
export type ShowcaseFilter = "" | "video" | "image" | "audio";
/** 卡片怎么画：`none` = 占位块（无媒体 / 认不出的类型）。 */
export type ShowcaseCardKind = "image" | "video" | "audio" | "none";

/** 分类按钮（AGT-41）。顺序即展示顺序；值原样作为 `type` 参数发给服务端。 */
export const SHOWCASE_FILTERS: ReadonlyArray<{ value: ShowcaseFilter; label: string }> = [
  { value: "", label: "全部" },
  { value: "video", label: "视频" },
  { value: "image", label: "图片" },
  { value: "audio", label: "音频" }
];

/**
 * 公开媒体地址（带 API 前缀）。`width` 只对位图有意义（服务端只认 160/320/640），
 * 视频 / 音频不传 —— 传了也是原样返回整个文件，徒增一个缓存键。
 */
export function showcaseMediaUrl(id: string, apiBase: string, width?: number): string {
  const path = `/showcase/${encodeURIComponent(id)}/media`;
  return absoluteMediaUrl(width ? `${path}?w=${width}` : path, apiBase);
}

/**
 * 公开作品的封面图地址（2026-11-01，`/showcase/{id}/cover`）：视频是第一帧（十几 KB），图片是缩略图，
 * 其余 / 封面帧还没截好的视频 404。作品墙的视频卡用它当封面、悬停才取视频本身 —— 此前用 `<video preload=metadata>`
 * 取封面，公开媒体端点不支持 Range，每张卡都把整段视频（10~20MB）下完，占满浏览器连接、页面接口跟着排队好几秒。
 */
export function showcaseCoverUrl(id: string, apiBase: string, width = 320): string {
  return absoluteMediaUrl(`/showcase/${encodeURIComponent(id)}/cover?w=${width}`, apiBase);
}

/**
 * 卡片按什么渲染。
 *
 * - 没有媒体 → 占位；
 * - `type` 缺失（老后端 / 字段未下发）→ 按图片渲染，`<img>` 出错时由组件回落占位；
 * - `other` 或认不出的值 → 占位（不是裂图）。
 */
export function showcaseCardKind(item: Pick<ShowcaseItem, "hasMedia"> & { type?: string | null }): ShowcaseCardKind {
  if (!item.hasMedia) return "none";
  switch (item.type) {
    case undefined:
    case null:
    case "":
      return "image";
    case "image":
    case "video":
    case "audio":
      return item.type;
    default:
      return "none";
  }
}

/** `/showcase` 的查询串：不带空 `cursor`、不带空 `type`。 */
export function buildShowcaseQuery(options: { limit: number; cursor?: string; type?: ShowcaseFilter }): string {
  const params = new URLSearchParams({ limit: String(options.limit) });
  if (options.cursor) params.set("cursor", options.cursor);
  if (options.type) params.set("type", options.type);
  return params.toString();
}

/**
 * 精选作品加载失败时这一块的文案。
 *
 * ⚠️ 断网要说成断网：说「加载失败」的话用户会反复点重试，而该做的是看自己的网络。
 */
export function showcaseErrorText(cause: unknown, online: boolean): string {
  if (!online) return "网络已断开，恢复后点重试";
  const message = typeof cause === "object" && cause !== null && "message" in cause
    ? String((cause as { message?: unknown }).message ?? "")
    : "";
  return message ? `精选作品加载失败：${message}` : "精选作品加载失败";
}
