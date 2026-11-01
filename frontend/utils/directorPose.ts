/**
 * 姿势相关的纯函数（DRT-21 新建，DRT-23 参考图转姿势、DRT-27 骨架图会继续往里加）。
 *
 * 左右镜像已在 `utils/directorRig.ts`（`directorMirrorBones`，DRT-11，四个模型核实过镜像轴）：
 * ⚠️ 不在这里再写一份 —— Nuxt 会自动导入 `utils/` 的全部导出，重名时悄悄顶替其中一个。
 */
import { DIRECTOR_LIMITS, DIRECTOR_OBJECT_DEFAULTS, type DirectorModelCode } from "./directorCatalog";
import { DIRECTOR_REST_BONES, DIRECTOR_REST_POSE, type DirectorRestBone } from "./directorRestPose";
import { DIRECTOR_BONE_PARENT, DIRECTOR_STANDARD_BONES, directorMirrorBones, type DirectorBoneRotation } from "./directorRig";
import { directorFindObject, directorPatchObject, directorSetTransform, type DirectorScene, type DirectorVec3 } from "./directorScene";

export type DirectorPoseBonesCheck =
  | {
    ok: true;
    /** 过滤之后的骨骼覆盖（只剩认识的骨骼与有限的角度）。 */
    bones: Record<string, DirectorBoneRotation>;
    /** 被过滤掉的骨骼名（不认识的、角度不是有限数的），界面据此告警。 */
    dropped: string[];
  }
  | { ok: false; error: "limit"; limit: number; count: number };

const KNOWN_BONES = new Set(DIRECTOR_STANDARD_BONES);

/**
 * 骨骼覆盖能不能写进场景（DR-PARAM-09：至多 70 项）：
 * - 超过上限的整体拒绝，按**原始项数**算（与场景校验同一口径）—— 截掉一部分的话姿势只剩半身，
 *   用户看不出少了哪几根；标准骨骼只有 67 根，超过 70 项的输入本身就不对；
 * - 不认识的骨骼名、角度不是有限数的过滤掉并报出来（参考图转姿势、手写数据都可能带进来）。
 * 不改入参。
 */
export function directorPoseBonesValid(bones: Readonly<Record<string, DirectorBoneRotation>>): DirectorPoseBonesCheck {
  const total = Object.keys(bones).length;
  if (total > DIRECTOR_LIMITS.maxBoneOverrides) {
    return { ok: false, error: "limit", limit: DIRECTOR_LIMITS.maxBoneOverrides, count: total };
  }
  const kept: Record<string, DirectorBoneRotation> = {};
  const dropped: string[] = [];
  for (const [name, rotation] of Object.entries(bones)) {
    const finite = [rotation?.x, rotation?.y, rotation?.z].every(value => typeof value === "number" && Number.isFinite(value));
    if (!KNOWN_BONES.has(name) || !finite) {
      dropped.push(name);
      continue;
    }
    kept[name] = { x: rotation.x, y: rotation.y, z: rotation.z };
  }
  return { ok: true, bones: kept, dropped };
}

/** 过滤掉骨骼时的告警：说出几根、是哪几根（最多列 5 个）。没有过滤时为空串。 */
export function directorPoseDroppedText(dropped: readonly string[]): string {
  if (!dropped.length) return "";
  const shown = dropped.slice(0, 5).join("、");
  return `有 ${dropped.length} 根骨骼无法识别，已忽略：${shown}${dropped.length > 5 ? " 等" : ""}`;
}

// ---------------------------------------------------------------- 关节模式（DRT-21，DR-F-43）

/**
 * 关节模式里给控制点的骨骼（顺序即界面顺序）。手指、眼睛、末端骨不给：太小、点不准，调它们的需求也少。
 * `label` 给界面与无障碍用。
 */
export const DIRECTOR_JOINT_BONES: readonly { readonly bone: string; readonly label: string }[] = Object.freeze([
  { bone: "Hips", label: "骨盆" }, { bone: "Spine", label: "腰" }, { bone: "Spine1", label: "胸腔" }, { bone: "Spine2", label: "胸口" },
  { bone: "Neck", label: "脖子" }, { bone: "Head", label: "头" },
  { bone: "LeftShoulder", label: "左肩" }, { bone: "LeftArm", label: "左上臂" }, { bone: "LeftForeArm", label: "左前臂" }, { bone: "LeftHand", label: "左手" },
  { bone: "RightShoulder", label: "右肩" }, { bone: "RightArm", label: "右上臂" }, { bone: "RightForeArm", label: "右前臂" }, { bone: "RightHand", label: "右手" },
  { bone: "LeftUpLeg", label: "左大腿" }, { bone: "LeftLeg", label: "左小腿" }, { bone: "LeftFoot", label: "左脚" }, { bone: "LeftToeBase", label: "左脚尖" },
  { bone: "RightUpLeg", label: "右大腿" }, { bone: "RightLeg", label: "右小腿" }, { bone: "RightFoot", label: "右脚" }, { bone: "RightToeBase", label: "右脚尖" }
].map(entry => Object.freeze(entry)));

