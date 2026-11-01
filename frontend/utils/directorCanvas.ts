/**
 * 3D 导演台接入画布的纯函数（DRT-16，dev/03 §6）：打开时的自愈判定、给编辑器的全景上游、
 * 下游节点上的连线建议条、节点卡的封面与截图数。都不碰网络、不碰 store —— 调用方是 CanvasBoard 与 ParamPanel。
 *
 * ⚠️ 场景正文不进 canvasData（DRK-9）：节点 params 里只有 `sceneId` 与截图时镜像过来的几个字段
 *    （`outputMediaId` / `cover` / `captures`，dev/03 §7.1），这里只读这些。
 */
import type { components } from "@tabtv/api-client";
import type { CanvasEdge, CanvasNode, JsonObject } from "~/types/canvas";
import { canvasMediaRole, canvasRoleCharacters, orderedTargetEdges, outputMediaIdOf, referenceResource, referenceResources, type UploadedReference } from "~/utils/canvas";

export const DIRECTOR_NODE_TYPE = "director";

type NodeLike = { id: string; data: { nodeType: string; title?: string; params: JsonObject } };

// ---------------------------------------------------------------- 打开与自愈（DRD-43）

/**
 * 打开时在**本项目**的场景摘要里查到的结果。
 * `missing`：本项目没有这个场景 —— 多半是从别的项目粘贴来的节点（或场景已随项目清理掉）。
 */
export type DirectorSceneLookup = { status: "found"; canvasNodeId: string } | { status: "missing" };

export type DirectorOpenPlan =
  /** 场景就是本节点的：直接打开。 */
  | { action: "open"; sceneId: string }
  /** 节点上没有 `sceneId`（Agent 建了一半、老数据）：新建场景并写回。 */
  | { action: "create" }
  /** 场景属于别的节点（复制、粘贴、Agent 复制后没来得及分叉）：从它分叉出本节点自己的场景（DR-F-07）。 */
  | { action: "fork"; sourceSceneId: string }
  /**
   * 场景不在本项目：先按它分叉（同一工作区的别的项目也能复制，契约 createDirectorScene），
   * 分叉不了（别的工作区、已清理）再新建空场景并提示 `notice`。
   */
  | { action: "recreate"; sourceSceneId: string; notice: string };

export const DIRECTOR_RECREATED_NOTICE = "原场景不在本项目，已新建";

/**
 * 打开导演台前的判定（dev/03 §6.3）。`lookup` 为 null 表示节点上没有 sceneId、不必查。
 *
 * ⚠️ 节点与场景按 `canvasNodeId` 一一对应：两个节点指向同一份场景时，改一个另一个跟着变（DR-AC-42）。
 *    复制时已经分叉过一次（`duplicateCanvasNode`），这里是第二道 —— 分叉失败、粘贴、Agent 复制都靠它兜住。
 */
export function directorOpenPlan(node: NodeLike, lookup: DirectorSceneLookup | null): DirectorOpenPlan {
  const sceneId = directorSceneIdOf(node.data.params);
  if (!sceneId) return { action: "create" };
  if (!lookup || lookup.status === "missing") {
    return { action: "recreate", sourceSceneId: sceneId, notice: DIRECTOR_RECREATED_NOTICE };
  }
  if (lookup.canvasNodeId !== node.id) return { action: "fork", sourceSceneId: sceneId };
  return { action: "open", sceneId };
}

/** 节点上的场景 ID（雪花号字符串）；没有或形状不对时为空串。 */
export function directorSceneIdOf(params: JsonObject): string {
  const value = String(params.sceneId ?? "").trim();
  return /^\d+$/.test(value) ? value : "";
}

/**
 * 场景接口失败时给用户看的话：服务端给了原因就用它（403 会说清缺哪一项权限、回收站里的项目会说 40905），
 * 没有响应体（断网、超时）时用调用方的兜底文案 —— useApi 在那种情况下只有一句「请求失败」。
 */
export function directorFailureText(cause: unknown, fallback: string): string {
  const message = (cause as { message?: unknown } | undefined)?.message;
  return typeof message === "string" && message.trim() && message.trim() !== "请求失败" ? message.trim() : fallback;
}

/** 请求是不是 404（自愈时区分「源场景分叉不了」与别的失败）。 */
export function isDirectorNotFound(cause: unknown): boolean {
  return (cause as { statusCode?: unknown } | undefined)?.statusCode === 404;
}

