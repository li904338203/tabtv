/**
 * 3D 导演台的时间轴（DRT-24，dev/03 §10）：采样、弧长、关键帧增删改、缩短校验。
 * 纯函数，不 import three —— 引擎（seek / play）与导出（DRT-25 逐帧 seek）都只调这里，**帧号决定一切**（DR-N-07）。
 *
 * 口径（dev/02 §2.4、DR-PARAM-09 / 10）：
 * - 固定 30 fps，总时长 30~450 帧；关键帧帧号 0~durationFrames；每条轨道 ≤ 120 个、全场景 ≤ 2000 个；
 * - 角度一律是度、欧拉 XYZ（与场景正文、three 的 `Euler(…, "XYZ")` 同一约定）；
 * - 一个关键帧的 `interpolation` 管的是**从它到下一个关键帧**的那一段；缺省按 linear。
 *
 * ⚠️ 旋转一律转成四元数 slerp（取最短路径）：直接对欧拉角线性插值，在 ±180° 附近会绕远路转一大圈。
 * ⚠️ 平滑段的位置按弧长重参数化（DR-F-124）：Catmull-Rom 的参数与弧长不成正比，不重参数化的话机位忽快忽慢。
 *    每一段内部恒速，并且在关键帧那一帧正好落在关键帧的位置上 —— 整条路径一个速度的话关键帧就对不上了。
 * ⚠️ 对象操作一律不可变（返回新时间轴），同 directorScene：撤销栈按快照工作（DRD-36）。
 */
import { DIRECTOR_LIMITS } from "./directorCatalog";

export const DIRECTOR_FPS = 30;
export const DIRECTOR_MIN_DURATION_FRAMES = 30;
export const DIRECTOR_MAX_DURATION_FRAMES = 450;
export const DIRECTOR_DEFAULT_DURATION_FRAMES = 150;
/** 弧长表默认的采样数：中点误差远小于 DR-AC-35 要求的 1%。 */
export const DIRECTOR_ARC_SAMPLES = 256;

export type DirectorTimelineVec3 = { x: number; y: number; z: number };
export type DirectorInterpolation = "linear" | "smooth" | "hold";
export type DirectorChannel = "transform" | "pose" | "camera";

/** transform / camera 通道的值（camera 多一个焦距）。 */
export type DirectorMotionValue = {
  position: DirectorTimelineVec3;
  rotation: DirectorTimelineVec3;
  focalLengthMm?: number;
  [key: string]: unknown;
};

/** pose 通道的值：预设 + 逐骨骼覆盖（骨骼名 → 欧拉角，度）。 */
export type DirectorPoseValue = {
  posePreset?: string;
  boneOverrides?: Record<string, DirectorTimelineVec3>;
  [key: string]: unknown;
};

export type DirectorKeyframe<V = DirectorMotionValue | DirectorPoseValue> = {
  frame: number;
  interpolation?: DirectorInterpolation;
  value: V;
  [key: string]: unknown;
};

export type DirectorTrack = {
  objectId: string;
  channel: DirectorChannel;
  keyframes: DirectorKeyframe[];
  [key: string]: unknown;
};

export type DirectorTimeline = {
  fps?: number;
  durationFrames?: number;
  loop?: boolean;
  tracks?: DirectorTrack[];
  [key: string]: unknown;
};

/** 关键帧编辑被拒的原因（界面据此说清楚，不笼统说「失败」）。 */
export type DirectorTimelineError =
  | { error: "limit"; what: "track" | "scene"; limit: number }
  | { error: "occupied"; frame: number }
  | { error: "missing" };

// ---------------------------------------------------------------- 帧与秒

export function directorFramesToSeconds(frames: number): number {
  return frames / DIRECTOR_FPS;
}

/** 秒 → 帧，四舍五入到整帧。 */
export function directorSecondsToFrames(seconds: number): number {
  return Math.round(seconds * DIRECTOR_FPS);
}

export function directorDurationFrames(timeline: DirectorTimeline | undefined): number {
  const value = timeline?.durationFrames;
  return typeof value === "number" && Number.isFinite(value) ? value : DIRECTOR_DEFAULT_DURATION_FRAMES;
}