const JOINT_LABELS = new Map(DIRECTOR_JOINT_BONES.map(entry => [entry.bone, entry.label]));

export function directorJointLabel(bone: string | null | undefined): string {
  return (bone && JOINT_LABELS.get(bone)) || bone || "";
}

/**
 * 这个关节能不能「移动」：手脚末端拖动 = IK，骨盆拖动 = 移动身体（锁定的脚留在原地）；其余关节只能转。
 */
export function directorJointTranslatable(bone: string | null | undefined): boolean {
  return bone === "Hips" || bone === "LeftHand" || bone === "RightHand" || bone === "LeftFoot" || bone === "RightFoot";
}

// ---------------------------------------------------------------- 姿势的场景改动（DRT-21，DR-F-42 / 45）

type CharacterSub = { posePreset?: unknown; boneOverrides?: unknown; footLock?: unknown };

function characterOf(scene: DirectorScene, id: string): CharacterSub | null {
  const object = directorFindObject(scene, id);
  if (!object || object.kind !== "character") return null;
  return (object.character && typeof object.character === "object" ? object.character : {}) as CharacterSub;
}

function overridesOf(character: CharacterSub): Record<string, DirectorBoneRotation> {
  const raw = character.boneOverrides;
  return raw && typeof raw === "object" && !Array.isArray(raw) ? { ...(raw as Record<string, DirectorBoneRotation>) } : {};
}

/** 套用姿势库里的一个姿势：写 `posePreset`、清空骨骼覆盖（覆盖是在上一个姿势上调出来的，叠上去会成四不像）。 */
export function directorApplyPosePreset(scene: DirectorScene, id: string, code: string): DirectorScene {
  if (!code || !characterOf(scene, id)) return scene;
  return directorPatchObject(scene, id, "character", { posePreset: code, boneOverrides: {} });
}

/** 重置姿势（DR-F-45）：回到自然站立、清空覆盖。脚底锁定是开关、不是姿势，保留。 */
export function directorResetPose(scene: DirectorScene, id: string): DirectorScene {
  if (!characterOf(scene, id)) return scene;
  return directorPatchObject(scene, id, "character", { posePreset: DIRECTOR_OBJECT_DEFAULTS.character.posePreset, boneOverrides: {} });
}

/**
 * 左右镜像整套姿势（DR-F-45）：把「预设 + 覆盖」合成后的完整姿势镜像，整组写成覆盖。
 * ⚠️ 只在预设里出现、镜像后没有对应项的骨骼要写成 0（回到 rest）：覆盖只能「盖住」预设，盖不住的话
 *    预设里那根左臂还在，镜像出来的是两只手都抬着。预设码不变：界面上仍显示是在哪个姿势上调的。
 * 镜像两次合成出来的姿势与原来相同。`presetBones` 是当前预设的骨骼（取不到就给空对象）。
 */
export function directorMirrorPose(
  presetBones: Readonly<Record<string, DirectorBoneRotation>>,
  overrides: Readonly<Record<string, DirectorBoneRotation>>
): Record<string, DirectorBoneRotation> {
  const mirrored = directorMirrorBones({ ...presetBones, ...overrides });
  for (const name of Object.keys(presetBones)) {
    if (!(name in mirrored)) mirrored[name] = { x: 0, y: 0, z: 0 };
  }
  return mirrored;
}

/** 镜像一个角色的姿势（见 `directorMirrorPose`）。 */
export function directorMirrorCharacterPose(scene: DirectorScene, id: string, presetBones: Readonly<Record<string, DirectorBoneRotation>>): DirectorScene {
  const character = characterOf(scene, id);
  if (!character) return scene;
  return directorPatchObject(scene, id, "character", { boneOverrides: directorMirrorPose(presetBones, overridesOf(character)) });
}

/** 关节模式下的 Delete：只把这一根骨骼的覆盖去掉（回到预设值），不删角色。没有覆盖时返回同一个场景。 */
export function directorResetJoint(scene: DirectorScene, id: string, bone: string): DirectorScene {
  const character = characterOf(scene, id);
  if (!character) return scene;
  const overrides = overridesOf(character);
  if (!(bone in overrides)) return scene;
  const rest = Object.fromEntries(Object.entries(overrides).filter(([name]) => name !== bone));
  return directorPatchObject(scene, id, "character", { boneOverrides: rest });
}

/**
 * 视口里的一次姿势编辑（拖关节、拖手脚、移骨盆）结束：骨骼覆盖、角色变换、脚底锁定一次写进去（一步撤销，DR-F-34）。
 * 超过 70 项的覆盖不写（返回同一个场景）—— 标准骨骼只有 67 根，超了说明数据不对。
 */
