/**
 * 3D 导演台的机位换算（DRT-11，DR-F-23，DR-PARAM-08）：焦距 ↔ 视角、焦距档位、画幅 ↔ 截图像素。
 * 纯函数，不 import three。镜头文字（景别 / 角度 / 焦段 / 运镜）由 DRT-26 加到这个文件里。
 *
 * ⚠️ 视角口径：`sensorMm`（场景字段 `camera.sensorWidthMm`）是传感器**长边**，与 three 的 `filmGauge` 同义——
 * 横幅时长边是宽，画面高 = 长边 ÷ 宽高比；竖幅时长边就是高。Go 求解器（`director/domain.verticalFOV`）
 * 用同一个算法按 `d = (coverage × H / 2) / tan(vfov / 2)` 摆机位，两边共用 `api/fixtures/director/camera/fov.json`。
 * 两边各写一种的话，AI 按中景摆的机位，截出来是近景或全景，而两边各自看都没错。
 */
import {
  DIRECTOR_ASPECTS,
  DIRECTOR_MODELS,
  DIRECTOR_OBJECT_DEFAULTS,
  DIRECTOR_SHOT_SIZES,
  type DirectorAspectCode,
  type DirectorShotSize
} from "./directorCatalog";
import type { DirectorObject, DirectorScene } from "./directorScene";

const DEG = 180 / Math.PI;

/** 焦距范围（mm，等效全画幅），与校验一致。 */
export const DIRECTOR_FOCAL_MIN = 12;
export const DIRECTOR_FOCAL_MAX = 200;

/** 焦距快捷档（DR-F-23）。 */
export const DIRECTOR_FOCAL_PRESETS: readonly number[] = Object.freeze([14, 18, 24, 35, 50, 85, 135]);

/** 画幅码 → 宽 ÷ 高；不认识的画幅抛错，不猜（与 Go 的 aspectRatio 一致）。也接受直接给的宽高比。 */
export function directorAspectRatio(aspect: DirectorAspectCode | string | number): number {
  if (typeof aspect === "number") {
    if (!Number.isFinite(aspect) || aspect <= 0) throw new Error(`宽高比必须是正数：${aspect}`);
    return aspect;
  }
  if (!DIRECTOR_ASPECTS.some((a) => a.code === aspect)) throw new Error(`不认识的画幅 ${JSON.stringify(aspect)}`);
  const [w, h] = aspect.split(":").map(Number);
  return (w as number) / (h as number);
}

function filmHeight(sensorMm: number, aspect: DirectorAspectCode | string | number): number {
  const ratio = directorAspectRatio(aspect);
  return ratio >= 1 ? sensorMm / ratio : sensorMm;
}

function assertPositive(name: string, v: number) {
  if (!Number.isFinite(v) || v <= 0) throw new Error(`${name}必须是正数：${v}`);
}

/** 竖直视角（度）：`vfov = 2·atan(画面高 / (2f))`，画面高按「传感器长边」换算。 */
export function directorVerticalFov(focalMm: number, sensorMm: number, aspect: DirectorAspectCode | string | number): number {
  assertPositive("焦距", focalMm);
  assertPositive("传感器尺寸", sensorMm);
  return 2 * Math.atan(filmHeight(sensorMm, aspect) / (2 * focalMm)) * DEG;
}

/** `directorVerticalFov` 的反函数：给定竖直视角（度）求焦距（mm）。 */
export function directorFocalForVerticalFov(vfovDeg: number, sensorMm: number, aspect: DirectorAspectCode | string | number): number {
  if (!Number.isFinite(vfovDeg) || vfovDeg <= 0 || vfovDeg >= 180) throw new Error(`竖直视角必须在 0~180 度之间：${vfovDeg}`);
  assertPositive("传感器尺寸", sensorMm);
  return filmHeight(sensorMm, aspect) / (2 * Math.tan(vfovDeg / DEG / 2));
}

/** 把焦距夹到 12~200 mm。非有限值回落到 35mm（默认焦距），不让一个 NaN 一路传进渲染。 */
export function directorClampFocal(focalMm: number): number {
  if (!Number.isFinite(focalMm)) return 35;
  return Math.min(DIRECTOR_FOCAL_MAX, Math.max(DIRECTOR_FOCAL_MIN, focalMm));
}