// ---------------------------------------------------------------- 向量与四元数

type Quat = { x: number; y: number; z: number; w: number };
const DEG = Math.PI / 180;

function lerp(a: number, b: number, t: number): number {
  return a + (b - a) * t;
}

function lerpVec(a: DirectorTimelineVec3, b: DirectorTimelineVec3, t: number): DirectorTimelineVec3 {
  return { x: lerp(a.x, b.x, t), y: lerp(a.y, b.y, t), z: lerp(a.z, b.z, t) };
}

function distance(a: DirectorTimelineVec3, b: DirectorTimelineVec3): number {
  return Math.hypot(b.x - a.x, b.y - a.y, b.z - a.z);
}

/** 欧拉 XYZ（度）→ 四元数，与 three 的 `Quaternion.setFromEuler(Euler(…, "XYZ"))` 同一公式。 */
export function directorQuatFromEuler(rotation: DirectorTimelineVec3): Quat {
  const c1 = Math.cos((rotation.x * DEG) / 2);
  const c2 = Math.cos((rotation.y * DEG) / 2);
  const c3 = Math.cos((rotation.z * DEG) / 2);
  const s1 = Math.sin((rotation.x * DEG) / 2);
  const s2 = Math.sin((rotation.y * DEG) / 2);
  const s3 = Math.sin((rotation.z * DEG) / 2);
  return {
    x: s1 * c2 * c3 + c1 * s2 * s3,
    y: c1 * s2 * c3 - s1 * c2 * s3,
    z: c1 * c2 * s3 + s1 * s2 * c3,
    w: c1 * c2 * c3 - s1 * s2 * s3
  };
}

/** 四元数 → 欧拉 XYZ（度），与 three 的 `Euler.setFromQuaternion(q, "XYZ")` 同一公式（含万向锁分支）。 */
export function directorEulerFromQuat(q: Quat): DirectorTimelineVec3 {
  const { x, y, z, w } = q;
  const m11 = 1 - 2 * (y * y + z * z);
  const m12 = 2 * (x * y - w * z);
  const m13 = 2 * (x * z + w * y);
  const m22 = 1 - 2 * (x * x + z * z);
  const m23 = 2 * (y * z - w * x);
  const m32 = 2 * (y * z + w * x);
  const m33 = 1 - 2 * (x * x + y * y);
  const ry = Math.asin(Math.min(1, Math.max(-1, m13)));
  let rx: number;
  let rz: number;
  if (Math.abs(m13) < 0.9999999) {
    rx = Math.atan2(-m23, m33);
    rz = Math.atan2(-m12, m11);
  } else {
    rx = Math.atan2(m32, m22);
    rz = 0;
  }
  return { x: rx / DEG, y: ry / DEG, z: rz / DEG };
}

/** 球面插值，取最短路径（点积为负时把一端取反）。 */
export function directorSlerp(a: Quat, b: Quat, t: number): Quat {
  let bx = b.x;
  let by = b.y;
  let bz = b.z;
  let bw = b.w;
  let dot = a.x * bx + a.y * by + a.z * bz + a.w * bw;
  if (dot < 0) {
    dot = -dot;
    bx = -bx;
    by = -by;
    bz = -bz;
    bw = -bw;
  }
  if (dot > 0.9995) {
    // 夹角很小时 sin 接近 0：退化成归一化的线性插值
    const r = { x: lerp(a.x, bx, t), y: lerp(a.y, by, t), z: lerp(a.z, bz, t), w: lerp(a.w, bw, t) };
    const n = Math.hypot(r.x, r.y, r.z, r.w) || 1;
    return { x: r.x / n, y: r.y / n, z: r.z / n, w: r.w / n };
  }
  const theta = Math.acos(Math.min(1, dot));
  const sin = Math.sin(theta);
  const wa = Math.sin((1 - t) * theta) / sin;
  const wb = Math.sin(t * theta) / sin;
  return { x: a.x * wa + bx * wb, y: a.y * wa + by * wb, z: a.z * wa + bz * wb, w: a.w * wa + bw * wb };
}

