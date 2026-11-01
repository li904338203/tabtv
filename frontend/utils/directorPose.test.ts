import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import { DIRECTOR_LIMITS } from "./directorCatalog";
import {
  DIRECTOR_JOINT_BONES,
  DIRECTOR_OPENPOSE_COLORS,
  DIRECTOR_OPENPOSE_LIMBS,
  DIRECTOR_OPENPOSE_POINTS,
  directorOpenPoseFromJoints,
  directorApplyPosePreset,
  directorJointLabel,
  directorJointTranslatable,
  directorMirrorCharacterPose,
  directorMirrorPose,
  directorPickLargestPose,
  directorPoseBonesValid,
  directorPoseDroppedText,
  directorPoseFromLandmarks,
  directorPoseJoints,
  directorResetJoint,
  directorResetPose,
  directorSetFootLock,
  directorSetPose,
  type DirectorLandmark
} from "./directorPose";
import { DIRECTOR_STANDARD_BONES, directorMirrorBones } from "./directorRig";
import { directorFindObject, directorValidateScene, type DirectorScene } from "./directorScene";

const DEFAULT_SCENE = JSON.parse(readFileSync(fileURLToPath(new URL("../../api/fixtures/director/scenes/valid-default.json", import.meta.url)), "utf8")) as DirectorScene;
const HERO = "obj_character_1";
const characterOf = (scene: DirectorScene) => directorFindObject(scene, HERO)?.character as Record<string, unknown>;

describe("姿势骨骼（DRT-21）", () => {
  it("directorMirrorBones 左右对调", () => {
    expect(directorMirrorBones({ LeftArm: { x: 10, y: 20, z: 30 }, Spine: { x: 5, y: 6, z: 7 } }))
      .toEqual({ RightArm: { x: 10, y: -20, z: -30 }, Spine: { x: 5, y: -6, z: -7 } });
  });

  it("directorMirrorBones 镜像两次复原", () => {
    const pose = { LeftUpLeg: { x: -80, y: 3, z: -4 }, RightForeArm: { x: 0, y: 0, z: 95 } };
    expect(directorMirrorBones(directorMirrorBones(pose))).toEqual(pose);
  });

  it("directorPoseBonesValid 超过70项拒绝", () => {
    // 标准骨骼 67 根 + 4 根不认识的 = 71 项：按原始项数拒绝（与场景校验同一口径），不截成半身
    expect(DIRECTOR_STANDARD_BONES).toHaveLength(67);
    const bones = Object.fromEntries([...DIRECTOR_STANDARD_BONES, "X1", "X2", "X3", "X4"].map(name => [name, { x: 1, y: 0, z: 0 }]));
    expect(directorPoseBonesValid(bones)).toEqual({ ok: false, error: "limit", limit: DIRECTOR_LIMITS.maxBoneOverrides, count: 71 });
    // 正好 70 项放行，不认识的 3 根过滤掉
    const seventy = Object.fromEntries([...DIRECTOR_STANDARD_BONES, "X1", "X2", "X3"].map(name => [name, { x: 0, y: 0, z: 0 }]));
    const result = directorPoseBonesValid(seventy);
    expect(result.ok && [Object.keys(result.bones).length, result.dropped]).toEqual([67, ["X1", "X2", "X3"]]);
  });

  it("directorPoseBonesValid 过滤未知骨骼并告警", () => {
    const result = directorPoseBonesValid({
      LeftArm: { x: 1, y: 2, z: 3 },
      "mixamorig:Tail": { x: 0, y: 0, z: 0 },
      RightArm: { x: Number.NaN, y: 0, z: 0 }
    });
    expect(result).toEqual({ ok: true, bones: { LeftArm: { x: 1, y: 2, z: 3 } }, dropped: ["mixamorig:Tail", "RightArm"] });
    if (result.ok) expect(directorPoseDroppedText(result.dropped)).toBe("有 2 根骨骼无法识别，已忽略：mixamorig:Tail、RightArm");
    expect(directorPoseDroppedText([])).toBe("");
    expect(directorPoseDroppedText(["a", "b", "c", "d", "e", "f"])).toBe("有 6 根骨骼无法识别，已忽略：a、b、c、d、e 等");
  });
});

