import type { InjectionKey } from "vue";

/**
 * 画布 ↔ Agent 面板的联动口（AGT-28，设计 03 §5）。由 `CanvasBoard` provide，面板里任何一层 inject。
 *
 * 面板不直接碰 Vue Flow：定位（平移 + 选中 + 闪烁）要用到 `useVueFlow()`，而它只在画布组件树里有意义；
 * 把这几个动作收成一个口，面板组件就不必知道画布是怎么画的。
 */
export type AgentPanelBridge = {
  /**
   * 画布是否真的载入在这一页（AGT-44）。全屏对话页不渲染画布：那里 `useCanvasStore()` 里可能还留着
   * **上一个打开过的画布**（Pinia 是全局的），按它判「节点在不在画布上」、甚至删节点后保存，都会作用到
   * 错误的项目上。为 false 时面板组件一律不读、不写画布 store。
   */
  canvasLoaded: boolean;
  /**
   * 在画布上定位节点：平移到节点、选中、闪烁 1 秒。返回实际找到的个数（0 = 都不在画布上了）。
   * 全屏对话页的实现是「跳到画布页并带 `?node=`」，返回请求定位的个数（找没找到由画布页说）。
   * ⚠️ 只在用户点了之后调用 —— Agent 写入时**不自动平移**（会打断用户）。
   */
  locate: (nodeIds: string[]) => number;
  /** 让焦点回到画布（面板里按 Esc）。 */
  focusCanvas: () => void;
};

export const AGENT_PANEL_BRIDGE: InjectionKey<AgentPanelBridge> = Symbol("agent-panel-bridge");

/** 拿不到桥（面板被单独挂在别处）时的空实现：点了没反应，但不报错；不碰画布 store。 */
export const NOOP_BRIDGE: AgentPanelBridge = { canvasLoaded: false, locate: () => 0, focusCanvas: () => undefined };

/** useApi 抛出的错误里取业务码、原话与附加信息。 */
export function apiFailure(cause: unknown): { code?: number; message: string; details: Record<string, unknown> } {
  const value = (cause ?? {}) as { message?: unknown; data?: { code?: unknown; message?: unknown; details?: unknown } };
  const data = value.data ?? {};
  const details = data.details && typeof data.details === "object" ? data.details as Record<string, unknown> : {};
  const message = typeof data.message === "string" && data.message
    ? data.message
    : typeof value.message === "string" && value.message ? value.message : "请求失败";
  return { ...(typeof data.code === "number" ? { code: data.code } : {}), message, details };
}