// ---------------------------------------------------------------- 节点卡（dev/03 §6.1）

/** 截图数：每个机位一条最新摘要（`params.captures`，截图时由画布写回）。 */
export function directorCaptureCount(params: JsonObject): number {
  return Array.isArray(params.captures) ? params.captures.length : 0;
}

/**
 * 封面地址（640 档缩略图）：`params.cover`（受保护媒体地址或媒体 ID），没有时用输出机位的最新截图。
 * ⚠️ 受保护媒体 `<img src>` 直接引用一律 401 —— 调用方要经 useAuthenticatedMedia 取成 blob。
 */
export function directorCoverUrl(params: JsonObject): string {
  const cover = String(params.cover ?? "").trim();
  if (/^\d+$/.test(cover)) return `/api/v1/media/${cover}?w=640`;
  if (cover.startsWith("/api/v1/media/")) return cover.includes("?") ? cover : `${cover}?w=640`;
  const output = String(params.outputMediaId ?? "").trim();
  return /^\d+$/.test(output) ? `/api/v1/media/${output}?w=640` : "";
}

// ---------------------------------------------------------------- 全景上游（DR-F-02、dev/03 §6.6）

export type DirectorUpstreamImage = {
  sourceNodeId: string;
  title: string;
  order: number;
  /** 上游当前的图片产出；取不到（还没生成、失败、不是图片）时为 null，界面显示「无可用产出」并禁用。 */
  mediaId: string | null;
};

/**
 * 连进导演台的上游图片（按连线顺序）：编辑器的「背景 → 全景图」从这里选，AI 搭景的参考图也按这个顺序。
 * 只认「生成成功且有图片产出」—— 上传在节点上的参考（`params.reference`）不算上游产出，同 generationInputs。
 */
export function directorUpstreamImages(
  nodes: readonly CanvasNode[],
  edges: readonly CanvasEdge[],
  directorId: string
): DirectorUpstreamImage[] {
  return orderedTargetEdges([...edges], directorId).map((edge, index) => {
    const source = nodes.find(node => node.id === edge.source);
    return {
      sourceNodeId: edge.source,
      title: source?.data.title ?? "上游节点",
      order: edge.data?.order ?? index + 1,
      mediaId: source ? imageOutputMediaId(source) : null
    };
  });
}

/** 节点当前的图片产出（生成成功、产出是图片、取得到媒体 ID）；连线参考与 `@` 参考共用这一个判定。 */
function imageOutputMediaId(node: CanvasNode): string | null {
  const generation = node.data.generation;
  const producesImage = (generation?.outputType ?? node.data.nodeType) === "image";
  return generation?.status === "success" && producesImage ? outputMediaIdOf(generation) ?? null : null;
}

// ---------------------------------------------------------------- 节点面板的 `@` 参考（DRT-20，DR-F-04）

export type DirectorMentionCandidate = { nodeId: string; title: string; mediaId: string };

/**
 * `@` 的候选：本画布**有产出的图片类节点**（含从导演台发布出来的节点），最近创建的在前、按关键字过滤标题；
 * 不含导演台自己。「有产出」与连线参考同一个判定 —— 还没出图的节点列出来也选不了。
 */
export function directorMentionCandidates(
  nodes: readonly CanvasNode[],
  selfId: string,
  query = "",
  limit = 20
): DirectorMentionCandidate[] {
  const keyword = query.trim().toLowerCase();
  const out: DirectorMentionCandidate[] = [];
  for (let i = nodes.length - 1; i >= 0 && out.length < limit; i -= 1) {
    const node = nodes[i]!;
    if (node.id === selfId) continue;
    const mediaId = imageOutputMediaId(node);
    if (!mediaId) continue;
    const title = node.data.title?.trim() || "图片节点";
    if (keyword && !title.toLowerCase().includes(keyword)) continue;
    out.push({ nodeId: node.id, title, mediaId });
  }
  return out;
}

/**
 * 把一个 `@` 候选并进 `params.reference`：与上传图同一个形状，只多一个 `fromNodeId`（DR-D-05）。
 * 媒体 ID 已在里面的不重复加（上游会白收两张一样的图）；满了不加。不能加时 `notice` 说明原因。
 * ⚠️ 只带媒体 ID，归属由服务端校验（GO-27）：节点标题只是显示用的名字。
 */