describe("姿势的场景改动（DRT-21，DR-F-42 / 45）", () => {
  it("套用姿势写预设码并清空覆盖；同一个姿势不产生新场景", () => {
    const tuned = directorSetPose(DEFAULT_SCENE, HERO, { boneOverrides: { LeftArm: { x: 10, y: 0, z: 0 } } });
    const sat = directorApplyPosePreset(tuned, HERO, "sit_chair");
    expect(characterOf(sat)).toMatchObject({ posePreset: "sit_chair", boneOverrides: {} });
    expect(directorApplyPosePreset(sat, HERO, "sit_chair")).toBe(sat);
    expect(directorValidateScene(sat).ok).toBe(true);
  });

  it("重置姿势回到自然站立、清空覆盖，脚底锁定保留", () => {
    let scene = directorApplyPosePreset(DEFAULT_SCENE, HERO, "squat");
    scene = directorSetFootLock(scene, HERO, "left", true);
    scene = directorSetPose(scene, HERO, { boneOverrides: { Head: { x: 0, y: 30, z: 0 } } });
    const reset = directorResetPose(scene, HERO);
    expect(characterOf(reset)).toMatchObject({ posePreset: "stand_neutral", boneOverrides: {}, footLock: { left: true, right: false } });
  });

  it("directorMirrorPose：预设里有、镜像后没有对应项的骨骼写成 0；镜像两次合成出的姿势复原", () => {
    const preset = { LeftArm: { x: 40, y: 10, z: 5 }, Spine: { x: 3, y: 4, z: 5 } };
    const overrides = { RightForeArm: { x: 0, y: 0, z: 60 } };
    const once = directorMirrorPose(preset, overrides);
    expect(once).toEqual({
      RightArm: { x: 40, y: -10, z: -5 }, Spine: { x: 3, y: -4, z: -5 }, LeftForeArm: { x: 0, y: 0, z: -60 },
      // 预设里的左臂被「盖成」rest，否则镜像之后两只手都抬着
      LeftArm: { x: 0, y: 0, z: 0 }
    });
    const effective = (o: Record<string, { x: number; y: number; z: number }>) => {
      const merged: Record<string, { x: number; y: number; z: number }> = { ...preset, ...o };
      // 只看不是 rest 的骨骼：写成 0 的与没写的是同一个姿势
      return Object.fromEntries(Object.entries(merged).filter(([, r]) => r.x !== 0 || r.y !== 0 || r.z !== 0));
    };
    expect(effective(directorMirrorPose(preset, once))).toEqual(effective(overrides));
  });

  it("镜像一个角色：覆盖整组换成镜像结果，预设码不变", () => {
    const scene = directorApplyPosePreset(DEFAULT_SCENE, HERO, "wave");
    const mirrored = directorMirrorCharacterPose(scene, HERO, { RightArm: { x: 10, y: 20, z: 160 } });
    expect(characterOf(mirrored)).toMatchObject({ posePreset: "wave", boneOverrides: { LeftArm: { x: 10, y: -20, z: -160 }, RightArm: { x: 0, y: 0, z: 0 } } });
  });

  it("关节模式下的 Delete 只去掉这一根骨骼的覆盖，不删角色；没有覆盖时返回同一个场景", () => {
    const scene = directorSetPose(DEFAULT_SCENE, HERO, { boneOverrides: { Head: { x: 0, y: 30, z: 0 }, Neck: { x: 5, y: 0, z: 0 } } });
    const reset = directorResetJoint(scene, HERO, "Head");
    expect(characterOf(reset).boneOverrides).toEqual({ Neck: { x: 5, y: 0, z: 0 } });
    expect(directorFindObject(reset, HERO)).toBeTruthy();
    expect(directorResetJoint(reset, HERO, "Head")).toBe(reset);
  });

  it("directorSetPose：变换、覆盖、脚底锁定一次写进去；超过 70 项不写", () => {
    const transform = { position: { x: 0.4, y: 0, z: 0 }, rotation: { x: 0, y: 0, z: 0 }, scale: { x: 1, y: 1, z: 1 } };
    const posed = directorSetPose(DEFAULT_SCENE, HERO, { boneOverrides: { LeftLeg: { x: 30, y: 0, z: 0 } }, transform, footLock: { left: false, right: true } });
    expect(directorFindObject(posed, HERO)?.transform.position).toEqual({ x: 0.4, y: 0, z: 0 });
    expect(characterOf(posed)).toMatchObject({ boneOverrides: { LeftLeg: { x: 30, y: 0, z: 0 } }, footLock: { left: false, right: true } });
    const tooMany = Object.fromEntries(Array.from({ length: 71 }, (_, i) => [`B${i}`, { x: 0, y: 0, z: 0 }]));
    expect(directorSetPose(DEFAULT_SCENE, HERO, { boneOverrides: tooMany })).toBe(DEFAULT_SCENE);
  });

  it("关节清单：都是标准骨骼；只有骨盆与手脚末端能移动", () => {
    expect(DIRECTOR_JOINT_BONES.every(entry => DIRECTOR_STANDARD_BONES.includes(entry.bone))).toBe(true);
    expect(DIRECTOR_JOINT_BONES.filter(entry => directorJointTranslatable(entry.bone)).map(entry => entry.bone))
      .toEqual(["Hips", "LeftHand", "RightHand", "LeftFoot", "RightFoot"]);
    expect(directorJointLabel("LeftForeArm")).toBe("左前臂");
    expect(directorJointLabel("Unknown")).toBe("Unknown");
  });
});