/** 离给定焦距最近的快捷档（等距时取较短的那一档）。 */
export function directorNearestFocalPreset(focalMm: number): number {
  let best = DIRECTOR_FOCAL_PRESETS[0] as number;
  for (const preset of DIRECTOR_FOCAL_PRESETS) {
    if (Math.abs(preset - focalMm) < Math.abs(best - focalMm)) best = preset;
  }
  return best;
}

/**
 * 截图像素（DR-PARAM-08）：按画幅查表，与屏幕分辨率、缩放无关（DR-F-71）。未知画幅抛错——
 * 猜一个尺寸的话，截出来的图与画幅框对不上，而下游拿它当构图参考。
 */
export function directorCaptureSize(aspect: DirectorAspectCode | string): { width: number; height: number } {
  const a = DIRECTOR_ASPECTS.find((x) => x.code === aspect);
  if (!a) throw new Error(`不认识的画幅 ${JSON.stringify(aspect)}`);
  return { width: a.width, height: a.height };
}

// ---------------------------------------------------------------- 视口与走位（DRT-12）

/** 视口里的一个矩形（CSS 像素，原点在左上角）。 */
export type DirectorRect = { x: number; y: number; width: number; height: number };

/**
 * 机位视角的画幅框（DR-AC-05）：在 `viewW × viewH` 的视口里放一个宽高比为机位画幅的最大矩形，居中，
 * 其余是黑边（框外的遮罩由界面画，DRT-15）。返回整数像素：渲染器的视口与裁剪只认整数，
 * 舍入误差不超过 1 像素。视口为空时返回 0×0。
 */
export function directorLetterbox(viewW: number, viewH: number, aspect: DirectorAspectCode | string | number): DirectorRect {
  const ratio = directorAspectRatio(aspect);
  if (!(viewW > 0) || !(viewH > 0)) return { x: 0, y: 0, width: 0, height: 0 };
  if (viewW / viewH > ratio) {
    // 视口比画幅更宽：上下撑满，左右留黑边。
    const width = Math.min(viewW, Math.round(viewH * ratio));
    return { x: Math.floor((viewW - width) / 2), y: 0, width, height: Math.round(viewH) };
  }
  const height = Math.min(viewH, Math.round(viewW / ratio));
  return { x: 0, y: Math.floor((viewH - height) / 2), width: Math.round(viewW), height };
}

/**
 * 方向光的位置（DR-F-63）：方位角从 +Z（默认机位所在的一侧、角色的正面）起算，转向 +X 为正；
 * 俯仰角 90° 在正上方，负值在地面以下。`r` 是光源到原点的距离（方向光只看方向，距离只影响阴影相机）。
 */
export function directorLightPosition(azimuthDeg: number, elevationDeg: number, r: number): DirectorVec3Like {
  const az = (azimuthDeg * Math.PI) / 180;
  const el = (elevationDeg * Math.PI) / 180;
  const flat = r * Math.cos(el);
  return { x: flat * Math.sin(az), y: r * Math.sin(el), z: flat * Math.cos(az) };
}

/** 与场景里的 Vec3 同形（这里不 import 场景模块，免得纯函数之间互相牵连）。 */
export type DirectorVec3Like = { x: number; y: number; z: number };

/** 自由走位的基础速度（米 / 秒，DR-F-25）；按住 Shift 时乘 3。 */
export const DIRECTOR_WALK_SPEED = 1.5;
export const DIRECTOR_WALK_BOOST = 3;

/**
 * 自由走位一帧的位移（DR-F-25，DR-AC-06）：W / S 沿视线的水平方向前后，A / D 左右，E / Q 升降，Shift 加速。
 * `keys` 是按住的 `KeyboardEvent.code`；`yawDeg` 是机位的偏航角（0 = 朝 −Z 看，绕 +Y 逆时针为正）。
 * 同时按多个方向键时按合成方向走，速度不变（斜着走不会更快）。没有按键时位移为 0。
 */
