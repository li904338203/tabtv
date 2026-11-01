/**
 * 多角度编辑器（docs/agent/imagetools IMG-F-30~38、IMGD-27）的纯函数：球形机位的投影 / 反算、拖动环绕、
 * 键盘步进、经纬网线、节点参数 ↔ 编辑器状态。
 *
 * ⚠️ 球用 SVG 画（IMGD-27，不引 3D 库），这里只做数学：单位球 + 固定俯视倾角的正交投影。
 *    坐标约定：屏幕 x 向右、**y 向下**（与 SVG 一致）；`depth ≥ 0` 是朝向观者的半球（看得见的那一面）。
 * ⚠️ 角度语义与服务端一致（IMG-F-31）：水平 0 = 正面、90 = 右侧、180 = 背面、270 = 左侧；俯仰 0 = 平视、正值 = 俯拍。
 * ⚠️ 导出名一律带 `multiAngle` 前缀并显式 import（Nuxt 自动导入 `utils/` 重名时悄悄顶替，CLAUDE.md §5.28）。
 */
import {
  IMAGE_TOOL_MULTI_ANGLE_PROMPT_MAX,
  imageToolDefaultOptions,
  imageToolSetAngle,
  imageToolStepAngle,
  type ImageToolMultiAngleOptions,
  type ImageToolsResolvedConfig
} from "~/utils/imageTools";

/** 示意球的俯视倾角（度）：从斜上方看过去，纬线才是椭圆、球才像个球。 */
export const MULTI_ANGLE_VIEW_TILT = 20;

/** 俯仰范围（与 `imageToolSetAngle` 同一份，IMG-F-31）。 */
export const MULTI_ANGLE_VERTICAL_RANGE = [-30, 90] as const;

const RAD = Math.PI / 180;

export type MultiAnglePoint = { x: number; y: number; depth: number };

/**
 * 机位（水平 h、俯仰 v）→ 单位球上的屏幕投影。
 * 世界坐标：x 右、y 上、z 朝观者；观者在上方 `tilt` 度处向下看。
 */
export function multiAngleProject(horizontal: number, vertical: number, tilt: number = MULTI_ANGLE_VIEW_TILT): MultiAnglePoint {
  const h = horizontal * RAD;
  const v = vertical * RAD;
  const t = tilt * RAD;
  const px = Math.sin(h) * Math.cos(v);
  const py = Math.sin(v);
  const pz = Math.cos(h) * Math.cos(v);
  const up = py * Math.cos(t) - pz * Math.sin(t);
  const depth = py * Math.sin(t) + pz * Math.cos(t);
  return { x: px, y: -up, depth };
}

/**
 * 屏幕点（单位球坐标，y 向下）→ 机位：取**看得见的那一面**（用户点的是他看到的位置）。
 * 球外的点夹到球的轮廓上；结果按契约取整、水平落在 0~359、俯仰夹在 -30~90。
 */
export function multiAngleFromScreen(x: number, y: number, tilt: number = MULTI_ANGLE_VIEW_TILT): { horizontal: number; vertical: number } {
  let sx = Number.isFinite(x) ? x : 0;
  let sy = Number.isFinite(y) ? -y : 0;
  const length = Math.hypot(sx, sy);
  if (length > 1) {
    sx /= length;
    sy /= length;
  }
  const depth = Math.sqrt(Math.max(0, 1 - sx * sx - sy * sy));
  const t = tilt * RAD;
  const py = sy * Math.cos(t) + depth * Math.sin(t);
  const pz = -sy * Math.sin(t) + depth * Math.cos(t);
  const vertical = Math.asin(Math.max(-1, Math.min(1, py))) / RAD;
  const horizontal = Math.atan2(sx, pz) / RAD;
  return {
    horizontal: multiAngleWrap(Math.round(horizontal)),
    vertical: clampVertical(Math.round(vertical))
  };
}

/** 水平角取模到 0~359（360 与 0 是同一个机位）。 */
export function multiAngleWrap(value: number): number {
  const rounded = Math.round(Number.isFinite(value) ? value : 0);
  return ((rounded % 360) + 360) % 360;
}

