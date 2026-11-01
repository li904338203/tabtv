/**
 * 打光编辑器（docs/agent/imagetools IMGT-16，IMG-F-40~44，IMGD-27）的纯函数：光源球的投影 / 反算、
 * 方向键步进、六方向按钮判定、颜色选择、光锥几何、节点选项归一。
 *
 * 坐标约定（与服务端 `imagetool/domain/relight.go` 一致）：`azimuth` 0 = 前方（朝向观看者）、90 = 右、180 = 后、270 = 左；
 * `elevation` -90~90，正值朝上。世界坐标：x 向右、y 向上、z 朝向观看者。
 *
 * ⚠️ 不在前端拼提示词：这里只产出结构化选项（方位角 / 仰角 / 亮度 / 颜色 / 轮廓光），翻译全在服务端（CLAUDE.md §5.9）。
 * ⚠️ 导出名一律带 `imageToolRelight` 前缀，调用处显式 import（Nuxt 自动导入 utils 时重名会悄悄顶替，CLAUDE.md §5.28）。
 * ⚠️ 角度一律输出**整数**：契约里 azimuth / elevation 是 integer，带小数的话服务端 400。
 */
import type { ImageToolRelightOptions, ImageToolsResolvedConfig } from "~/utils/imageTools";

export type ImageToolRelightView = "perspective" | "front";
export type ImageToolRelightPreset = ImageToolsResolvedConfig["lightPresets"][number];
export type ImageToolRelightNamedColor = ImageToolsResolvedConfig["namedColors"][number];

/** 透视视角的俯视角（度）：从略高处看球，赤道成椭圆、前后两半球分得开（前方光在下、后方光在上且被遮住）。 */
export const IMAGE_TOOL_RELIGHT_PITCH = 22;
/** 方向键一步的角度。 */
export const IMAGE_TOOL_RELIGHT_STEP = 15;

/** 投影结果：单位圆内的屏幕坐标（x 向右、y **向下**，可直接乘半径加圆心画 SVG）；`visible` = 在朝向观看者的半球上。 */
export type ImageToolRelightPoint = { x: number; y: number; visible: boolean };

const RAD = Math.PI / 180;

function pitchOf(view: ImageToolRelightView) {
  return view === "perspective" ? IMAGE_TOOL_RELIGHT_PITCH * RAD : 0;
}

/** 角度归一到 [0, 360) 的整数。 */
export function imageToolRelightNormalizeAzimuth(azimuth: number): number {
  if (!Number.isFinite(azimuth)) return 0;
  const value = Math.round(azimuth) % 360;
  return value < 0 ? value + 360 : value === 0 ? 0 : value;
}

/** 仰角夹到 [-90, 90] 的整数。 */
export function imageToolRelightClampElevation(elevation: number): number {
  if (!Number.isFinite(elevation)) return 0;
  const value = Math.min(90, Math.max(-90, Math.round(elevation)));
  return value === 0 ? 0 : value; // 去掉 -0（序列化无差别，但比较与快照对比会把它当成另一个值）
}

/** 方位角 / 仰角 → 世界坐标单位向量。 */
function toVector(azimuth: number, elevation: number) {
  const az = azimuth * RAD;
  const el = elevation * RAD;
  return { x: Math.cos(el) * Math.sin(az), y: Math.sin(el), z: Math.cos(el) * Math.cos(az) };
}

/**
 * 把光源方向投影到球的屏幕上（正交投影）。
 * 正面视角：从正前方看（z 轴朝观看者）；透视视角：相机再抬高 `IMAGE_TOOL_RELIGHT_PITCH` 度往下看。
 */
export function imageToolRelightProject(azimuth: number, elevation: number, view: ImageToolRelightView): ImageToolRelightPoint {
  const v = toVector(azimuth, elevation);
  const p = pitchOf(view);
  const yc = v.y * Math.cos(p) - v.z * Math.sin(p);
  const zc = v.y * Math.sin(p) + v.z * Math.cos(p);
  return { x: round4(v.x), y: round4(-yc), visible: zc >= -1e-9 };
}

/**
 * 屏幕坐标（单位圆内，y 向下）反算方位角 / 仰角（拖动光源点用）。
 *
 * - 圆外的点先收回圆周上（拖出球外不报错，光源贴着球的边缘走）；
 * - `back = true` 取背向观看者的那一半球：拖动开始时光源在背面（如「后方」）就一直在背面拖，
 *   否则一拖它就从球后面跳到前面来；
 * - 极点附近方位角无定义，沿用 `fallbackAzimuth`（否则拖过头顶时方位角会随机跳）。
 */