export function directorWalkStep(keys: Iterable<string>, yawDeg: number, speed: number, dt: number): DirectorVec3Like {
  const held = new Set(keys);
  const forward = (held.has("KeyW") ? 1 : 0) - (held.has("KeyS") ? 1 : 0);
  const strafe = (held.has("KeyD") ? 1 : 0) - (held.has("KeyA") ? 1 : 0);
  const lift = (held.has("KeyE") ? 1 : 0) - (held.has("KeyQ") ? 1 : 0);
  const yaw = (yawDeg * Math.PI) / 180;
  // 视线方向（−Z 绕 Y 转 yaw）与它右手边的方向。
  let x = forward * -Math.sin(yaw) + strafe * Math.cos(yaw);
  let z = forward * -Math.cos(yaw) + strafe * -Math.sin(yaw);
  let y = lift;
  const len = Math.hypot(x, y, z);
  if (len === 0 || !(dt > 0) || !(speed > 0)) return { x: 0, y: 0, z: 0 };
  const boost = held.has("ShiftLeft") || held.has("ShiftRight") ? DIRECTOR_WALK_BOOST : 1;
  const k = (speed * boost * dt) / len;
  x *= k;
  y *= k;
  z *= k;
  // `0 + v`：把 -0 规整成 0，免得序列化后多出一个「-0」。
  return { x: 0 + x, y: 0 + y, z: 0 + z };
}

/**
 * 「机位对准选中对象」（DR-F-26）：从 `from` 看向 `to` 时机位的朝向，欧拉 XYZ（度），不带横滚（横滚在 `camera.roll`）。
 * 与 three 的 `camera.lookAt(to)`（上方向 +Y）同一结果：机位看向 −Z。正上 / 正下方时偏航取 0。
 * 两点重合时返回 null（没有方向可言，调用方不改朝向）。
 */
export function directorLookAtRotation(from: DirectorVec3Like, to: DirectorVec3Like): DirectorVec3Like | null {
  // 视线的反方向是机位的 +Z 轴
  let zx = from.x - to.x;
  let zy = from.y - to.y;
  let zz = from.z - to.z;
  const zl = Math.hypot(zx, zy, zz);
  if (zl < 1e-9) return null;
  zx /= zl;
  zy /= zl;
  zz /= zl;
  // x 轴 = up × z（up = +Y）；正上 / 正下方时 up 与 z 共线，换一个参考方向（同 three 的处理：把 z 稍微挪一下）
  let xx = zz;
  let xz = -zx;
  let xl = Math.hypot(xx, xz);
  if (xl < 1e-9) {
    xx = 1;
    xz = 0;
    xl = 1;
  }
  xx /= xl;
  xz /= xl;
  // y 轴 = z × x
  const yx = zy * xz;
  const yy = zz * xx - zx * xz;
  const yz = -zy * xx;
  // 旋转矩阵（列为 x、y、z 轴）→ 欧拉 XYZ，与 three 的 `Euler.setFromRotationMatrix(m, "XYZ")` 同一公式
  const m11 = xx;
  const m12 = yx;
  const m13 = zx;
  const m22 = yy;
  const m23 = zy;
  const m32 = yz;
  const m33 = zz;
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
  const deg = 180 / Math.PI;
  const clean = (v: number) => (Math.abs(v) < 1e-9 ? 0 : v);
  return { x: clean(rx * deg), y: clean(ry * deg), z: clean(rz * deg) };
}

// ---------------------------------------------------------------- 镜头文字（DRT-26，DR-F-130~132，dev/03 §12）

/** 焦段说明用的常用档（「35mm」）：比机位面板的快捷档多 28 与 200，只用来写说明文字。 */
export const DIRECTOR_FOCAL_LABELS: readonly number[] = Object.freeze([14, 18, 24, 28, 35, 50, 85, 135, 200]);

/**
 * 景别（DR-F-130）：8 档，阈值取相邻两档 coverage（画面高 ÷ 目标高）的几何平均。
 * ⚠️ 码与中文就是求解器那张表（`DIRECTOR_SHOT_SIZES`，取自 catalog.json）：只改一边的话，
 *    「AI 按特写摆的机位，镜头文字却说近景」，而两边各自看都没错。
 */
