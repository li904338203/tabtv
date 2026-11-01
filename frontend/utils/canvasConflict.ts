/**
 * 画布保存冲突的判定、文案与本地草稿（AGT-07，CLAUDE.md §5.16、CD-58）。
 *
 * ⚠️ 冲突必须说得出**是谁**改的：「已被其他窗口修改」里的"其他窗口"有两种 ——
 *    自己的另一个标签页（直接覆盖就行）与同事（覆盖等于抹掉别人的工作）。
 *    分流一律按服务端给的 `bySelf` **布尔**，不匹配 message 文案（文案改一个字匹配就失效）。
 *    服务端给不出编辑者时退回中性说法，**不要猜**。
 *
 * ⚠️ 草稿放 `sessionStorage`（按标签页隔离）：放 localStorage 的话 A 标签页的草稿会出现在 B 里。
 */
import type { CanvasDocument } from "~/types/canvas";

/** 冲突详情（契约 `VersionConflictError.details`）；缺字段一律给空值，不编造。 */
export type ConflictDetails = {
  currentVersion?: number;
  currentUpdatedAt: string;
  currentEditorId: string;
  currentEditorName: string;
  bySelf: boolean;
  /**
   * 最后一次写入是真人整份保存（user）、Agent 的画布操作（agent，AGT-10）还是 MCP 的画布操作（mcp，MCPT-09）。
   * agent / mcp 时 store 会补取操作日志、重放、自动重存（AGT-11）；缺省按真人处理。
   */
  editorKind?: "user" | "agent" | "mcp";
  /** 冲突条直接显示的说明（如补取发现中间夹着真人保存、只能整页载入时）；有它就不再按编辑者拼文案。 */
  message?: string;
};

/** 从 `useApi` 抛出的错误里取冲突详情（`cause.data.details`）。 */
export function conflictDetails(cause: unknown): ConflictDetails {
  const data = cause && typeof cause === "object" && "data" in cause
    ? (cause as { data?: unknown }).data
    : undefined;
  const details = data && typeof data === "object" && "details" in data
    ? (data as { details?: unknown }).details
    : undefined;
  const raw = details && typeof details === "object" ? details as Record<string, unknown> : {};
  return {
    currentVersion: typeof raw.currentVersion === "number" ? raw.currentVersion : undefined,
    currentUpdatedAt: typeof raw.currentUpdatedAt === "string" ? raw.currentUpdatedAt : "",
    currentEditorId: typeof raw.currentEditorId === "string" ? raw.currentEditorId : "",
    currentEditorName: typeof raw.currentEditorName === "string" ? raw.currentEditorName.trim() : "",
    // ⚠️ 只认真正的 true：缺省 / 脏值都按"不是我"处理 —— 误判成"是我"的话，用户会放心地覆盖掉同事的工作
    bySelf: raw.bySelf === true,
    ...(raw.editorKind === "agent" || raw.editorKind === "user" || raw.editorKind === "mcp" ? { editorKind: raw.editorKind } : {})
  };
}

/** 「以我为准」按钮的文案：写明覆盖的是谁。 */
export function overwriteLabel(details: ConflictDetails | null | undefined): string {
  if (details?.bySelf) return "以我为准（覆盖我在另一个窗口的修改）";
  // MCP 写入时最后编辑者是用户本人：写「覆盖 林澈 的修改」等于让他以为在覆盖自己（MCPT-09）
  if (details?.editorKind === "mcp") return "以我为准（覆盖外部 AI 客户端的修改）";
  if (details?.currentEditorName) return `以我为准（覆盖 ${details.currentEditorName} 的修改）`;
  return "以我为准（覆盖服务器上的版本）";
}

/** hh:mm（本地时区）；解析不了返回空串。 */
export function clockTime(iso: string): string {
  if (!iso) return "";
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "";
  return `${String(date.getHours()).padStart(2, "0")}:${String(date.getMinutes()).padStart(2, "0")}`;
}

/** 冲突条的说明：谁在什么时候改的。 */
export function conflictSummary(details: ConflictDetails | null | undefined): string {
  if (details?.message) return details.message;
  const time = clockTime(details?.currentUpdatedAt ?? "");
  const at = time ? `于 ${time} ` : "";
  if (details?.bySelf) return `你在另一个窗口${at}保存了更新的版本`;
  if (details?.editorKind === "mcp") {
    const proxy = details.currentEditorName ? `（代 ${details.currentEditorName}）` : "";
    return `外部 AI 客户端${proxy}${time ? `在 ${time} ` : ""}修改了画布`;
  }
  if (details?.currentEditorName) return `${details.currentEditorName} ${time ? `在 ${time} ` : ""}修改了画布`;
  return `画布已${at}在别处被修改`;
}

/** 「采纳对方版本」按钮的文案。 */
export function adoptLabel(details: ConflictDetails | null | undefined): string {
  return details?.bySelf ? "采纳另一个窗口的版本" : "采纳对方版本";
}

/** 本地草稿：进冲突态时把当时的本地文档存下来，任何路径都不静默丢弃用户的改动。 */
export type CanvasDraft = {
  projectId: string;
  /** 草稿基于的服务端版本（排障用） */
  baseVersion: number;
  savedAt: string;
  document: CanvasDocument;
};

export function draftKey(projectId: string): string {
  return `seeaitv:canvas-draft:${projectId}`;
}

type DraftStorage = Pick<Storage, "getItem" | "setItem" | "removeItem">;

function sessionStore(): DraftStorage | undefined {
  try {
    return typeof globalThis.sessionStorage === "undefined" ? undefined : globalThis.sessionStorage;
  } catch {
    // 部分浏览器在禁用存储时访问 sessionStorage 本身就会抛
    return undefined;
  }
}

/** 写草稿；写不进去（无存储 / 超配额）返回 false —— 调用方据此**拒绝**任何会丢本地改动的动作。 */
export function writeDraft(draft: CanvasDraft, storage: DraftStorage | undefined = sessionStore()): boolean {
  if (!storage || !draft.projectId) return false;
  try {
    storage.setItem(draftKey(draft.projectId), JSON.stringify(draft));
    return true;
  } catch {
    return false;
  }
}

export function readDraft(projectId: string, storage: DraftStorage | undefined = sessionStore()): CanvasDraft | null {
  if (!storage || !projectId) return null;
  try {
    const raw = storage.getItem(draftKey(projectId));
    if (!raw) return null;
    const parsed = JSON.parse(raw) as Partial<CanvasDraft>;
    if (parsed?.projectId !== projectId || !parsed.document || typeof parsed.document !== "object") return null;
    return parsed as CanvasDraft;
  } catch {
    return null;
  }
}

export function removeDraft(projectId: string, storage: DraftStorage | undefined = sessionStore()): void {
  if (!storage || !projectId) return;
  try {
    storage.removeItem(draftKey(projectId));
  } catch {
    // 删不掉只是多留一份草稿，不影响画布
  }
}