export function directorAddMention(
  current: unknown,
  candidate: DirectorMentionCandidate,
  maxCount: number
): { references: UploadedReference[]; notice?: string } {
  const references = referenceResources(current);
  if (references.some(item => item.id === candidate.mediaId)) {
    return { references, notice: `「${candidate.title}」已经在参考图里了` };
  }
  if (references.length >= maxCount) return { references, notice: `参考图最多 ${maxCount} 张` };
  const added = referenceResource({ id: candidate.mediaId, type: "image", name: candidate.title, fromNodeId: candidate.nodeId });
  return { references: added ? [...references, added] : references };
}

// ---------------------------------------------------------------- 连线建议条（DR-F-93~95、dev/03 §6.5）

export type DirectorSuggestionKind = "preset" | "first_frame" | "character";

export type DirectorSuggestion = {
  kind: DirectorSuggestionKind;
  text: string;
  /** 「使用」按钮：点了才写这个参数。建议条本身**不改**用户已选的参数（DR-F-93）。 */
  apply?: { param: "preset" | "mode"; value: string; label: string };
  /** 可关闭（关闭状态只存内存与 sessionStorage，不写画布）。 */
  dismissible: boolean;
};

/** 建议用的配置值：都来自 `defaultByUpstream`（DRD-45），代码里不写死预设码与模式。 */
export type DirectorSuggestionDefaults = {
  presetCode?: string;
  presetName?: string;
  videoMode?: string;
  videoModeName?: string;
};

/** 会把截图当首帧用的视频模式（`image_reference` 在操作上就是图生视频，见 operationForNode）。 */
const FIRST_FRAME_MODES = new Set(["img2video", "first_last_frame", "image_reference"]);

export const DIRECTOR_FIRST_FRAME_TEXT = "截图会被当作首帧，成片多半是白模画面，建议先出真实画面再进视频";
export const DIRECTOR_CHARACTER_TEXT = "建议同时连上角色设定图，人物更不容易走样";

type UpstreamLike = { data: { nodeType: string; params?: JsonObject } };

/** 发布到画布的导演台产物（截图 / 深度图 / 骨架图 / 预演，DRT-17）：节点上带 `params.directorSource`。 */
function publishedKindOf(node: UpstreamLike): string {
  const source = node.data.params?.directorSource;
  return source && typeof source === "object" && !Array.isArray(source) ? String((source as { kind?: unknown }).kind ?? "") : "";
}

/** 这个上游给的是不是导演台截图：导演台节点本身，或发布出来的截图节点。 */
function isDirectorCapture(node: UpstreamLike): boolean {
  return node.data.nodeType === DIRECTOR_NODE_TYPE || publishedKindOf(node) === "capture";
}

/**
 * 下游节点（图片 / 视频）连了导演台截图时该给的建议。上游里没有导演台、也没有发布出来的截图节点，就一条都没有。
 *
 * - **预设**：连的是导演台节点、图片节点的预设又不是配置的默认值时，建议用它并给「使用」（DR-F-93）；
 * - **首帧**：视频节点在图生视频 / 首尾帧模式下提示截图会成为首帧，配置了默认模式时给「改用…」（DR-F-94）；
 *   上游是发布出来的截图节点时同样提示（DRT-17、DR-AC-25）；
 * - **角色图**：输入里只有导演台截图、没有别的图片（连线、上传的参考、角色库都算）时，建议连上角色设定图（DR-F-95）。
 *   发布出来的导演台产物（截图、深度图、骨架图）不算角色图。只提示不拦截。
 */