describe("OpenPose 骨架图（DRT-27）", () => {
  // 面朝 +Z 的人：人物的右手在 −X 一侧
  const joints = {
    Neck: { x: 0, y: 1.5, z: 0 }, Head: { x: 0, y: 1.6, z: 0 }, LeftEye: { x: 0.032, y: 1.68, z: 0.08 }, RightEye: { x: -0.032, y: 1.68, z: 0.08 },
    RightArm: { x: -0.18, y: 1.45, z: 0 }, RightForeArm: { x: -0.45, y: 1.45, z: 0 }, RightHand: { x: -0.7, y: 1.45, z: 0 },
    LeftArm: { x: 0.18, y: 1.45, z: 0 }, LeftForeArm: { x: 0.45, y: 1.45, z: 0 }, LeftHand: { x: 0.7, y: 1.45, z: 0 },
    RightUpLeg: { x: -0.1, y: 0.95, z: 0 }, RightLeg: { x: -0.1, y: 0.5, z: 0 }, RightFoot: { x: -0.1, y: 0.08, z: 0 },
    LeftUpLeg: { x: 0.1, y: 0.95, z: 0 }, LeftLeg: { x: 0.1, y: 0.5, z: 0 }, LeftFoot: { x: 0.1, y: 0.08, z: 0 }
  };

  it("directorOpenPoseFromJoints 输出18点且顺序正确", () => {
    const points = directorOpenPoseFromJoints(joints);
    expect(points).toHaveLength(18);
    expect(DIRECTOR_OPENPOSE_POINTS.map(p => p.name)).toEqual(["鼻", "颈", "右肩", "右肘", "右腕", "左肩", "左肘", "左腕", "右髋", "右膝", "右踝", "左髋", "左膝", "左踝", "右眼", "左眼", "右耳", "左耳"]);
    expect(points[1]).toEqual(joints.Neck);
    expect(points[4]).toEqual(joints.RightHand);
    expect(points[13]).toEqual(joints.LeftFoot);
    expect(points[14]).toEqual(joints.RightEye);
    // 鼻在两眼中点前下方；右耳在人物右侧（−X）且比眼睛靠后
    expect(points[0]!.z).toBeGreaterThan(0.08);
    expect(points[0]!.y).toBeLessThan(1.68);
    expect(points[16]!.x).toBeLessThan(-0.05);
    expect(points[16]!.z).toBeLessThan(0.08);
    expect(points[17]!.x).toBeGreaterThan(0.05);
    expect(DIRECTOR_OPENPOSE_LIMBS).toHaveLength(17);
    expect(DIRECTOR_OPENPOSE_COLORS).toHaveLength(18);
  });

  it("directorOpenPoseFromJoints 缺骨骼时该点为空", () => {
    const { LeftHand: _hand, LeftEye: _eye, ...rest } = joints;
    const points = directorOpenPoseFromJoints(rest);
    expect(points[7]).toBeNull();
    expect(points[15]).toBeNull();
    // 少了一只眼睛：鼻与两耳推不出来，不凭空猜
    expect([points[0], points[16], points[17]]).toEqual([null, null, null]);
    expect(points[14]).toEqual(joints.RightEye);
  });
});

// ---------------------------------------------------------------- 参考图转姿势（DRT-23）