export function directorSetPose(
  scene: DirectorScene,
  id: string,
  edit: { boneOverrides: Record<string, DirectorBoneRotation>; transform?: { position: DirectorVec3; rotation: DirectorVec3; scale: DirectorVec3 }; footLock?: { left: boolean; right: boolean } }
): DirectorScene {
  if (!characterOf(scene, id)) return scene;
  const checked = directorPoseBonesValid(edit.boneOverrides);
  if (!checked.ok) return scene;
  let next = edit.transform ? directorSetTransform(scene, id, edit.transform) : scene;
  next = directorPatchObject(next, id, "character", { boneOverrides: checked.bones, ...(edit.footLock ? { footLock: edit.footLock } : {}) });
  return next;
}

/**
 * 参考图识别出的姿势写进角色（DRT-23，DR-F-114）：`posePreset = ""`、`boneOverrides = 识别结果`。
 * 存为这个角色自己的姿势，不进姿势库；预设码清空，因为识别结果是相对 rest 算的、不是在某个预设上调的
 * （留着预设码的话，没算到的骨骼会按预设摆，与算出来的那几根拼不到一起）。覆盖不合法或为空时原样返回。
 */
export function directorApplyDetectedPose(scene: DirectorScene, id: string, bones: Readonly<Record<string, DirectorBoneRotation>>): DirectorScene {
  if (!characterOf(scene, id)) return scene;
  const checked = directorPoseBonesValid(bones);
  if (!checked.ok || !Object.keys(checked.bones).length) return scene;
  return directorPatchObject(scene, id, "character", { posePreset: "", boneOverrides: checked.bones });
}

/** 脚底锁定开关（DR-F-43）。 */
export function directorSetFootLock(scene: DirectorScene, id: string, side: "left" | "right", on: boolean): DirectorScene {
  const character = characterOf(scene, id);
  if (!character) return scene;
  const current = (character.footLock && typeof character.footLock === "object" ? character.footLock : {}) as { left?: boolean; right?: boolean };
  return directorPatchObject(scene, id, "character", { footLock: { left: current.left === true, right: current.right === true, [side]: on } });
}

// ---------------------------------------------------------------- OpenPose 骨架图（DRT-27，DR-F-133）

/**
 * OpenPose COCO 18 点（顺序固定）与素模骨骼的对应。`bone` 为空的点由眼睛推算（见 `directorOpenPoseFromJoints`）。
 * 左右是**人物自己的**左右（与 Mixamo 的 Left / Right 一致）。
 */
export const DIRECTOR_OPENPOSE_POINTS: readonly { readonly name: string; readonly bone: string | null }[] = Object.freeze([
  { name: "鼻", bone: null }, { name: "颈", bone: "Neck" },
  { name: "右肩", bone: "RightArm" }, { name: "右肘", bone: "RightForeArm" }, { name: "右腕", bone: "RightHand" },
  { name: "左肩", bone: "LeftArm" }, { name: "左肘", bone: "LeftForeArm" }, { name: "左腕", bone: "LeftHand" },
  { name: "右髋", bone: "RightUpLeg" }, { name: "右膝", bone: "RightLeg" }, { name: "右踝", bone: "RightFoot" },
  { name: "左髋", bone: "LeftUpLeg" }, { name: "左膝", bone: "LeftLeg" }, { name: "左踝", bone: "LeftFoot" },
  { name: "右眼", bone: "RightEye" }, { name: "左眼", bone: "LeftEye" },
  { name: "右耳", bone: null }, { name: "左耳", bone: null }
].map(point => Object.freeze(point)));

/** OpenPose 的 17 条连线（点的下标，0 起），顺序与配色一一对应。 */
export const DIRECTOR_OPENPOSE_LIMBS: readonly (readonly [number, number])[] = Object.freeze([
  [1, 2], [1, 5], [2, 3], [3, 4], [5, 6], [6, 7], [1, 8], [8, 9], [9, 10], [1, 11], [11, 12], [12, 13], [1, 0], [0, 14], [14, 16], [0, 15], [15, 17]
].map(limb => Object.freeze(limb as [number, number])));

/** OpenPose 标准配色（18 种）：第 i 个点用第 i 种，第 i 条连线也用第 i 种。 */
export const DIRECTOR_OPENPOSE_COLORS: readonly string[] = Object.freeze([
  "#ff0000", "#ff5500", "#ffaa00", "#ffff00", "#aaff00", "#55ff00", "#00ff00", "#00ff55", "#00ffaa",
  "#00ffff", "#00aaff", "#0055ff", "#0000ff", "#5500ff", "#aa00ff", "#ff00ff", "#ff00aa", "#ff0055"
]);

type P3 = { x: number; y: number; z: number };
const add3 = (a: P3, b: P3): P3 => ({ x: a.x + b.x, y: a.y + b.y, z: a.z + b.z });
const sub3 = (a: P3, b: P3): P3 => ({ x: a.x - b.x, y: a.y - b.y, z: a.z - b.z });
const scale3 = (a: P3, k: number): P3 => ({ x: a.x * k, y: a.y * k, z: a.z * k });
const len3 = (a: P3) => Math.hypot(a.x, a.y, a.z);
const norm3 = (a: P3): P3 => {
  const l = len3(a);
  return l > 1e-9 ? scale3(a, 1 / l) : { x: 0, y: 0, z: 0 };
};
const cross3 = (a: P3, b: P3): P3 => ({ x: a.y * b.z - a.z * b.y, y: a.z * b.x - a.x * b.z, z: a.x * b.y - a.y * b.x });

