import type {
  GroupDetail,
  ScriptAsset,
  ScriptDetail,
  ScriptEntity,
  ScriptEntryMode,
  ScriptGroup,
  ScriptInputReference,
  ScriptShot,
  ScriptSourceType
} from "~/types/script";
import { segmentScriptMentions, type ScriptMentionAsset } from "~/utils/scriptMentions";

/**
 * 脚本接口的**新契约**适配（单一来源 + 测试）。
 *
 * ⚠️ 移植过来的脚本节点照老后端的契约在调：创建发 `nodeId` / `input.text`、读 `scriptId`；
 *    分镜发 `{model, input}`。新后端读的是 `canvasNodeId` / `rawInput`、回 `id`，分镜读 `{modelCode, extraContext}`。
 *    于是创建「成功」了（201）却是一条**节点 ID 为空**的脚本，紧接着拿 `undefined` 去生成分镜 →「scriptID 非法」。
 *    更隐蔽的是：节点 ID 为空时，同一项目里所有脚本节点都会落到**同一条**脚本上（按节点唯一、重复即恢复）。
 * ⚠️ 剧情也换了地方：老后端每次生成都带 `input.text`；新后端把剧情存在脚本上（`rawInput`），生成时读它 ——
 *    所以剧情变了要先 `PATCH` 再生成，否则模型拿到的是上一次（甚至是空）的剧情。
 * 界面仍沿用老系统的形状（`ScriptDetail`），由 {@link normalizeScriptDetail} 归一。
 */

/** 新后端的镜头（`toShot`）。 */
export type ScriptShotResp = {
  id: string;
  shotNo: number;
  durationSec: number;
  description: string;
  shotSize?: string | null;
  lighting?: string | null;
  dialogue?: string | null;
  soundEffect?: string | null;
  cameraMove?: string | null;
  startState?: string | null;
  endState?: string | null;
  entities?: unknown;
  /** 本镜引用的资产（按参考图锚定顺序）。只在脚本详情里给；单个镜头的保存响应不带。 */
  assetIds?: string[] | null;
  imagePrompt?: string | null;
  imagePromptStatus?: string | null;
  imagePromptSource?: string | null;
  videoPrompt?: string | null;
  videoPromptStatus?: string | null;
  videoPromptSource?: string | null;
  version: number;
};

/** 新后端的脚本详情（`POST /scripts`、`GET /scripts/{id}`、`POST /scripts/{id}/storyboard` 的返回）。 */
export type ScriptDetailResp = {
  id: string;
  projectId: string;
  canvasNodeId: string;
  sourceType: ScriptSourceType;
  currentStep: number;
  status: string;
  rawInput?: string | null;
  globalStyle?: string | null;
  assetProfiles?: unknown;
  version: number;
  shots?: ScriptShotResp[] | null;
  llmCallId?: string;
  creditsCost?: number;
};

/** 新后端的资产（`GET /scripts/{id}/assets`、`PUT /script-assets/{id}`）。 */
export type ScriptAssetResp = {
  id: string;
  /** 老后端叫 `assetType` */
  type: string;
  name: string;
  description?: string | null;
  profile?: unknown;
  mediaId?: string;
  /** 媒体字节的受保护端点（老后端叫 `imageUrl`） */
  url?: string;
  source?: string;
  status: string;
  lints?: Array<{ code?: string; message?: string }> | null;
};

/** 新后端的组摘要（`GET /scripts/{id}/groups`）。 */
export type ScriptGroupResp = {
  id: string;
  canvasNodeId: string;
  groupType: string;
  name?: string | null;
  status: string;
  version: number;
};

/** 批量合成提示词的结果（`POST /scripts/{id}/prompts/compose`）。 */
export type ComposeResultResp = {
  quote: { aiCount: number; templateCount: number; skipCount: number; unitCost: number; total: number };
  items: Array<{ shotId: string; shotNo: number; kind: "image" | "video"; status: string; skipped: boolean; reason?: string }>;
};

/** 批量生成资产图的逐项结果（`POST /scripts/{id}/assets/generate`）。 */
export type AssetBatchItemResp = { assetId: string; name: string; generationId?: string; skipped: boolean; reason?: string };

