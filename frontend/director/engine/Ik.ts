/**
 * 手脚的两节解析 IK 与骨骼覆盖的换算（DRT-21，DR-F-43，dev/03 §9.3；为什么不用 CCDIKSolver 见 dev/03 R-11）。
 *
 * 一条链三根骨骼：上臂 → 前臂 → 手（腿：大腿 → 小腿 → 脚）。求前两根的旋转，让末端（手腕 / 脚踝）落到目标点：
 * - 解析解（余弦定理，Daniel Holden 的两节 IK）：够得着时末端误差只剩浮点误差，不迭代，伸直附近也不会翻向；
 * - **单向弯曲**：弯曲平面取当前肘 / 膝相对两端连线的那一侧；链几乎伸直、看不出朝哪边弯时，用调用方给的
 *   「弯曲方向」（膝盖朝角色前方、手肘朝后方）—— 肘与膝只会朝一个方向折；
 * - 够不着时伸直指向目标，返回剩下的距离：脚底锁定据此判断要不要解锁（腿伸直了也够不着就不能硬拉）。
 *
 * 只改骨骼的 `quaternion`，不碰场景数据：拖拽结束时由调用方把改过的骨骼换成 `boneOverrides` 写回（DR-F-34）。
 */
import { Euler, Quaternion, Vector3, type Object3D } from "three";
import type { DirectorBoneRotation } from "~/utils/directorRig";

export type DirectorIkEnd = "LeftHand" | "RightHand" | "LeftFoot" | "RightFoot";

/** 四条链：末端骨骼 → [上段, 下段, 末端]。 */
export const DIRECTOR_IK_CHAINS: Readonly<Record<DirectorIkEnd, readonly [string, string, string]>> = Object.freeze({
  LeftHand: ["LeftArm", "LeftForeArm", "LeftHand"],
  RightHand: ["RightArm", "RightForeArm", "RightHand"],
  LeftFoot: ["LeftUpLeg", "LeftLeg", "LeftFoot"],
  RightFoot: ["RightUpLeg", "RightLeg", "RightFoot"]
});

export function directorIsIkEnd(bone: string | null | undefined): bone is DirectorIkEnd {
  return bone === "LeftHand" || bone === "RightHand" || bone === "LeftFoot" || bone === "RightFoot";
}

const EPS = 1e-4;
const _a = new Vector3();
const _b = new Vector3();
const _c = new Vector3();
const _ac = new Vector3();
const _ab = new Vector3();
const _at = new Vector3();
const _ba = new Vector3();
const _bc = new Vector3();
const _axis0 = new Vector3();
const _axis1 = new Vector3();
const _local = new Vector3();
const _qa = new Quaternion();
const _qb = new Quaternion();
const _inv = new Quaternion();
const _r0 = new Quaternion();
const _r1 = new Quaternion();
const _r2 = new Quaternion();

function angleBetween(u: Vector3, v: Vector3): number {
  return Math.acos(Math.min(1, Math.max(-1, u.dot(v))));
}

/**
 * 解一条两节链：转动 `upper` 与 `lower`，让 `end` 的原点落到 `target`（世界坐标）。返回求解后末端到目标的距离（米）。
 *
 * @param bendHint 链几乎伸直时的弯曲方向（世界坐标下的方向，不必归一）：中间关节往这一侧折。
 */
