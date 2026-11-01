/**
 * 项目里 3D 导演台场景的摘要（DRT-16）：`sceneId → summary`。
 *
 * 用处有二：打开导演台时判断节点与场景是否一一对应（自愈，DRD-43）；节点卡判断截图是否过期（DRT-17 比指纹）。
 * 摘要**不带正文**（一个场景可达 512 KiB），正文只在编辑器里经 `stores/director` 取。
 *
 * - 画布页与各个节点卡共用一份、只请求一次（单飞，同 useMarketingConfig）；
 * - 收到 `director.scene_updated` 时**按帧更新**（`directorSummariesAfterEvent`），遇到不认识的场景才重新拉一次；
 * - 建场景 / 分叉也走这里（`create`），成功后把摘要记进来 —— 否则刚建的场景要等下一次整页拉取才认得。
 */
import type { components } from "@tabtv/api-client";
import { directorSummariesAfterEvent, directorSummaryOf, type DirectorSceneSummary } from "~/utils/directorCanvas";

type DirectorScene = components["schemas"]["DirectorScene"];
export type DirectorScenesState = "idle" | "loading" | "ready" | "error";

/** 进行中的那一次拉取（单飞）。按项目区分：切项目时不复用上一个项目的请求。 */
let inflight: { projectId: string; promise: Promise<void> } | null = null;

export function useDirectorScenes() {
  const projectId = useState<string>("director-scenes-project", () => "");
  const summaries = useState<Record<string, DirectorSceneSummary>>("director-scenes", () => ({}));
  const state = useState<DirectorScenesState>("director-scenes-state", () => "idle");

  /** 换了项目就清掉上一个项目的摘要：留着的话自愈判定会拿别的项目的场景当成本项目的。 */
  function scope(id: string) {
    if (projectId.value === id) return;
    projectId.value = id;
    summaries.value = {};
    state.value = "idle";
  }

  async function fetchList(id: string) {
    state.value = "loading";
    try {
      const response = await useApi()<{ items?: DirectorSceneSummary[] }>(`/projects/${id}/director-scenes`);
      if (projectId.value !== id) return;
      summaries.value = Object.fromEntries((response.items ?? []).map(item => [item.id, item]));
      state.value = "ready";
    } catch (cause) {
      if (projectId.value === id) state.value = "error";
      throw cause;
    }
  }

  function start(id: string): Promise<void> {
    if (inflight?.projectId === id) return inflight.promise;
    const promise = fetchList(id).finally(() => {
      if (inflight?.promise === promise) inflight = null;
    });
    inflight = { projectId: id, promise };
    return promise;
  }

  /** 首次加载：已有结论时不再请求。失败不抛（节点卡照常显示，只是判断不了过期），打开时会再拉。 */
  function load(id: string): Promise<void> {
    if (!import.meta.client || !id) return Promise.resolve();
    scope(id);
    if (state.value === "ready") return Promise.resolve();
    return start(id).catch(() => undefined);
  }

  /** 强制重新拉（打开时查不到某个场景）。失败照常抛给调用方：自愈判定不能拿一份不完整的列表下结论。 */
  function refresh(id: string): Promise<void> {
    scope(id);
    return start(id);
  }

  /** 新建 / 分叉场景（契约 createDirectorScene，按项目 + 画布节点幂等）。 */
  async function create(id: string, canvasNodeId: string, sourceSceneId?: string): Promise<DirectorScene> {
    const scene = await useApi()<DirectorScene>(`/projects/${id}/director-scenes`, {
      method: "POST",
      body: { canvasNodeId, ...(sourceSceneId ? { sourceSceneId } : {}) }
    });
    if (projectId.value === id) summaries.value = { ...summaries.value, [scene.id]: directorSummaryOf(scene) };
    return scene;
  }

  /** 画布页转来的 `director.scene_updated`（与 stores/director 的对账并列，两者互不依赖）。 */
  function applyEvent(frame: Record<string, unknown>) {
    if (!projectId.value) return;
    const result = directorSummariesAfterEvent(summaries.value, frame, projectId.value);
    if (result.unknown) {
      void start(projectId.value).catch(() => undefined);
      return;
    }
    if (result.summaries !== summaries.value) summaries.value = { ...result.summaries };
  }

  return { projectId, summaries, state, load, refresh, create, applyEvent };
}