/** 两个欧拉角（度）之间按最短路径插值。端点原样返回（不经四元数往返，避免 179.9999 这类抖动）。 */
export function directorSlerpEuler(a: DirectorTimelineVec3, b: DirectorTimelineVec3, t: number): DirectorTimelineVec3 {
  if (t <= 0) return { ...a };
  if (t >= 1) return { ...b };
  return directorEulerFromQuat(directorSlerp(directorQuatFromEuler(a), directorQuatFromEuler(b), t));
}

// ---------------------------------------------------------------- Catmull-Rom 与弧长

/** 均匀 Catmull-Rom：`u` 从 0 到 1 时从 p1 走到 p2，p0 / p3 是两侧的邻点。 */
export function directorCatmullRom(
  p0: DirectorTimelineVec3,
  p1: DirectorTimelineVec3,
  p2: DirectorTimelineVec3,
  p3: DirectorTimelineVec3,
  u: number
): DirectorTimelineVec3 {
  const u2 = u * u;
  const u3 = u2 * u;
  const axis = (a: number, b: number, c: number, d: number) =>
    0.5 * (2 * b + (-a + c) * u + (2 * a - 5 * b + 4 * c - d) * u2 + (-a + 3 * b - 3 * c + d) * u3);
  return { x: axis(p0.x, p1.x, p2.x, p3.x), y: axis(p0.y, p1.y, p2.y, p3.y), z: axis(p0.z, p1.z, p2.z, p3.z) };
}

export type DirectorArcTable = {
  /** `lengths[k]` = 从 u=0 到 u=k/samples 的累计弧长，`lengths[0] = 0`。 */
  lengths: number[];
  total: number;
  samples: number;
};

/**
 * 一段 Catmull-Rom 曲线（`[p0, p1, p2, p3]`，从 p1 到 p2）的弧长表：均匀取 `samples` 段折线累加。
 * 256 段时，一段四分之一圆弧的中点误差在 1e-5 量级。
 */
export function directorArcLengthTable(
  points: readonly [DirectorTimelineVec3, DirectorTimelineVec3, DirectorTimelineVec3, DirectorTimelineVec3],
  samples = DIRECTOR_ARC_SAMPLES
): DirectorArcTable {
  const n = Math.max(1, Math.floor(samples));
  const [p0, p1, p2, p3] = points;
  const lengths = [0];
  let previous = directorCatmullRom(p0, p1, p2, p3, 0);
  let total = 0;
  for (let k = 1; k <= n; k += 1) {
    const point = directorCatmullRom(p0, p1, p2, p3, k / n);
    total += distance(previous, point);
    lengths.push(total);
    previous = point;
  }
  return { lengths, total, samples: n };
}

/** 弧长 `s` → 曲线参数 `u`（0~1）：二分查表，再在相邻两个采样之间线性插值。 */
export function directorParamAtLength(table: DirectorArcTable, s: number): number {
  if (table.total <= 0 || s <= 0) return 0;
  if (s >= table.total) return 1;
  let lo = 0;
  let hi = table.samples;
  while (hi - lo > 1) {
    const mid = (lo + hi) >> 1;
    if ((table.lengths[mid] ?? 0) < s) lo = mid;
    else hi = mid;
  }
  const a = table.lengths[lo] ?? 0;
  const b = table.lengths[hi] ?? a;
  const fraction = b > a ? (s - a) / (b - a) : 0;
  return (lo + fraction) / table.samples;
}

// ---------------------------------------------------------------- 采样

/** 关键帧按帧号排好（不改入参）。 */
function sortedKeyframes<T extends DirectorKeyframe>(keyframes: readonly T[]): T[] {
  return [...keyframes].sort((a, b) => a.frame - b.frame);
}