function clampVertical(value: number): number {
  return Math.min(MULTI_ANGLE_VERTICAL_RANGE[1], Math.max(MULTI_ANGLE_VERTICAL_RANGE[0], value));
}

/**
 * 拖动环绕：横向位移按赤道弧长换算成水平角（往右拖 = 水平角增大、可绕到背面），纵向往上拖 = 俯仰增大（更俯）。
 * `radius` 是球在屏幕上的像素半径。改动后页签切回「自定义」（同滑杆，IMG-F-30）。
 */
export function multiAngleOrbit(
  start: ImageToolMultiAngleOptions,
  dx: number,
  dy: number,
  radius: number
): ImageToolMultiAngleOptions {
  const r = radius > 0 ? radius : 1;
  const horizontal = multiAngleWrap((start.horizontal ?? 0) + (dx / r) / RAD);
  const vertical = (start.vertical ?? 0) - (dy / r) / RAD;
  return imageToolSetAngle({ ...start, horizontal }, "vertical", vertical);
}

/**
 * 球上的键盘操作：方向键微调 5°（Shift = 与箭头按钮同一个大步进 ±45° / ±15°），Home 回正面平视。
 * 不认识的键返回 null（调用方不拦截默认行为）。
 */
export function multiAngleKeyStep(current: ImageToolMultiAngleOptions, key: string, big = false): ImageToolMultiAngleOptions | null {
  const directions: Record<string, "left" | "right" | "up" | "down"> = { ArrowLeft: "left", ArrowRight: "right", ArrowUp: "up", ArrowDown: "down" };
  const direction = directions[key];
  if (direction) {
    if (big) return imageToolStepAngle(current, direction);
    if (direction === "left" || direction === "right") {
      return { ...current, preset: "custom", horizontal: multiAngleWrap((current.horizontal ?? 0) + (direction === "right" ? 5 : -5)) };
    }
    return imageToolSetAngle(current, "vertical", (current.vertical ?? 0) + (direction === "up" ? 5 : -5));
  }
  if (key === "Home") return imageToolSetAngle(imageToolSetAngle(current, "horizontal", 0), "vertical", 0);
  return null;
}

/**
 * 相机离球心的距离（单位球半径的倍数）：景别 0（远）在球面上，10（特写）贴近主体。
 * 只是示意，不参与请求。
 */
export function multiAngleCameraDistance(zoom: number): number {
  const z = Math.min(10, Math.max(0, Number.isFinite(zoom) ? zoom : 5));
  return 1 - z * 0.045;
}

/** 一条经线 / 纬线投影后的一段：`back` = 在球背面（画成淡虚线）。 */
export type MultiAngleGridSegment = { d: string; back: boolean };

/**
 * 经纬网线（SVG path），按前 / 背半球切段：经线每 30°、纬线 -60~60 每 30°。
 * `cx, cy, radius` 是球在 SVG 里的圆心与像素半径。
 */
export function multiAngleGrid(cx: number, cy: number, radius: number, tilt: number = MULTI_ANGLE_VIEW_TILT): MultiAngleGridSegment[] {
  const lines: Array<Array<[number, number]>> = [];
  for (let h = 0; h < 180; h += 30) {
    const line: Array<[number, number]> = [];
    for (let v = -180; v <= 180; v += 6) line.push([h, v]);
    lines.push(line);
  }
  for (let v = -60; v <= 60; v += 30) {
    const line: Array<[number, number]> = [];
    for (let h = 0; h <= 360; h += 6) line.push([h, v]);
    lines.push(line);
  }
  const segments: MultiAngleGridSegment[] = [];
  for (const line of lines) {
    let current: string[] = [];
    let back: boolean | null = null;
    const flush = () => {
      if (current.length > 1 && back !== null) segments.push({ d: `M${current.join("L")}`, back });
      current = [];
    };
    for (const [h, v] of line) {
      // 经线越过极点（|v| > 90）等价于另一侧的经线：换算回合法的 (h, v)
      const overPole = Math.abs(v) > 90;
      const lat = overPole ? Math.sign(v) * 180 - v : v;
      const lon = overPole ? h + 180 : h;
      const point = multiAngleProject(lon, lat, tilt);
      const isBack = point.depth < 0;
      const coord = `${round2(cx + point.x * radius)},${round2(cy + point.y * radius)}`;
      if (back !== null && isBack !== back) {
        // 切段时让两段在交界处首尾相接，线不会断开一截
        current.push(coord);
        flush();
      }
      back = isBack;
      current.push(coord);
    }
    flush();
  }
  return segments;
}