/**
 * 由骨骼的世界坐标得到 OpenPose 18 点（顺序同 `DIRECTOR_OPENPOSE_POINTS`）；取不到的点为 null（不画）。
 * - 有骨骼的点直接取骨骼原点（肩 = 上臂根部、腕 = 手的根部……）；眼睛取素模自带的眼睛骨骼；
 * - 鼻与两耳是近似点：以两眼连线为左右、头顶方向（颈 → 头）为上，鼻在两眼中点前下方，耳在两侧偏后。
 *   缺眼睛骨骼时这三个点不画（不凭空猜一个位置）。
 */
export function directorOpenPoseFromJoints(joints: Readonly<Record<string, P3 | undefined>>): Array<P3 | null> {
  const points: Array<P3 | null> = DIRECTOR_OPENPOSE_POINTS.map(point => (point.bone && joints[point.bone] ? { ...(joints[point.bone] as P3) } : null));
  const leftEye = joints.LeftEye;
  const rightEye = joints.RightEye;
  const neck = joints.Neck;
  const head = joints.Head;
  if (leftEye && rightEye && neck && head) {
    const across = sub3(rightEye, leftEye);
    const eyeDistance = len3(across);
    const right = norm3(across);
    const up = norm3(sub3(head, neck));
    const forward = norm3(cross3(up, right));
    const mid = scale3(add3(leftEye, rightEye), 0.5);
    points[0] = add3(add3(mid, scale3(forward, 0.35 * eyeDistance)), scale3(up, -0.5 * eyeDistance));
    points[16] = add3(add3(mid, scale3(right, 1.15 * eyeDistance)), scale3(forward, -0.9 * eyeDistance));
    points[17] = add3(add3(mid, scale3(right, -1.15 * eyeDistance)), scale3(forward, -0.9 * eyeDistance));
  }
  return points;
}

// ---------------------------------------------------------------- 参考图转姿势（DRT-23，DR-F-110~114）

/** MediaPipe Pose Landmarker 的一个关键点：`worldLandmarks` 是以两髋中点为原点的米制坐标（x 向画面右、y 向下、z 远离镜头）。 */
export type DirectorLandmark = { x: number; y: number; z: number; visibility?: number };
/** Worker 回传的一个人：图像坐标（0~1）与世界坐标各 33 点。 */
export type DirectorDetectedPose = { landmarks: DirectorLandmark[]; worldLandmarks: DirectorLandmark[]; score?: number };