export function directorShotSize(coverage: number): DirectorShotSize {
  const sizes = DIRECTOR_SHOT_SIZES;
  for (let i = 0; i < sizes.length - 1; i++) {
    const current = sizes[i] as DirectorShotSize;
    const next = sizes[i + 1] as DirectorShotSize;
    if (coverage >= Math.sqrt(current.coverage * next.coverage)) return current;
  }
  return sizes[sizes.length - 1] as DirectorShotSize;
}

/**
 * 角度（DR-F-130）：`elevationDeg` 是**机位相对注视点**的仰角（机位在注视点下方为负）；横滚超过 ±10° 前面加「斜角」。
 * ⚠️ 不是相机自身的俯仰：相机低头看人时俯仰为负，而机位在人的上方 —— 两种口径的符号正好相反（dev/03 R-14）。
 */
export function directorShotAngle(elevationDeg: number, rollDeg = 0): string {
  const e = elevationDeg;
  let angle: string;
  if (e <= -15) angle = "低角度仰拍";
  else if (e <= -8) angle = "仰拍";
  else if (e < 8) angle = "平视";
  else if (e < 60) angle = "俯拍";
  else angle = "顶视";
  return Math.abs(rollDeg) > 10 ? `斜角${angle}` : angle;
}

/** 焦段（DR-F-130）：就近取常用档，如「35mm」。两档等距时取短的那一档。 */
export function directorFocalLabel(focalMm: number): string {
  let best = DIRECTOR_FOCAL_LABELS[0] as number;
  for (const value of DIRECTOR_FOCAL_LABELS) if (Math.abs(value - focalMm) < Math.abs(best - focalMm)) best = value;
  return `${best}mm`;
}

const RAD = Math.PI / 180;
type V3 = DirectorVec3Like;
const sub = (a: V3, b: V3): V3 => ({ x: a.x - b.x, y: a.y - b.y, z: a.z - b.z });
const dot = (a: V3, b: V3) => a.x * b.x + a.y * b.y + a.z * b.z;
const length = (a: V3) => Math.hypot(a.x, a.y, a.z);

/** 欧拉 XYZ（度）旋转一个向量：与 three 的 `Euler(…, "XYZ")` 同一约定（R = Rx·Ry·Rz）。 */
function rotateXYZ(rotation: V3, v: V3): V3 {
  const [cx, sx] = [Math.cos(rotation.x * RAD), Math.sin(rotation.x * RAD)];
  const [cy, sy] = [Math.cos(rotation.y * RAD), Math.sin(rotation.y * RAD)];
  const [cz, sz] = [Math.cos(rotation.z * RAD), Math.sin(rotation.z * RAD)];
  let x = v.x * cz - v.y * sz;
  let y = v.x * sz + v.y * cz;
  let z = v.z;
  [x, z] = [x * cy + z * sy, -x * sy + z * cy];
  [y, z] = [y * cx - z * sx, y * sx + z * cx];
  return { x, y, z };
}

/** 机位的视线方向（看向 −Z）与右方向。 */
function cameraAxes(rotation: V3): { forward: V3; right: V3; up: V3 } {
  return { forward: rotateXYZ(rotation, { x: 0, y: 0, z: -1 }), right: rotateXYZ(rotation, { x: 1, y: 0, z: 0 }), up: rotateXYZ(rotation, { x: 0, y: 1, z: 0 }) };
}

type CameraParams = { focalLengthMm: number; sensorWidthMm: number; aspect: string; roll: number };

/** 画幅的宽高比；认不出的画幅码按默认画幅（场景校验会拦下坏值，这里只是不让说明文字算崩）。 */
function aspectOf(code: string): number {
  try {
    return directorAspectRatio(code);
  } catch {
    return directorAspectRatio(DIRECTOR_OBJECT_DEFAULTS.camera.aspect);
  }
}

function cameraParamsOf(object: DirectorObject): CameraParams {
  const d = DIRECTOR_OBJECT_DEFAULTS.camera;
  const c = (object.camera ?? {}) as Record<string, unknown>;
  const num = (v: unknown, fallback: number) => (typeof v === "number" && Number.isFinite(v) ? v : fallback);
  return {
    focalLengthMm: num(c.focalLengthMm, d.focalLengthMm),
    sensorWidthMm: num(c.sensorWidthMm, d.sensorWidthMm),
    aspect: typeof c.aspect === "string" ? c.aspect : d.aspect,
    roll: num(c.roll, d.roll)
  };
}

