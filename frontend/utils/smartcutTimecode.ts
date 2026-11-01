/**
 * 帧级时码（docs/agent/smartcut SC-F-23、SC-UX-03）：`HH:MM:SS:FF`，按时间线帧率（24 / 25 / 30，契约允许 1~120）。
 *
 * 量化规则（三条一起保证「往返一致」）：
 * - 毫秒 → 帧：`floor(ms × fps / 1000)` —— 播放头落在哪一帧里，显示的就是哪一帧（不提前半帧进位）；
 * - 帧 → 毫秒：`ceil(帧 × 1000 / fps)` —— 帧的起点向上取整到整毫秒（24fps 的帧边界不是整毫秒）；
 *   向下取整的话 `83ms` 会被算回第 1 帧，逐帧步进在 24fps 下会原地踏步；
 * - 「量化」= 先换成帧再换回毫秒：得到这一帧的起点，且不会越过原值。
 *
 * ⚠️ 不做丢帧时码（29.97 / 59.94）：时间线帧率是整数（契约 1~120）。
 * ⚠️ 负数、NaN 一律按 0；超过 99 小时的不截断，小时位照常变宽（成片最长 30 分钟，这只是防御）。
 */

/** 剪辑台可选的导出帧率（SC-PARAM-17）。 */
export const smartcutTimecodeFpsOptions = [24, 25, 30] as const;

/** 帧率取整、非法时回落 30（时间线的默认帧率）。 */
function fpsOf(fps: number): number {
  return Number.isFinite(fps) && fps >= 1 ? Math.round(fps) : 30;
}

/** 毫秒 → 帧序号（这一时刻正显示的那一帧）。 */
export function smartcutMsToFrames(ms: number, fps: number): number {
  if (!Number.isFinite(ms) || ms <= 0) return 0;
  return Math.floor((Math.round(ms) * fpsOf(fps)) / 1000);
}

/** 帧序号 → 这一帧起点的毫秒（向上取整到整毫秒）。 */
export function smartcutFramesToMs(frames: number, fps: number): number {
  if (!Number.isFinite(frames) || frames <= 0) return 0;
  return Math.ceil((Math.floor(frames) * 1000) / fpsOf(fps));
}

/** 毫秒量化到所在帧的起点。 */
export function smartcutQuantizeMs(ms: number, fps: number): number {
  return smartcutFramesToMs(smartcutMsToFrames(ms, fps), fps);
}

/** 一帧有多长（毫秒，向上取整）：给「逐帧」按钮的提示与吸附阈值用。 */
export function smartcutFrameMs(fps: number): number {
  return Math.ceil(1000 / fpsOf(fps));
}

const pad = (n: number, width = 2) => String(n).padStart(width, "0");

/**
 * `HH:MM:SS:FF`。帧位宽度跟着帧率走（≥100fps 时 3 位），其余各位至少 2 位。
 */
export function smartcutFormatTimecode(ms: number, fps: number): string {
  const rate = fpsOf(fps);
  const frames = smartcutMsToFrames(ms, rate);
  const ff = frames % rate;
  const seconds = Math.floor(frames / rate);
  const ss = seconds % 60;
  const mm = Math.floor(seconds / 60) % 60;
  const hh = Math.floor(seconds / 3600);
  return `${pad(hh)}:${pad(mm)}:${pad(ss)}:${pad(ff, Math.max(2, String(rate - 1).length))}`;
}

/**
 * 反向解析：`HH:MM:SS:FF` 或 `MM:SS:FF` → 毫秒（这一帧的起点）。格式不对、分秒 ≥ 60、帧 ≥ 帧率时返回 null，
 * 不猜（输入框里打错一个数时宁可不跳，也不要把播放头送到一个莫名其妙的位置）。
 */
export function smartcutParseTimecode(text: string, fps: number): number | null {
  const rate = fpsOf(fps);
  const parts = text.trim().split(":");
  if (parts.length !== 3 && parts.length !== 4) return null;
  if (!parts.every(part => /^\d{1,4}$/.test(part))) return null;
  const nums = parts.map(Number);
  const [hh, mm, ss, ff] = nums.length === 4 ? nums : [0, ...nums];
  if (mm! >= 60 || ss! >= 60 || ff! >= rate) return null;
  const frames = ((hh! * 60 + mm!) * 60 + ss!) * rate + ff!;
  return smartcutFramesToMs(frames, rate);
}

/**
 * ← / →（逐帧）与 Shift+← / →（逐秒）之后播放头去哪（SC-UX-03）。结果总在帧边界上，夹在 [0, maxMs]。
 * 逐秒按「帧率个帧」走，不按 1000 毫秒走：后者在 24fps 下会让播放头离开帧边界。
 */
export function smartcutStepMs(ms: number, fps: number, unit: "frame" | "second", delta: number, maxMs = Number.POSITIVE_INFINITY): number {
  const rate = fpsOf(fps);
  const step = unit === "second" ? rate : 1;
  const frames = smartcutMsToFrames(ms, rate) + Math.trunc(delta) * step;
  const next = smartcutFramesToMs(Math.max(0, frames), rate);
  return Math.min(next, Math.max(0, maxMs));
}
