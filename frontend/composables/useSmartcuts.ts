import type { components } from "@tabtv/api-client";
import { smartcutOutputMediaIdOf } from "~/utils/smartcutCanvas";

/**
 * 项目里智能剪辑节点的摘要（docs/agent/smartcut SCT-23，SCD-47 / SCD-50）：`canvasNodeId → summary`。
 *
 * 节点卡（封面、迷你时间轴、状态与两个角标）、下游取输入（最近一次成功导出的成片）、打开时的自愈判定都看它。
 * 摘要不带时间线正文，正文只在剪辑台里经 `stores/smartcut` 取。
 *
 * - 画布页与各个节点卡共用一份、只请求一次（单飞，同 useDirectorScenes）；
 * - 收到 `timeline.updated` / `smartcut.updated` / `render.update` 时**只当信号**：合并 300ms 内的帧，重拉一次列表；
 * - 建时间线 / 复制也走这里（`create`），成功后把摘要记进来 —— 否则刚建的节点要等下一次整页拉取才认得。
 */

type Summary = components["schemas"]["SmartcutSummary"];
type SyncResult = components["schemas"]["SmartcutSyncResult"];
export type SmartcutsState = "idle" | "loading" | "ready" | "error";

let inflight: { projectId: string; promise: Promise<void> } | null = null;
let eventTimer: ReturnType<typeof setTimeout> | undefined;
const EVENT_TYPES = new Set(["timeline.updated", "smartcut.updated", "render.update"]);

export function useSmartcuts() {
  const projectId = useState<string>("smartcuts-project", () => "");
  const summaries = useState<Record<string, Summary>>("smartcuts", () => ({}));
  const state = useState<SmartcutsState>("smartcuts-state", () => "idle");

  /** 换了项目就清掉上一个项目的摘要：留着的话自愈判定会拿别的项目的时间线当成本项目的。 */
  function scope(id: string) {
    if (projectId.value === id) return;
    projectId.value = id;
    summaries.value = {};
    state.value = "idle";
  }

  async function fetchList(id: string) {
    state.value = "loading";
    try {
      const response = await useApi()<{ items?: Summary[] }>(`/projects/${id}/smartcuts`);
      if (projectId.value !== id) return;
      summaries.value = Object.fromEntries((response.items ?? []).map(item => [item.canvasNodeId, item]));
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

  /** 首次加载：已有结论时不再请求。失败不抛（节点卡照常显示占位），打开时会再拉。 */
  function load(id: string): Promise<void> {
    if (!import.meta.client || !id) return Promise.resolve();
    scope(id);
    if (state.value === "ready") return Promise.resolve();
    return start(id).catch(() => undefined);
  }

  /** 强制重新拉。失败照常抛：自愈判定不能拿一份不完整的列表下结论。 */
  function refresh(id: string): Promise<void> {
    scope(id);
    return start(id);
  }

  function get(canvasNodeId: string): Summary | undefined {
    return summaries.value[canvasNodeId];
  }

  function byTimeline(timelineId: string): Summary | undefined {
    return Object.values(summaries.value).find(item => item.timelineId === timelineId);
  }

  /** 下游取输入（SCD-50）：节点 → 最近一次成功导出的成片媒体 ID。 */
  const outputs = computed<Record<string, string>>(() => Object.fromEntries(
    Object.entries(summaries.value).map(([nodeId, summary]) => [nodeId, smartcutOutputMediaIdOf(summary)]).filter(([, id]) => id)
  ));

  /** 新建 / 复制时间线（契约 createSmartcut，按项目 + 画布节点幂等）。 */
  async function create(id: string, canvasNodeId: string, sourceTimelineId?: string): Promise<Summary> {
    const summary = await useApi()<Summary>(`/projects/${id}/smartcuts`, {
      method: "POST",
      body: { canvasNodeId, ...(sourceTimelineId ? { sourceTimelineId } : {}) }
    });
    if (projectId.value === id) summaries.value = { ...summaries.value, [canvasNodeId]: summary };
    return summary;
  }

  /**
   * 按已保存的画布重新解析某个节点的输入（SCD-49）：`autofill` 在没剪过时把新素材铺上轨道。
   * 同步改了时间线的话摘要跟着变 —— 返回后重拉一次。失败抛给调用方（节点卡吞掉，不打扰）。
   */
  async function sync(id: string, canvasNodeId: string, mode: "autofill" | "listOnly" = "autofill"): Promise<SyncResult | null> {
    const summary = summaries.value[canvasNodeId];
    if (!summary) return null;
    const result = await useApi()<SyncResult>(`/projects/${id}/smartcuts/${summary.timelineId}/sync`, { method: "POST", body: { mode } });
    if (mode === "autofill" && result.timelineVersion !== summary.version) void start(id).catch(() => undefined);
    return result;
  }

  /** 画布页转来的三种事件：只当信号，300ms 内合并成一次重拉。 */
  function applyEvent(frame: Record<string, unknown>) {
    if (!projectId.value || !EVENT_TYPES.has(String(frame?.type ?? ""))) return;
    if (frame.projectId !== undefined && String(frame.projectId) !== projectId.value) return;
    clearTimeout(eventTimer);
    const id = projectId.value;
    eventTimer = setTimeout(() => { void start(id).catch(() => undefined); }, 300);
  }

  return { projectId, summaries, state, outputs, load, refresh, get, byTimeline, create, sync, applyEvent };
}
