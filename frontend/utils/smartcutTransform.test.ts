import { describe, expect, it } from "vitest";
import {
  smartcutNormalizeAngle,
  smartcutNormalizeTransform,
  smartcutRotateHandleOffsetPx,
  smartcutTransformCss,
  smartcutTransformGeometry,
  smartcutTransformHandles,
  smartcutTransformIsIdentity,
  smartcutTransformMove,
  smartcutTransformRotate,
  smartcutTransformScale,
  smartcutTransformWith
} from "./smartcutTransform";

const HD = { width: 1920, height: 1080 };

describe("smartcutNormalizeTransform（与 Go ParseTransform 同一套夹取）", () => {
  it("不是对象、空对象、夹取后恒等的都回 null", () => {
    expect(smartcutNormalizeTransform(null)).toBeNull();
    expect(smartcutNormalizeTransform(undefined)).toBeNull();
    expect(smartcutNormalizeTransform([1, 2])).toBeNull();
    expect(smartcutNormalizeTransform("x")).toBeNull();
    expect(smartcutNormalizeTransform({})).toBeNull();
    // scale 0 表示没设 → 1；其余都是 0 → 恒等
    expect(smartcutNormalizeTransform({ scale: 0, crop: null })).toBeNull();
    expect(smartcutNormalizeTransform({ x: 0, y: 0, scale: 1, rotation: 0, crop: { left: 0 } })).toBeNull();
  });

  it("各字段夹到范围内", () => {
    expect(smartcutNormalizeTransform({ x: 150, y: -150, scale: 10, rotation: 270, crop: { left: 60, top: -5, right: 10, bottom: 45 } }))
      .toEqual({ x: 100, y: -100, scale: 5, rotation: 180, crop: { left: 45, top: 0, right: 10, bottom: 45 } });
    expect(smartcutNormalizeTransform({ scale: 0.01 })?.scale).toBe(0.1);
  });

  it("非有限数按缺省（JSON 序列化后服务端看到的是 null）", () => {
    expect(smartcutNormalizeTransform({ x: Number.NaN, y: 5 })).toEqual({ x: 0, y: 5, scale: 1, rotation: 0, crop: { left: 0, top: 0, right: 0, bottom: 0 } });
    expect(smartcutNormalizeTransform({ scale: Number.POSITIVE_INFINITY, x: 1 })?.scale).toBe(1);
  });

  it("字段类型不对时整个按「没有变换」（Go 的 Unmarshal 整次报错）", () => {
    expect(smartcutNormalizeTransform({ x: "10" })).toBeNull();
    expect(smartcutNormalizeTransform({ x: 10, crop: 5 })).toBeNull();
    expect(smartcutNormalizeTransform({ x: 10, crop: { left: "5" } })).toBeNull();
  });

  it("未知字段不影响解析", () => {
    expect(smartcutNormalizeTransform({ x: 10, futureField: { a: 1 } })?.x).toBe(10);
  });

  it("恒等判断", () => {
    expect(smartcutTransformIsIdentity(null)).toBe(true);
    expect(smartcutTransformIsIdentity({ x: 0, y: 0, scale: 1, rotation: 0, crop: { left: 0, top: 0, right: 0, bottom: 1 } })).toBe(false);
  });
});

describe("smartcutTransformGeometry（顺序：裁切 → 等比放进画布×缩放 → 旋转 → 放到中心偏移处）", () => {
  it("没有变换：与 scale+pad 一致，满幅居中", () => {
    expect(smartcutTransformGeometry(null, HD, HD)).toEqual({
      centerXPct: 50, centerYPct: 50, widthPct: 100, heightPct: 100, rotation: 0,
      innerLeftPct: -0, innerTopPct: -0, innerWidthPct: 100, innerHeightPct: 100
    });
  });

  it("没有变换的竖屏素材放进横屏画布：等比、左右黑边（宽按 ffmpeg 的四舍五入）", () => {
    const g = smartcutTransformGeometry(undefined, HD, { width: 1080, height: 1920 });
    // round(1080 × 1080 / 1920) = round(607.5) = 608
    expect(g.widthPct).toBeCloseTo((608 / 1920) * 100, 6);
    expect(g.heightPct).toBe(100);
    expect(g.centerXPct).toBe(50);
  });

  it("与服务端渲染测试同一个例子：缩放 50%、x=-25 y=-25 → 画面占左上四分之一", () => {
    const g = smartcutTransformGeometry({ x: -25, y: -25, scale: 0.5 }, { width: 320, height: 180 }, { width: 640, height: 360 });
    expect(g).toMatchObject({ centerXPct: 25, centerYPct: 25, widthPct: 50, heightPct: 50 });
    const css = smartcutTransformCss({ x: -25, y: -25, scale: 0.5 }, { width: 320, height: 180 }, { width: 640, height: 360 });
    expect(css.outer).toMatchObject({ left: "0%", top: "0%", width: "50%", height: "50%", transform: "none" });
  });

  it("裁切按素材本身的宽高、在缩放之前：左右各裁 10%，外层宽 80%，内层放大 125% 并左移 12.5%", () => {
    const g = smartcutTransformGeometry({ crop: { left: 10, right: 10 } }, HD, HD);
    expect(g.widthPct).toBeCloseTo(80, 6);
    expect(g.heightPct).toBe(100);
    expect(g.innerWidthPct).toBeCloseTo(125, 6);
    expect(g.innerLeftPct).toBeCloseTo(-12.5, 6);
    expect(g.innerTopPct).toBeCloseTo(0, 6);
    expect(g.innerHeightPct).toBe(100);
  });

  it("缩放框取偶数（与服务端 even 一致）：1918 宽的画布 × 0.5 = 959 → 958", () => {
    const g = smartcutTransformGeometry({ scale: 0.5 }, { width: 1918, height: 1080 }, { width: 1918, height: 1080 });
    expect(g.widthPct).toBeCloseTo((958 / 1918) * 100, 6);
  });

  it("素材宽高未知时按与画布同比例处理", () => {
    const g = smartcutTransformGeometry({ x: 10 }, HD, null);
    expect(g).toMatchObject({ centerXPct: 60, widthPct: 100, heightPct: 100 });
    expect(smartcutTransformGeometry({ x: 10 }, HD, { width: 0, height: 100 }).widthPct).toBe(100);
  });

  it("旋转：外层带 rotate，样式串不出现 -0 与科学计数法", () => {
    const css = smartcutTransformCss({ rotation: 30, y: 0.000001 }, HD, HD);
    expect(css.outer.transform).toBe("rotate(30deg)");
    expect(css.outer.top).toBe("0%");
    expect(css.inner).toEqual({ left: "0%", top: "0%", width: "100%", height: "100%" });
  });
});