/** 创建脚本的请求体（`POST /scripts`）。 */
export function scriptCreateBody(input: {
  projectId: string;
  nodeId: string;
  sourceType: ScriptSourceType;
  rawInput?: string | null;
  globalStyle?: string | null;
}) {
  return {
    projectId: input.projectId,
    // ⚠️ 脚本与画布节点一一对应（项目 + 节点唯一）：漏了它，同一项目的脚本节点会共用一条脚本
    canvasNodeId: input.nodeId,
    sourceType: input.sourceType,
    rawInput: (input.rawInput ?? "").trim(),
    ...(input.globalStyle ? { globalStyle: input.globalStyle } : {})
  };
}

function referenceBlock(references: ScriptInputReference[], type: "text" | "image") {
  return references
    .filter(reference => reference.type === type)
    .map(reference => {
      const title = reference.title?.trim() || (type === "text" ? "引用文本" : "角色参考");
      if (type === "image") return `【${title}】（角色参考图，见上游节点）`;
      const text = reference.text?.trim();
      return text ? `【${title}】\n${text}` : "";
    })
    .filter(Boolean)
    .join("\n\n");
}

/**
 * 按入口模式把「剧情」与「补充上下文」分开（新后端：剧情存 `rawInput`，补充上下文随生成请求走 `extraContext`）。
 *
 * - 直接输入：剧情 = 用户写的；连了文本节点就当补充上下文；
 * - 文本参考：剧情 = 上游文本（那才是剧本），用户写的是补充要求；
 * - 角色参考：剧情 = 用户写的（两个角色的故事），角色节点当补充上下文。
 *   ⚠️ 新后端的分镜是纯文本模型，参考**图本身**送不进去，只能给出角色名 —— 角色外观要在剧情里写清。
 */
export function scriptStoryInput(
  mode: ScriptEntryMode,
  userText: string,
  references: ScriptInputReference[]
): { rawInput: string; extraContext: string } {
  const text = userText.trim();
  const textRefs = referenceBlock(references, "text");
  if (mode === "text_reference") {
    return { rawInput: textRefs || text, extraContext: textRefs && text ? `补充要求：\n${text}` : "" };
  }
  if (mode === "character_reference") {
    const roles = referenceBlock(references, "image");
    return { rawInput: text, extraContext: [roles, textRefs].filter(Boolean).join("\n\n") };
  }
  return { rawInput: text, extraContext: textRefs };
}

/** 分镜请求体（`POST /scripts/{id}/storyboard`）。⚠️ 字段是 `modelCode`（老后端 `model`）。 */
export function storyboardBody(modelCode: string, extraContext = "") {
  return { modelCode, ...(extraContext.trim() ? { extraContext: extraContext.trim() } : {}) };
}

function progress(completed: number, total: number) {
  return { completed, total, status: total > 0 && completed >= total ? "done" : completed > 0 ? "running" : "pending" };
}

/** 镜头里的实体（新后端 `shot.entities`）：名字 + 类型，决定参考图的锚定顺序。坏数据丢掉不报错。 */
export function parseEntities(raw: unknown): ScriptEntity[] {
  if (!Array.isArray(raw)) return [];
  return raw.flatMap((item) => {
    if (!item || typeof item !== "object") return [];
    const name = String((item as Record<string, unknown>).name ?? "").trim();
    const type = String((item as Record<string, unknown>).type ?? "").trim();
    return name ? [{ name, type: type || "character" }] : [];
  });
}

function normalizeShot(shot: ScriptShotResp): ScriptShot {
  return {
    id: shot.id,
    shotNo: shot.shotNo,
    durationSec: shot.durationSec,
    description: shot.description ?? "",
    shotSize: shot.shotSize ?? null,
    lighting: shot.lighting ?? null,
    dialogue: shot.dialogue ?? null,
    soundEffect: shot.soundEffect ?? null,
    cameraMove: shot.cameraMove ?? null,
    imagePrompt: shot.imagePrompt ?? null,
    videoPrompt: shot.videoPrompt ?? null,
    // ⚠️ 详情里的状态是服务端按上游指纹**算出来的**（与 `GET /prompts` 同一个答案，CD-77），不要再自己判
    imagePromptStatus: shot.imagePromptStatus || "pending",
    videoPromptStatus: shot.videoPromptStatus || "pending",
    imagePromptSource: shot.imagePromptSource || null,
    videoPromptSource: shot.videoPromptSource || null,
    // 新后端的镜头乐观锁叫 `version`（老后端 `contentVersion`），排序即镜号
    contentVersion: shot.version,
    sort: shot.shotNo,
    // ⚠️ 引用关系由服务端给（锚定顺序只有它知道），前端不按名字去配
    assetIds: shot.assetIds ?? [],
    entities: parseEntities(shot.entities)
  };
}