/** 找到 `frame` 所在的那一段：返回段首的下标与段内的时间比例 t（0~1）。在首帧之前 / 末帧之后时 t 落在端点。 */
function locate(frames: readonly number[], frame: number): { index: number; t: number } {
  const last = frames.length - 1;
  if (frame <= (frames[0] ?? 0)) return { index: 0, t: 0 };
  if (frame >= (frames[last] ?? 0)) return { index: last, t: 0 };
  let index = 0;
  while (index < last - 1 && (frames[index + 1] ?? 0) <= frame) index += 1;
  const start = frames[index] ?? 0;
  const end = frames[index + 1] ?? start;
  return { index, t: end > start ? (frame - start) / (end - start) : 0 };
}

function motionOf(value: unknown): DirectorMotionValue {
  const v = (value ?? {}) as Partial<DirectorMotionValue>;
  const vec = (p: unknown): DirectorTimelineVec3 => {
    const o = (p ?? {}) as Partial<DirectorTimelineVec3>;
    return { x: Number(o.x ?? 0), y: Number(o.y ?? 0), z: Number(o.z ?? 0) };
  };
  return {
    position: vec(v.position),
    rotation: vec(v.rotation),
    ...(typeof v.focalLengthMm === "number" ? { focalLengthMm: v.focalLengthMm } : {})
  };
}

/** 平滑段的位置：Catmull-Rom（邻点取相邻关键帧，两端复制端点）+ 弧长重参数化，段内恒速。 */
function smoothPosition(points: readonly DirectorTimelineVec3[], index: number, t: number): DirectorTimelineVec3 {
  const p1 = points[index]!;
  const p2 = points[index + 1] ?? p1;
  const p0 = points[index - 1] ?? p1;
  const p3 = points[index + 2] ?? p2;
  const table = directorArcLengthTable([p0, p1, p2, p3]);
  return directorCatmullRom(p0, p1, p2, p3, directorParamAtLength(table, t * table.total));
}

/**
 * transform / camera 通道在第 `frame` 帧的值（frame 可以是小数）。
 * - linear：位置线性、旋转 slerp、焦距线性；
 * - smooth：位置 Catmull-Rom + 弧长重参数化（只有两个点时就是直线），旋转 slerp、焦距线性；
 * - hold：保持段首的值，到下一个关键帧那一帧才跳变；
 * - 首帧之前取首帧、末帧之后取末帧；只有一个关键帧时恒等于它；没有关键帧时为 null。
 */
export function directorSampleTrack(track: Pick<DirectorTrack, "keyframes">, frame: number): DirectorMotionValue | null {
  const keys = sortedKeyframes(track.keyframes ?? []);
  if (!keys.length) return null;
  const values = keys.map(key => motionOf(key.value));
  const { index, t } = locate(keys.map(key => key.frame), frame);
  const start = values[index]!;
  const next = values[index + 1];
  if (!next || t <= 0) return start;
  const interpolation = keys[index]?.interpolation ?? "linear";
  if (interpolation === "hold") return start;
  const position = interpolation === "smooth"
    ? smoothPosition(values.map(value => value.position), index, t)
    : lerpVec(start.position, next.position, t);
  const sample: DirectorMotionValue = { position, rotation: directorSlerpEuler(start.rotation, next.rotation, t) };
  if (start.focalLengthMm !== undefined || next.focalLengthMm !== undefined) {
    const a = start.focalLengthMm ?? next.focalLengthMm ?? 0;
    const b = next.focalLengthMm ?? a;
    sample.focalLengthMm = lerp(a, b, t);
  }
  return sample;
}

// ---------------------------------------------------------------- 姿势

/** 姿势目录：预设码 → 完整骨骼值（度）与可选的离地比例（R-3 #5）。查不到时为 undefined（按站姿处理由引擎决定）。 */
export type DirectorPoseLookup = (code: string) => { bones: Record<string, DirectorTimelineVec3>; lift?: number } | undefined;

/** 采样出来的姿势：完整的骨骼值（预设 + 覆盖，覆盖优先、不叠加，R-3 #4）。 */
export type DirectorPoseSample = {
  /** 正好落在某个关键帧上（或之前 / 之后）时就是那个关键帧的预设；两个姿势之间时为 undefined。 */
  posePreset?: string;
  bones: Record<string, DirectorTimelineVec3>;
  lift: number;
};

