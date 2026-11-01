import type { GroupDetail, ScriptAsset, ScriptDetail, ScriptInputReference, ScriptShot, ScriptSourceType } from "~/types/script";
import {
  groupMissingLabels,
  groupNeedsRepair,
  mergeShotResp,
  normalizeGroupDetail,
  normalizeScriptAsset,
  normalizeScriptDetail,
  planShotSync,
  rebaseScriptDetail,
  scriptCreateBody,
  storyboardBody,
  type AssetBatchItemResp,
  type ComposeResultResp,
  type GroupResp,
  type UngroupResp,
  type ScriptAssetResp,
  type ScriptDetailResp,
  type ScriptGroupResp,
  type ScriptShotResp
} from "~/utils/scriptContract";

type Extras = { name?: string; references?: ScriptInputReference[] };

/** 参考图上传的响应（`POST /media/references`）。 */
type UploadedMedia = { id: string; mime: string; sizeBytes: number };

/** 产物 → 受保护媒体地址（`GET /generations/{id}/outputs/{i}` 回 JSON，不是字节）。同一次生成的产物不会变，按 ID 缓存。 */
const outputUrlCache = new Map<string, string>();

/**
 * 已经为它取过镜号 / 资产名上下文的组（本页内每组最多取一次）。
 * ⚠️ 镜头被删掉的子项永远补不上镜号：不记的话运行中的组每 1.4 秒轮询一次就多取一次整份脚本。
 */
const labelContextLoaded = new Set<string>();

/**
 * 脚本接口的**唯一入口**（新契约，规则与理由见 `utils/scriptContract`）。
 *
 * ⚠️ 此前脚本节点面板、画布工具栏、脚本工作台各自直连老契约的路径与字段 —— 三处各抄一份，
 *    结果是创建建出节点 ID 为空的脚本、分镜拿着 `undefined` 去调用（「scriptID 非法」），
 *    工作台照老形状读详情（`assets` / `groups` 不在新详情里）当场报错，一直停在「正在恢复脚本工作台」。
 */