/**
 * 对象的大致包围盒（stage 局部坐标，轴对齐，不随朝向转）：只用来算景别与注视点，精度到「站着的人有多高」就够。
 * 角色按身高（脚底锚点），道具按尺寸（底面锚点），群众按行列铺开的范围。机位不算。
 */
function objectBox(object: DirectorObject): { min: V3; max: V3 } | null {
  const t = object.transform;
  const p = t.position;
  const s = t.scale ?? { x: 1, y: 1, z: 1 };
  if (object.kind === "character") {
    const c = (object.character ?? {}) as { model?: string; height?: number };
    const base = DIRECTOR_MODELS.find(m => m.code === c.model)?.heightM ?? 1.7;
    const h = (typeof c.height === "number" ? c.height : base) * s.x;
    const half = 0.13 * h;
    return { min: { x: p.x - half, y: p.y, z: p.z - half }, max: { x: p.x + half, y: p.y + h, z: p.z + half } };
  }
  if (object.kind === "prop") {
    const prop = (object.prop ?? {}) as { size?: V3; shape?: string };
    const size = prop.size ?? { x: 1, y: 1, z: 1 };
    const sx = (size.x * s.x) / 2;
    const sz = (size.z * s.z) / 2;
    const h = prop.shape === "plane" ? 0.01 : size.y * s.y;
    return { min: { x: p.x - sx, y: p.y, z: p.z - sz }, max: { x: p.x + sx, y: p.y + h, z: p.z + sz } };
  }
  if (object.kind === "crowd") {
    const c = (object.crowd ?? {}) as { rows?: number; cols?: number; spacing?: number; model?: string };
    const spacing = (c.spacing ?? 1) * s.x;
    const w = ((c.cols ?? 1) * spacing) / 2;
    const d = ((c.rows ?? 1) * spacing) / 2;
    const h = (DIRECTOR_MODELS.find(m => m.code === c.model)?.heightM ?? 1.7) * s.x;
    return { min: { x: p.x - w, y: p.y, z: p.z - d }, max: { x: p.x + w, y: p.y + h, z: p.z + d } };
  }
  return null;
}

export type DirectorShotTarget = {
  /** 注视点：画面里目标（可见角色的联合包围盒，没有角色时是可见对象）的中心。 */
  center: DirectorVec3Like;
  /** 目标的高（米），算 coverage 用。 */
  height: number;
  ids: string[];
};

/**
 * 画面里的目标（DR-F-130）：机位视锥里可见角色的联合包围盒；没有角色时取可见对象（道具、群众）。
 * 「在画面里」按包围盒中心落在视锥里判断（留 5% 余量）。一个都没有时为 null。
 */
export function directorShotTarget(scene: DirectorScene | null | undefined, cameraId: string): DirectorShotTarget | null {
  const camera = scene?.objects?.find(o => o.id === cameraId && o.kind === "camera");
  if (!scene || !camera) return null;
  const params = cameraParamsOf(camera);
  const aspect = aspectOf(params.aspect);
  const tanV = Math.tan((directorVerticalFov(params.focalLengthMm, params.sensorWidthMm, aspect) * RAD) / 2) * 1.05;
  const tanH = tanV * aspect;
  const { forward, right, up } = cameraAxes(camera.transform.rotation);
  const origin = camera.transform.position;
  const visible = (scene.objects ?? []).flatMap((object) => {
    if (object.kind === "camera" || object.visible === false) return [];
    const box = objectBox(object);
    if (!box) return [];
    const center = { x: (box.min.x + box.max.x) / 2, y: (box.min.y + box.max.y) / 2, z: (box.min.z + box.max.z) / 2 };
    const rel = sub(center, origin);
    const depth = dot(rel, forward);
    if (depth <= 0.05) return [];
    if (Math.abs(dot(rel, right)) > depth * tanH || Math.abs(dot(rel, up)) > depth * tanV) return [];
    return [{ object, box }];
  });
  const characters = visible.filter(item => item.object.kind === "character");
  const chosen = characters.length ? characters : visible;
  if (!chosen.length) return null;
  const min = { x: Infinity, y: Infinity, z: Infinity };
  const max = { x: -Infinity, y: -Infinity, z: -Infinity };
  for (const { box } of chosen) {
    min.x = Math.min(min.x, box.min.x); min.y = Math.min(min.y, box.min.y); min.z = Math.min(min.z, box.min.z);
    max.x = Math.max(max.x, box.max.x); max.y = Math.max(max.y, box.max.y); max.z = Math.max(max.z, box.max.z);
  }
  return {
    center: { x: (min.x + max.x) / 2, y: (min.y + max.y) / 2, z: (min.z + max.z) / 2 },
    height: Math.max(0.01, max.y - min.y),
    ids: chosen.map(item => item.object.id)
  };
}