function fullPose(value: unknown, poses: DirectorPoseLookup): { preset?: string; bones: Record<string, DirectorTimelineVec3>; lift: number } {
  const v = (value ?? {}) as DirectorPoseValue;
  const preset = typeof v.posePreset === "string" && v.posePreset ? v.posePreset : undefined;
  const base = preset ? poses(preset) : undefined;
  // 覆盖值取代预设值（同一根骨骼两边都有时不叠加）
  return { preset, bones: { ...(base?.bones ?? {}), ...(v.boneOverrides ?? {}) }, lift: base?.lift ?? 0 };
}

/**
 * pose 通道在第 `frame` 帧的姿势：两个姿势之间**逐骨骼** slerp。预设不同也照常插值 ——
 * 两边都先展开成完整骨骼值（预设 + 覆盖），只有一边有的骨骼另一边按 0 度。
 * hold 段保持段首姿势；首帧之前 / 末帧之后取端点。
 */
export function directorPoseAt(track: Pick<DirectorTrack, "keyframes">, frame: number, poses: DirectorPoseLookup): DirectorPoseSample | null {
  const keys = sortedKeyframes(track.keyframes ?? []);
  if (!keys.length) return null;
  const { index, t } = locate(keys.map(key => key.frame), frame);
  const a = fullPose(keys[index]!.value, poses);
  const nextKey = keys[index + 1];
  const interpolation = keys[index]?.interpolation ?? "linear";
  if (!nextKey || t <= 0 || interpolation === "hold") {
    return { ...(a.preset ? { posePreset: a.preset } : {}), bones: a.bones, lift: a.lift };
  }
  const b = fullPose(nextKey.value, poses);
  const zero = { x: 0, y: 0, z: 0 };
  const bones: Record<string, DirectorTimelineVec3> = {};
  for (const name of new Set([...Object.keys(a.bones), ...Object.keys(b.bones)])) {
    bones[name] = directorSlerpEuler(a.bones[name] ?? zero, b.bones[name] ?? zero, t);
  }
  return { bones, lift: lerp(a.lift, b.lift, t) };
}

// ---------------------------------------------------------------- 编辑

function tracksOf(timeline: DirectorTimeline): DirectorTrack[] {
  return Array.isArray(timeline.tracks) ? timeline.tracks : [];
}

function totalKeyframes(tracks: readonly DirectorTrack[]): number {
  return tracks.reduce((sum, track) => sum + (track.keyframes?.length ?? 0), 0);
}

/**
 * 缩短总时长前的检查（DR-F-123）：有关键帧落在新时长之后就拒绝，并说明有几个。
 * 时长本身的范围（30~450 帧）由调用方按输入框的上下限管，这里只回答「会不会丢关键帧」。
 */
export function directorCanShrink(timeline: DirectorTimeline, newDurationFrames: number): { ok: boolean; outside: number } {
  const outside = tracksOf(timeline)
    .reduce((sum, track) => sum + (track.keyframes ?? []).filter(key => key.frame > newDurationFrames).length, 0);
  return { ok: outside === 0, outside };
}

/** 「有 N 个关键帧落在 X 秒之后」：缩短被拒时的提示（DR-AC-35）。 */
export function directorShrinkRejectedText(outside: number, newDurationFrames: number): string {
  const seconds = directorFramesToSeconds(newDurationFrames);
  const text = Number.isInteger(seconds) ? String(seconds) : seconds.toFixed(2).replace(/0+$/, "");
  return `有 ${outside} 个关键帧落在 ${text} 秒之后，请先删除或移动它们`;
}

function withTracks(timeline: DirectorTimeline, tracks: DirectorTrack[]): DirectorTimeline {
  return { ...timeline, tracks };
}

/**
 * 在某个对象的某条通道上写入关键帧：同一帧已有就覆盖（插值方式没给时沿用原来的）；没有轨道就建一条。
 * 新增会超过每轨 120 / 全场景 2000 时拒绝（DR-PARAM-09），覆盖不受上限影响。帧号夹在 `[0, durationFrames]` 并取整。
 */