type V = { x: number; y: number; z: number };
type Q = { x: number; y: number; z: number; w: number };
const POSES = JSON.parse(readFileSync(fileURLToPath(new URL("../public/director/poses.json", import.meta.url)), "utf8")) as Array<{ code: string; bones: Record<string, V> }>;
const poseBones = (code: string) => (POSES.find(p => p.code === code) as { bones: Record<string, V> }).bones;
const armsUp = () => JSON.parse(readFileSync(fileURLToPath(new URL("../../api/fixtures/director/pose/arms-up-landmarks.json", import.meta.url)), "utf8")) as {
  worldLandmarks: DirectorLandmark[];
};
const rot = (q: Q, v: V): V => {
  const t = { x: 2 * (q.y * v.z - q.z * v.y), y: 2 * (q.z * v.x - q.x * v.z), z: 2 * (q.x * v.y - q.y * v.x) };
  return { x: v.x + q.w * t.x + (q.y * t.z - q.z * t.y), y: v.y + q.w * t.y + (q.z * t.x - q.x * t.z), z: v.z + q.w * t.z + (q.x * t.y - q.y * t.x) };
};
const plus = (...vs: V[]) => vs.reduce((a, b) => ({ x: a.x + b.x, y: a.y + b.y, z: a.z + b.z }));
const minus = (a: V, b: V): V => ({ x: a.x - b.x, y: a.y - b.y, z: a.z - b.z });
const times = (a: V, k: number): V => ({ x: a.x * k, y: a.y * k, z: a.z * k });
const angleDeg = (a: V, b: V) => {
  const d = (a.x * b.x + a.y * b.y + a.z * b.z) / (Math.hypot(a.x, a.y, a.z) * Math.hypot(b.x, b.y, b.z));
  return (Math.acos(Math.min(1, Math.max(-1, d))) * 180) / Math.PI;
};

/**
 * 按素模摆出的姿势合成 33 个 MediaPipe 世界坐标关键点（模型坐标 (x, y, z) → MediaPipe 的 (x, −y, −z)）：
 * 肩 / 肘 / 腕 / 髋 / 膝 / 踝取关节位置，食指与小指取中指根（手的方向）、脚尖取脚趾根，
 * 脸上的点按头骨的局部轴摆（两耳在左右、眼在上、嘴在下）。
 */
function landmarksOf(bones: Record<string, V>, visibility: (index: number) => number = () => 0.99): DirectorLandmark[] {
  const j = directorPoseJoints(bones);
  const at = (name: string) => (j[name] as { position: V }).position;
  const head = j.Head as { position: V; quaternion: Q };
  const axis = (v: V) => rot(head.quaternion, v);
  const center = plus(head.position, axis({ x: 0, y: 0.08, z: 0.02 }));
  const face = (x: number, y: number, z: number) => plus(center, axis({ x, y, z }));
  const model: V[] = [
    face(0, -0.01, 0.1),
    face(0.015, 0.03, 0.08), face(0.03, 0.03, 0.08), face(0.045, 0.03, 0.075),
    face(-0.015, 0.03, 0.08), face(-0.03, 0.03, 0.08), face(-0.045, 0.03, 0.075),
    face(0.075, 0.01, 0), face(-0.075, 0.01, 0),
    face(0.025, -0.05, 0.08), face(-0.025, -0.05, 0.08),
    at("LeftArm"), at("RightArm"), at("LeftForeArm"), at("RightForeArm"), at("LeftHand"), at("RightHand"),
    at("LeftHandMiddle1"), at("RightHandMiddle1"), at("LeftHandMiddle1"), at("RightHandMiddle1"), at("LeftHand"), at("RightHand"),
    at("LeftUpLeg"), at("RightUpLeg"), at("LeftLeg"), at("RightLeg"), at("LeftFoot"), at("RightFoot"),
    at("LeftFoot"), at("RightFoot"), at("LeftToeBase"), at("RightToeBase")
  ];
  const hips = times(plus(at("LeftUpLeg"), at("RightUpLeg")), 0.5);
  return model.map((p, i) => ({ x: p.x - hips.x, y: -(p.y - hips.y), z: -(p.z - hips.z), visibility: visibility(i) }));
}