function round2(value: number) {
  return Math.round(value * 100) / 100;
}

/** 编辑器状态：选项 + 「提示词」开关（开关不进契约，关着时不发 prompt）。 */
export type MultiAngleEditorState = { options: ImageToolMultiAngleOptions; promptOn: boolean; promptText: string };

/**
 * 子节点上的编辑器带上次的值（IMG-F-38）：从 `params.toolOptions` 读，缺的字段补默认值；
 * 有补充提示词 = 开关是开着的。
 */
export function multiAngleStateFromParams(params: Record<string, unknown> | undefined, config?: ImageToolsResolvedConfig): MultiAngleEditorState {
  const defaults = imageToolDefaultOptions("multi_angle", { config });
  const raw = params?.toolOptions;
  const saved = raw && typeof raw === "object" && !Array.isArray(raw) ? raw as Record<string, unknown> : {};
  const num = (value: unknown, fallback: number) => (typeof value === "number" && Number.isFinite(value) ? value : fallback);
  const prompt = typeof saved.prompt === "string" ? saved.prompt : "";
  return {
    options: {
      preset: (typeof saved.preset === "string" ? saved.preset : defaults.preset) as ImageToolMultiAngleOptions["preset"],
      horizontal: num(saved.horizontal, defaults.horizontal ?? 0),
      vertical: num(saved.vertical, defaults.vertical ?? 0),
      zoom: num(saved.zoom, defaults.zoom ?? 5)
    },
    promptOn: prompt.trim().length > 0,
    promptText: prompt
  };
}

/**
 * 发出去 / 写进节点的选项：开关关着时**不带** prompt（IMG-F-33/34），开着时截到 200 个码点（与服务端同一口径）。
 * 去首尾空白与空串删除由 `imageToolWireOptions` 负责。
 */
export function multiAngleOptionsOf(state: MultiAngleEditorState): ImageToolMultiAngleOptions {
  const options: ImageToolMultiAngleOptions = { ...state.options };
  delete options.prompt;
  if (state.promptOn) {
    const text = multiAnglePromptClamp(state.promptText);
    if (text.trim()) options.prompt = text;
  }
  return options;
}

/** 按码点截断到 200（`maxlength` 按 UTF-16 计，emoji 会被算成两个）。 */
export function multiAnglePromptClamp(text: string): string {
  const chars = Array.from(text ?? "");
  return chars.length > IMAGE_TOOL_MULTI_ANGLE_PROMPT_MAX ? chars.slice(0, IMAGE_TOOL_MULTI_ANGLE_PROMPT_MAX).join("") : text ?? "";
}

/** 码点数（输入框右下角的 N/200）。 */
export function multiAnglePromptLength(text: string): number {
  return Array.from(text ?? "").length;
}

/** 水平角的方位说明（读屏 / 悬停）：正面 / 右侧 / 背面 / 左侧，介于之间的说「右前方」等。 */
export function multiAngleHorizontalText(horizontal: number): string {
  const h = multiAngleWrap(horizontal);
  const names = ["正面", "右前方", "右侧", "右后方", "背面", "左后方", "左侧", "左前方"];
  return names[Math.round(h / 45) % 8]!;
}

/** 俯仰的说明：平视 / 俯拍 / 仰拍。 */
export function multiAngleVerticalText(vertical: number): string {
  if (vertical > 5) return "俯拍";
  if (vertical < -5) return "仰拍";
  return "平视";
}