/** 资产 → 界面形状。`shotIds` 由镜头的引用反推（新后端的资产上没有这一列）。 */
export function normalizeScriptAsset(resp: ScriptAssetResp, shotIds: string[] = []): ScriptAsset {
  return {
    id: resp.id,
    assetType: resp.type,
    name: resp.name,
    description: resp.description ?? "",
    imageUrl: resp.url ?? null,
    mediaId: resp.mediaId ?? null,
    status: resp.status,
    // 新后端不报资产图的进度：界面显示「处理中」，不编一个百分比
    progress: 0,
    source: resp.source ?? null,
    // 新后端的资产没有乐观锁（`PUT /script-assets/{id}` 不收 version）
    version: 1,
    shotIds
  };
}

export function normalizeScriptGroup(resp: ScriptGroupResp): ScriptGroup {
  return { id: resp.id, groupType: resp.groupType, name: resp.name ?? "", status: resp.status, itemCount: 0 };
}

function shotIdsByAsset(shots: ScriptShot[]) {
  const byAsset = new Map<string, string[]>();
  for (const shot of shots) {
    for (const assetId of shot.assetIds) byAsset.set(assetId, [...(byAsset.get(assetId) ?? []), shot.id]);
  }
  return byAsset;
}

function assemble(
  resp: ScriptDetailResp,
  shots: ScriptShot[],
  assets: ScriptAsset[],
  groups: ScriptGroup[],
  extras: { name?: string; references?: ScriptInputReference[] }
): ScriptDetail {
  const promptsReady = shots.filter(shot => shot.imagePromptStatus === "ready" && shot.videoPromptStatus === "ready").length;
  return {
    scriptId: resp.id,
    projectId: resp.projectId,
    nodeId: resp.canvasNodeId,
    name: extras.name ?? "",
    sourceType: resp.sourceType,
    globalStyle: resp.globalStyle ?? null,
    currentStep: resp.currentStep,
    status: resp.status,
    version: resp.version,
    // ⚠️ 新后端不存参考节点：不知道时留空（undefined），不要给 [] —— 画布节点据此回写，给 [] 会把节点上的参考配置清空
    input: { text: resp.rawInput ?? "", assetIds: [], ...(extras.references ? { references: extras.references } : {}) },
    shotsProgress: progress(shots.length, shots.length),
    assetsProgress: progress(assets.filter(asset => asset.status === "ready").length, assets.length),
    promptsProgress: progress(promptsReady, shots.length),
    shots,
    assets,
    groups,
    storyboardRunning: false
  };
}

/**
 * 新后端的脚本详情 → 界面用的形状（字段名照老系统）。
 *
 * ⚠️ 新后端把详情拆开了：资产（`GET /scripts/{id}/assets`）与组（`GET /scripts/{id}/groups`）是独立端点，
 *    不在详情里，由调用方另取后经 `extras` 合进来；没取就是空列表 —— **不是**「没有资产」，界面不要据此下结论
 *    （脚本工作台走 `useScriptApi().loadWorkbench`，三样一起取）。
 * ⚠️ 分镜在新后端是**同步**的（请求返回即生成完），没有「生成中」的服务端状态：`storyboardRunning` 恒为 false。
 */
export function normalizeScriptDetail(
  resp: ScriptDetailResp,
  extras: { name?: string; references?: ScriptInputReference[]; assets?: ScriptAssetResp[]; groups?: ScriptGroupResp[] } = {}
): ScriptDetail {
  const shots = (resp.shots ?? []).map(normalizeShot);
  const usage = shotIdsByAsset(shots);
  const assets = (extras.assets ?? []).map(asset => normalizeScriptAsset(asset, usage.get(asset.id) ?? []));
  return assemble(resp, shots, assets, (extras.groups ?? []).map(normalizeScriptGroup), extras);
}

