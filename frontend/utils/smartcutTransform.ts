/**
 * 片段的画面变换（docs/agent/smartcut SCT-21，SC-F-43）：预览、变换控制点与服务端渲染**同一套换算**。
 *
 * 服务端（`backend/internal/timeline/domain/render.go` 的 clipChain / placeChain）的顺序是：
 * 1. 按**素材本身**的宽高裁切（四边百分比）；
 * 2. 裁切后的画面**等比**放进「画布 × scale」的框（`force_original_aspect_ratio=decrease`，不拉伸）；
 * 3. 绕自身中心旋转（`rotate=…:c=none`，露出的角透明 —— 叠到黑底上就是黑）；
 * 4. 中心放在「画布中心 + (W·x/100, H·y/100)」：x 向右、y 向下为正。
 * 没有变换的片段 = scale 1、居中等比适配（与 scale + pad 一致）。
 *
 * 预览照这个顺序还原：外层 div（裁切后的可见区域）按百分比定位、`overflow:hidden` 并旋转，
 * 内层放完整的 video / img，按裁切比例放大并左上偏移 —— 裁切就被外层精确地切出来了。
 * 换算顺序一旦与服务端不同（比如先旋转再裁切），预览里拖好的构图导出来就是另一个样子，而两边都「没报错」。
 *
 * ⚠️ 夹取与 Go 的 `ParseTransform` 同一套：x / y ±100、scale 缺省或 0 按 1 再夹 0.1~5、rotation ±180、
 *    裁切每边 0~45。**非有限数（NaN / Infinity）按缺省**：`JSON.stringify` 会把它们写成 null，服务端看到的就是缺省。
 * ⚠️ 已知字段之外的键原样保留（契约：原样 JSON 字段里的未知字段要保留），控制点只改它动过的那几个字段。
 * ⚠️ 本文件不碰 DOM（Nuxt 会把 `utils/` 自动导入进所有页面）；百分比串由组件自己套到 style 上。
 */
import type { components } from "@tabtv/api-client";

type ClipTransform = components["schemas"]["ClipTransform"];

/** 裁切：四边裁掉的百分比（按素材本身的宽高）。 */
export type SmartcutCrop = { left: number; top: number; right: number; bottom: number };

/** 夹取之后的变换（全部字段都有值）。 */
export type SmartcutTransform = { x: number; y: number; scale: number; rotation: number; crop: SmartcutCrop };

/** 夹取范围（与 Go 的 `ParseTransform` 同值）。 */
export const smartcutTransformLimits = {
  offsetPct: 100,
  minScale: 0.1,
  maxScale: 5,
  maxRotation: 180,
  maxCropPct: 45
} as const;

/** 宽高（像素）。 */
export type SmartcutSize = { width: number; height: number };

/** 舞台（显示出来的预览框）上的一个点，像素。 */
export type SmartcutPoint = { x: number; y: number };

const CROP_SIDES = ["left", "top", "right", "bottom"] as const;

type JsonMap = Record<string, unknown>;

