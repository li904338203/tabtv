import { describe, expect, it } from "vitest";
import {
  CANVAS_FIT_MAX_ZOOM,
  CANVAS_GRID_SIZE,
  CANVAS_MAX_ZOOM,
  CANVAS_MIN_ZOOM,
  CANVAS_SNAP_GRID,
  CANVAS_VIEW_DEFAULTS,
  CANVAS_VIEW_PREFS_KEY,
  CANVAS_ZOOM_PRESETS,
  parseCanvasViewPrefs,
  parseZoomPercent,
  readCanvasViewPrefs,
  writeCanvasViewPrefs,
  zoomPercent
} from "./canvasView";

describe("缩放范围与档位（2026-11-01）", () => {
  it("菜单里的档位都在画布能缩放的范围内（800% 不会被夹成 180%）", () => {
    expect(CANVAS_ZOOM_PRESETS).toEqual([0.5, 1, 8]);
    for (const zoom of CANVAS_ZOOM_PRESETS) {
      expect(zoom).toBeGreaterThanOrEqual(CANVAS_MIN_ZOOM);
      expect(zoom).toBeLessThanOrEqual(CANVAS_MAX_ZOOM);
    }
  });

  it("定位 / 适应画布的上限仍是原来的 180%，比画布的最大缩放小", () => {
    expect(CANVAS_FIT_MAX_ZOOM).toBe(1.8);
    expect(CANVAS_FIT_MAX_ZOOM).toBeLessThan(CANVAS_MAX_ZOOM);
  });

  it("吸附格距与背景点阵同一个数", () => {
    expect(CANVAS_SNAP_GRID).toEqual([CANVAS_GRID_SIZE, CANVAS_GRID_SIZE]);
  });

  it("百分比显示取整", () => {
    expect(zoomPercent(1)).toBe(100);
    expect(zoomPercent(0.5)).toBe(50);
    expect(zoomPercent(1.2345)).toBe(123);
    expect(zoomPercent(8)).toBe(800);
  });
});

describe("parseZoomPercent", () => {
  it("带不带 % 都认", () => {
    expect(parseZoomPercent("150")).toBe(1.5);
    expect(parseZoomPercent("150%")).toBe(1.5);
    expect(parseZoomPercent(" 75 % ")).toBe(0.75);
  });

  it("超出范围的夹到 15%~800%", () => {
    expect(parseZoomPercent("5")).toBe(CANVAS_MIN_ZOOM);
    expect(parseZoomPercent("2000")).toBe(CANVAS_MAX_ZOOM);
  });

  it("认不出来的回 null（不动画布）", () => {
    for (const text of ["", "  ", "%", "abc", "0", "-50"]) expect(parseZoomPercent(text)).toBeNull();
  });
});

describe("视图偏好（本机）", () => {
  it("默认：小地图收起、不吸附", () => {
    expect(CANVAS_VIEW_DEFAULTS).toEqual({ minimap: false, snapToGrid: false });
    expect(parseCanvasViewPrefs(null)).toEqual(CANVAS_VIEW_DEFAULTS);
  });

  it("只认布尔；坏数据回默认、不抛", () => {
    expect(parseCanvasViewPrefs('{"minimap":true,"snapToGrid":true}')).toEqual({ minimap: true, snapToGrid: true });
    expect(parseCanvasViewPrefs('{"minimap":"yes","snapToGrid":1}')).toEqual(CANVAS_VIEW_DEFAULTS);
    expect(parseCanvasViewPrefs("not json")).toEqual(CANVAS_VIEW_DEFAULTS);
    expect(parseCanvasViewPrefs("null")).toEqual(CANVAS_VIEW_DEFAULTS);
  });

  it("写进去再读出来是同一份；不记「隐藏连线」这类别的键", () => {
    const store = new Map<string, string>();
    const storage = { getItem: (key: string) => store.get(key) ?? null, setItem: (key: string, value: string) => void store.set(key, value) };
    writeCanvasViewPrefs(storage, { minimap: true, snapToGrid: false, edgesHidden: true } as never);
    expect(JSON.parse(store.get(CANVAS_VIEW_PREFS_KEY) ?? "")).toEqual({ minimap: true, snapToGrid: false });
    expect(readCanvasViewPrefs(storage)).toEqual({ minimap: true, snapToGrid: false });
  });

  it("存储不可用时读回默认、写也不抛", () => {
    const broken = {
      getItem: () => { throw new Error("SecurityError"); },
      setItem: () => { throw new Error("QuotaExceededError"); }
    };
    expect(readCanvasViewPrefs(broken)).toEqual(CANVAS_VIEW_DEFAULTS);
    expect(() => writeCanvasViewPrefs(broken, { minimap: true, snapToGrid: true })).not.toThrow();
    expect(readCanvasViewPrefs(undefined)).toEqual(CANVAS_VIEW_DEFAULTS);
  });
});
