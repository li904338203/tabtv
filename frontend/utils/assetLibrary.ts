/**
 * 我的资产（画布左侧「资产」页签、创作资产页）的纯函数。接口见 openapi `/library/*`（迁移 00074）。
 */
import type { components } from "@tabtv/api-client";

export type LibraryItem = components["schemas"]["LibraryItem"];
export type LibraryFolder = components["schemas"]["LibraryFolder"];

export const LIBRARY_KIND_LABELS: Record<string, string> = { text: "文本", image: "图片", video: "视频", audio: "音频" };

/** 侧栏的树：顶层资产在前、文件夹在后（参考产品同款）；文件夹里的资产挂在各自文件夹下。 */
export function libraryTree(folders: LibraryFolder[], items: LibraryItem[]) {
  const byFolder = new Map<string, LibraryItem[]>();
  const root: LibraryItem[] = [];
  const known = new Set(folders.map(f => f.id));
  for (const item of items) {
    // 文件夹列表里没有的（刚被别人删掉）当顶层显示，不让它凭空消失
    if (item.folderId && known.has(item.folderId)) {
      const list = byFolder.get(item.folderId) ?? [];
      list.push(item);
      byFolder.set(item.folderId, list);
    } else {
      root.push(item);
    }
  }
  return { root, folders: folders.map(folder => ({ folder, items: byFolder.get(folder.id) ?? [] })) };
}

/** 筛选里点一个标签：选中 / 取消。 */
export function toggleTag(selected: readonly string[], tag: string): string[] {
  return selected.includes(tag) ? selected.filter(t => t !== tag) : [...selected, tag];
}

/** 本地时间「2026-10-30 04:13」。 */
export function libraryTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  const p = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

/** 缩略图的媒体 ID：图片取自己，视频取封面帧（没截出来时没有），文本与音频没有。 */
export function libraryThumbMediaId(item: Pick<LibraryItem, "kind" | "mediaId" | "posterMediaId">): string | undefined {
  if (item.kind === "image") return item.mediaId;
  if (item.kind === "video") return item.posterMediaId;
  return undefined;
}

/** 添加到画布时的节点描述（文本建手写文本节点，媒体建「已有产出」节点）。 */
export function libraryCanvasResource(item: LibraryItem): { type: "text" | "image" | "video" | "audio"; mediaId?: string; text?: string; title: string } | null {
  if (item.kind === "text") return { type: "text", text: item.text ?? "", title: item.title };
  if (!item.mediaId) return null;
  return { type: item.kind, mediaId: item.mediaId, title: item.title };
}