describe("控制点", () => {
  const stage = { width: 320, height: 180 };

  it("没有变换时四角在舞台四角，旋转把手在上边中点再往上", () => {
    const h = smartcutTransformHandles(null, HD, HD, stage);
    expect(h.center).toEqual({ x: 160, y: 90 });
    expect(h.corners.nw).toEqual({ x: 0, y: 0 });
    expect(h.corners.se).toEqual({ x: 320, y: 180 });
    expect(h.edges.e).toEqual({ x: 320, y: 90 });
    expect(h.rotate).toEqual({ x: 160, y: -smartcutRotateHandleOffsetPx });
  });

  it("控制点随旋转一起转（90° 时上边中点转到右侧）", () => {
    // 缩放 0.5：外层高 90px，上边中点在中心上方 45px；顺时针转 90° 后到中心右侧 45px。
    const h = smartcutTransformHandles({ rotation: 90, scale: 0.5 }, HD, HD, stage);
    expect(h.edges.n.x).toBeCloseTo(160 + 45, 6);
    expect(h.edges.n.y).toBeCloseTo(90, 6);
  });

  it("移动：舞台像素换算成画布百分比，夹到 ±100，其余字段与未知字段原样保留", () => {
    const raw = { x: 10, scale: 2, future: { a: 1 } };
    expect(smartcutTransformMove(raw, 32, -18, stage)).toEqual({ x: 20, y: -10, scale: 2, future: { a: 1 } });
    expect(smartcutTransformMove(raw, 3200, 0, stage).x).toBe(100);
    expect(smartcutTransformMove(null, 16, 9, stage)).toEqual({ x: 5, y: 5 });
  });

  it("角点缩放按到中心的距离之比；起点在中心上时不变", () => {
    const c = { x: 0, y: 0 };
    expect(smartcutTransformScale({ scale: 1 }, "se", c, { x: 30, y: 40 }, { x: 60, y: 80 }).scale).toBe(2);
    expect(smartcutTransformScale({ scale: 4 }, "nw", c, { x: 10, y: 0 }, { x: 100, y: 0 }).scale).toBe(5);
    expect(smartcutTransformScale({ scale: 1 }, "ne", c, { x: 0, y: 0 }, { x: 10, y: 10 }).scale).toBe(1);
  });

  it("边点缩放按外层自身坐标轴上的投影（旋转 90° 时拖右边是沿舞台的竖直方向）", () => {
    const c = { x: 0, y: 0 };
    const t = smartcutTransformScale({ rotation: 90, scale: 1 }, "e", c, { x: 5, y: 50 }, { x: 40, y: 100 });
    expect(t.scale).toBe(2);
    expect(t.rotation).toBe(90);
  });

  it("旋转：顺时针为正（舞台 y 向下），规整到 ±180，可吸附", () => {
    const c = { x: 0, y: 0 };
    expect(smartcutTransformRotate({}, c, { x: 1, y: 0 }, { x: 0, y: 1 }).rotation).toBe(90);
    expect(smartcutTransformRotate({ rotation: 170 }, c, { x: 1, y: 0 }, { x: 0, y: 1 }).rotation).toBe(-100);
    expect(smartcutTransformRotate({}, c, { x: 1, y: 0 }, { x: Math.cos(0.77), y: Math.sin(0.77) }, 15).rotation).toBe(45);
  });

  it("角度规整", () => {
    expect(smartcutNormalizeAngle(180)).toBe(180);
    expect(smartcutNormalizeAngle(-180)).toBe(180);
    expect(smartcutNormalizeAngle(540)).toBe(180);
    expect(smartcutNormalizeAngle(-190)).toBe(170);
    expect(smartcutNormalizeAngle(Number.NaN)).toBe(0);
  });
});

describe("smartcutTransformWith（面板数值共用）", () => {
  it("裁切按边合并，未知字段保留；crop:null 清掉裁切", () => {
    const raw = { x: 1, crop: { left: 5, futureCrop: 1 }, future: true };
    expect(smartcutTransformWith(raw, { crop: { right: 70 } })).toEqual({ x: 1, crop: { left: 5, futureCrop: 1, right: 45 }, future: true });
    expect(smartcutTransformWith(raw, { crop: null })).toEqual({ x: 1, future: true });
  });

  it("给出的字段夹取并规整小数，没给的不动", () => {
    expect(smartcutTransformWith({ rotation: 10 }, { scale: 0.123456, x: 33.33333 })).toEqual({ rotation: 10, scale: 0.123, x: 33.33 });
    expect(smartcutTransformWith(null, { scale: -1 })).toEqual({ scale: 1 });
    expect(smartcutTransformWith(null, { rotation: 359 })).toEqual({ rotation: -1 });
  });
});
