/**
 * 泼溅背景的纯函数（DRT-28，DR-F-140~144，dev/03 §11）：贴地用的平面拟合、导入前的文件粗筛、场景里 `splat` 段的读写。
 * 不 import three（DRD-33）。
 *
 * 场景里最多一个泼溅：`splat = {mediaId, position, rotation, scale, visible}`，位置与旋转是 stage 局部坐标
 * （跟着场景整体变换走，角色站在上面），旋转单位度、缩放 0.01~100。
 */
import { errorCode } from "./asyncState";
import type { DirectorScene, DirectorVec3 } from "./directorScene";

/** 前端粗筛的大小上限：与服务端默认值一致（DR-PARAM-11，100 MiB）；真正的上限以服务端为准。 */
export const DIRECTOR_SPLAT_MAX_BYTES = 100 * 1024 * 1024;
/** 拟合一块表面至少要这么多个泼溅中心点；少于它就退回地面高度（DR-F-141）。 */
export const DIRECTOR_SPLAT_MIN_POINTS = 20;
/** 拟合残差（均方根，米）超过它就当成「不是一块平面」，退回地面高度。 */
export const DIRECTOR_SPLAT_MAX_RESIDUAL = 0.08;

export type DirectorSplat = { mediaId: string; position: DirectorVec3; rotation: DirectorVec3; scale: number; visible: boolean };
type Point = { x: number; y: number; z: number };

/**
 * 最小二乘拟合 y = a·x + b·z + c，返回 `at` 处的高度、平面法线与残差（均方根）。
 * 拟合一次后剔掉残差超过 2.5 倍均方根（且超过 2 cm）的点再拟合一次：泼溅里总有些飘在空中的点，混进来会把平面抬高。
 * 点少于 `minPoints`、或点几乎共线（定不出平面）时返回 null。
 */
export function directorFitGroundPlane(
  points: readonly Point[],
  at: { x: number; z: number },
  minPoints = DIRECTOR_SPLAT_MIN_POINTS
): { height: number; normal: DirectorVec3; residual: number; count: number } | null {
  const first = fitOnce(points, minPoints);
  if (!first) return null;
  const limit = Math.max(0.02, first.rms * 2.5);
  const kept = points.filter((p) => Math.abs(p.y - first.predict(p.x, p.z)) <= limit);
  const fit = kept.length === points.length ? first : fitOnce(kept, minPoints);
  if (!fit) return null;
  const length = Math.hypot(fit.a, 1, fit.b);
  return {
    height: fit.predict(at.x, at.z),
    normal: { x: -fit.a / length, y: 1 / length, z: -fit.b / length },
    residual: fit.rms,
    count: kept.length
  };
}

function fitOnce(points: readonly Point[], minPoints: number) {
  const n = points.length;
  if (n < minPoints) return null;
  let mx = 0;
  let my = 0;
  let mz = 0;
  for (const p of points) {
    mx += p.x;
    my += p.y;
    mz += p.z;
  }
  mx /= n;
  my /= n;
  mz /= n;
  let sxx = 0;
  let sxz = 0;
  let szz = 0;
  let sxy = 0;
  let szy = 0;
  for (const p of points) {
    const dx = p.x - mx;
    const dz = p.z - mz;
    const dy = p.y - my;
    sxx += dx * dx;
    sxz += dx * dz;
    szz += dz * dz;
    sxy += dx * dy;
    szy += dz * dy;
  }
  const det = sxx * szz - sxz * sxz;
  // 点几乎排成一条线（或全挤在一处）：这时的「平面」可以绕那条线任意转，求出来的坡度没有意义
  if (!(det > 1e-9 * Math.max(1, (sxx + szz) ** 2))) return null;
  const a = (sxy * szz - szy * sxz) / det;
  const b = (szy * sxx - sxy * sxz) / det;
  const predict = (x: number, z: number) => my + a * (x - mx) + b * (z - mz);
  let sum = 0;
  for (const p of points) sum += (p.y - predict(p.x, p.z)) ** 2;
  return { a, b, predict, rms: Math.sqrt(sum / n) };
}

/**
 * 脚下取到的点里先挑出「最密的那一层」（DR-F-141）：按 2 cm 一档统计高度，取相邻三档加起来最多的那一档，
 * 只留它上下 8 cm 内的点再去拟合平面。脚边常有石头、桌腿、墙根，它们的点高高低低地混进来，
 * 直接拟合的话残差大到被当成「太稀」退回地面 —— 而脚下的地面其实很密。两层一样密时取高的那层（与射线取最高表面一致）。
 */