export function imageToolRelightUnproject(
  point: { x: number; y: number },
  view: ImageToolRelightView,
  options: { back?: boolean; fallbackAzimuth?: number } = {}
): { azimuth: number; elevation: number } {
  let x = Number.isFinite(point.x) ? point.x : 0;
  let yc = Number.isFinite(point.y) ? -point.y : 0;
  const length = Math.hypot(x, yc);
  if (length > 1) {
    x /= length;
    yc /= length;
  }
  const zc = Math.sqrt(Math.max(0, 1 - x * x - yc * yc)) * (options.back ? -1 : 1);
  const p = pitchOf(view);
  const vx = x;
  const vy = yc * Math.cos(p) + zc * Math.sin(p);
  const vz = -yc * Math.sin(p) + zc * Math.cos(p);
  const elevation = imageToolRelightClampElevation(Math.asin(Math.max(-1, Math.min(1, vy))) / RAD);
  const horizontal = Math.hypot(vx, vz);
  const azimuth = horizontal < 1e-3
    ? imageToolRelightNormalizeAzimuth(options.fallbackAzimuth ?? 0)
    : imageToolRelightNormalizeAzimuth(Math.atan2(vx, vz) / RAD);
  return { azimuth, elevation };
}

/** 是否在背向观看者的半球（拖动开始时用来定 `back`）。 */
export function imageToolRelightIsBack(azimuth: number, elevation: number, view: ImageToolRelightView): boolean {
  return !imageToolRelightProject(azimuth, elevation, view).visible;
}

/**
 * 方向键步进（键盘可操作，IMG-UX）：
 * - ← / →：光源朝屏幕左 / 右移动 —— 沿较短的弧走向左侧（270°）/ 右侧（90°），到了就停。
 *   ⚠️ 不能简单地 azimuth ∓ 15：光在背面（180°）时减 15 是往**右**移，按左键却往右走。
 * - ↑ / ↓：仰角 ±15，夹在 ±90。
 */
export function imageToolRelightStep(
  current: { azimuth: number; elevation: number },
  direction: "left" | "right" | "up" | "down",
  step = IMAGE_TOOL_RELIGHT_STEP
): { azimuth: number; elevation: number } {
  const azimuth = imageToolRelightNormalizeAzimuth(current.azimuth);
  const elevation = imageToolRelightClampElevation(current.elevation);
  if (direction === "up") return { azimuth, elevation: imageToolRelightClampElevation(elevation + step) };
  if (direction === "down") return { azimuth, elevation: imageToolRelightClampElevation(elevation - step) };
  const target = direction === "left" ? 270 : 90;
  let diff = ((target - azimuth + 540) % 360) - 180;
  // 正对面（差 180°）时两条路一样长：走经过前方的那条（用户看得见的那一半）
  if (Math.abs(diff) === 180) diff = direction === "left" ? -180 : 180;
  const move = Math.sign(diff) * Math.min(Math.abs(diff), step);
  return { azimuth: imageToolRelightNormalizeAzimuth(azimuth + move), elevation };
}

/**
 * 当前方向对应的预设码（六方向按钮的高亮，IMG-F-41）：方位角与仰角都**精确等于**某个预设才算；
 * 拖动之后不再匹配任何预设时返回 null（按钮全不亮 = 自定义方向）。
 */
export function imageToolRelightMatchPreset(presets: readonly ImageToolRelightPreset[], azimuth: number, elevation: number): string | null {
  const az = imageToolRelightNormalizeAzimuth(azimuth);
  const el = imageToolRelightClampElevation(elevation);
  const hit = presets.find(item => imageToolRelightNormalizeAzimuth(item.azimuth) === az && imageToolRelightClampElevation(item.elevation) === el);
  return hit?.code ?? null;
}

/**
 * 写入一个方向：`preset` 跟着「是否恰好等于某个预设」走（服务端只校验它是已知码，方向以角度为准）。
 * 点按钮与拖动走同一个函数 —— 拖回恰好等于「右侧」的位置时按钮重新亮起，而不是停在「自定义」。
 */
export function imageToolRelightWithDirection(
  options: ImageToolRelightOptions,
  presets: readonly ImageToolRelightPreset[],
  direction: { azimuth: number; elevation: number }
): ImageToolRelightOptions {
  const azimuth = imageToolRelightNormalizeAzimuth(direction.azimuth);
  const elevation = imageToolRelightClampElevation(direction.elevation);
  const preset = imageToolRelightMatchPreset(presets, azimuth, elevation);
  const next: ImageToolRelightOptions = { ...options, azimuth, elevation };
  if (preset) next.preset = preset as ImageToolRelightOptions["preset"];
  else delete next.preset;
  return next;
}