const SEGMENTS: Array<[string, string]> = [
  ["LeftArm", "LeftForeArm"], ["LeftForeArm", "LeftHand"], ["LeftHand", "LeftHandMiddle1"],
  ["RightArm", "RightForeArm"], ["RightForeArm", "RightHand"], ["RightHand", "RightHandMiddle1"],
  ["LeftUpLeg", "LeftLeg"], ["LeftLeg", "LeftFoot"], ["LeftFoot", "LeftToeBase"],
  ["RightUpLeg", "RightLeg"], ["RightLeg", "RightFoot"], ["RightFoot", "RightToeBase"],
  ["RightUpLeg", "LeftUpLeg"], ["RightArm", "LeftArm"], ["Head", "HeadTop_End"]
];
/** 两套骨骼摆出来的每一节方向的最大夹角（度）。 */
function worstSegmentAngle(a: Record<string, V>, b: Record<string, V>) {
  const ja = directorPoseJoints(a);
  const jb = directorPoseJoints(b);
  const dir = (j: typeof ja, from: string, to: string) => minus((j[to] as { position: V }).position, (j[from] as { position: V }).position);
  return Math.max(...SEGMENTS.map(([from, to]) => angleDeg(dir(ja, from, to), dir(jb, from, to))));
}

describe("参考图转姿势（DRT-23，DR-F-110~114）", () => {
  it("directorPoseFromLandmarks 双手举过头顶时手腕高于头顶", () => {
    // 真实识别结果：MediaPipe Pose Landmarker（full）对证据目录里那张测试图的输出（来源与许可见 evidence/DRT-23/README.md）
    const { bones, warnings } = directorPoseFromLandmarks(armsUp().worldLandmarks);
    const j = directorPoseJoints(bones);
    const top = (j.HeadTop_End as { position: V }).position.y;
    expect((j.LeftHand as { position: V }).position.y).toBeGreaterThan(top);
    expect((j.RightHand as { position: V }).position.y).toBeGreaterThan(top);
    expect(directorPoseBonesValid(bones).ok).toBe(true);
    expect(warnings).toEqual([]);
  });

  it("directorPoseFromLandmarks 真实照片的识别结果（树式、背对镜头、双臂 V 字上举）", () => {
    // MediaPipe 对证据目录里那张 CC0 照片的真实输出：带深度噪声、背对镜头，左右手的仰角不一样（约 60° / 45°）
    const real = JSON.parse(readFileSync(fileURLToPath(new URL("../../api/fixtures/director/pose/tree-pose-arms-raised-landmarks.json", import.meta.url)), "utf8")) as {
      worldLandmarks: DirectorLandmark[];
    };
    const { bones, warnings } = directorPoseFromLandmarks(real.worldLandmarks);
    expect(Object.keys(bones).sort()).toEqual([
      "Head", "Hips", "LeftArm", "LeftFoot", "LeftForeArm", "LeftHand", "LeftLeg", "LeftUpLeg",
      "RightArm", "RightFoot", "RightForeArm", "RightHand", "RightLeg", "RightUpLeg", "Spine2"
    ]);
    expect(warnings).toEqual([]);
    const j = directorPoseJoints(bones);
    const y = (name: string) => (j[name] as { position: V }).position.y;
    expect(y("LeftHand")).toBeGreaterThan(y("HeadTop_End"));
    expect(y("RightHand")).toBeGreaterThan(y("Head"));
    // 背对镜头：骨盆的前方（+Z）转到了远离镜头的一侧
    const forward = rot((j.Hips as { quaternion: Q }).quaternion, { x: 0, y: 0, z: 1 });
    expect(forward.z).toBeLessThan(-0.5);
  });

  it("directorPoseFromLandmarks 换算回去每一节方向与原姿势一致（欢呼、出拳、蹲下）", () => {
    for (const code of ["cheer", "punch", "squat", "kick_side"]) {
      const source = poseBones(code);
      const { bones } = directorPoseFromLandmarks(landmarksOf(source));
      expect(worstSegmentAngle(source, bones), code).toBeLessThan(1);
    }
  });

  it("directorPoseFromLandmarks 肘与膝不反关节：屈肘、屈膝按原方向弯", () => {
    // 屈膝 90°、屈肘 90° 的姿势换算回去，弯曲的方向（弯曲平面法线）与原姿势相同
    const source = { LeftLeg: { x: -90, y: 0, z: 0 }, RightLeg: { x: -60, y: 0, z: 0 }, LeftForeArm: { x: 0, y: 0, z: 80 }, RightForeArm: { x: 0, y: 0, z: -80 } };
    const { bones } = directorPoseFromLandmarks(landmarksOf(source));
    expect(worstSegmentAngle(source, bones)).toBeLessThan(1);
    for (const side of ["Left", "Right"]) {
      expect(Math.abs((bones[`${side}Leg`] as V).y) + Math.abs((bones[`${side}Leg`] as V).z), `${side}Leg 只该绕铰链转`).toBeLessThan(12);
      expect((bones[`${side}Leg`] as V).x, `${side}Leg 屈膝是 −X`).toBeLessThan(-40);
    }
  });

  it("directorPoseFromLandmarks 镜像对调左右", () => {
    const raised = landmarksOf({ LeftArm: { x: -150, y: 0, z: 0 } });
    const plain = directorPoseJoints(directorPoseFromLandmarks(raised).bones);
    const mirrored = directorPoseJoints(directorPoseFromLandmarks(raised, { mirror: true }).bones);
    const y = (j: typeof plain, name: string) => (j[name] as { position: V }).position.y;
    expect(y(plain, "LeftHand")).toBeGreaterThan(y(plain, "HeadTop_End"));
    expect(y(plain, "RightHand")).toBeLessThan(y(plain, "RightArm"));
    expect(y(mirrored, "RightHand")).toBeGreaterThan(y(mirrored, "HeadTop_End"));
    expect(y(mirrored, "LeftHand")).toBeLessThan(y(mirrored, "LeftArm"));
    // 镜像两次复原：把输入先左右对调再开镜像，结果与原样换算逐项相同
    const swap = [0, 4, 5, 6, 1, 2, 3, 8, 7, 10, 9, 12, 11, 14, 13, 16, 15, 18, 17, 20, 19, 22, 21, 24, 23, 26, 25, 28, 27, 30, 29, 32, 31];
    const flipped = swap.map(i => ({ ...(raised[i] as DirectorLandmark), x: -(raised[i] as DirectorLandmark).x }));
    expect(directorPoseFromLandmarks(flipped, { mirror: true }).bones).toEqual(directorPoseFromLandmarks(raised).bones);
  });

  it("directorPoseFromLandmarks 空输入返回空并告警", () => {
    const standing = landmarksOf({});
    for (const input of [null, undefined, [], standing.slice(0, 20)]) {
      expect(directorPoseFromLandmarks(input)).toEqual({ bones: {}, warnings: ["没有识别到人物"] });
    }
    const broken = standing.map((p, i) => (i === 5 ? { ...p, x: Number.NaN } : p));
    expect(directorPoseFromLandmarks(broken).warnings).toEqual(["没有识别到人物"]);
  });

  it("directorPoseFromLandmarks 看不清的部位不摆并说出来", () => {
    // 半身照：膝盖以下的点都只是猜的（可见度低）→ 大腿、小腿、脚保持 rest，手臂与躯干照常
    const half = landmarksOf(poseBones("cheer"), i => (i >= 25 ? 0.1 : 0.99));
    const { bones, warnings } = directorPoseFromLandmarks(half);
    expect(bones.LeftArm).toBeDefined();
    expect(bones.Hips).toBeDefined();
    expect(bones.LeftUpLeg).toBeUndefined();
    expect(bones.LeftFoot).toBeUndefined();
    expect(warnings).toEqual(["看不清的部位保持原样：左大腿、左小腿、左脚、右大腿、右小腿、右脚"]);
    // 上一节没摆，下一节也不摆：只有手腕看不清时，手也跟着不摆
    const noWrist = directorPoseFromLandmarks(landmarksOf(poseBones("cheer"), i => (i === 15 ? 0.2 : 0.99)));
    expect(noWrist.bones.LeftArm).toBeDefined();
    expect(noWrist.bones.LeftForeArm).toBeUndefined();
    expect(noWrist.bones.LeftHand).toBeUndefined();
  });

  it("directorPickLargestPose 多人取包围盒最大", () => {
    const box = (x0: number, y0: number, x1: number, y1: number) => ({ landmarks: [{ x: x0, y: y0, z: 0, visibility: 1 }, { x: x1, y: y1, z: 0, visibility: 1 }, { x: 5, y: 5, z: 0, visibility: 0.1 }] });
    const small = box(0.1, 0.1, 0.2, 0.3);
    const large = box(0.4, 0.1, 0.9, 0.9);
    expect(directorPickLargestPose([small, large, box(0, 0, 0.05, 0.05)])).toBe(large);
    expect(directorPickLargestPose([])).toBeNull();
    expect(directorPickLargestPose(null)).toBeNull();
  });

  it("directorPickLargestPose 只有一人时原样返回", () => {
    const only = { landmarks: [{ x: 0.3, y: 0.2, z: 0 }, { x: 0.6, y: 0.9, z: 0 }], score: 0.9 };
    expect(directorPickLargestPose([only])).toBe(only);
  });
});