/** 运镜的速度阈值：每秒低于它算「缓慢」。距离类（推拉移升降）按米、角度类（摇、环绕）按度。 */
const SLOW_METERS_PER_SECOND = 0.5;
const SLOW_DEGREES_PER_SECOND = 15;

function wrapDegrees(deg: number): number {
  return ((((deg + 180) % 360) + 360) % 360) - 180;
}

type MotionKey = { frame: number; value: { position: V3; rotation: V3 } };

/**
 * 运镜（DR-F-131）：比较首尾两个机位关键帧，判定一种主要动作并按每秒变化量分快慢，如「缓慢推近」。
 * 优先级：环绕（位置绕注视点转动超过 20°）→ 推 / 拉（到注视点的距离变化超过 10%）→ 摇（朝向变化超过 10°、位移小于 0.2 m）
 * → 移（横向位移超过 0.3 m）→ 升 / 降（竖直位移超过 0.3 m）。没有关键帧、首尾一样或都没到阈值时返回空串。
 * `gaze` 是注视点；不给时取首帧视线前方 5 米处。
 */
export function directorCameraMotion(keyframes: readonly MotionKey[], fps = 30, gaze?: V3 | null): string {
  if (keyframes.length < 2) return "";
  const sorted = [...keyframes].sort((a, b) => a.frame - b.frame);
  const first = sorted[0] as MotionKey;
  const last = sorted[sorted.length - 1] as MotionKey;
  const seconds = (last.frame - first.frame) / (fps > 0 ? fps : 30);
  if (!(seconds > 0)) return "";
  const p0 = first.value.position;
  const p1 = last.value.position;
  const r0 = first.value.rotation;
  const r1 = last.value.rotation;
  const axes0 = cameraAxes(r0);
  const axes1 = cameraAxes(r1);
  const g = gaze ?? { x: p0.x + axes0.forward.x * 5, y: p0.y + axes0.forward.y * 5, z: p0.z + axes0.forward.z * 5 };
  const move = sub(p1, p0);
  const travelled = length(move);
  const turn = Math.acos(Math.min(1, Math.max(-1, dot(axes0.forward, axes1.forward)))) / RAD;
  const d0 = length(sub(g, p0));
  const d1 = length(sub(g, p1));
  const orbit = Math.abs(wrapDegrees((Math.atan2(p1.x - g.x, p1.z - g.z) - Math.atan2(p0.x - g.x, p0.z - g.z)) / RAD));
  const label = (name: string, perSecond: number, slowBelow: number) => `${perSecond < slowBelow ? "缓慢" : "快速"}${name}`;
  if (orbit > 20 && travelled > 0.2) return label("环绕", orbit / seconds, SLOW_DEGREES_PER_SECOND);
  if (d0 > 1e-6 && Math.abs(d1 - d0) / d0 > 0.1) return label(d1 < d0 ? "推近" : "拉远", Math.abs(d1 - d0) / seconds, SLOW_METERS_PER_SECOND);
  if (turn > 10 && travelled < 0.2) {
    const yaw = wrapDegrees(r1.y - r0.y);
    const pitch = wrapDegrees(r1.x - r0.x);
    const name = Math.abs(yaw) >= Math.abs(pitch) ? (yaw > 0 ? "左摇" : "右摇") : pitch > 0 ? "上摇" : "下摇";
    return label(name, turn / seconds, SLOW_DEGREES_PER_SECOND);
  }
  const lateral = dot(move, axes0.right);
  if (Math.abs(lateral) > 0.3) return label(lateral > 0 ? "右移" : "左移", Math.abs(lateral) / seconds, SLOW_METERS_PER_SECOND);
  if (Math.abs(move.y) > 0.3) return label(move.y > 0 ? "上升" : "下降", Math.abs(move.y) / seconds, SLOW_METERS_PER_SECOND);
  return "";
}