export function useScriptApi() {
  const api = useApi();

  async function load(scriptId: string, extras?: Extras): Promise<ScriptDetail> {
    return normalizeScriptDetail(await api<ScriptDetailResp>(`/scripts/${scriptId}`), extras);
  }

  /**
   * 工作台要的全部数据：详情 + 资产 + 组（新后端是三个端点）。
   * ⚠️ 只取详情的话资产恒为空：工作台会以为「还没提取资产」并再对账一次 —— 而对账会把还没出图的资产描述
   *    改回模板，用户刚改的那句外观就没了。
   */
  async function loadWorkbench(scriptId: string, extras?: Extras): Promise<ScriptDetail> {
    const [detail, assets, groups] = await Promise.all([
      api<ScriptDetailResp>(`/scripts/${scriptId}`),
      api<ScriptAssetResp[]>(`/scripts/${scriptId}/assets`),
      api<ScriptGroupResp[]>(`/scripts/${scriptId}/groups`)
    ]);
    return normalizeScriptDetail(detail, { ...extras, assets: assets ?? [], groups: groups ?? [] });
  }

  /**
   * 建脚本。⚠️ 同一节点再建一次，新后端回的是**已有的那条**（按节点恢复），剧情可能是旧的 ——
   * 所以生成前一律再过一次 {@link saveStory}。
   */
  async function create(input: {
    projectId: string;
    nodeId: string;
    sourceType: ScriptSourceType;
    rawInput?: string;
    name?: string;
    references?: ScriptInputReference[];
  }): Promise<ScriptDetail> {
    const resp = await api<ScriptDetailResp>("/scripts", { method: "POST", body: scriptCreateBody(input) });
    return normalizeScriptDetail(resp, { name: input.name, references: input.references });
  }

  /**
   * 把剧情写进脚本：**变了才写**，带版本号（乐观锁）。
   * ⚠️ 新后端生成分镜读的是脚本上存的剧情 —— 不先写的话，模型拿到的是上一次（甚至是空）的剧情。
   */
  async function saveStory(current: ScriptDetail, rawInput: string): Promise<ScriptDetail> {
    const text = rawInput.trim();
    if (text === (current.input.text ?? "").trim()) return current;
    const resp = await api<ScriptDetailResp>(`/scripts/${current.scriptId}`, {
      method: "PATCH",
      body: { version: current.version, rawInput: text }
    });
    return rebaseScriptDetail(current, resp);
  }

  /** 推进步骤（服务端独立校验前置条件，AV-18）。 */
  async function patchStep(current: ScriptDetail, step: number): Promise<ScriptDetail> {
    const resp = await api<ScriptDetailResp>(`/scripts/${current.scriptId}`, {
      method: "PATCH",
      body: { version: current.version, currentStep: step }
    });
    return rebaseScriptDetail(current, resp);
  }

  /** 生成分镜（同步，返回即生成完；扣费、解析失败退款都在服务端）。 */
  async function generateStoryboard(scriptId: string, modelCode: string, extraContext: string, extras?: Extras): Promise<ScriptDetail> {
    const resp = await api<ScriptDetailResp>(`/scripts/${scriptId}/storyboard`, {
      method: "POST",
      body: storyboardBody(modelCode, extraContext)
    });
    return normalizeScriptDetail(resp, extras);
  }

  /**
   * 保存镜头表：把草稿拆成逐镜的删 / 建 / 改 / 排序依次执行（新后端没有整表保存）。
   *
   * ⚠️ 中途失败时**已经做完的那几步留在服务端**（每步各自落库，没有整表事务）：
   *    调用方要重新取一次，界面才与服务端一致；错误原样抛出，让用户知道停在了哪。
   * 返回最后一次带整张表的响应（没有任何增删排序时为 undefined —— 逐镜修改的响应不带整张表）。
   */
  async function syncShots(scriptId: string, baseline: ScriptShot[], draft: ScriptShot[]) {
    const plan = planShotSync(baseline, draft);
    let latest: ScriptDetailResp | undefined;
    let serverOrder = baseline.map(shot => shot.id);
    for (const id of plan.deletes) {
      latest = await api<ScriptDetailResp>(`/scripts/${scriptId}/shots/${id}`, { method: "DELETE" });
      serverOrder = (latest.shots ?? []).map(shot => shot.id);
    }
    const realId = new Map<string, string>();
    for (const create of plan.creates) {
      const after = create.after ? (realId.get(create.after) ?? create.after) : "";
      const known = new Set(serverOrder);
      latest = await api<ScriptDetailResp>(`/scripts/${scriptId}/shots`, {
        method: "POST",
        body: { ...(after ? { afterShotId: after } : {}), durationSec: create.durationSec, description: create.description }
      });
      const created = (latest.shots ?? []).find(shot => !known.has(shot.id));
      serverOrder = (latest.shots ?? []).map(shot => shot.id);
      if (!created) throw new Error("新镜头已提交，但响应里找不到它，请刷新后检查");
      realId.set(create.draftId, created.id);
      if (Object.keys(create.rest).length) {
        await api<ScriptShotResp>(`/scripts/${scriptId}/shots/${created.id}`, {
          method: "PUT",
          body: { version: created.version, ...create.rest }
        });
      }
    }
    for (const update of plan.updates) {
      await api<ScriptShotResp>(`/scripts/${scriptId}/shots/${update.id}`, {
        method: "PUT",
        body: { version: update.version, ...update.changes }
      });
    }
    // 顺序：建镜头时已按「插在前一镜之后」落位；只有用户拖动过已有镜头时才需要整体重排
    const desired = plan.order.map(id => realId.get(id) ?? id);
    if (JSON.stringify(desired) !== JSON.stringify(serverOrder)) {
      latest = await api<ScriptDetailResp>(`/scripts/${scriptId}/shots/order`, { method: "PUT", body: { shotIds: desired } });
    }
    return { latest, plan };
  }

  /**
   * 资产对账（按镜头实体建 / 更新资产并重绑引用）。
   * ⚠️ 对账会把**还没出图、不是上传来的**资产描述改回模板（服务端规则）—— 只在真需要时调。
   */
  function reconcileAssets(scriptId: string) {
    return api<{ assets: ScriptAssetResp[]; unmatched?: string[] | null; orphan?: ScriptAssetResp[] | null }>(
      `/scripts/${scriptId}/assets/reconcile`,
      { method: "POST", body: {} }
    );
  }

  /** 编辑资产。新后端在 `/script-assets/{id}`、字段叫 `type`，返回的 `lints` 是质量提示（不拦截）。 */
  function saveAsset(asset: ScriptAsset, patch: { name?: string; description?: string }) {
    return api<ScriptAssetResp>(`/script-assets/${asset.id}`, {
      method: "PUT",
      body: { type: asset.assetType, ...patch }
    });
  }

  /**
   * 上传资产图：先经媒体域上传（它按真实文件头识别类型、登记归属），再绑到资产上。
   * ⚠️ 老后端的 `POST /scripts/{id}/assets/{assetId}/upload` 在新后端不存在。
   */
  async function uploadAssetImage(input: { projectId: string; nodeId: string; asset: ScriptAsset; file: File }) {
    const body = new FormData();
    body.append("projectId", input.projectId);
    body.append("nodeId", input.nodeId);
    body.append("mediaType", "image");
    body.append("file", input.file);
    const media = await api<UploadedMedia>("/media/references", { method: "POST", body });
    return api<ScriptAssetResp>(`/script-assets/${input.asset.id}/media`, {
      method: "PUT",
      body: { mediaId: media.id, contentType: media.mime, sizeBytes: media.sizeBytes }
    });
  }

  /** 批量生成资产图（每项独立任务、独立扣费）。`params` 只认画幅与画质。 */
  function generateAssets(scriptId: string, input: { modelCode: string; assetIds: string[]; params?: Record<string, unknown> }) {
    return api<AssetBatchItemResp[]>(`/scripts/${scriptId}/assets/generate`, { method: "POST", body: input });
  }

  /**
   * 合成提示词（`ai` 收费 / `template` 0 积分；`dryRun` 只报价）。
   * ⚠️ 请求成功 ≠ 每一条都成功：逐项结果在 `items` 里，失败的那几条各自退款。
   */
  function composePrompts(scriptId: string, input: {
    source: "ai" | "template";
    modelCode?: string;
    shotIds?: string[];
    kinds?: Array<"image" | "video">;
    force?: boolean;
    dryRun?: boolean;
  }) {
    return api<ComposeResultResp>(`/scripts/${scriptId}/prompts/compose`, { method: "POST", body: input });
  }

  /** 手写一条提示词（来源记 manual、置 ready）。带镜头版本号；每存一次版本 +1。 */
  async function savePrompt(scriptId: string, shot: ScriptShot, kind: "image" | "video", text: string) {
    const resp = await api<ScriptShotResp>(`/scripts/${scriptId}/shots/${shot.id}/prompts/${kind}`, {
      method: "PUT",
      body: { text, version: shot.contentVersion }
    });
    return mergeShotResp(shot, resp);
  }

  /**
   * 导出脚本（服务端的导出格式：显式字段、`schemaVersion`、未完成部分写进 `warnings`）。
   * ⚠️ 不要把详情接口的原样 JSON 当导出给用户 —— 那是内部结构，加一列就会漏进用户的文件。
   */
  function exportScript(scriptId: string) {
    return api<unknown>(`/scripts/${scriptId}/export`);
  }

  /**
   * 建组。⚠️ 新后端只有一个建组端点（老后端按类型三条路径），`canvasNodeId` 由前端给（同节点重复建组即恢复）。
   * 分镜图组**建组即提交**（一张几积分）；视频组先落 draft，执行时再选模型提交（一条几十积分）。
   */
  function createGroup(scriptId: string, body: {
    groupType: "asset" | "storyboard_image" | "video_generator";
    canvasNodeId: string;
    name: string;
    shotIds?: string[];
    modelCode?: string;
    params?: Record<string, unknown>;
    durations?: Record<string, number>;
  }) {
    return api<GroupResp>(`/scripts/${scriptId}/groups`, { method: "POST", body });
  }

  function getGroup(groupId: string) {
    return api<GroupResp>(`/groups/${groupId}`);
  }

  /** 按画布节点取组（节点上没有 groupId 时用，见 {@link refreshGroup}）。 */
  function getGroupByNode(projectId: string, nodeId: string) {
    return api<GroupResp>(`/projects/${projectId}/nodes/${encodeURIComponent(nodeId)}/group`);
  }

  /** 补标签用的脚本上下文：资产组要资产名、镜头组要镜号与景别。 */
  async function groupLabelContext(group: GroupDetail): Promise<{ shots?: ScriptShot[]; assets?: ScriptAsset[] }> {
    if (group.groupType === "asset") {
      const assets = await api<ScriptAssetResp[]>(`/scripts/${group.scriptId}/assets`);
      return { assets: (assets ?? []).map(asset => normalizeScriptAsset(asset)) };
    }
    return { shots: (await load(group.scriptId)).shots };
  }

  /**
   * 接口回的组 → 画布组节点要的形状：归一化 + 补镜号 / 资产名 + 补产物地址（CD-81）。
   *
   * ⚠️ 组节点的**每一条写入路径**都要过这里（轮询、刷新、执行、取消）。此前组节点自己的轮询直接把接口原样写进节点，
   *    节点上的 `groupId` 成了空串、子项没有 `itemId` —— 规则与后果见 {@link groupNeedsRepair}。
   * ⚠️ 上一次的组本身是坏的就不拿它当 `previous`：按 `itemId` 对不上，沿用不到任何东西，反倒让人以为沿用了。
   */
  async function adoptGroup(resp: GroupResp, previous?: GroupDetail): Promise<GroupDetail> {
    const before = previous && !groupNeedsRepair(previous) ? previous : undefined;
    let group = normalizeGroupDetail(resp, { previous: before });
    if (group.scriptId && groupMissingLabels(group) && !labelContextLoaded.has(group.groupId)) {
      labelContextLoaded.add(group.groupId);
      try {
        group = normalizeGroupDetail(resp, { ...(await groupLabelContext(group)), previous: before });
      } catch {
        // 取不到就按序号显示：标签是体验，不该让整个组节点刷新失败
      }
    }
    return resolveGroupOutputs(group);
  }

  /**
   * 刷新画布上的一个组节点。节点上没有 `groupId`（早先被写坏的节点，见 {@link groupNeedsRepair}）时按画布节点取 ——
   * 新后端「一个画布节点一个组」，按节点取回来的就是它。
   */
  async function refreshGroup(current: GroupDetail): Promise<GroupDetail> {
    const resp = current.groupId
      ? await getGroup(current.groupId)
      : await getGroupByNode(current.projectId, current.canvasNodeId);
    return adoptGroup(resp, current);
  }

  /** 执行（整组或指定子项）。⚠️ 新后端执行时必须给模型：组上不存模型，建组时选的记在画布节点上。 */
  function executeGroup(groupId: string, body: { modelCode: string; version: number; itemIds?: string[]; force?: boolean }) {
    return api<GroupResp>(`/groups/${groupId}/execute`, { method: "POST", body });
  }

  function cancelGroup(groupId: string, itemIds: string[] = []) {
    return api<GroupResp>(`/groups/${groupId}/cancel`, { method: "POST", body: { itemIds } });
  }

  function ungroup(groupId: string, version: number) {
    return api<UngroupResp>(`/groups/${groupId}/ungroup`, { method: "POST", body: { version } });
  }

  /** 改一个 draft 子项的参数（不给输入时服务端按当前上游重算输入 —— 这也是 stale 子项的出路）。 */
  function saveGroupItem(groupId: string, itemId: string, body: { params?: Record<string, unknown>; inputs?: unknown[] }) {
    return api<unknown>(`/groups/${groupId}/items/${itemId}`, { method: "PUT", body });
  }

  /**
   * 给成功的生成子项补上产物地址（`/api/v1/media/{id}`，受保护，界面要 fetch 成 blob）。
   * ⚠️ 取不到不抛：一张预览出不来不该让整个组节点刷新失败。
   */
  async function resolveGroupOutputs(group: GroupDetail): Promise<GroupDetail> {
    const items = await Promise.all(group.items.map(async (item) => {
      if (item.status !== "success" || !item.generationId || item.outputUrl) return item;
      let url = outputUrlCache.get(item.generationId);
      if (!url) {
        try {
          const output = await api<{ type?: string; mediaUrl?: string }>(`/generations/${item.generationId}/outputs/0`);
          url = output.mediaUrl ?? "";
          if (url) outputUrlCache.set(item.generationId, url);
        } catch {
          return item;
        }
      }
      return url ? { ...item, outputUrl: url } : item;
    }));
    return { ...group, items };
  }

  return {
    createGroup,
    getGroup,
    getGroupByNode,
    adoptGroup,
    refreshGroup,
    executeGroup,
    cancelGroup,
    ungroup,
    saveGroupItem,
    resolveGroupOutputs,
    load,
    loadWorkbench,
    create,
    saveStory,
    patchStep,
    generateStoryboard,
    syncShots,
    reconcileAssets,
    saveAsset,
    uploadAssetImage,
    generateAssets,
    composePrompts,
    savePrompt,
    exportScript
  };
}
