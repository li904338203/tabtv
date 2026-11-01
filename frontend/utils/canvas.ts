import type { CanvasEdge, CanvasNode, GenerationView, JsonObject } from "~/types/canvas";
import type { components as ApiComponents } from "@tabtv/api-client";
import type { ScriptEntryMode, ScriptInputReference } from "~/types/script";
import type { ControlOption } from "~/utils/controlOptions";
import { optionValue, restrictionFixes } from "~/utils/controlOptions";

export const CONTROL_TYPES = new Set([
  "select",
  "segmented",
  "slider",
  "grid",
  "tag_group",
  "switch",
  "dialog",
  "button",
  "upload",
  "inline",
  "text"
]);

export function cloneJson<T>(value: T): T {
  return JSON.parse(JSON.stringify(value)) as T;
}

export function asObject(value: unknown): JsonObject {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? { ...(value as JsonObject) }
    : {};
}

export function mergeObjects(base: JsonObject, override: JsonObject): JsonObject {
  const result: JsonObject = { ...base };
  for (const [key, value] of Object.entries(override)) {
    const current = result[key];
    result[key] = typeof current === "object" && current !== null && !Array.isArray(current)
      && typeof value === "object" && value !== null && !Array.isArray(value)
      ? mergeObjects(asObject(current), asObject(value))
      : value;
  }
  return result;
}

export function videoModeOptions(
  baseOptions: unknown,
  overrideOptions: unknown,
  inputTypes: string[]
) {
  const base = Array.isArray(baseOptions) ? baseOptions : [];
  const allowed = Array.isArray(overrideOptions) ? overrideOptions : base;
  const baseByValue = new Map(base.map((option) => {
    const entry = typeof option === "string" ? { label: option, value: option } : asObject(option);
    return [String(entry.value ?? ""), entry];
  }));

  return allowed.map((option) => {
    const override = typeof option === "string" ? { value: option } : asObject(option);
    const value = String(override.value ?? "");
    const fallback = baseByValue.get(value) ?? { label: value, value };
    return {
      ...fallback,
      ...override,
      value,
      disabled: !isVideoModeAvailable(value, inputTypes)
    };
  });
}

export function orderedTargetEdges(edges: CanvasEdge[], targetId: string) {
  return edges
    .filter(edge => edge.target === targetId)
    .sort((left, right) => (left.data?.order ?? 0) - (right.data?.order ?? 0));
}

/**
 * 一条生成输入 —— **形状由新后端的受理入口决定**
 * （`generation/domain.declaredInput`）。
 *
 * ⚠️ 老后端用 `{nodeId, type}`，新后端用 **`{sourceNodeId, kind}`**。
 * 两边都是"节点 ID + 类型"，但名字不同 —— 而发错名字**不报字段错**：
 * 服务端读到空 `kind`，报的是「输入类型非法」，指向输入内容而不是字段名。
 */
/**
 * 画布这边的一条生成输入。
 *
 * ⚠️ `kind` 是**节点的媒体类型**（`text`/`image`/`video`/`audio`/`style`/`effect`/`script`）：
 *    视频模式可用性、输入是否就绪、报价按「几张图」计数用的都是它。
 *    它**不是**受理契约里的 kind（来源类型 `generation`/`reference`/…）—— 发给受理前
 *    必须经 `acceptInputsOf` 换一次，见那里的说明。
 */
export type GenerationInput = {
  sourceNodeId: string;
  kind: string;
  order: number;
  generationId?: string;
  referenceId?: string;
  text?: string;
  /** 风格/特效节点选中的素材码（素材只贡献提示词，服务端按码拼词）。 */
  materialCode?: string;
};

export type ScriptCanvasReference = ScriptInputReference & {
  edgeId: string;
  order: number;
  previewUrl?: string;
  ready: boolean;
};

export type UploadedReference = {
  id: string;
  type: "image" | "video" | "audio";
  contentType: string;
  name: string;
  size: number;
  url: string;
  /**
   * 从画布上 `@` 引用来的图片（3D 导演台 DRT-20）：来源节点的 ID，面板上显示「来自某某节点」。
   * 媒体 ID 就是那个节点的产出，归属由服务端校验；上传来的参考没有这个字段。
   */
  fromNodeId?: string;
};

export function referenceResource(value: unknown): UploadedReference | undefined {
  const resource = asObject(value);
  const id = typeof resource.id === "string" ? resource.id : "";
  const type = typeof resource.type === "string" ? resource.type : "";
  if (!/^\d+$/.test(id) || !["image", "video", "audio"].includes(type)) return undefined;
  const contentType = typeof resource.contentType === "string" && resource.contentType.startsWith(`${type}/`)
    ? resource.contentType
    : `${type}/unknown`;
  const name = typeof resource.name === "string" && resource.name.trim()
    ? resource.name.trim().slice(0, 255)
    : `reference-${id}`;
  const size = typeof resource.size === "number" && Number.isFinite(resource.size) && resource.size > 0
    ? resource.size
    : 0;
  return {
    id,
    type: type as UploadedReference["type"],
    contentType,
    name,
    size,
    // ⚠️ 地址按 id 现算、不信存下来的：新后端的参考就是工作区媒体，取它走受保护的 `/media/{id}`
    //    （老后端是 `/media/references/{id}/content`，新后端没有这条路由，老画布里存的旧地址一律 404）。
    url: `/media/${id}`,
    ...(typeof resource.fromNodeId === "string" && resource.fromNodeId ? { fromNodeId: resource.fromNodeId } : {})
  };
}

/**
 * 参考上传接口（`POST /media/references`）的响应 → 节点上存的一份参考。
 *
 * ⚠️ 新后端回的是 `{id, kind, mime, sizeBytes}`，没有文件名；老后端是 `{id, type, contentType, name, size}`。
 *    按老形状读的话每一次上传都「成功」了却被判成「参考资源接口响应无效」—— 文件已经进了素材库，
 *    节点上什么都没有。文件名取用户选的那个文件。
 */
export function uploadedReferenceOf(response: unknown, file?: { name?: string; size?: number; type?: string }) {
  const value = asObject(response);
  return referenceResource({
    id: value.id === undefined || value.id === null ? "" : String(value.id),
    type: value.kind ?? value.type,
    contentType: value.mime ?? value.contentType ?? file?.type,
    name: value.name ?? file?.name,
    size: value.sizeBytes ?? value.size ?? file?.size
  });
}

/** 视频节点「角色库」里选中的一个角色（存在 `params.roleLib`）：主图作为参考图进生成输入。 */
export type CanvasRoleCharacter = {
  kind: "marketing_character" | "asset";
  id: string;
  name: string;
  /** 主图的工作区媒体 ID；没有图的角色不收（选的时候就拦下并说明）。 */
  mediaId: string;
};

/**
 * 读出节点上选中的角色。⚠️ 老画布的 `roleLib` 存的是画布节点 ID（字符串，老角色库选的是画布上的图片节点，
 * 那些图片节点当时就已经连成了线），这里只认对象形态，字符串一律跳过 —— 线还在，不会因此丢参考。
 */
export function canvasRoleCharacters(value: unknown): CanvasRoleCharacter[] {
  if (!Array.isArray(value)) return [];
  const out: CanvasRoleCharacter[] = [];
  for (const item of value) {
    const raw = asObject(item);
    const kind = raw.kind === "asset" ? "asset" : raw.kind === "marketing_character" ? "marketing_character" : undefined;
    const id = typeof raw.id === "string" ? raw.id : "";
    const mediaId = typeof raw.mediaId === "string" ? raw.mediaId : "";
    if (!kind || !id || !/^\d+$/.test(mediaId)) continue;
    if (out.some(existing => existing.kind === kind && existing.id === id)) continue;
    out.push({ kind, id, name: typeof raw.name === "string" && raw.name.trim() ? raw.name.trim() : "角色", mediaId });
  }
  return out;
}