/**
 * 一句镜头描述（DR-F-130 / 131）：「景别 · 角度 · 焦段（· 运镜）」，如「全景 · 低角度仰拍 · 35mm · 缓慢推近」。
 * 画面里没有可见对象时没有景别，角度按机位自己的俯仰换算（低头看 = 机位在上方）。机位不存在时为空串。
 * 用的是场景里机位的样子：要按播放头那一帧说，调用方传投影过的场景。
 */
export function directorShotText(input: { scene: DirectorScene | null | undefined; cameraId: string; fps?: number }): string {
  const camera = input.scene?.objects?.find(o => o.id === input.cameraId && o.kind === "camera");
  if (!input.scene || !camera) return "";
  const params = cameraParamsOf(camera);
  const target = directorShotTarget(input.scene, input.cameraId);
  const origin = camera.transform.position;
  const parts: string[] = [];
  let elevation: number;
  if (target) {
    const aspect = aspectOf(params.aspect);
    const vfov = directorVerticalFov(params.focalLengthMm, params.sensorWidthMm, aspect);
    const depth = dot(sub(target.center, origin), cameraAxes(camera.transform.rotation).forward);
    parts.push(directorShotSize((2 * depth * Math.tan((vfov * RAD) / 2)) / target.height).name);
    const horizontal = Math.hypot(origin.x - target.center.x, origin.z - target.center.z);
    elevation = Math.atan2(origin.y - target.center.y, horizontal) / RAD;
  } else {
    elevation = -Math.asin(Math.min(1, Math.max(-1, cameraAxes(camera.transform.rotation).forward.y))) / RAD;
  }
  parts.push(directorShotAngle(elevation, params.roll));
  parts.push(directorFocalLabel(params.focalLengthMm));
  const track = input.scene.timeline?.tracks?.find(t => t.objectId === input.cameraId && t.channel === "camera") as { keyframes?: MotionKey[] } | undefined;
  const motion = track?.keyframes ? directorCameraMotion(track.keyframes, input.fps ?? 30, target?.center ?? null) : "";
  if (motion) parts.push(motion);
  return parts.join(" · ");
}

// ---------------------------------------------------------------- 控制图的投影与深度范围（DRT-27，DR-F-133）

/** 投影用的机位参数（场景里机位对象的位置、朝向与镜头参数）。 */
export type DirectorProjectionCamera = {
  position: DirectorVec3Like;
  rotation: DirectorVec3Like;
  focalLengthMm: number;
  sensorWidthMm: number;
  aspect: string;
  roll: number;
  near?: number;
};

export function directorProjectionCameraOf(object: DirectorObject): DirectorProjectionCamera {
  const params = cameraParamsOf(object);
  const near = Number((object.camera as { near?: unknown } | undefined)?.near);
  return {
    position: object.transform.position,
    rotation: object.transform.rotation,
    ...params,
    ...(Number.isFinite(near) && near > 0 ? { near } : {})
  };
}

/** 世界点在机位坐标系里的位置（机位看向 −Z、横滚绕视线转）。 */
function toCameraSpace(point: V3, camera: DirectorProjectionCamera): V3 {
  const { forward, right, up } = cameraAxes(camera.rotation);
  const rel = sub(point, camera.position);
  const x0 = dot(rel, right);
  const y0 = dot(rel, up);
  const z = -dot(rel, forward);
  // 横滚是绕视线把画面转 roll 度（机位的 rollNode）：反着转回来
  const r = -camera.roll * RAD;
  return { x: x0 * Math.cos(r) - y0 * Math.sin(r), y: x0 * Math.sin(r) + y0 * Math.cos(r), z };
}

/**
 * 把世界点投影到截图的像素坐标（左上角为原点，y 向下）。与 three 的 `PerspectiveCamera` 同一个透视：
 * 纵向视角由焦距、传感器长边与画幅算（`directorVerticalFov`），横向按截图的宽高比。
 * 在机位背后、近裁剪面以内或画面外时 `visible` 为假（坐标照算，方便调用方判断偏到哪边）。
 */
