/**
 * 画布视图（2026-11-01，画布左下角工具条）：缩放范围与档位、网格、小地图 / 网格吸附的本地偏好。
 * 纯函数，单测见 `canvasView.test.ts`。
 */

export const CANVAS_MIN_ZOOM = 0.15;
/** 最大 800%（缩放菜单里有「缩放至800%」）。此前是 180%。 */
export const CANVAS_MAX_ZOOM = 8;
/**
 * 定位到节点 / 适应画布最多放大到 180%（原来的上限）。fitView 不给上限时用的是画布的最大缩放 ——
 * 画布能放大到 800% 以后，定位一个小节点会把它放大到糊满整屏。
 */
export const CANVAS_FIT_MAX_ZOOM = 1.8;
/** 缩放菜单的三个档位。 */
export const CANVAS_ZOOM_PRESETS = [0.5, 1, 8] as const;

/** 画布背景点阵的间距，也是网格吸附的格距：吸附后节点的左上角正好落在点上。 */
export const CANVAS_GRID_SIZE = 22;
export const CANVAS_SNAP_GRID: [number, number] = [CANVAS_GRID_SIZE, CANVAS_GRID_SIZE];

/** 缩放显示成整数百分比（1 → 100）。 */
export function zoomPercent(zoom: number): number {
  return Math.round(zoom * 100);
}

/**
 * 缩放菜单输入框里的值 → 缩放倍数。「150」「150%」都认，超出范围的夹到 15%~800%；
 * 认不出来（空、非数字、≤0）回 null，调用方什么都不做。
 */
export function parseZoomPercent(text: string): number | null {
  const value = Number(text.replace(/[%\s]/g, ""));
  if (!text.trim() || !Number.isFinite(value) || value <= 0) return null;
  return Math.min(CANVAS_MAX_ZOOM, Math.max(CANVAS_MIN_ZOOM, value / 100));
}

/**
 * 记在本机的视图开关：小地图、网格吸附（下次打开画布还是这样）。
 * ⚠️ 「隐藏节点连线」**不记**：记住的话，下次打开画布连线全不见了，看上去像是数据丢了。
 */
export type CanvasViewPrefs = { minimap: boolean; snapToGrid: boolean };

export const CANVAS_VIEW_PREFS_KEY = "seeaitv.canvas.view";
/** 小地图默认收起（参考图里平时不显示，点了按钮才出来）。 */
export const CANVAS_VIEW_DEFAULTS: Readonly<CanvasViewPrefs> = { minimap: false, snapToGrid: false };

/** 存下来的值 → 偏好。坏数据、缺字段、不是布尔的一律回默认，不抛。 */
export function parseCanvasViewPrefs(raw: string | null | undefined): CanvasViewPrefs {
  const prefs = { ...CANVAS_VIEW_DEFAULTS };
  if (!raw) return prefs;
  try {
    const value: unknown = JSON.parse(raw);
    if (typeof value !== "object" || value === null) return prefs;
    const record = value as Record<string, unknown>;
    if (typeof record.minimap === "boolean") prefs.minimap = record.minimap;
    if (typeof record.snapToGrid === "boolean") prefs.snapToGrid = record.snapToGrid;
  } catch {
    // 不是 JSON：按默认
  }
  return prefs;
}

/** 读不到本地存储（隐私模式、服务端渲染）就是默认值。 */
export function readCanvasViewPrefs(storage: Pick<Storage, "getItem"> | undefined): CanvasViewPrefs {
  try {
    return parseCanvasViewPrefs(storage?.getItem(CANVAS_VIEW_PREFS_KEY));
  } catch {
    return { ...CANVAS_VIEW_DEFAULTS };
  }
}

/** 写不进去（存储满了、被禁用）就算了：只是下次打开回到默认。 */
export function writeCanvasViewPrefs(storage: Pick<Storage, "setItem"> | undefined, prefs: CanvasViewPrefs): void {
  try {
    storage?.setItem(CANVAS_VIEW_PREFS_KEY, JSON.stringify({ minimap: prefs.minimap, snapToGrid: prefs.snapToGrid }));
  } catch {
    // 忽略
  }
}