type Q4 = { x: number; y: number; z: number; w: number };
const Q_IDENTITY: Q4 = { x: 0, y: 0, z: 0, w: 1 };
const dot3 = (a: P3, b: P3) => a.x * b.x + a.y * b.y + a.z * b.z;
const mid3 = (a: P3, b: P3): P3 => scale3(add3(a, b), 0.5);
/** 同 three 的 `multiplyQuaternions(a, b)`：先转 b 再转 a。 */
const qMul = (a: Q4, b: Q4): Q4 => ({
  x: a.x * b.w + a.w * b.x + a.y * b.z - a.z * b.y,
  y: a.y * b.w + a.w * b.y + a.z * b.x - a.x * b.z,
  z: a.z * b.w + a.w * b.z + a.x * b.y - a.y * b.x,
  w: a.w * b.w - a.x * b.x - a.y * b.y - a.z * b.z
});
const qConj = (q: Q4): Q4 => ({ x: -q.x, y: -q.y, z: -q.z, w: q.w });
const qNorm = (q: Q4): Q4 => {
  const l = Math.hypot(q.x, q.y, q.z, q.w) || 1;
  return { x: q.x / l, y: q.y / l, z: q.z / l, w: q.w / l };
};
const qRotate = (q: Q4, v: P3): P3 => {
  const u = { x: q.x, y: q.y, z: q.z };
  const t = scale3(cross3(u, v), 2);
  return add3(add3(v, scale3(t, q.w)), cross3(u, t));
};
/** 同 three 的 `setFromUnitVectors`（两个都是单位向量）。 */
function qFromUnitVectors(a: P3, b: P3): Q4 {
  const r = dot3(a, b) + 1;
  if (r < 1e-9) {
    return Math.abs(a.x) > Math.abs(a.z) ? qNorm({ x: -a.y, y: a.x, z: 0, w: 0 }) : qNorm({ x: 0, y: -a.z, z: a.y, w: 0 });
  }
  return qNorm({ x: a.y * b.z - a.z * b.y, y: a.z * b.x - a.x * b.z, z: a.x * b.y - a.y * b.x, w: r });
}
/** 三根轴（局部 X / Y / Z 在世界里的方向，彼此正交的单位向量）→ 四元数，同 three 的 `setFromRotationMatrix`。 */
function qFromBasis(X: P3, Y: P3, Z: P3): Q4 {
  const [m11, m12, m13, m21, m22, m23, m31, m32, m33] = [X.x, Y.x, Z.x, X.y, Y.y, Z.y, X.z, Y.z, Z.z];
  const trace = m11 + m22 + m33;
  if (trace > 0) {
    const s = 0.5 / Math.sqrt(trace + 1);
    return { w: 0.25 / s, x: (m32 - m23) * s, y: (m13 - m31) * s, z: (m21 - m12) * s };
  }
  if (m11 > m22 && m11 > m33) {
    const s = 2 * Math.sqrt(1 + m11 - m22 - m33);
    return { w: (m32 - m23) / s, x: 0.25 * s, y: (m12 + m21) / s, z: (m13 + m31) / s };
  }
  if (m22 > m33) {
    const s = 2 * Math.sqrt(1 + m22 - m11 - m33);
    return { w: (m13 - m31) / s, x: (m12 + m21) / s, y: 0.25 * s, z: (m23 + m32) / s };
  }
  const s = 2 * Math.sqrt(1 + m33 - m11 - m22);
  return { w: (m21 - m12) / s, x: (m13 + m31) / s, y: (m23 + m32) / s, z: 0.25 * s };
}
const DEG = Math.PI / 180;
/** 同 three 的 `Quaternion.setFromEuler(Euler(x, y, z, "XYZ"))`，角度单位度。 */
function qFromEulerXYZ(r: DirectorBoneRotation): Q4 {
  const [c1, c2, c3] = [Math.cos((r.x * DEG) / 2), Math.cos((r.y * DEG) / 2), Math.cos((r.z * DEG) / 2)];
  const [s1, s2, s3] = [Math.sin((r.x * DEG) / 2), Math.sin((r.y * DEG) / 2), Math.sin((r.z * DEG) / 2)];
  return { x: s1 * c2 * c3 + c1 * s2 * s3, y: c1 * s2 * c3 - s1 * c2 * s3, z: c1 * c2 * s3 + s1 * s2 * c3, w: c1 * c2 * c3 - s1 * s2 * s3 };
}
/** 同 three 的 `Euler.setFromQuaternion(q, "XYZ")`，角度单位度。 */
function eulerXYZOf(q: Q4): DirectorBoneRotation {
  const { x, y, z, w } = qNorm(q);
  const m11 = 1 - 2 * (y * y + z * z);
  const m12 = 2 * (x * y - w * z);
  const m13 = 2 * (x * z + w * y);
  const m22 = 1 - 2 * (x * x + z * z);
  const m23 = 2 * (y * z - w * x);
  const m32 = 2 * (y * z + w * x);
  const m33 = 1 - 2 * (x * x + y * y);
  const ry = Math.asin(Math.min(1, Math.max(-1, m13)));
  const [rx, rz] = Math.abs(m13) < 0.9999999 ? [Math.atan2(-m23, m33), Math.atan2(-m12, m11)] : [Math.atan2(m32, m22), 0];
  return { x: rx / DEG, y: ry / DEG, z: rz / DEG };
}
/** 左右 + 上 → 身体坐标系（X 向左、Y 向上、Z 向前，与素模躯干骨骼的局部轴同口径）；方向退化时返回 null。 */
function bodyFrame(left: P3, up: P3): Q4 | null {
  const x = norm3(left);
  const z = norm3(cross3(x, up));
  if (len3(x) < 0.5 || len3(z) < 0.5) return null;
  return qFromBasis(x, cross3(z, x), z);
}

const restPoseOf = (model: DirectorModelCode) => DIRECTOR_REST_POSE[model] ?? DIRECTOR_REST_POSE.adult_male;
const restQ = (bone: DirectorRestBone): Q4 => ({ x: bone.q[0], y: bone.q[1], z: bone.q[2], w: bone.q[3] });
const restP = (bone: DirectorRestBone): P3 => ({ x: bone.p[0], y: bone.p[1], z: bone.p[2] });

/**
 * 素模关节的模型坐标（米；Y 向上、面朝 +Z）与世界旋转，按「rest × 覆盖」逐级算（同引擎的套用方式，见 `directorRig.ts`）。
 * 只算 rest 表里那几根骨骼（躯干、头、四肢）；参考图转姿势的测试用它核对结果，界面不用。
 */