export function directorSolveTwoBone(upper: Object3D, lower: Object3D, end: Object3D, target: Vector3, bendHint: Vector3): number {
  upper.updateWorldMatrix(true, true);
  upper.getWorldPosition(_a);
  lower.getWorldPosition(_b);
  end.getWorldPosition(_c);
  const lab = _a.distanceTo(_b);
  const lcb = _b.distanceTo(_c);
  if (lab < EPS || lcb < EPS) return _c.distanceTo(target);
  const lat = Math.min(Math.max(_a.distanceTo(target), EPS), lab + lcb - EPS);

  _ac.subVectors(_c, _a).normalize();
  _ab.subVectors(_b, _a).normalize();
  _at.subVectors(target, _a);
  if (_at.lengthSq() < EPS * EPS) return _c.distanceTo(target);
  _at.normalize();
  _ba.subVectors(_a, _b).normalize();
  _bc.subVectors(_c, _b).normalize();

  const acAb0 = angleBetween(_ac, _ab);
  const baBc0 = angleBetween(_ba, _bc);
  const acAt0 = angleBetween(_ac, _at);
  const acAb1 = Math.acos(Math.min(1, Math.max(-1, (lcb * lcb - lab * lab - lat * lat) / (-2 * lab * lat))));
  const baBc1 = Math.acos(Math.min(1, Math.max(-1, (lat * lat - lab * lab - lcb * lcb) / (-2 * lab * lcb))));

  // 弯曲平面的法线：当前中间关节在两端连线的哪一侧；伸直时看不出来，就按调用方给的弯曲方向
  _axis0.crossVectors(_ac, _ab);
  if (_axis0.lengthSq() < 1e-8) _axis0.crossVectors(_ac, _local.copy(bendHint).normalize());
  if (_axis0.lengthSq() < 1e-8) return _c.distanceTo(target);
  _axis0.normalize();
  _axis1.crossVectors(_ac, _at);
  const turnToTarget = _axis1.lengthSq() > 1e-12;
  if (turnToTarget) _axis1.normalize();

  upper.getWorldQuaternion(_qa);
  lower.getWorldQuaternion(_qb);
  _r0.setFromAxisAngle(_local.copy(_axis0).applyQuaternion(_inv.copy(_qa).invert()), acAb1 - acAb0);
  _r1.setFromAxisAngle(_local.copy(_axis0).applyQuaternion(_inv.copy(_qb).invert()), baBc1 - baBc0);
  if (turnToTarget) _r2.setFromAxisAngle(_local.copy(_axis1).applyQuaternion(_inv.copy(_qa).invert()), acAt0);
  else _r2.identity();

  // 右乘局部旋转 = 在世界里左乘：q' = q · r2 · r0 ⇒ Q' = R2 · R0 · Q —— 先在弯曲平面里改三角形（r0、r1），
  // 再整条链转向目标（r2）。改三角形不改变「起点 → 末端」的方向，所以 r2 可以按改之前的末端算。
  upper.quaternion.multiply(_r2).multiply(_r0);
  lower.quaternion.multiply(_r1);
  upper.updateWorldMatrix(false, true);
  return end.getWorldPosition(_c).distanceTo(target);
}

/** 把一根骨骼的世界朝向设回 `world`（只改它自己的局部旋转）：脚底锁定时让脚掌保持原来的朝向贴地。 */
export function directorSetWorldQuaternion(bone: Object3D, world: Quaternion): void {
  const parent = bone.parent;
  if (!parent) {
    bone.quaternion.copy(world);
    return;
  }
  parent.updateWorldMatrix(true, false);
  parent.getWorldQuaternion(_qa);
  bone.quaternion.copy(_qa.invert().multiply(world));
  bone.updateWorldMatrix(false, true);
}

const _euler = new Euler();
const _rel = new Quaternion();

/** 覆盖值保留的小数位（度）：0.001° 在 1 m 的骨骼末端约 0.02 mm，远低于脚底锁定 1 mm 的要求。 */
const OVERRIDE_DIGITS = 1000;

function roundDeg(v: number): number {
  const r = Math.round(v * OVERRIDE_DIGITS) / OVERRIDE_DIGITS;
  return Object.is(r, -0) ? 0 : r;
}

/**
 * 骨骼当前的局部旋转换成 `boneOverrides` 的一项：相对 rest 的欧拉角（度，XYZ）—— 与 `poses.json` 同口径（DRD-4）：
 * `bone.quaternion = rest × q(euler)` ⇒ `q(euler) = rest⁻¹ × bone.quaternion`。
 */
export function directorBoneOverrideOf(rest: Quaternion, current: Quaternion): DirectorBoneRotation {
  _rel.copy(rest).invert().multiply(current);
  _euler.setFromQuaternion(_rel, "XYZ");
  const deg = 180 / Math.PI;
  return { x: roundDeg(_euler.x * deg), y: roundDeg(_euler.y * deg), z: roundDeg(_euler.z * deg) };
}