export function directorProjectPoint(
  point: DirectorVec3Like,
  camera: DirectorProjectionCamera,
  size: { width: number; height: number }
): { x: number; y: number; depth: number; visible: boolean } {
  const local = toCameraSpace(point, camera);
  const depth = -local.z;
  const tanV = Math.tan((directorVerticalFov(camera.focalLengthMm, camera.sensorWidthMm, aspectOf(camera.aspect)) * RAD) / 2);
  const tanH = tanV * (size.width / size.height);
  const safe = Math.abs(depth) < 1e-9 ? 1e-9 : depth;
  const ndcX = local.x / (safe * tanH);
  const ndcY = local.y / (safe * tanV);
  return {
    x: ((ndcX + 1) / 2) * size.width,
    y: ((1 - ndcY) / 2) * size.height,
    depth,
    visible: depth > (camera.near ?? 0.1) && Math.abs(ndcX) <= 1 && Math.abs(ndcY) <= 1
  };
}

/**
 * 深度图的近远范围（米）：各包围盒 8 个角沿视线的最近 / 最远距离，前后各留 5% 余量。
 * 完全在机位背后的盒子不算；机位在盒子里时近处取 0.05 m。一个都没有时为 null（调用方按整张黑图处理）。
 * 机位给朝向（欧拉角，度）或直接给视线方向（引擎手上是世界坐标的视线，省得来回换算）。
 */
export function directorDepthRange(
  boxes: ReadonlyArray<{ min: DirectorVec3Like; max: DirectorVec3Like }>,
  camera: { position: DirectorVec3Like; rotation: DirectorVec3Like } | { position: DirectorVec3Like; forward: DirectorVec3Like }
): { near: number; far: number } | null {
  const forward = "forward" in camera ? camera.forward : cameraAxes(camera.rotation).forward;
  let near = Infinity;
  let far = -Infinity;
  for (const box of boxes) {
    const depths: number[] = [];
    for (const x of [box.min.x, box.max.x]) {
      for (const y of [box.min.y, box.max.y]) {
        for (const z of [box.min.z, box.max.z]) depths.push(dot(sub({ x, y, z }, camera.position), forward));
      }
    }
    const max = Math.max(...depths);
    if (max <= 0) continue;
    near = Math.min(near, Math.min(...depths));
    far = Math.max(far, max);
  }
  if (!Number.isFinite(far)) return null;
  return { near: Math.max(0.05, near * 0.95), far: far * 1.05 };
}

// ---------------------------------------------------------------- 预演视频（DRT-25，DR-F-125 / 126）

/** 预演视频的上限（DR-PARAM-10）：超过就不上传，提示降低分辨率或缩短时长。 */
export const DIRECTOR_PREVIEW_MAX_BYTES = 50 * 1024 * 1024;
export const DIRECTOR_PREVIEW_FPS = 30;

/** 预演视频的像素尺寸：短边 720，宽高取偶数（H.264 要求偶数）。 */
export function directorPreviewSize(aspect: DirectorAspectCode | string): { width: number; height: number } {
  const ratio = aspectOf(String(aspect));
  const even = (v: number) => Math.max(2, Math.round(v / 2) * 2);
  return ratio >= 1 ? { width: even(720 * ratio), height: 720 } : { width: 720, height: even(720 / ratio) };
}

/**
 * 预演的码率（比特 / 秒）：1280×720 取 6 Mbps，其余按像素数等比。固定码率才能在导出前估出文件大小（对话框要显示）。
 * 白模画面细节少，这个码率在 720p 下看不出块效应。
 */
export function directorPreviewBitrate(width: number, height: number): number {
  return Math.round((6_000_000 * width * height) / (1280 * 720));
}

/** 预估的文件大小（字节）：码率 × 时长，加 5% 的封装开销。 */
export function directorPreviewEstimateBytes(width: number, height: number, frames: number, fps = DIRECTOR_PREVIEW_FPS): number {
  return Math.round(((directorPreviewBitrate(width, height) * (frames / fps)) / 8) * 1.05);
}