function isMap(v: unknown): v is JsonMap {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function clamp(v: number, lo: number, hi: number): number {
  return Math.max(lo, Math.min(hi, v));
}

/**
 * 一个数值字段：缺省 / null / 非有限数按 0（与 JSON 序列化之后服务端看到的一致）；
 * 其它类型回 undefined —— Go 的 `json.Unmarshal` 遇到类型不对的字段整次报错，`ParseTransform` 于是整个按「没有变换」处理。
 */
function numField(m: JsonMap, key: string): number | undefined {
  const v = m[key];
  if (v === undefined || v === null) return 0;
  if (typeof v !== "number") return undefined;
  return Number.isFinite(v) ? v : 0;
}

/**
 * 解析并夹取一个变换（与 Go 的 `ParseTransform` 同一规则）。不是对象、字段类型不对、或夹取后是恒等变换时回 null。
 */
export function smartcutNormalizeTransform(raw: unknown): SmartcutTransform | null {
  if (!isMap(raw)) return null;
  const x = numField(raw, "x");
  const y = numField(raw, "y");
  const scale = numField(raw, "scale");
  const rotation = numField(raw, "rotation");
  if (x === undefined || y === undefined || scale === undefined || rotation === undefined) return null;
  const crop: SmartcutCrop = { left: 0, top: 0, right: 0, bottom: 0 };
  const rawCrop = raw.crop;
  if (rawCrop !== undefined && rawCrop !== null) {
    if (!isMap(rawCrop)) return null;
    for (const side of CROP_SIDES) {
      const v = numField(rawCrop, side);
      if (v === undefined) return null;
      crop[side] = clamp(v, 0, smartcutTransformLimits.maxCropPct);
    }
  }
  const t: SmartcutTransform = {
    x: clamp(x, -smartcutTransformLimits.offsetPct, smartcutTransformLimits.offsetPct),
    y: clamp(y, -smartcutTransformLimits.offsetPct, smartcutTransformLimits.offsetPct),
    // 0 表示没设（与 Go 的「Scale == 0 → 1」一致），再夹到 0.1~5。
    scale: clamp(scale === 0 ? 1 : scale, smartcutTransformLimits.minScale, smartcutTransformLimits.maxScale),
    rotation: clamp(rotation, -smartcutTransformLimits.maxRotation, smartcutTransformLimits.maxRotation),
    crop
  };
  return smartcutTransformIsIdentity(t) ? null : t;
}

/** 是不是恒等变换（不需要变换链）：没有位移、缩放 1、不旋转、不裁切。 */
export function smartcutTransformIsIdentity(t: SmartcutTransform | null | undefined): boolean {
  if (!t) return true;
  return t.x === 0 && t.y === 0 && t.scale === 1 && t.rotation === 0
    && t.crop.left === 0 && t.crop.top === 0 && t.crop.right === 0 && t.crop.bottom === 0;
}

// ---------------------------------------------------------------- 几何

/**
 * 变换后的位置（全部是百分比，不依赖显示尺寸）：
 * - 外层 = 裁切后的可见区域：中心与宽高是**画布**宽高的百分比，`rotation` 为度（顺时针为正，与 ffmpeg rotate 同向）；
 * - 内层 = 完整素材：宽高与左上角偏移是**外层**宽高的百分比（外层 `overflow:hidden` 正好切出裁切区域）。
 */
export type SmartcutTransformGeometry = {
  centerXPct: number;
  centerYPct: number;
  widthPct: number;
  heightPct: number;
  rotation: number;
  innerLeftPct: number;
  innerTopPct: number;
  innerWidthPct: number;
  innerHeightPct: number;
};

/** 取不小于 2 的偶数（与 Go 的 `even` 一致：缩放框是奇数的话编码器报错，服务端先取偶）。 */
function even(v: number): number {
  const n = Math.round(v);
  if (n < 2) return 2;
  return Math.trunc(n / 2) * 2;
}

function positive(v: number | undefined): v is number {
  return typeof v === "number" && Number.isFinite(v) && v > 0;
}

/**
 * 一段在画布里的几何位置。`media` 是素材（预览版）的宽高，未知时按与画布同比例处理（不报错）。
 *
 * 等比适配照 ffmpeg 的 `force_original_aspect_ratio=decrease`：`w = min(round(boxH × 裁切宽 / 裁切高), boxW)`、
 * `h = min(round(boxW × 裁切高 / 裁切宽), boxH)`；有变换时框是 `even(画布 × scale)`，没有变换时就是画布。
 * 与服务端最多差 1 像素（ffmpeg 的裁切尺寸是整数），预览看不出来。
 */
export function smartcutTransformGeometry(raw: unknown, canvas: SmartcutSize, media?: Partial<SmartcutSize> | null): SmartcutTransformGeometry {
  const W = positive(canvas.width) ? canvas.width : 16;
  const H = positive(canvas.height) ? canvas.height : 9;
  const t = smartcutNormalizeTransform(raw);
  const iw = positive(media?.width) && positive(media?.height) ? media!.width! : W;
  const ih = positive(media?.width) && positive(media?.height) ? media!.height! : H;
  const crop = t?.crop ?? { left: 0, top: 0, right: 0, bottom: 0 };
  const cwf = 1 - (crop.left + crop.right) / 100;
  const chf = 1 - (crop.top + crop.bottom) / 100;
  const cropW = iw * cwf;
  const cropH = ih * chf;
  const boxW = t ? even(W * t.scale) : W;
  const boxH = t ? even(H * t.scale) : H;
  const w = Math.max(1, Math.min(Math.round((boxH * cropW) / cropH), boxW));
  const h = Math.max(1, Math.min(Math.round((boxW * cropH) / cropW), boxH));
  return {
    centerXPct: 50 + (t?.x ?? 0),
    centerYPct: 50 + (t?.y ?? 0),
    widthPct: (w / W) * 100,
    heightPct: (h / H) * 100,
    rotation: t?.rotation ?? 0,
    innerLeftPct: (-100 * crop.left) / 100 / cwf,
    innerTopPct: (-100 * crop.top) / 100 / chf,
    innerWidthPct: 100 / cwf,
    innerHeightPct: 100 / chf
  };
}

/** 两层的样式字段（百分比串，直接赋给 `element.style`）。外层要 `position:absolute; overflow:hidden`，内层 `object-fit: fill`。 */
export type SmartcutTransformCss = {
  outer: { left: string; top: string; width: string; height: string; transform: string; transformOrigin: string };
  inner: { left: string; top: string; width: string; height: string };
};

/** 百分比串：保留 4 位小数（足够亚像素，又不会写出 1e-7 这种科学计数法）。 */
function pct(v: number): string {
  const r = Math.round(v * 10000) / 10000;
  return `${Object.is(r, -0) ? 0 : r}%`;
}

function deg(v: number): string {
  const r = Math.round(v * 1000) / 1000;
  return `${Object.is(r, -0) ? 0 : r}deg`;
}

/** 变换对应的两层样式（预览用）：外层定位并旋转、内层按裁切放大偏移。 */
export function smartcutTransformCss(raw: unknown, canvas: SmartcutSize, media?: Partial<SmartcutSize> | null): SmartcutTransformCss {
  const g = smartcutTransformGeometry(raw, canvas, media);
  return {
    outer: {
      left: pct(g.centerXPct - g.widthPct / 2),
      top: pct(g.centerYPct - g.heightPct / 2),
      width: pct(g.widthPct),
      height: pct(g.heightPct),
      transform: g.rotation ? `rotate(${deg(g.rotation)})` : "none",
      transformOrigin: "50% 50%"
    },
    inner: {
      left: pct(g.innerLeftPct),
      top: pct(g.innerTopPct),
      width: pct(g.innerWidthPct),
      height: pct(g.innerHeightPct)
    }
  };
}

// ---------------------------------------------------------------- 控制点

/** 八个控制点 + 旋转把手的位置（舞台像素）。 */
export type SmartcutTransformHandles = {
  center: SmartcutPoint;
  corners: { nw: SmartcutPoint; ne: SmartcutPoint; se: SmartcutPoint; sw: SmartcutPoint };
  edges: { n: SmartcutPoint; e: SmartcutPoint; s: SmartcutPoint; w: SmartcutPoint };
  rotate: SmartcutPoint;
  /** 外层在舞台上的宽高（未旋转时），像素。 */
  width: number;
  height: number;
  rotation: number;
};

export type SmartcutHandle = "nw" | "ne" | "se" | "sw" | "n" | "e" | "s" | "w";

/** 旋转把手离上边中点的距离（像素，沿外层的「上方」方向）。 */
export const smartcutRotateHandleOffsetPx = 28;

function rotatePoint(p: SmartcutPoint, c: SmartcutPoint, rad: number): SmartcutPoint {
  const dx = p.x - c.x;
  const dy = p.y - c.y;
  const cos = Math.cos(rad);
  const sin = Math.sin(rad);
  return { x: c.x + dx * cos - dy * sin, y: c.y + dx * sin + dy * cos };
}

/**
 * 控制点在舞台上的位置。`stage` 是显示出来的预览框尺寸（与画幅同比例），控制点随旋转一起转。
 */
export function smartcutTransformHandles(
  raw: unknown,
  canvas: SmartcutSize,
  media: Partial<SmartcutSize> | null | undefined,
  stage: SmartcutSize
): SmartcutTransformHandles {
  const g = smartcutTransformGeometry(raw, canvas, media);
  const c = { x: (g.centerXPct / 100) * stage.width, y: (g.centerYPct / 100) * stage.height };
  const w = (g.widthPct / 100) * stage.width;
  const h = (g.heightPct / 100) * stage.height;
  const rad = (g.rotation * Math.PI) / 180;
  const at = (fx: number, fy: number) => rotatePoint({ x: c.x + fx * w / 2, y: c.y + fy * h / 2 }, c, rad);
  return {
    center: c,
    corners: { nw: at(-1, -1), ne: at(1, -1), se: at(1, 1), sw: at(-1, 1) },
    edges: { n: at(0, -1), e: at(1, 0), s: at(0, 1), w: at(-1, 0) },
    rotate: rotatePoint({ x: c.x, y: c.y - h / 2 - smartcutRotateHandleOffsetPx }, c, rad),
    width: w,
    height: h,
    rotation: g.rotation
  };
}

/** 规整到两位、三位、一位小数：存进 JSON 的数短一些，撤销前后比较也稳定（拖动算出来的浮点尾巴没有意义）。 */
function round(v: number, digits: number): number {
  const f = 10 ** digits;
  const r = Math.round(v * f) / f;
  return Object.is(r, -0) ? 0 : r;
}

/** 角度规整到 (-180, 180]。 */
export function smartcutNormalizeAngle(degrees: number): number {
  if (!Number.isFinite(degrees)) return 0;
  let a = degrees % 360;
  if (a > 180) a -= 360;
  if (a <= -180) a += 360;
  return a;
}

/** 变换的原样对象（保留未知字段）。null / 非对象时为空对象。 */
function baseOf(raw: unknown): JsonMap {
  return isMap(raw) ? { ...raw } : {};
}

/** 补丁：只改给出的字段；`crop` 按边合并（没给的边与未知字段原样保留）。 */
export type SmartcutTransformPatch = {
  x?: number;
  y?: number;
  scale?: number;
  rotation?: number;
  crop?: Partial<SmartcutCrop> | null;
};

/**
 * 按补丁改一个变换（面板数值、控制点共用）：给出的字段夹取并规整小数，其余字段（含未知字段）原样保留。
 * `crop: null` 清掉裁切。结果仍是原样 JSON 对象（可能是恒等变换 —— 「重置」由调用方直接删掉 `transform`）。
 */
export function smartcutTransformWith(raw: unknown, patch: SmartcutTransformPatch): ClipTransform {
  const next = baseOf(raw);
  const L = smartcutTransformLimits;
  if (patch.x !== undefined) next.x = round(clamp(Number.isFinite(patch.x) ? patch.x : 0, -L.offsetPct, L.offsetPct), 2);
  if (patch.y !== undefined) next.y = round(clamp(Number.isFinite(patch.y) ? patch.y : 0, -L.offsetPct, L.offsetPct), 2);
  if (patch.scale !== undefined) {
    const s = Number.isFinite(patch.scale) && patch.scale > 0 ? patch.scale : 1;
    next.scale = round(clamp(s, L.minScale, L.maxScale), 3);
  }
  if (patch.rotation !== undefined) next.rotation = round(clamp(smartcutNormalizeAngle(patch.rotation), -L.maxRotation, L.maxRotation), 1);
  if (patch.crop === null) {
    delete next.crop;
  } else if (patch.crop) {
    const crop: JsonMap = isMap(next.crop) ? { ...next.crop } : {};
    for (const side of CROP_SIDES) {
      const v = patch.crop[side];
      if (v !== undefined) crop[side] = round(clamp(Number.isFinite(v) ? v : 0, 0, L.maxCropPct), 2);
    }
    next.crop = crop;
  }
  return next as ClipTransform;
}

/**
 * 拖动移动（`dx` / `dy` 是舞台像素）：位置是画布宽高的百分比，所以按舞台尺寸换算（舞台与画布同比例）。
 * `raw` 是**拖动开始时**的变换：每次 pointermove 都从它算，不累加（累加会把舍入误差越拖越大）。
 */
export function smartcutTransformMove(raw: unknown, dx: number, dy: number, stage: SmartcutSize): ClipTransform {
  const t = smartcutNormalizeTransform(raw);
  const w = positive(stage.width) ? stage.width : 1;
  const h = positive(stage.height) ? stage.height : 1;
  return smartcutTransformWith(raw, { x: (t?.x ?? 0) + (dx / w) * 100, y: (t?.y ?? 0) + (dy / h) * 100 });
}

/**
 * 拖控制点缩放（等比：变换只有一个 scale）。角点按到中心的距离之比；边点按在外层**自身坐标轴**上的投影之比
 * （旋转过的片段，拖右边就是沿它自己的横轴拉）。全部是舞台像素；`raw` 是拖动开始时的变换。
 */
export function smartcutTransformScale(
  raw: unknown,
  handle: SmartcutHandle,
  center: SmartcutPoint,
  start: SmartcutPoint,
  current: SmartcutPoint
): ClipTransform {
  const t = smartcutNormalizeTransform(raw);
  const base = t?.scale ?? 1;
  const rad = ((t?.rotation ?? 0) * Math.PI) / 180;
  const s = { x: start.x - center.x, y: start.y - center.y };
  const c = { x: current.x - center.x, y: current.y - center.y };
  let from: number;
  let to: number;
  if (handle === "e" || handle === "w" || handle === "n" || handle === "s") {
    // 外层自己的横轴 (cos, sin)、竖轴 (-sin, cos)。
    const axis = handle === "e" || handle === "w" ? { x: Math.cos(rad), y: Math.sin(rad) } : { x: -Math.sin(rad), y: Math.cos(rad) };
    from = Math.abs(s.x * axis.x + s.y * axis.y);
    to = Math.abs(c.x * axis.x + c.y * axis.y);
  } else {
    from = Math.hypot(s.x, s.y);
    to = Math.hypot(c.x, c.y);
  }
  // 起点就在中心上（距离 0）时没有比例可言：保持不变。
  if (!(from > 0)) return smartcutTransformWith(raw, { scale: base });
  return smartcutTransformWith(raw, { scale: (base * to) / from });
}

/**
 * 拖旋转把手：转过的角度 = 指针相对中心的方位角之差（舞台坐标 y 向下，顺时针为正，与 CSS / ffmpeg 同向）。
 * `snapDeg > 0` 时吸到它的整数倍（按住 Shift 的 15°）。结果规整到 ±180。
 */
export function smartcutTransformRotate(
  raw: unknown,
  center: SmartcutPoint,
  start: SmartcutPoint,
  current: SmartcutPoint,
  snapDeg = 0
): ClipTransform {
  const t = smartcutNormalizeTransform(raw);
  const a0 = Math.atan2(start.y - center.y, start.x - center.x);
  const a1 = Math.atan2(current.y - center.y, current.x - center.x);
  let next = smartcutNormalizeAngle((t?.rotation ?? 0) + ((a1 - a0) * 180) / Math.PI);
  if (snapDeg > 0) next = smartcutNormalizeAngle(Math.round(next / snapDeg) * snapDeg);
  return smartcutTransformWith(raw, { rotation: next });
}
