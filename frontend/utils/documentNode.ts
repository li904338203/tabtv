/**
 * 文档节点（AGT-09）的纯函数：路径生成、错误归类、冲突措辞。
 *
 * 节点只存 `params.docId`，正文在 `project_document`（GET/PUT /projects/{id}/documents/{docId}）。
 */

/** 文档节点的存储类型（与后端 `canvas/domain.NodeTypeDocument`、seed 的 node_type 一致）。 */
export const DOCUMENT_NODE_TYPE = "document";

/**
 * 手动新建的文档放在 `/notes/` 下（后端白名单只认 /plan.md、/shots.md、/.memory.md、/notes/<名字>.md）。
 * 名字带时间戳 + 随机段：同一毫秒连点两次也不会撞「同路径已存在」。
 */
export function newDocumentPath(now: number = Date.now(), random: () => number = Math.random) {
  const suffix = Math.floor(random() * 36 ** 4).toString(36).padStart(4, "0");
  return `/notes/doc-${now.toString(36)}-${suffix}.md`;
}

/** 路径的最后一段，作节点体里的文件名显示。 */
export function documentFileName(path: string) {
  const segments = path.split("/").filter(Boolean);
  return segments[segments.length - 1] ?? path;
}

export type DocumentFailure =
  | { kind: "conflict"; message: string; currentVersion?: number }
  | { kind: "not_found"; message: string }
  | { kind: "forbidden"; message: string }
  | { kind: "other"; message: string };

type FailureLike = {
  statusCode?: number;
  message?: string;
  data?: { code?: number; details?: Record<string, unknown> };
};

/**
 * 把 useApi 抛出的错误归成几类。
 *
 * ⚠️ 冲突按**业务码 40902** 判，不按 HTTP 409：40905（项目在回收站）也是 409，
 *    当成冲突的话会给用户一个「以我为准」按钮，点下去永远失败。
 */
export function classifyDocumentFailure(cause: unknown): DocumentFailure {
  const failure = (cause ?? {}) as FailureLike;
  const code = failure.data?.code;
  const message = failure.message || "请求失败";
  if (code === 40902) {
    const details = failure.data?.details ?? {};
    const version = Number(details.currentVersion);
    return {
      kind: "conflict",
      message: documentConflictText(details),
      currentVersion: Number.isFinite(version) ? version : undefined
    };
  }
  if (failure.statusCode === 404) return { kind: "not_found", message: "文档不存在或已被删除" };
  if (failure.statusCode === 403) return { kind: "forbidden", message: "你在该工作区是只读成员，不能编辑文档" };
  return { kind: "other", message };
}

/**
 * 冲突措辞：按 `bySelf` / `currentSource` / `currentEditorName` 这几个**字段**分流，
 * 不匹配 message 文案（改一个字就失效，失效时所有冲突都退回默认那句）。
 *
 * `subject` 是被改的东西（默认「这份文档」，措辞逐字不变）；3D 导演台传「这个场景」（DRD-46）：
 * 同一种冲突在文档节点与导演台上说两种话，用户会以为是两个问题。
 * ⚠️ MCP 写入时最后编辑者是授权所属的用户本人：先于名字判断，否则会说成「林澈 修改了…」，
 *    让他以为是自己（或同名同事）改的（同画布 MCPT-09）。
 */
export function documentConflictText(details: Record<string, unknown>, subject = "这份文档") {
  if (details.bySelf === true) return `你在另一个窗口修改过${subject}`;
  if (details.currentSource === "agent" && !details.currentEditorName) return `Agent 刚修改了${subject}`;
  if (details.currentSource === "mcp" || details.editorKind === "mcp") return `外部 AI 客户端刚修改了${subject}`;
  const name = typeof details.currentEditorName === "string" ? details.currentEditorName.trim() : "";
  if (name) return `${name} 修改了${subject}`;
  return subject === "这份文档" ? "文档已被修改" : `${subject.replace(/^(这份|这个)/, "")}已被修改`;
}