/** 点六方向按钮：写预设的固定值；预设码不在表里（配置下线了它）时原样返回。 */
export function imageToolRelightApplyPreset(
  options: ImageToolRelightOptions,
  presets: readonly ImageToolRelightPreset[],
  code: string
): ImageToolRelightOptions {
  const preset = presets.find(item => item.code === code);
  if (!preset) return options;
  return imageToolRelightWithDirection(options, presets, preset);
}

/** 亮度：0~100 的整数；非数字时保持 `fallback`（输入框清空的那一刻不该把亮度改成 0）。 */
export function imageToolRelightClampBrightness(value: unknown, fallback = 50): number {
  const number = typeof value === "string" ? Number(value.replace(/%$/, "").trim()) : Number(value);
  if (value === "" || value === null || value === undefined || !Number.isFinite(number)) return fallback;
  return Math.min(100, Math.max(0, Math.round(number)));
}

/** 颜色规范成 `#rrggbb`（小写）；空 / 非法返回 null（= 「无」，保留原色温）。接受 `#RGB` 简写。 */
export function imageToolRelightNormalizeColor(value: unknown): string | null {
  if (typeof value !== "string") return null;
  let hex = value.trim().replace(/^#/, "");
  if (/^[0-9a-f]{3}$/i.test(hex)) hex = hex.split("").map(ch => ch + ch).join("");
  return /^[0-9a-f]{6}$/i.test(hex) ? `#${hex.toLowerCase()}` : null;
}

/** 色板里第几个（不区分大小写）；不在色板里（自定义取色或「无」）返回 -1。 */
export function imageToolRelightColorIndex(colors: readonly ImageToolRelightNamedColor[], color: string | null | undefined): number {
  const normalized = imageToolRelightNormalizeColor(color);
  if (!normalized) return -1;
  return colors.findIndex(item => imageToolRelightNormalizeColor(item.hex) === normalized);
}

/** 当前颜色的说明文字：「无（保留原色温）」/ 色板名 / 「自定义 #rrggbb」。 */
export function imageToolRelightColorLabel(colors: readonly ImageToolRelightNamedColor[], color: string | null | undefined): string {
  const normalized = imageToolRelightNormalizeColor(color);
  if (!normalized) return "无（保留原色温）";
  const index = imageToolRelightColorIndex(colors, normalized);
  return index >= 0 ? colors[index]!.name : `自定义 ${normalized}`;
}

/**
 * 节点 params 里存着的选项 → 编辑器用的完整选项（node 模式回显，IMG-F-48）。
 * 缺的字段用默认值补；`smart:true` 时节点上只存了 `{smart:true}`（只发这一句），其余回到默认 ——
 * 关掉智能模式时用户看到的是默认的前方 / 50%，而不是一堆空控件。
 */
export function imageToolRelightOptionsOf(raw: unknown, defaults: ImageToolRelightOptions, presets: readonly ImageToolRelightPreset[]): ImageToolRelightOptions {
  const value = (raw && typeof raw === "object" ? raw : {}) as Record<string, unknown>;
  const base: ImageToolRelightOptions = {
    ...defaults,
    smart: value.smart === true,
    brightness: imageToolRelightClampBrightness(value.brightness, defaults.brightness ?? 50),
    color: imageToolRelightNormalizeColor(value.color),
    rim: value.rim === true
  };
  const azimuth = typeof value.azimuth === "number" ? value.azimuth : defaults.azimuth ?? 0;
  const elevation = typeof value.elevation === "number" ? value.elevation : defaults.elevation ?? 0;
  return imageToolRelightWithDirection(base, presets, { azimuth, elevation });
}

/**
 * 光锥：从光源点照向源图卡片中心的三角形（SVG polygon 的 points）。
 * 光源与中心几乎重合（正面视角下的前方光）时没有方向可画，返回空串 —— 由调用方画一圈光晕代替。
 */
export function imageToolRelightCone(light: { x: number; y: number }, center: { x: number; y: number }, halfWidth: number): string {
  const dx = center.x - light.x;
  const dy = center.y - light.y;
  const length = Math.hypot(dx, dy);
  if (length < 1e-6 || halfWidth <= 0) return "";
  const nx = -dy / length * halfWidth;
  const ny = dx / length * halfWidth;
  const fmt = (n: number) => String(round2(n));
  return `${fmt(light.x)},${fmt(light.y)} ${fmt(center.x + nx)},${fmt(center.y + ny)} ${fmt(center.x - nx)},${fmt(center.y - ny)}`;
}

function round4(n: number) {
  const value = Math.round(n * 10000) / 10000;
  return Object.is(value, -0) ? 0 : value;
}

function round2(n: number) {
  const value = Math.round(n * 100) / 100;
  return Object.is(value, -0) ? 0 : value;
}