/**
 * 只带脚本与镜头的响应（`PATCH`、加/删/排镜头）→ 界面形状，**保留当前的资产与组**。
 * 资产的 `shotIds` 按新镜头表重新反推（删掉一镜后它不该还挂在资产上）。
 */
export function rebaseScriptDetail(current: ScriptDetail, resp: ScriptDetailResp): ScriptDetail {
  const shots = (resp.shots ?? []).map(normalizeShot);
  const usage = shotIdsByAsset(shots);
  const assets = current.assets.map(asset => ({ ...asset, shotIds: usage.get(asset.id) ?? [] }));
  return assemble(resp, shots, assets, current.groups, { name: current.name, references: current.input.references });
}

/**
 * 单个镜头的响应（`PUT .../shots/{id}`、`PUT .../prompts/{kind}`）合进当前镜头。
 * ⚠️ 这类响应**不带引用关系**：沿用当前的 `assetIds`，不要清空。
 */
export function mergeShotResp(shot: ScriptShot, resp: ScriptShotResp): ScriptShot {
  const next = normalizeShot(resp);
  return { ...next, assetIds: resp.assetIds ?? shot.assetIds };
}

// ---------------------------------------------------------------- 镜头表保存

/**
 * 描述里新写的 `@名字` → 实体（追加到镜头已有实体之后）。
 *
 * ⚠️ 只追加不删除：新后端的实体是分镜产出的结构化结果，描述里大多没有 `@`（模型把实体单独列出）——
 *    按「描述里有哪些 @」整体替换的话，打开再关上一次描述编辑器就会把这一镜的资产全部解绑。
 */
export function mergeMentionEntities(existing: ScriptEntity[], description: string, assets: ScriptMentionAsset[]): ScriptEntity[] {
  const out = [...existing];
  const seen = new Set(existing.map(entity => entity.name));
  for (const segment of segmentScriptMentions(description, assets)) {
    if (!segment.mention || !segment.name || seen.has(segment.name)) continue;
    seen.add(segment.name);
    out.push({ name: segment.name, type: segment.type && segment.type !== "unknown" ? segment.type : "character" });
  }
  return out;
}

const SHOT_TEXT_FIELDS = ["description", "shotSize", "lighting", "dialogue", "soundEffect", "cameraMove"] as const;

function durationOf(shot: ScriptShot) {
  const value = Math.round(Number(shot.durationSec));
  return Number.isFinite(value) && value >= 1 ? Math.min(60, value) : 5;
}

/**
 * 一个镜头相对基线改了哪些字段 —— `PUT /scripts/{id}/shots/{shotId}` 的请求体（不含 version）。
 * ⚠️ 只发改过的：没改的字段一起发出去，服务端会照样把提示词标过期（AV-16），用户白白再合成一次。
 */
export function shotChanges(base: ScriptShot | undefined, draft: ScriptShot): Record<string, unknown> {
  const changes: Record<string, unknown> = {};
  if (!base || durationOf(base) !== durationOf(draft)) changes.durationSec = durationOf(draft);
  for (const field of SHOT_TEXT_FIELDS) {
    const next = String(draft[field] ?? "");
    if ((base ? String(base[field] ?? "") : "") !== next) changes[field] = next;
  }
  const entities = draft.entities ?? [];
  if (JSON.stringify(base?.entities ?? []) !== JSON.stringify(entities)) changes.entities = entities;
  return changes;
}

export type ShotSyncPlan = {
  /** 基线里有、草稿里没了的 */
  deletes: string[];
  /** 新镜头（按草稿顺序）。`after` 是前一镜的 ID —— 可能也是一个新镜头的草稿 ID，执行时换成真 ID */
  creates: Array<{ draftId: string; after: string | null; durationSec: number; description: string; rest: Record<string, unknown> }>;
  updates: Array<{ id: string; version: number; changes: Record<string, unknown> }>;
  /** 草稿的最终顺序（新镜头用草稿 ID 占位） */
  order: string[];
};

/**
 * 把「整张镜头表的草稿」拆成新后端的逐镜操作。
 *
 * ⚠️ 新后端**没有**整表保存（老后端的 `PUT /scripts/{id}/shots`），只有逐镜的增 / 删 / 改 / 排序 ——
 *    照老路径发的话 405，镜头表一次都保存不上。
 * ⚠️ 新镜头先建（只带时长与描述），其余字段建好后再补一次 PUT：建镜头的接口只收这两项。
 */