export function directorPoseJoints(
  bones: Readonly<Record<string, DirectorBoneRotation>>,
  model: DirectorModelCode = "adult_male"
): Record<string, { position: P3; quaternion: Q4 }> {
  const rest = restPoseOf(model);
  const world: Record<string, { q: Q4; p: P3 }> = {};
  for (const name of DIRECTOR_REST_BONES) {
    const parentName = DIRECTOR_BONE_PARENT[name];
    const parent = parentName ? world[parentName] : { q: Q_IDENTITY, p: { x: 0, y: 0, z: 0 } };
    if (!parent) continue;
    const bone = rest[name] as DirectorRestBone;
    const local = bones[name] ? qMul(restQ(bone), qFromEulerXYZ(bones[name] as DirectorBoneRotation)) : restQ(bone);
    world[name] = { q: qMul(parent.q, local), p: add3(parent.p, qRotate(parent.q, restP(bone))) };
  }
  return Object.fromEntries(Object.entries(world).map(([name, value]) => [name, { position: value.p, quaternion: value.q }]));
}

/** 33 点的左右对调表（BlazePose：0 鼻子不对调）。 */
const MP_MIRROR = [0, 4, 5, 6, 1, 2, 3, 8, 7, 10, 9, 12, 11, 14, 13, 16, 15, 18, 17, 20, 19, 22, 21, 24, 23, 26, 25, 28, 27, 30, 29, 32, 31];
const MP = {
  leftEye: 2, rightEye: 5, leftEar: 7, rightEar: 8, mouthLeft: 9, mouthRight: 10,
  leftShoulder: 11, rightShoulder: 12, leftElbow: 13, rightElbow: 14, leftWrist: 15, rightWrist: 16,
  leftPinky: 17, rightPinky: 18, leftIndex: 19, rightIndex: 20,
  leftHip: 23, rightHip: 24, leftKnee: 25, rightKnee: 26, leftAnkle: 27, rightAnkle: 28, leftFootIndex: 31, rightFootIndex: 32
} as const;
/** 关键点可见度低于这个值就不用它求骨骼（画面外、被挡住的点 MediaPipe 照样给，但只是猜的）。 */
const MP_VISIBLE = 0.5;
/** 肢体弯曲小于约 15° 时弯曲平面不可靠，不调转向，只做最小转动。 */
const MIN_BEND_SIN = Math.sin(15 * DEG);

/**
 * 四肢的「铰链」：上一节对准方向后，绕自身再转一下，让下一节弯曲所绕的局部轴落在实测的弯曲平面法线上，
 * 免得肘、膝往反方向折（只对准方向的话，下一节照样指得对，但扭转可能是反关节的）。
 * - 肘：前臂的局部 Z，左臂 +Z 是屈肘、右臂 −Z（素模 rest 的 A 字姿势本身就带约 15° 屈肘，四个模型都核实过）；
 * - 膝：小腿的局部 X，−X 是屈膝（`directorRig.ts`，左右同号）。
 */
const LIMB_HINGES = {
  LeftArm: { lower: "LeftForeArm", axis: { x: 0, y: 0, z: 1 }, sign: 1 },
  RightArm: { lower: "RightForeArm", axis: { x: 0, y: 0, z: 1 }, sign: -1 },
  LeftUpLeg: { lower: "LeftLeg", axis: { x: 1, y: 0, z: 0 }, sign: -1 },
  RightUpLeg: { lower: "RightLeg", axis: { x: 1, y: 0, z: 0 }, sign: -1 }
} as const satisfies Record<string, { lower: string; axis: P3; sign: 1 | -1 }>;

const BODY_PART_NAMES: Record<string, string> = {
  torso: "躯干", head: "头部", LeftArm: "左上臂", LeftForeArm: "左前臂", LeftHand: "左手", RightArm: "右上臂", RightForeArm: "右前臂", RightHand: "右手",
  LeftUpLeg: "左大腿", LeftLeg: "左小腿", LeftFoot: "左脚", RightUpLeg: "右大腿", RightLeg: "右小腿", RightFoot: "右脚"
};

/**
 * 把 MediaPipe 的 33 个世界坐标关键点换算成素模的骨骼覆盖（DR-F-111）：相对 rest 的局部欧拉角（度，XYZ），
 * 与 `character.boneOverrides` 同口径。只算能可靠求出的 15 根主骨骼，其余不写（保持 rest）：
 * - 骨盆（Hips）：两髋连线为左右、髋 → 肩为上；胸（Spine2）：两肩连线为左右、同一个上；脊柱的弯曲都算在骨盆上；
 * - 头（Head）：两耳连线为左右、嘴 → 眼为上；
 * - 上臂 / 前臂 / 手、大腿 / 小腿 / 脚：逐节对准「这一节到下一节」的方向（父骨骼先定），上一节按铰链转好扭转。
 *
 * 坐标换算：MediaPipe 的 x 向画面右（=人的左侧，对着镜头时）、y 向下、z 远离镜头 → 素模的 (x, −y, −z)。
 * `mirror`：先把关键点左右对调再换算（自拍常是镜像，DR-F-113）—— 镜像两次与不镜像的结果相同。
 * 关键点可见度低于 0.5 的那一节不算（保持 rest）并在 `warnings` 里说出来；单张图有深度歧义，结果只当初值。
 * 不 import three（DRD-33）。
 */