export function directorUpsertKeyframe(
  timeline: DirectorTimeline,
  objectId: string,
  channel: DirectorChannel,
  frame: number,
  value: DirectorKeyframe["value"],
  interpolation?: DirectorInterpolation
): { timeline: DirectorTimeline } | DirectorTimelineError {
  const at = Math.min(directorDurationFrames(timeline), Math.max(0, Math.round(frame)));
  const tracks = tracksOf(timeline);
  const trackIndex = tracks.findIndex(track => track.objectId === objectId && track.channel === channel);
  const track = trackIndex >= 0 ? tracks[trackIndex]! : { objectId, channel, keyframes: [] };
  const keyframes = track.keyframes ?? [];
  const existing = keyframes.findIndex(key => key.frame === at);
  let nextKeyframes: DirectorKeyframe[];
  if (existing >= 0) {
    const old = keyframes[existing]!;
    nextKeyframes = keyframes.map((key, i) => i === existing
      ? { ...old, frame: at, value, ...(interpolation ? { interpolation } : {}) }
      : key);
  } else {
    if (keyframes.length >= DIRECTOR_LIMITS.maxKeyframesPerTrack) {
      return { error: "limit", what: "track", limit: DIRECTOR_LIMITS.maxKeyframesPerTrack };
    }
    if (totalKeyframes(tracks) >= DIRECTOR_LIMITS.maxKeyframes) {
      return { error: "limit", what: "scene", limit: DIRECTOR_LIMITS.maxKeyframes };
    }
    nextKeyframes = sortedKeyframes([...keyframes, { frame: at, interpolation: interpolation ?? "linear", value }]);
  }
  const nextTrack = { ...track, keyframes: nextKeyframes };
  const nextTracks = trackIndex >= 0
    ? tracks.map((item, i) => (i === trackIndex ? nextTrack : item))
    : [...tracks, nextTrack];
  return { timeline: withTracks(timeline, nextTracks) };
}

/**
 * 把一个关键帧从 `fromFrame` 挪到 `toFrame`：帧号夹在 `[0, durationFrames]` 并取整；落点已被同轨别的关键帧占着时拒绝
 * （合并两个关键帧会悄悄丢掉其中一个，用户看不出少了什么）。挪到原地视为成功、不产生变化。
 */
export function directorMoveKeyframe(
  timeline: DirectorTimeline,
  objectId: string,
  channel: DirectorChannel,
  fromFrame: number,
  toFrame: number
): { timeline: DirectorTimeline } | DirectorTimelineError {
  const tracks = tracksOf(timeline);
  const trackIndex = tracks.findIndex(track => track.objectId === objectId && track.channel === channel);
  const track = tracks[trackIndex];
  const keyIndex = track?.keyframes?.findIndex(key => key.frame === fromFrame) ?? -1;
  if (!track || keyIndex < 0) return { error: "missing" };
  const at = Math.min(directorDurationFrames(timeline), Math.max(0, Math.round(toFrame)));
  if (at === fromFrame) return { timeline };
  if (track.keyframes.some((key, i) => i !== keyIndex && key.frame === at)) return { error: "occupied", frame: at };
  const keyframes = sortedKeyframes(track.keyframes.map((key, i) => (i === keyIndex ? { ...key, frame: at } : key)));
  return { timeline: withTracks(timeline, tracks.map((item, i) => (i === trackIndex ? { ...track, keyframes } : item))) };
}

/** 删掉一个关键帧；轨道删空了就连轨道一起去掉（空轨道没有意义，还会让「有轨道就自动打帧」误判）。 */
export function directorDeleteKeyframe(
  timeline: DirectorTimeline,
  objectId: string,
  channel: DirectorChannel,
  frame: number
): { timeline: DirectorTimeline } | DirectorTimelineError {
  const tracks = tracksOf(timeline);
  const trackIndex = tracks.findIndex(track => track.objectId === objectId && track.channel === channel);
  const track = tracks[trackIndex];
  if (!track || !track.keyframes?.some(key => key.frame === frame)) return { error: "missing" };
  const keyframes = track.keyframes.filter(key => key.frame !== frame);
  const nextTracks = keyframes.length
    ? tracks.map((item, i) => (i === trackIndex ? { ...track, keyframes } : item))
    : tracks.filter((_, i) => i !== trackIndex);
  return { timeline: withTracks(timeline, nextTracks) };
}