export function planShotSync(baseline: ScriptShot[], draft: ScriptShot[]): ShotSyncPlan {
  const byId = new Map(baseline.map(shot => [shot.id, shot]));
  const kept = new Set(draft.map(shot => shot.id));
  const plan: ShotSyncPlan = {
    deletes: baseline.filter(shot => !kept.has(shot.id)).map(shot => shot.id),
    creates: [],
    updates: [],
    order: draft.map(shot => shot.id)
  };
  draft.forEach((shot, index) => {
    const base = byId.get(shot.id);
    if (!base) {
      const { durationSec, description, ...fields } = shotChanges(undefined, shot);
      // 新镜头上的空字段不用补发（服务端建出来本就是空的）
      const rest = Object.fromEntries(Object.entries(fields)
        .filter(([, value]) => value !== "" && !(Array.isArray(value) && !value.length)));
      plan.creates.push({
        draftId: shot.id,
        after: index > 0 ? draft[index - 1]!.id : null,
        durationSec: Number(durationSec ?? 5),
        description: String(description ?? ""),
        rest
      });
      return;
    }
    const changes = shotChanges(base, shot);
    if (Object.keys(changes).length) plan.updates.push({ id: shot.id, version: base.contentVersion, changes });
  });
  return plan;
}

export function shotPlanIsEmpty(plan: ShotSyncPlan, serverOrder: string[]) {
  return !plan.deletes.length && !plan.creates.length && !plan.updates.length
    && JSON.stringify(plan.order) === JSON.stringify(serverOrder);
}

// ---------------------------------------------------------------- 组节点

type GroupItemResp = {
  id: string;
  itemNo: number;
  shotId?: string;
  assetId?: string;
  generationId?: string;
  status: string;
  progress?: number;
  creditsCost?: number;
  params?: Record<string, unknown> | null;
  inputs?: unknown[] | null;
  errorMessage?: string;
};

/** 新后端的组详情（建组、`GET /groups/{id}`、执行 / 重试 / 取消的返回）。 */
export type GroupResp = {
  id: string;
  projectId: string;
  canvasNodeId: string;
  groupType: string;
  scriptId: string;
  name?: string | null;
  status: string;
  version: number;
  items?: GroupItemResp[] | null;
};

/** 解组的返回：每个子项落到画布上用哪个节点 ID（`POST /groups/{id}/ungroup`）。 */
export type UngroupResp = {
  id: string;
  status: string;
  version: number;
  nodes?: Array<{ itemId: string; itemNo: number; nodeId: string; status: string; shotId?: string; generationId?: string }> | null;
};

/**
 * 新后端的组详情 → 画布组节点用的形状（字段名照老系统：`groupId` / `itemId` / `paramsSnapshot`…）。
 *
 * ⚠️ 新后端的子项不带「镜号 / 景别 / 资产名」与产物地址：建组时由工作台按镜头与资产补上（`context`），
 *    之后刷新沿用上一次的（`previous`）—— 否则一刷新组节点上的「镜头 05」「唐僧」就全变成序号。
 * ⚠️ 产物地址：资产子项用资产图；生成子项要另问 `/generations/{id}/outputs/0`（见 useScriptApi.resolveGroupOutputs），
 *    老系统那条 `/generations/{id}/outputs/0` 在新后端回的是 JSON 不是字节，直接当图片地址用是一张裂图。
 */