export function directorPoseFromLandmarks(
  world: readonly DirectorLandmark[] | null | undefined,
  options: { mirror?: boolean; model?: DirectorModelCode } = {}
): { bones: Record<string, DirectorBoneRotation>; warnings: string[] } {
  const usable = Array.isArray(world) && world.length === 33
    && world.every(p => p && [p.x, p.y, p.z].every(v => typeof v === "number" && Number.isFinite(v)));
  if (!usable) return { bones: {}, warnings: ["没有识别到人物"] };
  const source = options.mirror ? MP_MIRROR.map(i => world[i] as DirectorLandmark) : world;
  const pts: P3[] = source.map(p => ({ x: options.mirror ? -p.x : p.x, y: -p.y, z: -p.z }));
  const visible = (...indexes: number[]) => indexes.every(i => (source[i]?.visibility ?? 1) >= MP_VISIBLE);
  const at = (i: number) => pts[i] as P3;

  const rest = restPoseOf(options.model ?? "adult_male");
  const restWorld: Record<string, { q: Q4; p: P3 }> = {};
  for (const name of DIRECTOR_REST_BONES) {
    const parentName = DIRECTOR_BONE_PARENT[name];
    const parent = parentName ? restWorld[parentName] : { q: Q_IDENTITY, p: { x: 0, y: 0, z: 0 } };
    if (!parent) continue;
    const bone = rest[name] as DirectorRestBone;
    restWorld[name] = { q: qMul(parent.q, restQ(bone)), p: add3(parent.p, qRotate(parent.q, restP(bone))) };
  }
  const restAt = (name: string) => (restWorld[name] as { q: Q4; p: P3 }).p;

  // 摆好的骨骼的世界旋转；没摆的按「rest 相对已摆好的父骨骼」现算
  const posed: Record<string, Q4> = {};
  const worldOf = (name: string): Q4 => {
    if (posed[name]) return posed[name] as Q4;
    const parentName = DIRECTOR_BONE_PARENT[name];
    return qMul(parentName ? worldOf(parentName) : Q_IDENTITY, restQ(rest[name] as DirectorRestBone));
  };
  const solved: string[] = [];
  const skipped: string[] = [];

  // 骨盆与胸：身体坐标系从 rest 转到实测
  const restUp = sub3(mid3(restAt("LeftArm"), restAt("RightArm")), mid3(restAt("LeftUpLeg"), restAt("RightUpLeg")));
  const up = sub3(mid3(at(MP.leftShoulder), at(MP.rightShoulder)), mid3(at(MP.leftHip), at(MP.rightHip)));
  const pelvis = bodyFrame(sub3(at(MP.leftHip), at(MP.rightHip)), up);
  const pelvisRest = bodyFrame(sub3(restAt("LeftUpLeg"), restAt("RightUpLeg")), restUp);
  const chest = bodyFrame(sub3(at(MP.leftShoulder), at(MP.rightShoulder)), up);
  const chestRest = bodyFrame(sub3(restAt("LeftArm"), restAt("RightArm")), restUp);
  if (visible(MP.leftShoulder, MP.rightShoulder, MP.leftHip, MP.rightHip) && pelvis && pelvisRest && chest && chestRest) {
    posed.Hips = qMul(qMul(pelvis, qConj(pelvisRest)), (restWorld.Hips as { q: Q4 }).q);
    posed.Spine2 = qMul(qMul(chest, qConj(chestRest)), (restWorld.Spine2 as { q: Q4 }).q);
    solved.push("Hips", "Spine2");
  } else {
    skipped.push("torso");
  }

  // 头：rest 时头骨的局部轴就是「X 向左、Y 向上、Z 向前」
  const head = bodyFrame(sub3(at(MP.leftEar), at(MP.rightEar)), sub3(mid3(at(MP.leftEye), at(MP.rightEye)), mid3(at(MP.mouthLeft), at(MP.mouthRight))));
  if (visible(MP.leftEar, MP.rightEar, MP.leftEye, MP.rightEye, MP.mouthLeft, MP.mouthRight) && head) {
    posed.Head = qMul(head, (restWorld.Head as { q: Q4 }).q);
    solved.push("Head");
  } else {
    skipped.push("head");
  }

  /** 让 `bone` 指向 `target`（这一节到下一节的方向）；有铰链的再绕自身转到弯曲平面上。 */
  const aim = (bone: string, tip: string, target: P3, lowerTarget?: P3) => {
    const current = worldOf(bone);
    const direction = norm3(target);
    const from = norm3(qRotate(current, restP(rest[tip] as DirectorRestBone)));
    let next = qMul(qFromUnitVectors(from, direction), current);
    const hinge = (LIMB_HINGES as Record<string, { lower: string; axis: P3; sign: 1 | -1 }>)[bone];
    const bend = hinge && lowerTarget ? cross3(direction, norm3(lowerTarget)) : null;
    if (hinge && bend && len3(bend) > MIN_BEND_SIN) {
      const want = scale3(norm3(bend), hinge.sign);
      const axis = qRotate(qMul(next, restQ(rest[hinge.lower] as DirectorRestBone)), hinge.axis);
      const a = norm3(sub3(axis, scale3(direction, dot3(axis, direction))));
      const b = norm3(sub3(want, scale3(direction, dot3(want, direction))));
      if (len3(a) > 0.5 && len3(b) > 0.5) {
        // a、b 都垂直于肢体方向：转动轴就是肢体本身，方向不变；正好反向时显式绕肢体转半圈
        const twist = dot3(a, b) < -0.999999 ? { ...direction, w: 0 } : qFromUnitVectors(a, b);
        next = qMul(twist, next);
      }
    }
    posed[bone] = next;
    solved.push(bone);
  };

  const limbs: Array<{ bone: string; tip: string; from: number; to: number[]; lower?: number[] }> = [];
  for (const side of ["Left", "Right"] as const) {
    const s = side === "Left"
      ? { shoulder: MP.leftShoulder, elbow: MP.leftElbow, wrist: MP.leftWrist, index: MP.leftIndex, pinky: MP.leftPinky, hip: MP.leftHip, knee: MP.leftKnee, ankle: MP.leftAnkle, foot: MP.leftFootIndex }
      : { shoulder: MP.rightShoulder, elbow: MP.rightElbow, wrist: MP.rightWrist, index: MP.rightIndex, pinky: MP.rightPinky, hip: MP.rightHip, knee: MP.rightKnee, ankle: MP.rightAnkle, foot: MP.rightFootIndex };
    limbs.push(
      { bone: `${side}Arm`, tip: `${side}ForeArm`, from: s.shoulder, to: [s.elbow], lower: [s.elbow, s.wrist] },
      { bone: `${side}ForeArm`, tip: `${side}Hand`, from: s.elbow, to: [s.wrist] },
      { bone: `${side}Hand`, tip: `${side}HandMiddle1`, from: s.wrist, to: [s.index, s.pinky] },
      { bone: `${side}UpLeg`, tip: `${side}Leg`, from: s.hip, to: [s.knee], lower: [s.knee, s.ankle] },
      { bone: `${side}Leg`, tip: `${side}Foot`, from: s.knee, to: [s.ankle] },
      { bone: `${side}Foot`, tip: `${side}ToeBase`, from: s.ankle, to: [s.foot] }
    );
  }
  const centerOf = (indexes: number[]) => scale3(indexes.map(at).reduce(add3, { x: 0, y: 0, z: 0 }), 1 / indexes.length);
  const lostParents = new Set<string>();
  for (const limb of limbs) {
    const parentName = DIRECTOR_BONE_PARENT[limb.bone] ?? "";
    if (lostParents.has(parentName) || !visible(limb.from, ...limb.to)) {
      // 上一节没摆的话这一节也不摆：方向对准要以摆好的父骨骼为准，父骨骼还在 rest 时对出来的角度没有意义
      lostParents.add(limb.bone);
      skipped.push(limb.bone);
      continue;
    }
    const lower = limb.lower && visible(...limb.lower) ? sub3(at(limb.lower[1] as number), at(limb.lower[0] as number)) : undefined;
    aim(limb.bone, limb.tip, sub3(centerOf(limb.to), at(limb.from)), lower);
  }

  const bones: Record<string, DirectorBoneRotation> = {};
  for (const name of solved) {
    const parentName = DIRECTOR_BONE_PARENT[name];
    const local = qMul(qConj(parentName ? worldOf(parentName) : Q_IDENTITY), posed[name] as Q4);
    const override = eulerXYZOf(qMul(qConj(restQ(rest[name] as DirectorRestBone)), local));
    bones[name] = { x: round1(override.x), y: round1(override.y), z: round1(override.z) };
  }
  const warnings = skipped.length
    ? [`看不清的部位保持原样：${[...new Set(skipped.map(name => BODY_PART_NAMES[name] ?? name))].join("、")}`]
    : [];
  return { bones, warnings };
}
const round1 = (v: number) => Math.round(v * 10) / 10 || 0;

/**
 * 画面里有多个人时取 2D 包围盒（图像坐标，只算可见度够的点）面积最大的那一个（DR-F-112）；没有人时返回 null。
 */
export function directorPickLargestPose<T extends { landmarks: readonly DirectorLandmark[] }>(poses: readonly T[] | null | undefined): T | null {
  let best: T | null = null;
  let bestArea = -1;
  for (const pose of poses ?? []) {
    const seen = pose.landmarks.filter(p => (p.visibility ?? 1) >= MP_VISIBLE);
    const points = seen.length >= 2 ? seen : pose.landmarks;
    if (!points.length) continue;
    const xs = points.map(p => p.x);
    const ys = points.map(p => p.y);
    const area = (Math.max(...xs) - Math.min(...xs)) * (Math.max(...ys) - Math.min(...ys));
    if (area > bestArea) {
      best = pose;
      bestArea = area;
    }
  }
  return best;
}