/**
 * 读取节点上的上传参考，统一成数组。
 *
 * <p>兼容两种形态：早期存的是单个对象，放开多张后存数组。老画布的 canvas_data 不会迁移，
 * 所以这里必须同时认；只写新形态会让历史节点上的参考凭空消失。
 */
export function referenceResources(value: unknown): UploadedReference[] {
  const list = Array.isArray(value) ? value : [value];
  const resources: UploadedReference[] = [];
  for (const item of list) {
    const resource = referenceResource(item);
    // 同一份参考只留一条：重复的 id 会在生成输入里占两个角标，上游也会收到两张一样的图
    if (resource && !resources.some(existing => existing.id === resource.id)) resources.push(resource);
  }
  return resources;
}

/**
 * 产物的工作区级媒体 ID：结果里存了就用；没存的（本字段加上之前的结果、老副本）从 `outputUrl` 取 ——
 * 契约写明产物地址就是 `/api/v1/media/{mediaId}`（`GenerationOutput.url`）。
 */
export function outputMediaIdOf(generation?: GenerationView): string | undefined {
  if (!generation) return undefined;
  if (generation.mediaId) return String(generation.mediaId);
  const match = /^\/api\/v1\/media\/(\d+)(?:[?#].*)?$/.exec(generation.outputUrl ?? "");
  return match?.[1];
}

/**
 * 节点类型 → 它作为生成输入时的媒体角色（DRT-16）。前端**只此一处**，对应后端 `catalog/domain.MediaRoleOf`（DRD-20）。
 *
 * 3D 导演台给下游的是输出机位的最新截图：模式判断、报价计数、就绪检查都按图片算。其余类型原样返回。
 * ⚠️ 各写一份映射的话，「连线时说能走全能参考、执行时按图生视频跑」，两边各自看都没错（CLAUDE.md §5.8）。
 */
export function canvasMediaRole(nodeType: string): string {
  if (nodeType === "director") return "image";
  // 智能剪辑给下游的是最近一次导出的成片（SCD-50）
  if (nodeType === "smartcut") return "video";
  return nodeType;
}

/**
 * 上游还没就绪时发送按钮上的提示（DR-F-03）。导演台没有「生成」可等，缺的是一张截图 ——
 * 说「请先完成上游节点」的话，用户会去找一个并不存在的生成按钮。
 */
export function upstreamNotReadyText(source: Pick<CanvasNode, "data"> | undefined): string {
  if (source?.data.nodeType === "director") return `请先在导演台截图：${source.data.title}`;
  // 智能剪辑没有「生成」可等，缺的是一次导出（SCD-50）
  if (source?.data.nodeType === "smartcut") return `请先在智能剪辑里导出成片：${source.data.title}`;
  return `请先完成上游节点：${source?.data.title ?? "引用节点"}`;
}

const MEDIA_NODE_TYPES = ["image", "video", "audio"];

/**
 * `smartcutOutputs`：智能剪辑节点 → 最近一次成功导出的成片媒体 ID（`useSmartcuts().outputs`）。成片不进节点 params
 * （SCK-13：导出在 Worker 里异步完成，写进节点的值会被画布的自动保存覆盖），所以由调用方从摘要带进来。
 */
export function generationInputs(
  nodes: CanvasNode[],
  edges: CanvasEdge[],
  targetId: string,
  smartcutOutputs: Readonly<Record<string, string>> = {}
): GenerationInput[] {
  return orderedTargetEdges(edges, targetId).map((edge) => {
    const source = nodes.find(node => node.id === edge.source);
    const type = source?.data.nodeType ?? "text";
    if (type === "smartcut") {
      // 智能剪辑（SCD-50）：按「参考」声明最近一次导出的成片，服务端校验归属与真实文件头。
      // 还没导出时不带 referenceId → 未就绪：不受理、不扣费，发送按钮说「请先在智能剪辑里导出成片」。
      const outputMediaId = smartcutOutputs[edge.source] ?? "";
      return {
        kind: canvasMediaRole(type),
        sourceNodeId: edge.source,
        order: edge.data?.order ?? 1,
        ...(/^\d+$/.test(outputMediaId) ? { referenceId: outputMediaId } : {})
      };
    }
    if (type === "director") {
      // 3D 导演台（DR-F-03、DRD-44）：给下游的是输出机位的最新截图 `params.outputMediaId`（截图时由画布写回），
      // 按「参考」声明，服务端校验归属与真实文件头。还没有截图时不带 referenceId → 未就绪：不受理、不扣费。
      const outputMediaId = String(source?.data.params.outputMediaId ?? "").trim();
      return {
        kind: canvasMediaRole(type),
        sourceNodeId: edge.source,
        order: edge.data?.order ?? 1,
        ...(/^\d+$/.test(outputMediaId) ? { referenceId: outputMediaId } : {})
      };
    }
    const generation = source?.data.generation;
    const succeeded = generation?.status === "success";
    // 文本节点的生成结果被用户改过：按改过的文字作输入，不再引用那次生成
    const editedText = type === "text" && succeeded ? editedGenerationText(source?.data.params, generation?.id) : undefined;
    const generationId = succeeded && generation?.id && editedText === undefined ? generation.id : undefined;
    // ⚠️ 副本里的节点：复制时 `result.id` 被解绑成 `sourceGenerationId`（GAP-16），生成 ID 只能在
    //    原项目里当输入。但产物是**工作区级的媒体**，按媒体 ID 声明成「参考」照样成立（服务端校验归属
    //    与真实文件头）。此前这里只认 id —— 副本里明明有图的节点永远「未就绪」：派生出来的预设节点
    //    不报价、发送按钮灰着，而画面上看不出任何原因。
    const referenceId = succeeded && !generationId && MEDIA_NODE_TYPES.includes(type)
      ? outputMediaIdOf(generation)
      : undefined;
    const value = editedText ?? source?.data.params.text ?? source?.data.params.prompt;
    const text = !generationId && ["text", "script"].includes(type) && typeof value === "string"
      ? value.trim()
      : "";
    const materialCode = ["style", "effect"].includes(type)
      ? String(source?.data.params.materialCode ?? source?.data.params.code ?? "").trim()
      : "";

    return {
      // ⚠️ 受理侧的字段是 **`kind`** 与 **`sourceNodeId`**
      //    （老后端是 `type` / `nodeId`）。这是**所有生成输入的唯一构造处**，
      //    改在这里就覆盖了全部调用点 —— 各调用点自己拼的话，漏掉一处
      //    的表现是"这一类节点的输入服务端收不到"，而它不报错：
      //    服务端读到空 kind，报的是「输入类型非法」，指向输入内容而不是字段名。
      kind: type,
      sourceNodeId: edge.source,
      order: edge.data?.order ?? 1,
      ...(generationId ? { generationId } : {}),
      ...(referenceId ? { referenceId } : {}),
      ...(text ? { text } : {}),
      ...(materialCode ? { materialCode } : {})
    };
  });
}

/** 受理请求里的一条输入 —— 形状以契约为准（`resolve-inputs` / `POST /generations` 的 inputs）。 */
export type AcceptInput = {
  kind: "text" | "generation" | "reference" | "material";
  order: number;
  sourceNodeId?: string;
  generationId?: string;
  referenceId?: string;
  text?: string;
  materialCode?: string;
  materialType?: string;
};

/**
 * 画布输入 → 受理契约。**映射只在这一处**。
 *
 * ⚠️ 契约的 kind 是**来源类型**：`text | generation | reference | material | voice`。画布这边的
 *    `kind` 是节点的媒体类型，原样发过去服务端报「不支持的输入类型: image」—— **所有带图/视频/音频
 *    输入的生成**（连线的图生图、上传的参考图、图生视频、首尾帧、全能参考）在受理时全部被拒，
 *    而报价一切正常（报价只数个数，不解析输入），在点「发送」之前看不出任何异常。
 *
 * 有 generationId → `generation`；有 referenceId → `reference`（上传的参考、副本里沿用的产物）；
 * 文本 → `text`；风格/特效 → `material`，⚠️ 没选素材的**丢掉而不是发空码**（空码会让整个节点受理失败）。
 */
export function acceptInputsOf(inputs: readonly GenerationInput[]): AcceptInput[] {
  const result: AcceptInput[] = [];
  for (const input of inputs) {
    const base = { order: input.order, ...(input.sourceNodeId ? { sourceNodeId: input.sourceNodeId } : {}) };
    if (["style", "effect"].includes(input.kind)) {
      if (input.materialCode) result.push({ ...base, kind: "material", materialCode: input.materialCode, materialType: input.kind });
    } else if (input.generationId) {
      result.push({ ...base, kind: "generation", generationId: input.generationId });
    } else if (input.referenceId) {
      result.push({ ...base, kind: "reference", referenceId: input.referenceId });
    } else if (input.text?.trim()) {
      result.push({ ...base, kind: "text", text: input.text.trim() });
    }
    // 其余（上游还没产出）不发：发出去也只会得到一条「未就绪」，而发送按钮本就在就绪前灰着。
  }
  return result;
}

/**
 * 报价用的「每种角色几条」—— 与服务端受理时的计数一致（`canvas/app.RoleCountsOf`）：
 * 素材不算媒体；文本（含脚本节点给的文本）算 `text`；其余按媒体类型。
 */
export function roleCountsOf(inputs: readonly GenerationInput[]): Record<string, number> {
  const counts: Record<string, number> = {};
  for (const input of inputs) {
    if (["style", "effect"].includes(input.kind)) continue;
    const role = input.kind === "script" ? "text" : input.kind;
    counts[role] = (counts[role] ?? 0) + 1;
  }
  return counts;
}

export function scriptCanvasReferences(nodes: CanvasNode[], edges: CanvasEdge[], targetId: string): ScriptCanvasReference[] {
  return orderedTargetEdges(edges, targetId).flatMap((edge, index) => {
    const source = nodes.find(node => node.id === edge.source);
    const type = source?.data.nodeType;
    if (!source || (type !== "text" && type !== "image")) return [];
    const generation = source.data.generation;
    const value = editedGenerationText(source.data.params, generation?.id) ?? generation?.outputText ?? source.data.params.text ?? source.data.params.prompt;
    const text = typeof value === "string" ? value.trim() : "";
    const generationId = generation?.status === "success" && generation.id ? generation.id : undefined;
    const ready = type === "text" ? Boolean(text) : Boolean(generationId);
    return [{
      edgeId: edge.id,
      nodeId: source.id,
      type,
      title: source.data.title,
      text: text || undefined,
      generationId,
      order: edge.data?.order ?? index + 1,
      previewUrl: generation?.previewUrl,
      ready
    } satisfies ScriptCanvasReference];
  });
}

/**
 * 发给后端的上游引用：只丢掉还没就绪的（文本没内容、图片没出图），
 * 已就绪的一律带上，不按入口模式筛。用户连了线就是想让它参与生成。
 */
export function scriptRequestReferences(references: ScriptCanvasReference[]): ScriptInputReference[] {
  return references.filter(reference => reference.ready).map(({ nodeId, type, title, text, generationId }) => ({
    nodeId,
    type,
    title,
    text,
    generationId
  }));
}

/**
 * 入口模式要求的"主源"引用，只用于就绪校验（够不够、生成完没有）。
 *
 * <p>不要拿它当请求内容 —— 那会把 manual/direct 模式下用户明明连上的上游整批丢掉：
 * 连线画着、界面显示已连接，数据却没进请求体，模型只看到一句"根据引用的文本…"
 * 却不知道文本是什么，且全程没有任何提示。请求内容用 {@link scriptRequestReferences}
 * 传全部已连接的上游。
 */
export function scriptReferencesForEntryMode(
  references: ScriptCanvasReference[],
  entryMode: ScriptEntryMode | ""
) {
  if (entryMode === "text_reference") return references.filter(reference => reference.type === "text");
  if (entryMode === "character_reference") return references.filter(reference => reference.type === "image");
  return [];
}

export function scriptHasStoryboard(params: Record<string, unknown>) {
  const shotCount = Number(params.scriptShotCount ?? 0);
  return Number.isFinite(shotCount) && shotCount > 0;
}

/**
 * 开启"翻译并优化提示词"时给剧情文本加的前缀。
 * 面板与画布两处都要用，务必共用同一份，避免改一处漏一处。
 */
export function scriptTranslatedInput(text: string, translate: boolean) {
  const value = text.trim();
  if (!translate || !value) return value;
  return `请先将以下内容准确转换为适合影视分镜生成的中文提示词，再据此生成分镜：\n${value}`;
}

export function scriptCanGenerateFromPanel(
  entryMode: ScriptEntryMode | "",
  model: string,
  sourceReady: boolean,
  inputText: string
) {
  if (!model.trim()) return false;
  if (!entryMode || entryMode === "direct") return Boolean(inputText.trim());
  if (entryMode === "text_reference") return sourceReady;
  if (entryMode === "character_reference") return sourceReady && Boolean(inputText.trim());
  return false;
}

export function scriptEntryMode(
  params: Record<string, unknown>,
  referenceTypes: string[] = []
): ScriptEntryMode | "" {
  const mode = params.scriptEntryMode;
  if (mode === "direct" || mode === "text_reference" || mode === "character_reference" || mode === "manual") {
    return mode;
  }
  const sourceType = scriptSourceType(params);
  if (sourceType === "manual") return "manual";
  if (sourceType === "character") return "character_reference";
  if (sourceType === "script") return referenceTypes.includes("text") ? "text_reference" : "direct";
  return "";
}

/**
 * Keep the backend source type explicit at the canvas boundary. Older nodes
 * did not persist sourceType, so a script id still falls back to the manual
 * editor for backwards compatibility.
 */
export function scriptSourceType(params: Record<string, unknown>) {
  const sourceType = params.sourceType;
  if (sourceType === "script" || sourceType === "character" || sourceType === "manual") return sourceType;
  return params.scriptId ? "manual" : "";
}

/**
 * Manual scripts are valid workspaces before their first shot is saved. AI
 * entries are only openable after the model has returned and persisted shots.
 */
export function scriptCanOpen(params: Record<string, unknown>) {
  const scriptId = String(params.scriptId ?? "").trim();
  if (!scriptId) return false;
  return scriptSourceType(params) === "manual" || scriptHasStoryboard(params);
}

const SCRIPT_ENTRY_PARAM_KEYS = new Set([
  "sourceType",
  "scriptEntryMode",
  "sourceText",
  "sourceReferences",
  "model",
  "translatePrompt",
  "scriptEntryInitialized",
  "scriptStatus",
  "scriptStep",
  "scriptVersion",
  "scriptShotCount",
  "scriptPromptReady",
  "scriptImagePromptReady",
  "scriptVideoPromptReady",
  "scriptStudioOpened",
  "scriptAutoGeneratePending"
]);

export function resetScriptEntryParams(params: JsonObject): JsonObject {
  return Object.fromEntries(
    Object.entries(params).filter(([key]) => !SCRIPT_ENTRY_PARAM_KEYS.has(key))
  );
}

export function detachScriptSourceParams(params: JsonObject): JsonObject {
  const { scriptSourceNode: _scriptSourceNode, scriptNodeId: _scriptNodeId, roleIndex: _roleIndex, ...rest } = params;
  return rest;
}

export function scriptEntryResetPlan(nodes: CanvasNode[], edges: CanvasEdge[], removedNodeId: string) {
  const removed = nodes.find(node => node.id === removedNodeId);
  if (!removed) return { targetIds: [] as string[], sourceIds: [] as string[] };

  if (removed.data.nodeType === "script") {
    return {
      targetIds: [] as string[],
      sourceIds: nodes
        .filter(node => node.data.params.scriptSourceNode === true
          && String(node.data.params.scriptNodeId ?? "") === removedNodeId)
        .map(node => node.id)
    };
  }

  const targetIds = [...new Set(edges
    .filter(edge => edge.source === removedNodeId)
    .map(edge => nodes.find(node => node.id === edge.target))
    .filter((target): target is CanvasNode => target !== undefined
      && target.data.nodeType === "script"
      && !String(target.data.params.scriptId ?? "").trim()
      && ((target.data.params.sourceType === "script" && removed.data.nodeType === "text")
        || (target.data.params.sourceType === "character"
          && removed.data.nodeType === "image"
          && !edges.some(edge => edge.target === target.id
            && edge.source !== removedNodeId
            && nodes.find(node => node.id === edge.source)?.data.nodeType === "image"))))
    .map(target => target.id))];
  return {
    targetIds,
    sourceIds: [] as string[]
  };
}

export function normalizeEdgeOrders(edges: CanvasEdge[]): CanvasEdge[] {
  const orders = new Map<string, number>();
  for (const targetId of new Set(edges.map(edge => edge.target))) {
    orderedTargetEdges(edges, targetId).forEach((edge, index) => orders.set(edge.id, index + 1));
  }
  return edges.map((edge) => {
    const order = orders.get(edge.id) ?? 1;
    return edge.data?.order === order
      ? edge
      : { ...edge, data: { ...edge.data, order } };
  });
}

export function generationInputReady(input: GenerationInput) {
  if (["style", "effect"].includes(input.kind)) return true;
  if (input.generationId || input.referenceId) return true;
  return ["text", "script"].includes(input.kind) && Boolean(input.text?.trim());
}

export function operationForNode(node: CanvasNode, edges: CanvasEdge[]) {
  if (node.data.nodeType === "video") {
    // 视频以选中的 mode 为准。⚠️ `image_reference` 是画布上的**模式名**，
    // 不是操作码 —— 直接发给后端会报「操作不存在」。
    const mode = String(node.data.params.mode || "text2video");
    return mode === "image_reference" ? "img2video" : mode;
  }
  // 图片工具子节点（docs/agent/imagetools，2026-10-31 方案变更）：四个工具都是图片模型的图生图，操作码一律 img2img，
  // 工具身份只看 `params.tool`（服务端按它拼提示词）。⚠️ 不能落到下面的「入边 operation」：老节点的入边记的是工具码。
  if (node.data.nodeType === "image" && ["multi_angle", "relight", "expression_edit", "upscale"].includes(String(node.data.params.tool ?? ""))) {
    return "img2img";
  }
  if (node.data.nodeType === "image"
    && referenceResources(node.data.params.reference).some(item => item.type === "image")) {
    return "img2img";
  }
  const incoming = orderedTargetEdges(edges, node.id);
  if (incoming[0]?.data?.operation) return incoming[0].data.operation;
  // ⚠️ **默认操作码在新系统里改了名**：
  //      `text_polish` → **`text2text`**（新系统把润色/扩写/续写合成一个）
  //      `tts`         → **`text2audio`**
  //    用老码的话 `operation` 算出来是一个**库里不存在的操作**，
  //    于是报价与受理都失败 —— 而界面上的表现是
  //    **消耗显示「—」、发送按钮一直灰着**，不报任何错。
  return {
    text: "text2text",
    image: "text2img",
    audio: "text2audio"
  }[node.data.nodeType];
}

export function nextInputOrder(edges: CanvasEdge[], targetId: string) {
  return orderedTargetEdges(edges, targetId).length + 1;
}

/**
 * 连线校验的结论（DRB-1）：返回拒绝原因，允许时为 null。
 *
 * ⚠️ `valid:false` 是 **HTTP 200**（一次询问，不是失败的请求）。只 catch 异常的话，不合规则的线照样落下、
 *    节点照样建出来，要等到生成时才报错 —— 而那时用户早已忘了是哪条线的问题。
 *    缺 `valid` 的响应也按拒绝处理：放行的话，一次契约漂移就会让所有连线的校验静默失效。
 */
export function connectionRejection(
  result: Partial<ApiComponents["schemas"]["CanvasConnectionValidateResponse"]> | null | undefined
): string | null {
  if (result?.valid === true) return null;
  return result?.reason?.trim() || "此连接不受支持";
}

/**
 * 报价被拒的原因（DRT-25，DR-F-128）：只有 422 的业务拒绝（模型不收这种输入、没配价、等级不够…）原样给出，
 * 其余（断网、5xx）返回空串 —— 报价只是预估，一次网络抖动不该在节点上挂一句红字。
 *
 * ⚠️ 原因要原样显示：报价已经说清是哪种输入不收（例如预演视频连到不收参考视频的模型），
 *    节点上只显示「—」的话，用户点了生成才在一个转瞬即逝的提示里看到同一句话。
 */
export function estimateRejectionText(cause: unknown): string {
  if (typeof cause !== "object" || cause === null) return "";
  const { statusCode, message, data } = cause as { statusCode?: unknown; message?: unknown; data?: { code?: unknown } };
  const code = typeof data?.code === "number" ? data.code : undefined;
  const rejected = statusCode === 422 || (code !== undefined && code >= 42200 && code < 42300);
  return rejected && typeof message === "string" ? message.trim() : "";
}

type ConnectionTargetNodeType = {
  code?: string;
  kind?: string;
};

/**
 * 一条连线规则 —— **字段名由新后端决定**。
 *
 * ⚠️ 与老后端**四个字段全都不同**：
 *   `source`→`sourceType` · `target`→`targetType`
 *   `allowed`→`isAllowed` · `operation`→`defaultOperation`
 *
 * 读错的表现是**画布上一条线都连不出来**，而且不报错：
 * `rule.source` 恒为 undefined，过滤后规则集为空，于是每个目标类型
 * 都被判成"不允许"。界面上就是所有创建按钮都灰着。
 *
 * 老字段名保留只为兼容尚未重接的端点；**新代码一律用 `ruleOf()` 取值**。
 */
type ConnectionTargetRule = {
  sourceType?: string;
  targetType?: string;
  isAllowed?: boolean;
  defaultOperation?: string;
  /** 老后端字段，逐步移除 */
  source?: string;
  target?: string;
  allowed?: boolean;
  operation?: string;
};

/** 把两种字段名归一到一处 —— 各处自己 `??` 的话，漏掉一处就是一条连不上的线。 */
export function ruleOf(rule: ConnectionTargetRule) {
  return {
    source: rule.sourceType ?? rule.source,
    target: rule.targetType ?? rule.target,
    allowed: rule.isAllowed ?? rule.allowed ?? false,
    operation: rule.defaultOperation ?? rule.operation,
  };
}

/**
 * 图片预设的字段归一 —— 同 `ruleOf`，**只此一处**。
 *
 * ⚠️ 老后端：`{id, group, params:{presetCode}}`；新后端：**`{code, presetGroup}`**
 *    （预设码升到了顶层，分组字段避开了 PG 保留字）。
 * ⚠️ 读错的表现不是报错，是预设下拉里**每一项的 key 都是 `"undefined"`**
 *    （互相顶掉）、分组全部落进"其他"、选中态永远对不上。
 */
export type PresetLike = {
  code?: unknown;
  id?: unknown;
  name?: unknown;
  presetGroup?: unknown;
  group?: unknown;
  params?: unknown;
  dimensions?: unknown;
  sort?: unknown;
};

export function presetCodeOf(preset: PresetLike): string {
  const fromParams = asObject(preset.params).presetCode;
  return String(preset.code ?? fromParams ?? preset.id ?? "");
}

export function presetGroupOf(preset: PresetLike): string {
  return String(preset.presetGroup ?? preset.group ?? "");
}

/**
 * 这个预设是不是「玩法」—— 出现在「图片预设」弹窗与工具条「九宫格」下拉里的那 15 个（老系统定稿）。
 *
 * ⚠️ `tool` 组是 P09 的**图片加工工具**（涂抹重绘、抠图、高清放大……）：它们对应工具条上的
 *    按钮，涂抹重绘还要蒙版编辑器，放进选择器里点一下是用不对的。不排掉的话九个工具全落进
 *    「其他」，选择器里还会出现两个「角色三视图」。判据只在这里，两个选择器共用。
 */
export function isPickerPreset(preset: PresetLike): boolean {
  return presetGroupOf(preset) !== "tool";
}

/**
 * 平铺列表（工具条「九宫格」下拉）里的次序：照老系统**全局按 sort**
 * （老查询 `order by sort, id`：分镜叙事 → 质感调节 → 空间与机位 → 设定图）。
 *
 * ⚠️ 新后端按 `preset_group, sort` 下发 —— 那是给分组弹窗用的，分组弹窗按组取、组内次序两种排法一样；
 *    平铺列表直接用的话，设定图会因为组名的字母序排到最前面。
 */
export function presetsInMenuOrder<T extends PresetLike>(presets: readonly T[]): T[] {
  const rank = (preset: T) => {
    const value = Number(preset.sort);
    return Number.isFinite(value) ? value : Number.POSITIVE_INFINITY;
  };
  return [...presets].sort((a, b) => rank(a) - rank(b) || presetCodeOf(a).localeCompare(presetCodeOf(b)));
}

/**
 * 这个节点类型能不能发起生成。
 *
 * ⚠️ 新后端 `generator`，老后端 `generate` —— 两个都认。
 * 判据放一处：各处自己比字符串的话，漏改一处就是"这类节点点不动"，
 * 而它不报错（按钮只是灰着）。
 */
export function isGeneratorKind(kind?: string): boolean {
  return kind === "generator" || kind === "generate";
}

export function connectionTargetOptions<T extends ConnectionTargetNodeType>(
  sourceType: string,
  nodeTypes: T[],
  rules: ConnectionTargetRule[]
) {
  const sourceRules = new Map(
    rules
      .map(ruleOf)
      .filter(rule => rule.source === sourceType && rule.target)
      .map(rule => [rule.target as string, rule])
  );
  return nodeTypes
    // ⚠️ 新后端的 kind 是 **`generator`**（老后端 `generate`）。
    //    判错的表现是**可连接的节点类型一个都列不出来**，而不报错。
    .filter(nodeType => isGeneratorKind(nodeType.kind) && Boolean(nodeType.code))
    .map((nodeType) => {
      const rule = sourceRules.get(nodeType.code as string);
      return {
        nodeType,
        allowed: rule?.allowed === true,
        operation: rule?.allowed ? rule.operation : undefined
      };
    });
}

/**
 * 从一个上游拖出新视频节点时的默认模式。
 * ⚠️ 从导演台发布的**预演**（`directorSource.kind === "preview"`）默认全能参考，不是视频源通常的视频生视频（DR-F-128）：
 *    预演是给模型的运镜与走位参考，不是要被改写的那段视频。
 */
export function connectedVideoMode(targetType: string, operation: string | undefined, sourceType: string, sourceParams?: Record<string, unknown>) {
  if (targetType !== "video") return undefined;
  const source = sourceParams?.directorSource;
  const preview = Boolean(source && typeof source === "object" && (source as { kind?: unknown }).kind === "preview");
  if (preview && (!operation || operation === "video2video")) return "ref2video";
  if (["text2video", "img2video", "first_last_frame", "ref2video", "video2video"].includes(operation ?? "")) {
    return operation;
  }
  return {
    text: "text2video",
    image: "img2video",
    audio: "ref2video",
    video: "video2video"
  }[canvasMediaRole(sourceType)];
}

export function videoModeForInputs(currentMode: string, inputTypes: string[]) {
  // 先按媒体角色归一（导演台按图片算，DRT-16）：直接过滤节点类型的话导演台上游会被整个丢掉
  const runtimeTypes = inputTypes.map(canvasMediaRole).filter(type => ["text", "image", "video", "audio"].includes(type));
  if (!runtimeTypes.length) return "text2video";
  if (isVideoModeAvailable(currentMode, runtimeTypes)) return currentMode;
  if (runtimeTypes.length === 1 && runtimeTypes[0] === "image") return "img2video";
  if (runtimeTypes.length === 2 && runtimeTypes.every(type => type === "image")) return "first_last_frame";
  // 单独一段视频仍是视频生视频；视频与图片 / 音频混在一起时只有全能参考能收（DRK-12）。
  if (runtimeTypes.length === 1 && runtimeTypes[0] === "video") return "video2video";
  if (runtimeTypes.every(type => ["text", "image", "audio", "video"].includes(type))) {
    return runtimeTypes.every(type => type === "text") ? "text2video" : "ref2video";
  }
  return currentMode;
}

export function connectedNodePosition(point: { x: number; y: number }) {
  return { x: Math.round(point.x + 72), y: Math.round(point.y - 110) };
}

export function imagePreviewHeight(frameWidth: number, naturalWidth: number, naturalHeight: number) {
  const minimum = 180;
  if (![frameWidth, naturalWidth, naturalHeight].every(value => Number.isFinite(value) && value > 0)) {
    return minimum;
  }
  return Math.round(Math.min(520, Math.max(minimum, frameWidth * naturalHeight / naturalWidth)));
}

/** 视频节点（有产出时）摆放的框：宽不超过 390（即视频节点的默认宽度）、高不超过 480。 */
export const VIDEO_NODE_FRAME = { maxWidth: 390, maxHeight: 480, minWidth: 220, minHeight: 150 } as const;

/**
 * 视频节点按产物的**真实比例**摆：竖屏就是竖的卡片（9:16 → 270×480），横屏 16:9 → 390×219。
 * 此前一律是 390×220 的横框，竖屏片子缩在中间、两边留白。
 *
 * 宽高不知道时按 16:9（就是此前的固定尺寸）。特别细长的比例由最小宽 / 最小高兜住，剩下的由
 * `object-fit: contain` 留边 —— 卡片窄到放不下标题、或扁到看不清画面，都比留一点边糟。
 */
export function videoPreviewFrame(width?: number, height?: number): { width: number; height: number } {
  const known = [width, height].every(value => typeof value === "number" && Number.isFinite(value) && value > 0);
  const ratio = known ? (width as number) / (height as number) : 16 / 9;
  let frameWidth: number = VIDEO_NODE_FRAME.maxWidth;
  let frameHeight = frameWidth / ratio;
  if (frameHeight > VIDEO_NODE_FRAME.maxHeight) {
    frameHeight = VIDEO_NODE_FRAME.maxHeight;
    frameWidth = frameHeight * ratio;
  }
  return {
    width: Math.round(Math.max(VIDEO_NODE_FRAME.minWidth, frameWidth)),
    height: Math.round(Math.max(VIDEO_NODE_FRAME.minHeight, frameHeight))
  };
}

/**
 * 画幅参数（`"9:16"`）→ 宽高比例；自适应、空值、写坏了都返回 undefined。
 * 视频元数据到达之前，节点先按生成时选的画幅摆，免得一打开画布先是横框、再跳成竖的。
 */
export function aspectFromRatio(value: unknown): { width: number; height: number } | undefined {
  const text = typeof value === "string" ? value : optionValue(value as ControlOption);
  const match = /^\s*(\d+(?:\.\d+)?)\s*:\s*(\d+(?:\.\d+)?)\s*$/.exec(text);
  if (!match) return undefined;
  const width = Number(match[1]);
  const height = Number(match[2]);
  return width > 0 && height > 0 ? { width, height } : undefined;
}

const GENERATION_STATUS_LABELS: Record<string, string> = {
  queued: "排队中",
  running: "生成中",
  success: "已完成",
  failed: "生成失败",
  cancelled: "已取消",
  // 轮询次数用完还没落定（asyncState.pollExhausted）：服务端多半还在跑，不是失败
  unknown: "处理中"
};

/**
 * 节点标题栏右侧的生成状态（2026-11-01）。此前直接显示服务端的状态词（running、success……），英文、生成中还常挂着「0%」。
 * - 生成中只在真有进度（0 < progress < 100）时带百分比：文本模型一次出完整段、中途不报进度；
 * - 本页点取消落成 `failed + errorCode=cancelled`（stores/generation），显示「已取消」而不是「生成失败」；
 * - 词表外的值原样显示（同 groupItemStatusLabel：配置可以比前端新，显示一个英文词好过显示错的中文）。
 */
export function generationStatusLabel(generation: { status?: string; progress?: number; errorCode?: string } | undefined): string {
  const status = generation?.status;
  if (!status) return "";
  if (status === "failed" && generation.errorCode === "cancelled") return GENERATION_STATUS_LABELS.cancelled;
  const label = GENERATION_STATUS_LABELS[status] ?? status;
  const progress = Number(generation.progress);
  return status === "running" && progress > 0 && progress < 100 ? `${label} ${Math.round(progress)}%` : label;
}

/**
 * 节点生成中的文案（2026-11-01）：`head` 是「排队中 / 生成中」，`detail` 是进度或已经等了多久。
 * 文本节点用在卡片底部的胶囊上，图片 / 视频 / 音频节点用在卡片中间的状态层上。
 *
 * ⚠️ 此前写死「生成中 0%」：文本模型一次出完整段、中途不报进度，不少图片 / 视频模型也只报开始和结束，
 *    服务端的进度从头到尾都是 0，看上去像卡住了。改成真有进度才显示百分比，否则显示等了多久
 *    （「12 秒」「1 分 05 秒」），不满 1 秒不显示。
 * `waitedMs` 从这张卡看到「排队 / 生成中」时开始算（刷新页面后从头算）。
 */
export function generatingLabel(status: string | undefined, progress: number | undefined, waitedMs: number): { head: string; detail: string } {
  const head = status === "queued" ? "排队中" : "生成中";
  const percent = Number(progress);
  if (status !== "queued" && percent > 0 && percent < 100) return { head, detail: `${Math.round(percent)}%` };
  const seconds = Number.isFinite(waitedMs) && waitedMs > 0 ? Math.floor(waitedMs / 1000) : 0;
  if (seconds < 1) return { head, detail: "" };
  if (seconds < 60) return { head, detail: `${seconds} 秒` };
  return { head, detail: `${Math.floor(seconds / 60)} 分 ${String(seconds % 60).padStart(2, "0")} 秒` };
}

/** 视频节点控制条上的时间（2026-11-01）：`0:05`、`12:30`、`1:02:03`。按秒向下取整；非法值、负数按 0。 */
export function formatVideoClock(seconds: number): string {
  const total = Number.isFinite(seconds) && seconds > 0 ? Math.floor(seconds) : 0;
  const hours = Math.floor(total / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const rest = String(total % 60).padStart(2, "0");
  return hours ? `${hours}:${String(minutes).padStart(2, "0")}:${rest}` : `${minutes}:${rest}`;
}

/**
 * 进度条上按下 / 拖到的横坐标 → 播放时间（秒），夹在 [0, duration] 之内。
 * 轨道量不到宽度、时长未知（元数据没到、直播流是 Infinity）时返回 0。
 * 横坐标与轨道都是屏幕坐标（getBoundingClientRect），画布缩放过也照样对得上。
 */
export function videoSeekTime(clientX: number, track: { left: number; width: number }, duration: number): number {
  if (!(track.width > 0) || !Number.isFinite(duration) || duration <= 0) return 0;
  const ratio = Math.min(1, Math.max(0, (clientX - track.left) / track.width));
  return ratio * duration;
}

/**
 * 截帧建出来的图片节点放在哪（2026-11-01）：视频节点右侧一个身位（与派生节点同为 +420，见 presetNodePosition），
 * 那里已经有节点（上一次截的帧、别的节点）就挪到它下面再隔 40，直到与任何节点都不重叠 —— 连截几帧是竖着排下去的。
 * `nodes` 用画布量过的尺寸（Vue Flow 的 dimensions）：存档里的节点多半没有高度，按默认高度算会叠上去。
 */
export function frameCaptureNodePosition(
  source: { x: number; y: number },
  nodes: ReadonlyArray<{ x: number; y: number; width: number; height: number }>,
  size: { width: number; height: number }
): { x: number; y: number } {
  const { x } = presetNodePosition(source);
  let y = Math.round(source.y);
  for (let guard = 0; guard <= nodes.length; guard += 1) {
    const hit = nodes.find(node => node.x < x + size.width && x < node.x + node.width && node.y < y + size.height && y < node.y + node.height);
    if (!hit) break;
    y = Math.round(hit.y + hit.height + 40);
  }
  return { x, y };
}

/** 一个可调维度（例如「人景融合」），已归一成新后端的字段名。 */
export type PresetDimension = {
  code: string;
  name: string;
  default?: string;
  options: Array<{ value: string; label: string }>;
};

/**
 * 预设维度的字段归一 —— **只此一处**。
 *
 * ⚠️ 新后端：`{code, name, default, options:[{value,label,prompt}]}`；
 *    老后端：`{key, label, …}`。只认老名字的话新后端的维度**键恒为空** ——
 *    调节面板里每一档都点不亮，`presetOptions` 永远发不出去，服务端按默认档出词，
 *    用户调了「强度」而画面没有任何变化。
 * ⚠️ 选项的 `prompt` 不带出来：拼词在服务端，前端拿着它只会诱使人在这边拼。
 */
export function presetDimensionsOf(dimensions: unknown): PresetDimension[] {
  if (!Array.isArray(dimensions)) return [];
  const result: PresetDimension[] = [];
  for (const raw of dimensions) {
    const entry = asObject(raw);
    const code = String(entry.code ?? entry.key ?? "");
    if (!code) continue;
    const options = (Array.isArray(entry.options) ? entry.options : [])
      .map(option => asObject(option))
      .map(option => ({ value: String(option.value ?? ""), label: String(option.label ?? option.value ?? "") }))
      .filter(option => option.value);
    result.push({
      code,
      name: String(entry.name ?? entry.label ?? code),
      default: typeof entry.default === "string" ? entry.default : undefined,
      options
    });
  }
  return result;
}

/** 多维预设各维度的默认档。用户没打开过面板也要有取值，否则同一个预设时有时无地缺几句。 */
export function presetOptionDefaults(dimensions: unknown): JsonObject {
  const result: JsonObject = {};
  for (const dimension of presetDimensionsOf(dimensions)) {
    const value = dimension.default ?? dimension.options[0]?.value;
    if (value) result[dimension.code] = value;
  }
  return result;
}

/**
 * 老前端的「尺寸」对象 `size:{ratio,quality,clarity}` 拆平成新后端的 `aspect_ratio` / `quality`。
 *
 * ⚠️ 新后端与上游协议读的都是平铺键。对象留着的话，用户当时选的画幅会被默认值顶掉
 *    （新面板读不到它），而那个对象还会一直跟着请求发出去。已经有平铺值的**不覆盖**。
 *    `clarity` 直接丢掉：老系统里图片的清晰度从没发给过上游，是个摆设。
 */
export function liftLegacySize(params: JsonObject): JsonObject {
  const result = cloneJson(params);
  const legacy = result.size && typeof result.size === "object" && !Array.isArray(result.size)
    ? asObject(result.size)
    : undefined;
  const ratio = legacy?.ratio ?? result.ratio;
  const quality = legacy?.quality;
  if (result.aspect_ratio === undefined && typeof ratio === "string" && ratio) result.aspect_ratio = ratio;
  if (result.quality === undefined && typeof quality === "string" && quality) result.quality = quality;
  if (legacy) delete result.size;
  delete result.ratio;
  delete result.clarity;
  return result;
}

/**
 * 打开节点时对老形态参数做一次归一（没变化时返回同一个对象，调用方据此判断要不要写回）。
 *
 * ⚠️ `video_mode`：P15-02 的一处缺陷让视频节点在打开时把模式的**默认值**写进了这个键
 *    （库里 4 个节点，全是 `text2video` 且与 `mode` 相同）。生效的键是 `mode`，
 *    这里**只清掉不采纳** —— 它记的是默认值，不是用户的选择。
 */
export function normalizeLegacyParams(params: JsonObject): JsonObject {
  const lifted = unwrapOptionObjects(liftLegacySize(params));
  delete lifted.video_mode;
  return JSON.stringify(lifted) === JSON.stringify(params) ? params : lifted;
}

/** 选项对象的键：老数据里被整个写进 params 的就是它（`{label, value}`）。 */
const OPTION_OBJECT_KEYS = new Set(["label", "value", "disabled"]);

/**
 * 把被整个写进 params 的**选项对象**还原成它的值（CD-70 §1 留下的数据）。
 *
 * ⚠️ 修复（09-23）挡住了新写入，但已经存下的没人修：库里 3 个视频节点（同一个节点被复制了两次）的
 *    时长是 `{"label":"10 秒","value":"10"}`。上游按 `paramInt` 读不到对象、回落默认 5 秒 —— 用户选的
 *    10 秒**一直没生效**，而任务是成功的。
 * ⚠️ 判定从严：只认「键全在 label/value/disabled 里、且有 value」的对象。`presetOptions`（维度 → 档位）、
 *    参考图列表这些正经的对象/数组一个都不能碰。
 */
export function unwrapOptionObjects(params: JsonObject): JsonObject {
  const result = cloneJson(params);
  for (const [key, value] of Object.entries(result)) {
    if (!value || typeof value !== "object" || Array.isArray(value)) continue;
    const keys = Object.keys(value);
    const raw = (value as JsonObject).value;
    if (!keys.includes("value") || !keys.every(item => OPTION_OBJECT_KEYS.has(item))) continue;
    if (typeof raw === "string" || typeof raw === "number") result[key] = String(raw);
  }
  return result;
}

/** 老预设参数的键 → 节点参数的键；`null` 表示丢掉（见 liftLegacySize）。 */
const PRESET_PARAM_KEYS: Record<string, string | null> = {
  presetCode: null,
  ratio: "aspect_ratio",
  clarity: null
};

/**
 * 选中一个预设：写预设码、画幅/画质、各维度的默认档。
 *
 * 画幅照老系统**以预设为准**（选「故事板」就换成 16:9）：25 宫格画在 1:1 上是挤不下的，
 * 而节点在打开时已经把默认画幅写进了 params，按「用户没设过才填」判的话预设的画幅永远落不上。
 * 这个改动是看得见的 —— 底部栏「尺寸」按钮上的摘要会跟着变，用户可以再改回去。
 */
export function applyImagePreset(
  params: JsonObject,
  preset: PresetLike
) {
  const presetParams = asObject(preset.params);
  // ⚠️ 预设码在新后端升到了**顶层 `code`**（老后端藏在 `params.presetCode`）。
  //    只读老位置的话 `result.preset` 恒为空 —— 服务端按"没选预设"处理，
  //    于是配方词一句都不拼，出来的图只是画幅变了。这正是 GAP-01 的老毛病。
  const presetCode = presetCodeOf(preset);
  const result = liftLegacySize(params);
  if (presetCode) result.preset = presetCode;

  for (const [key, value] of Object.entries(presetParams)) {
    const target = key in PRESET_PARAM_KEYS ? PRESET_PARAM_KEYS[key] : key;
    if (target) result[target] = value;
  }

  // 换预设必须重置维度：上一个预设的档位键在新预设里不存在，留着会一直发给服务端并触发未知档位告警
  const defaults = presetOptionDefaults(preset.dimensions);
  if (Object.keys(defaults).length) result.presetOptions = defaults;
  else delete result.presetOptions;
  return result;
}

/**
 * 已经出图的节点再选预设，应当派生一个下游节点，而不是覆盖原图。
 *
 * <p>预设是"基于这张图再做一次加工"，覆盖原节点等于把用户已经拿到的结果毁掉，
 * 也没法从同一张图并联出多个玩法 —— 而这正是节点画布相对图片编辑器的价值。
 * 还没出图的节点没有这个顾虑，直接在本节点应用即可。
 */
export function shouldSpawnPresetNode(node: CanvasNode) {
  if (node.data.nodeType !== "image") return false;
  const generation = node.data.generation;
  if (generation?.status !== "success") return false;
  return Boolean(generation.previewUrl || generation.outputUrl);
}

/**
 * 派生节点的初始参数：继承模型，尺寸与维度交给预设决定。
 * 刻意不继承提示词 —— 原图的描述换到新玩法多半不成立（"唐代女官"配上九宫格会变成九个女官）。
 */
export function presetNodeParams(params: JsonObject, preset: Parameters<typeof applyImagePreset>[1]) {
  const seed: JsonObject = {};
  if (typeof params.model === "string" && params.model) seed.model = params.model;
  return applyImagePreset(seed, preset);
}

/** 派生节点摆在源节点右侧一个身位，避免叠在原图上挡住产出。 */
export function presetNodePosition(point: { x: number; y: number }) {
  return { x: Math.round(point.x + 420), y: Math.round(point.y) };
}

/** 清空预设：连同维度与预设码一起摘掉，回到普通出图。尺寸保留 —— 那是用户可见的选择。 */
export function clearImagePreset(params: JsonObject) {
  const result = cloneJson(params);
  delete result.preset;
  delete result.presetOptions;
  return result;
}

export function newNodePosition(index: number) {
  const position = Math.max(1, Math.floor(index)) - 1;
  return {
    x: 180 + (position % 2) * 390,
    y: 100 + Math.floor(position / 2) * 360
  };
}

export function mergeMaterialReferences(
  params: JsonObject,
  nodes: CanvasNode[],
  edges: CanvasEdge[],
  targetId: string
) {
  const result = cloneJson(params);
  delete result.reference;
  delete result.styleId;
  delete result.effectId;
  for (const edge of orderedTargetEdges(edges, targetId)) {
    const source = nodes.find(node => node.id === edge.source);
    if (source?.data.nodeType === "style" && source.data.params.styleId != null) {
      result.styleId = String(source.data.params.styleId);
    }
    if (source?.data.nodeType === "effect" && source.data.params.effectId != null) {
      result.effectId = String(source.data.params.effectId);
    }
  }
  return result;
}

/**
 * 视频模式的中文文案。
 *
 * ⚠️ 新后端的 `/canvas/config.videoModes` 是**纯 operation 码的字符串数组**
 * （`["text2video","img2video",…]`），没有 label —— 老后端在 `param_def` 里
 * 存了 `[{label:"文生视频",value:"text2video"}]`。
 *
 * ⚠️ 认不出的码**原样显示**，不隐藏：配置可以比前端新，
 *    藏起来的话用户会以为这个模式不存在（而它其实能用）。
 */
export const VIDEO_MODE_LABELS: Record<string, string> = {
  text2video: "文生视频",
  img2video: "图生视频",
  first_last_frame: "首尾帧生视频",
  image_reference: "图片参考",
  ref2video: "全能参考",
  video2video: "视频生视频"
};

export function isVideoModeAvailable(mode: string, inputTypes: string[]) {
  const runtimeTypes = inputTypes.map(canvasMediaRole).filter(type => ["text", "image", "video", "audio"].includes(type));
  if (mode === "text2video") return runtimeTypes.every(type => type === "text");
  if (mode === "ref2video") {
    // 全能参考收参考视频（3D 导演台 DRK-12，与后端 operationSpec / videoModes 同步放开）：
    // 某个模型收不收视频由服务端报价按能力矩阵把关，前端不替模型做判断。
    return runtimeTypes.length > 0
      && runtimeTypes.every(type => ["text", "image", "audio", "video"].includes(type));
  }
  if (mode === "video2video") return runtimeTypes.length === 1 && runtimeTypes[0] === "video";
  if (mode === "first_last_frame") {
    return runtimeTypes.length === 2 && runtimeTypes.every(type => type === "image");
  }
  if (["img2video", "image_reference"].includes(mode)) {
    return runtimeTypes.length === 1 && runtimeTypes[0] === "image";
  }
  return false;
}

function reconcileValue(current: unknown, rules: JsonObject): unknown {
  if (Array.isArray(rules.options)) {
    // ⚠️ 选项可能是 `{label,value}` 对象（新后端）也可能是裸字符串（老后端）。
    //    直接 `options.includes(current)` 在对象形态下**恒为 false**，
    //    于是每次校准都把参数重置成 `options[0]` ——
    //    而那是个**对象**，接着被原样存进 `canvas_data.params` 发给后端。
    //    这是 stringify 之外的第二条写坏参数的路径。
    const values = rules.options.map(option => optionValue(option as ControlOption));
    if (current !== undefined && current !== null && values.includes(String(current))) return current;
    return rules.default ?? rules.defaultValue ?? values[0];
  }
  // ⚠️ `restrictBy` 是参数之间的约束（见 restrictionFixes），不是嵌套参数：当成嵌套参数校准的话，
  //    画幅这个字符串会被改写成 `{restrictBy:{…}}` 对象发给后端。
  const nestedRules = Object.entries(rules)
    .filter(([key, value]) => key !== "restrictBy" && typeof value === "object" && value !== null && !Array.isArray(value));
  if (!nestedRules.length) return current ?? rules.default;
  const value = asObject(current);
  for (const [key, nested] of nestedRules) value[key] = reconcileValue(value[key], asObject(nested));
  return value;
}

/**
 * 切模型时的校准规则 = **节点配置 + 新模型的覆盖**（参数合并的三层，按参数逐个合好）。
 *
 * ⚠️ 只按新模型的覆盖校准的话：新模型没给某一项覆盖时，上一个模型留下的值原样留着 ——
 *    从可灵 Omni（画幅只有 adaptive）切到 Grok，规格按钮上显示一个不在选项里的「adaptive」，发出去的也是它；
 *    从 Seedance 2.5 切过去，21:9、4 秒也一样留着。
 * 懒加载的列表（模型、预设、素材：`domainFrom`）与没有选项的控件不在这里校准。
 * 节点的默认值只在它仍是可选项时才当默认（否则校准会把一个选不了的值写回去），不然取第一个可选项。
 */
export function modelReconcileRules(
  params: ReadonlyArray<{ code?: string; config?: unknown; defaultValue?: unknown }>,
  overrides: JsonObject
): JsonObject {
  const rules: JsonObject = { ...overrides };
  for (const param of params) {
    const code = param.code ?? "";
    const base = asObject(param.config);
    if (!code || base.domainFrom) continue;
    const merged = mergeObjects(base, asObject(overrides[code]));
    if (!Array.isArray(merged.options)) continue;
    const values = merged.options.map(option => optionValue(option as ControlOption));
    const fallback = param.defaultValue === undefined || param.defaultValue === null ? "" : String(param.defaultValue);
    rules[code] = merged.default === undefined && values.includes(fallback) ? { ...merged, default: fallback } : merged;
  }
  return rules;
}

export function reconcileParams(params: JsonObject, overrides: JsonObject) {
  const result = cloneJson(params);
  for (const [code, rules] of Object.entries(overrides)) result[code] = reconcileValue(result[code], asObject(rules));
  // ⚠️ 逐项都合法、组合起来仍可能是上游不收的（1080p 下只有三种画幅）：切到这个模型时
  //    清晰度落在 1080p、画幅还是上一个模型的 21:9，所以逐项校准之后再按参数之间的约束过一遍。
  const configs = Object.fromEntries(Object.entries(overrides).map(([code, rules]) => [code, asObject(rules)]));
  return { ...result, ...restrictionFixes(result, configs) };
}

/**
 * 上游导演台带下来的镜头文字（DRT-26，DR-F-132）：下游提示词框「插入镜头文字」用，**只由用户手动插入**。
 * - 导演台节点：输出机位最新那张截图的文字（`params.captures` 里 `mediaId` 等于 `outputMediaId` 的那条）；
 * - 从导演台发布出来的节点：发布那张图时的文字（`params.directorSource.shotText`）。
 * 按连线顺序，没有文字的上游跳过。读的是节点参数里的摘要（截图那一刻的文字），不加载场景：场景后来改了也不跟着变。
 */
export function upstreamShotTextOf(
  nodes: readonly { id: string; data: { nodeType: string; title?: string; params: Record<string, unknown> } }[],
  edges: readonly CanvasEdge[],
  nodeId: string
): Array<{ nodeId: string; title: string; text: string }> {
  const out: Array<{ nodeId: string; title: string; text: string }> = [];
  for (const edge of orderedTargetEdges([...edges], nodeId)) {
    const source = nodes.find(node => node.id === edge.source);
    if (!source) continue;
    const params = source.data.params ?? {};
    let text = "";
    if (source.data.nodeType === "director") {
      const output = String(params.outputMediaId ?? "");
      const captures = Array.isArray(params.captures) ? params.captures as Array<Record<string, unknown>> : [];
      const hit = captures.find(capture => String(capture.mediaId ?? "") === output && output);
      text = typeof hit?.shotText === "string" ? hit.shotText.trim() : "";
    } else {
      const ref = params.directorSource;
      const shot = ref && typeof ref === "object" && !Array.isArray(ref) ? (ref as Record<string, unknown>).shotText : undefined;
      text = typeof shot === "string" ? shot.trim() : "";
    }
    if (text) out.push({ nodeId: source.id, title: source.data.title?.trim() || "导演台", text });
  }
  return out;
}

/** 把一段文字插到光标处（没有光标时追加到末尾）：前后补一个空格，免得和原文粘在一起。返回新文本与插入后的光标位置。 */
export function insertTextAt(value: string, insert: string, selectionStart?: number | null, selectionEnd?: number | null): { value: string; caret: number } {
  const start = typeof selectionStart === "number" && selectionStart >= 0 && selectionStart <= value.length ? selectionStart : value.length;
  const end = typeof selectionEnd === "number" && selectionEnd >= start && selectionEnd <= value.length ? selectionEnd : start;
  const before = value.slice(0, start);
  const after = value.slice(end);
  const lead = before && !/\s$/.test(before) ? " " : "";
  const trail = after && !/^\s/.test(after) ? " " : "";
  const piece = `${lead}${insert}${trail}`;
  return { value: `${before}${piece}${after}`, caret: before.length + piece.length };
}

/**
 * 文本节点上用户改过的生成结果（2026-10-30：生成结果像手写文本一样可以直接编辑）。改过之后下游拿的是改过的文字，
 * 不是那次生成的原文 —— 否则用户改了半天，接下来的生成用的还是没改之前的版本，而界面上看不出来。
 *
 * 改动记在 `params.text`，并用 `params.textEditedFor` 记下改的是哪一次生成：重新生成之后 ID 变了，
 * 旧的改动自然失效，卡片显示新结果（不会出现「重新生成了、卡片上还是我上次改的那段」）。
 */
export function editedGenerationText(params: Record<string, unknown> | undefined, generationId: string | undefined): string | undefined {
  if (!params || !generationId || params.textEditedFor !== generationId) return undefined;
  return typeof params.text === "string" ? params.text : undefined;
}