export function normalizeGroupDetail(
  resp: GroupResp,
  context: { shots?: ScriptShot[]; assets?: ScriptAsset[]; previous?: GroupDetail } = {}
): GroupDetail {
  const shots = new Map((context.shots ?? []).map(shot => [shot.id, shot]));
  const assets = new Map((context.assets ?? []).map(asset => [asset.id, asset]));
  const previous = new Map((context.previous?.items ?? []).map(item => [item.itemId, item]));
  const outputType = resp.groupType === "video_generator" ? "video" : "image";
  return {
    groupId: resp.id,
    projectId: resp.projectId,
    scriptId: resp.scriptId,
    canvasNodeId: resp.canvasNodeId,
    groupType: resp.groupType,
    name: resp.name || context.previous?.name || "",
    status: resp.status,
    version: resp.version,
    items: (resp.items ?? []).map((item) => {
      const before = previous.get(item.id);
      const shot = item.shotId ? shots.get(item.shotId) : undefined;
      const asset = item.assetId ? assets.get(item.assetId) : undefined;
      const params = item.params ?? {};
      const snapshot: Record<string, unknown> = { ...(before?.paramsSnapshot ?? {}), ...params };
      if (shot) Object.assign(snapshot, { shotNo: shot.shotNo, shotSize: shot.shotSize ?? "" });
      if (asset) Object.assign(snapshot, { name: asset.name, assetType: asset.assetType });
      // 同一次生成的产物地址可以沿用；换了一次生成（重试）就不能再显示上一次的图
      const keepOutput = before && before.generationId === (item.generationId ?? null) ? before.outputUrl : null;
      return {
        itemId: item.id,
        shotId: item.shotId ?? "",
        assetId: item.assetId ?? null,
        shotVersion: 0,
        canvasNodeId: before?.canvasNodeId ?? "",
        generationId: item.generationId ?? null,
        status: item.status,
        progress: item.progress ?? (item.status === "success" ? 100 : 0),
        outputType,
        outputUrl: asset?.status === "ready" && asset.imageUrl ? asset.imageUrl : keepOutput ?? null,
        paramsSnapshot: snapshot,
        inputRefs: item.inputs ?? [],
        error: item.errorMessage ?? null
      };
    })
  };
}

/**
 * 画布组节点上存的这份组**没归一化过**（CD-81）：`groupId` 为空，或有子项没有 `itemId`。
 *
 * ⚠️ 早先组节点自己的轮询（BaseNode）绕开了 {@link normalizeGroupDetail}，把接口原样写进节点：新后端回的是 `id`，
 *    于是 `groupId` 成了空串、子项只剩 `id` / `itemNo`。没有 `itemId` 时「选中的是哪一项」判成 `'' === ''` ——
 *    每一项都弹出一个编辑面板、全写着「镜头 01」叠在网格上；`groupId` 为空又让轮询停掉，节点从此停在「排队中」。
 *    这样的节点已经存进了用户的画布，打开时要按画布节点取一次组把它修好。
 */
export function groupNeedsRepair(group: { groupId?: unknown; items?: unknown }): boolean {
  if (!String(group.groupId ?? "").trim()) return true;
  const items = Array.isArray(group.items) ? group.items as Array<Record<string, unknown> | null> : [];
  return items.some(item => !String(item?.itemId ?? "").trim());
}

/**
 * 归一化之后还缺不缺标签：镜头子项缺镜号、资产子项缺资产名。
 * 新后端的子项不带这些，刷新时只能沿用上一次的；上一次本身是坏的（{@link groupNeedsRepair}）就沿用不到，
 * 要取一次脚本上下文再归一化 —— 否则修好的节点上「镜头 05」「唐僧」全变成序号。
 */
export function groupMissingLabels(group: GroupDetail): boolean {
  return group.items.some((item) => {
    const snapshot = item.paramsSnapshot ?? {};
    if (item.assetId) return !String(snapshot.name ?? "").trim();
    if (item.shotId) return snapshot.shotNo === undefined || snapshot.shotNo === null || snapshot.shotNo === "";
    return false;
  });
}

const GROUP_STATUS_LABELS: Record<string, string> = {
  draft: "待执行",
  running: "执行中",
  success: "已完成",
  partial_success: "部分完成",
  failed: "失败",
  cancelled: "已停止",
  ungrouped: "已解组"
};

const GROUP_ITEM_STATUS_LABELS: Record<string, string> = {
  draft: "待生成",
  queued: "排队中",
  running: "生成中",
  success: "已完成",
  ready: "已完成",
  completed: "已完成",
  failed: "生成失败",
  cancelled: "已停止",
  // 镜头在建组之后改过：不能拿旧快照去跑（FLOW-05），要先刷新这一项
  stale: "镜头已修改"
};

/**
 * 组的状态 → 界面文案（服务端词表见契约 `GroupStatus`）。
 * ⚠️ `partial_success` 单列，不并进「已完成」或「失败」（AV-24）：并进前者用户不会去翻失败的那几项，并进后者他会把成功的也重跑一遍。
 * 词表外的值原样显示 —— 配置可以比前端新，显示一个英文词好过显示错的中文。
 */