/** 改一个关键帧的插值方式。 */
export function directorSetInterpolation(
  timeline: DirectorTimeline,
  objectId: string,
  channel: DirectorChannel,
  frame: number,
  interpolation: DirectorInterpolation
): { timeline: DirectorTimeline } | DirectorTimelineError {
  const tracks = tracksOf(timeline);
  const trackIndex = tracks.findIndex(track => track.objectId === objectId && track.channel === channel);
  const track = tracks[trackIndex];
  if (!track || !track.keyframes?.some(key => key.frame === frame)) return { error: "missing" };
  const keyframes = track.keyframes.map(key => (key.frame === frame ? { ...key, interpolation } : key));
  return { timeline: withTracks(timeline, tracks.map((item, i) => (i === trackIndex ? { ...track, keyframes } : item))) };
}

/** 关键帧编辑被拒时给用户看的话。 */
export function directorTimelineErrorText(error: DirectorTimelineError): string {
  if (error.error === "limit") {
    return error.what === "track"
      ? `这条轨道的关键帧已到上限（${error.limit} 个）`
      : `整个场景的关键帧已到上限（${error.limit} 个）`;
  }
  if (error.error === "occupied") return `第 ${error.frame} 帧已经有关键帧了`;
  return "这个关键帧已经不在了";
}

// ---------------------------------------------------------------- 时间轴界面（DRT-24，dev/03 §10）

/** 标尺缩放档位（每帧多少像素）：固定几档，连续缩放的话同一条时间轴在两台机器上看起来不一样。 */
export const DIRECTOR_TIMELINE_ZOOMS = Object.freeze([2, 4, 8, 16] as const);

const INTERPOLATION_NAMES: Record<DirectorInterpolation, string> = { linear: "线性", smooth: "平滑", hold: "保持" };

export function directorInterpolationName(interpolation: DirectorInterpolation | undefined): string {
  return INTERPOLATION_NAMES[interpolation ?? "linear"];
}

/** 「帧 45 · 1.50 s」：头部的当前位置。 */
export function directorFrameTimeText(frame: number): string {
  return `帧 ${Math.round(frame)} · ${directorFramesToSeconds(frame).toFixed(2)} s`;
}

/** 关键帧的无障碍名称：「机位 1 · 第 90 帧 · 平滑」。 */
export function directorKeyframeLabel(objectName: string, frame: number, interpolation: DirectorInterpolation | undefined): string {
  return `${objectName} · 第 ${frame} 帧 · ${directorInterpolationName(interpolation)}`;
}

/**
 * 标尺刻度：整秒（major，带文字）与半秒。缩到每帧 2 像素时半秒只有 30 像素，照样画得下。
 */
export function directorTimelineTicks(durationFrames: number): Array<{ frame: number; major: boolean; label: string }> {
  const ticks: Array<{ frame: number; major: boolean; label: string }> = [];
  const half = DIRECTOR_FPS / 2;
  for (let frame = 0; frame <= durationFrames; frame += half) {
    const major = frame % DIRECTOR_FPS === 0;
    ticks.push({ frame, major, label: major ? `${frame / DIRECTOR_FPS}s` : "" });
  }
  return ticks;
}

/** 标尺 / 轨道上横坐标（像素，从第 0 帧起）对应的帧号：四舍五入到整帧、夹在 [0, 总时长]。 */
export function directorFrameAtX(x: number, pxPerFrame: number, durationFrames: number): number {
  if (!(pxPerFrame > 0)) return 0;
  return Math.min(durationFrames, Math.max(0, Math.round(x / pxPerFrame)));
}
