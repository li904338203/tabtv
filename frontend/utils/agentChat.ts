import type { components } from "@tabtv/api-client";
import { MEMORY_DOC_PATH } from "./agentMessages";

/**
 * 全屏对话页与「画布」开关（AGT-44，设计 02 §12）的纯函数：路由拼接、查询参数解析、开关记忆、对话文件列表。
 *
 * 对话页与画布页是**同一项目、同一会话**的两种视图（Agent 写画布在服务端完成，D-2）：
 * 两边互相切换只换路由，不搬任何数据。
 */

export type ProjectDocumentSummary = components["schemas"]["ProjectDocumentSummary"];
export type ProjectDocument = components["schemas"]["ProjectDocument"];

type StorageLike = Pick<Storage, "getItem" | "setItem">;

// ---------------------------------------------------------------- 路由

/** 会话 ID 只收十进制（雪花号）；别的一律当没给 —— 与画布页读 `?session=` 的规则一致。 */
export function parseSessionParam(raw: unknown): string | undefined {
  const value = Array.isArray(raw) ? raw[0] : raw;
  return typeof value === "string" && /^\d{1,20}$/.test(value) ? value : undefined;
}

/** 画布节点 ID 的安全字符集（只用于定位，不进任何请求）。 */
const NODE_ID_PATTERN = /^[A-Za-z0-9_-]{1,64}$/;

/**
 * `?node=` 解析：逗号分隔、去重、只收安全字符、最多 20 个。
 * 不合法的项直接丢掉（地址栏是用户可改的，定位不到就是不定位，不报错）。
 */
export function parseNodeParam(raw: unknown): string[] {
  const value = Array.isArray(raw) ? raw[0] : raw;
  if (typeof value !== "string" || !value) return [];
  const ids = value.split(",").map(item => item.trim()).filter(item => NODE_ID_PATTERN.test(item));
  return [...new Set(ids)].slice(0, 20);
}

/** 全屏对话页：`/video/chat/{projectId}?session={sessionId}`（没有会话就不带参数，页面打开最近的会话）。 */
export function agentChatPath(projectId: string, sessionId?: string | null): string {
  const base = `/video/chat/${encodeURIComponent(projectId)}`;
  return sessionId ? `${base}?session=${encodeURIComponent(sessionId)}` : base;
}

/**
 * 画布页：带 `?session=`（面板展开、选中这个会话）与可选的 `?node=`（定位节点，只平移选中、不写数据）。
 * 与 `quickGenerate.agentCanvasPath` 对同一组输入给出同一个地址。
 */
export function agentCanvasViewPath(projectId: string, options: { sessionId?: string | null; nodeIds?: string[] } = {}): string {
  const query: string[] = [];
  if (options.sessionId) query.push(`session=${encodeURIComponent(options.sessionId)}`);
  const nodes = (options.nodeIds ?? []).filter(id => NODE_ID_PATTERN.test(id));
  if (nodes.length) query.push(`node=${nodes.map(encodeURIComponent).join(",")}`);
  return `/video/canvas/${encodeURIComponent(projectId)}${query.length ? `?${query.join("&")}` : ""}`;
}

// ---------------------------------------------------------------- 「画布」开关记忆

export const CANVAS_TOGGLE_KEY = "seeaitv:create-agent-canvas";

/** 「画布」开关：默认开；只有明确存过 "0" 才是关（存坏了 / 读不到都按默认）。 */
export function readCanvasToggle(storage: Pick<Storage, "getItem"> | undefined): boolean {
  try {
    return storage?.getItem(CANVAS_TOGGLE_KEY) !== "0";
  } catch {
    return true;
  }
}

export function writeCanvasToggle(storage: StorageLike | undefined, on: boolean) {
  try {
    storage?.setItem(CANVAS_TOGGLE_KEY, on ? "1" : "0");
  } catch {
    // 隐私模式下写不进去：只是下次不记得，不影响这次发送。
  }
}

/** 创造页 Agent 模式发送成功后去哪：开 → 画布页（面板展开）；关 → 全屏对话页。 */
export function agentLandingPath(projectId: string, sessionId: string, canvasOn: boolean): string {
  return canvasOn ? agentCanvasViewPath(projectId, { sessionId }) : agentChatPath(projectId, sessionId);
}

// ---------------------------------------------------------------- 对话文件

/** 文件名：去掉开头的 `/`（`/notes/a.md` → `notes/a.md`）；项目记忆单独起名。 */
export function documentDisplayName(path: string): string {
  if (path === MEMORY_DOC_PATH) return "项目记忆";
  return path.replace(/^\/+/, "") || path;
}

/** 字节数的人话（1 KB = 1024 B）。 */
export function documentSizeText(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return "";
  if (bytes < 1024) return `${bytes} B`;
  return `${(bytes / 1024).toFixed(bytes < 10 * 1024 ? 1 : 0)} KB`;
}

/**
 * 对话文件的排列：`/plan.md` 置顶（它是这次创作的规划，最常被翻），其余按更新时间倒序，
 * 项目记忆放最后（它有自己的抽屉）。时间解析不了的排在同组末尾。
 */
export function sortDocuments<T extends Pick<ProjectDocumentSummary, "path" | "updatedAt">>(items: T[]): T[] {
  const rank = (item: T) => (item.path === "/plan.md" ? 0 : item.path === MEMORY_DOC_PATH ? 2 : 1);
  const time = (item: T) => {
    const value = Date.parse(item.updatedAt);
    return Number.isNaN(value) ? -Infinity : value;
  };
  return [...items].sort((a, b) => rank(a) - rank(b) || time(b) - time(a) || a.path.localeCompare(b.path));
}