export function directorSuggestions(
  target: { data: { nodeType: string; params: JsonObject } },
  upstream: ReadonlyArray<UpstreamLike>,
  defaults: DirectorSuggestionDefaults = {}
): DirectorSuggestion[] {
  if (!upstream.some(isDirectorCapture)) return [];
  const { nodeType, params } = target.data;
  if (nodeType !== "image" && nodeType !== "video") return [];
  const tips: DirectorSuggestion[] = [];
  const fromDirectorNode = upstream.some(node => node.data.nodeType === DIRECTOR_NODE_TYPE);

  if (nodeType === "image" && fromDirectorNode && defaults.presetCode && String(params.preset ?? "") !== defaults.presetCode) {
    tips.push({
      kind: "preset",
      text: `建议使用『${defaults.presetName || defaults.presetCode}』`,
      apply: { param: "preset", value: defaults.presetCode, label: "使用" },
      dismissible: true
    });
  }

  if (nodeType === "video" && FIRST_FRAME_MODES.has(String(params.mode ?? ""))) {
    tips.push({
      kind: "first_frame",
      text: DIRECTOR_FIRST_FRAME_TEXT,
      ...(defaults.videoMode && !FIRST_FRAME_MODES.has(defaults.videoMode)
        ? { apply: { param: "mode" as const, value: defaults.videoMode, label: `改用${defaults.videoModeName || defaults.videoMode}` } }
        : {}),
      // 跟着模式走：换掉首帧模式它就消失，关掉它等于让用户忘了自己选的是什么模式
      dismissible: false
    });
  }

  const otherImages = upstream.filter(node => node.data.nodeType !== DIRECTOR_NODE_TYPE
    && !publishedKindOf(node)
    && canvasMediaRole(node.data.nodeType) === "image").length
    + referenceResources(params.reference).filter(item => item.type === "image").length
    + canvasRoleCharacters(params.roleLib).length;
  if (!otherImages) tips.push({ kind: "character", text: DIRECTOR_CHARACTER_TEXT, dismissible: true });

  return tips;
}

/** 建议条关闭状态在 sessionStorage 里的键：按项目 + 节点 + 建议种类，关掉一条不影响别的节点。 */
export function directorSuggestionKey(projectId: string, nodeId: string, kind: DirectorSuggestionKind): string {
  return `seeaitv:director-tip:${projectId}:${nodeId}:${kind}`;
}

// ---------------------------------------------------------------- 项目场景摘要（useDirectorScenes）

export type DirectorSceneSummary = components["schemas"]["DirectorSceneSummary"];
type DirectorSceneResponse = components["schemas"]["DirectorScene"];
type EditorKind = DirectorSceneSummary["editorKind"];

/** 建场景 / 分叉回来的整份场景 → 摘要（与 `GET director-scenes` 的列表项同一形状）。 */
export function directorSummaryOf(scene: DirectorSceneResponse): DirectorSceneSummary {
  return {
    id: scene.id,
    canvasNodeId: scene.canvasNodeId,
    version: scene.version,
    renderFingerprint: scene.renderFingerprint,
    outputCameraId: scene.scene?.outputCameraId ?? null,
    lastEditorName: scene.lastEditorName,
    editorKind: scene.editorKind,
    updatedAt: scene.updatedAt
  };
}

function editorKindOf(value: unknown): EditorKind | undefined {
  return value === "user" || value === "agent" || value === "mcp" ? value : undefined;
}

/**
 * 收到 `director.scene_updated` 后的摘要（按帧更新，不重新拉列表）。
 *
 * - 别的项目、形状不对、版本不比已知的新（自己保存的回声、乱序到达的旧事件）→ 原样返回；
 * - 不认识的场景 → `unknown: true`，调用方重新拉一次列表：事件不带输出机位，凭它拼不出一份完整的摘要。
 * - ⚠️ 事件也不带输出机位：已知场景沿用原来的 `outputCameraId`（改输出机位会让指纹跟着变，过期判断不受影响）。
 */
export function directorSummariesAfterEvent(
  summaries: Readonly<Record<string, DirectorSceneSummary>>,
  frame: Record<string, unknown>,
  projectId: string
): { summaries: Readonly<Record<string, DirectorSceneSummary>>; unknown: boolean } {
  const unchanged = { summaries, unknown: false };
  if (frame?.type !== "director.scene_updated" || String(frame.projectId ?? "") !== projectId) return unchanged;
  const sceneId = String(frame.sceneId ?? "");
  const version = Number(frame.version);
  if (!sceneId || !Number.isFinite(version)) return unchanged;
  const known = summaries[sceneId];
  if (!known) return { summaries, unknown: true };
  if (version <= known.version) return unchanged;
  return {
    unknown: false,
    summaries: {
      ...summaries,
      [sceneId]: {
        ...known,
        version,
        renderFingerprint: typeof frame.renderFingerprint === "string" && frame.renderFingerprint
          ? frame.renderFingerprint
          : known.renderFingerprint,
        lastEditorName: typeof frame.editorName === "string" ? frame.editorName : known.lastEditorName,
        editorKind: editorKindOf(frame.editorKind) ?? known.editorKind,
        updatedAt: typeof frame.occurredAt === "string" ? frame.occurredAt : known.updatedAt
      }
    }
  };
}