export function groupStatusLabel(status: string): string {
  return GROUP_STATUS_LABELS[status] ?? status;
}

/** 组子项的状态 → 界面文案（服务端词表见契约 `GroupItem.status`）。 */
export function groupItemStatusLabel(status: string | undefined | null): string {
  const value = String(status ?? "") || "draft";
  return GROUP_ITEM_STATUS_LABELS[value] ?? value;
}

/** 组子项的一张参考图（界面用）。 */
export type GroupItemReference = {
  key: string;
  /** 1 起，按 `order` 排好：就是提示词里「参考图对应关系：图N = 名称」的那个 N */
  index: number;
  mediaId: string;
  generationId: string;
  /** 媒体的受保护端点；生成产物要另问地址（`/outputs/0` 回的是 JSON），这里留空 */
  url: string;
  /** 认得出是哪个资产时给名字与类型，认不出（上游分镜图、旧数据）留空 */
  name: string;
  assetType: string;
};

/**
 * 组子项实际发给模型的参考图（CD-82）——和单独的图片节点一样，一镜带着它要用的那几张资产设定图。
 *
 * ⚠️ 新后端的子项输入只带 ID：`{kind:"reference", referenceId}`（资产图等媒体）/ `{kind:"generation", generationId}`（上游产物）。
 *    移植过来的面板只认老形状 `{type:"image"}` —— 一张都解析不出来，参考图条是空的，看起来就像这一镜没有关联任何资源。
 *    老形状（存在旧画布里的）照样认；文本输入不是图，跳过。
 * ⚠️ 编号按 `order` 排：显示的「1、2、3」必须与提示词里的「图1、图2、图3」是同一套，错位的话用户核对不出张冠李戴。
 */
export function groupItemReferences(inputRefs: unknown, assets: ScriptAsset[] = []): GroupItemReference[] {
  const raw = Array.isArray(inputRefs) ? inputRefs : [];
  const byMedia = new Map(assets.filter(asset => asset.mediaId).map(asset => [String(asset.mediaId), asset]));
  return raw
    .map((entry, position) => ({ entry: (entry && typeof entry === "object" ? entry : {}) as Record<string, unknown>, position }))
    .filter(({ entry }) => {
      const kind = String(entry.kind ?? "");
      return kind ? kind === "reference" || kind === "generation" : String(entry.type ?? "") === "image";
    })
    .map(({ entry, position }) => {
      const generationId = String(entry.generationId ?? "");
      const mediaId = generationId ? "" : String(entry.referenceId ?? entry.mediaId ?? "");
      return { position, generationId, mediaId, order: Number(entry.order ?? position + 1) };
    })
    .filter(ref => ref.generationId || ref.mediaId)
    .sort((left, right) => left.order - right.order || left.position - right.position)
    .map((ref, index) => {
      const asset = ref.mediaId ? byMedia.get(ref.mediaId) : undefined;
      return {
        key: `${ref.generationId ? "g" : "m"}:${ref.generationId || ref.mediaId}:${index}`,
        index: index + 1,
        mediaId: ref.mediaId,
        generationId: ref.generationId,
        url: ref.mediaId ? `/api/v1/media/${ref.mediaId}` : "",
        name: asset?.name ?? "",
        assetType: asset?.assetType ?? ""
      };
    });
}

/**
 * 选了却没进组的镜头（镜号升序）。
 * ⚠️ 服务端建组时**跳过**提示词没就绪、或引用的资产还没有设定图的镜头，只在全被跳过时才报错（CD-82）：
 *    部分跳过时不说的话，用户选了 8 镜只看到 6 张，以为丢了。
 */
export function shotsLeftOut(selectedShotIds: string[], shots: ScriptShot[], group: Pick<GroupDetail, "items">): number[] {
  const included = new Set(group.items.map(item => item.shotId));
  const selected = new Set(selectedShotIds);
  return shots
    .filter(shot => selected.has(shot.id) && !included.has(shot.id))
    .map(shot => shot.shotNo)
    .sort((left, right) => left - right);
}

/** 新的画布节点 ID（组节点由前端起名，服务端按它「同节点重复建组即恢复」）。 */
export function newCanvasNodeId(prefix = "n") {
  return `${prefix}_${Date.now().toString(36)}_${Math.random().toString(36).slice(2, 7)}`;
}