export function directorSplatGroundBand<T extends { y: number }>(points: readonly T[], band = 0.08, bin = 0.02): T[] {
  if (!points.length) return [];
  const counts = new Map<number, number>();
  for (const p of points) {
    const k = Math.floor(p.y / bin);
    counts.set(k, (counts.get(k) ?? 0) + 1);
  }
  let bestKey = 0;
  let best = -1;
  for (const [k, n] of counts) {
    const c = (counts.get(k - 1) ?? 0) + n + (counts.get(k + 1) ?? 0);
    if (c > best || (c === best && k > bestKey)) {
      best = c;
      bestKey = k;
    }
  }
  const center = (bestKey + 0.5) * bin;
  return points.filter((p) => Math.abs(p.y - center) <= band);
}

/** 导入前按扩展名与大小粗筛（DR-F-143）；内容是不是真的泼溅由服务端按文件头判断（41502）。 */
export function directorSplatFileCheck(file: { name: string; size: number }): { ok: true } | { ok: false; reason: "extension" | "size"; text: string } {
  const ext = /\.([a-z0-9]+)$/i.exec(file.name)?.[1]?.toLowerCase();
  if (ext !== "spz" && ext !== "ply") return { ok: false, reason: "extension", text: "只支持 .spz 或 .ply 格式的泼溅文件" };
  if (file.size > DIRECTOR_SPLAT_MAX_BYTES) {
    return { ok: false, reason: "size", text: `文件有 ${(file.size / 1024 / 1024).toFixed(1)} MB，超过 100 MB：建议先转成 SPZ（体积通常只有 PLY 的十分之一）` };
  }
  return { ok: true };
}

/** 上传失败的说明（DR-F-143）：按错误码给出用户能照着改的话。 */
export function directorSplatUploadErrorText(cause: unknown): string {
  switch (errorCode(cause)) {
    case 41301:
      return "文件超过服务端的大小上限：建议先转成 SPZ 或删减点数";
    case 41501:
      return "不支持的文件类型：只收 .spz 或 .ply 格式的泼溅文件";
    case 41502:
      return "文件内容与扩展名不符：请确认它真的是 .spz / .ply 泼溅文件（不是改了扩展名的图片或压缩包）";
    default: {
      const message = (cause as { message?: unknown } | null)?.message;
      return typeof message === "string" && message ? `导入失败：${message}` : "导入失败，请重试";
    }
  }
}

const vec = (value: unknown): DirectorVec3 => {
  const v = (value && typeof value === "object" ? value : {}) as Partial<DirectorVec3>;
  const n = (x: unknown) => (typeof x === "number" && Number.isFinite(x) ? x : 0);
  return { x: n(v.x), y: n(v.y), z: n(v.z) };
};

/** 场景里的泼溅（缺省字段按默认补齐）；没有或没有 mediaId 时为 null。 */
export function directorSplatOf(scene: DirectorScene | null | undefined): DirectorSplat | null {
  const raw = scene?.splat as Record<string, unknown> | undefined;
  if (!raw || typeof raw !== "object" || typeof raw.mediaId !== "string" || !raw.mediaId) return null;
  const scale = typeof raw.scale === "number" && Number.isFinite(raw.scale) ? Math.min(100, Math.max(0.01, raw.scale)) : 1;
  return { mediaId: raw.mediaId, position: vec(raw.position), rotation: vec(raw.rotation), scale, visible: raw.visible !== false };
}

/** 导入 / 替换泼溅：新文件从原点、不旋转、原大开始；替换时保留原来的变换与显示状态（用户多半刚对好位置）。 */
export function directorSetSplat(scene: DirectorScene, mediaId: string): DirectorScene {
  const current = directorSplatOf(scene);
  const next: DirectorSplat = current
    ? { ...current, mediaId }
    : { mediaId, position: { x: 0, y: 0, z: 0 }, rotation: { x: 0, y: 0, z: 0 }, scale: 1, visible: true };
  return { ...scene, splat: next };
}

/** 改泼溅的变换或显示（未知字段保留）；没有泼溅、或值都没变时返回同一个场景。 */
export function directorPatchSplat(scene: DirectorScene, patch: Partial<Omit<DirectorSplat, "mediaId">>): DirectorScene {
  const raw = scene.splat as Record<string, unknown> | undefined;
  if (!directorSplatOf(scene) || !raw) return scene;
  const next = { ...raw, ...patch };
  return JSON.stringify(next) === JSON.stringify(raw) ? scene : { ...scene, splat: next };
}

/** 移除泼溅（字段整个去掉，不留空对象：场景校验要求有 splat 就必须有 mediaId）。 */
export function directorRemoveSplat(scene: DirectorScene): DirectorScene {
  if (scene.splat === undefined) return scene;
  const { splat: _removed, ...rest } = scene;
  return rest as DirectorScene;
}
