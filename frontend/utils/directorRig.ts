/**
 * 3D 导演台素模的骨骼与体型（DRT-10，DR-F-40 / 41，DRK-2）。纯数据 + 几个小函数，不 import three。
 *
 * **标准骨骼名** = 去掉 `mixamorig:` 前缀的 Mixamo 命名，共 67 根（含 13 根末端骨，如 `HeadTop_End`、
 * `LeftHandIndex4`、`LeftToe_End`）。姿势数据（`public/director/poses.json`）、`character.boneOverrides`、
 * IK 链（DRT-21）、MediaPipe 映射（DRT-23）、OpenPose 关键点（DRT-27）**一律只用标准名**。
 *
 * ⚠️ 发布的 4 个 GLB（`public/director/models/`）里骨骼**已经是标准名**，运行时直接按标准名找骨骼，不再映射：
 * - 上游的 13 根末端骨叫 `leaf` / `leaf.001`…，**少年男的编号与另外三个不同**（同一个 `leaf.004`，
 *   在少年男是左手小指末端，在另外三个是左手无名指末端）；
 * - three 的 GLTFLoader 会删掉节点名里的 `.`（`PropertyBinding.sanitizeNodeName`），运行时拿到的是 `leaf004`，
 *   按原名映射根本对不上。
 * 所以改名在构建时做一次：`scripts/director-assets.mjs` 读本文件的 `DIRECTOR_UPSTREAM_BONE_RENAMES`，
 * 并把实际改了哪些名字写进 GLB 的 `extras.directorAsset.boneRenames`，`directorRig.test.ts` 逐项核对。
 *
 * **姿势口径**（`poses.json` 的 `bones` 与 `character.boneOverrides` 相同）：相对 rest 的局部欧拉角，单位度，
 * 顺序 XYZ（three 的 `Euler` 默认顺序）。套用方式：
 * `bone.quaternion = restQuaternion × quaternion(Euler(x°, y°, z°, "XYZ"))` —— 右乘，即绕骨骼**自己的 rest 局部轴**转。
 * 同一根骨骼在 `boneOverrides` 里有值时**取覆盖值、不与预设叠加**（两者同口径，叠加等于转两遍）。
 *
 * rest 是 A 字姿势：手臂向外下方约 45°。几根主干骨骼的局部轴（四个模型之间躯干与四肢偏差在 7° 以内，手指在 12° 以内）：
 * - Hips / Spine* / Neck / Head：X 指向角色左侧，Y 向上，Z 向前。+X = 向前弯（低头），+Y = 向左转，+Z = 向右侧弯；
 * - LeftArm / RightArm：Y 沿手臂向外。+X = 手臂放下（贴向身体），−X = 抬起；左臂 +Z = 向前摆，右臂 +Z = 向后摆；
 * - LeftUpLeg / RightUpLeg：Y 沿大腿向下。+X = 抬腿向前；左腿 +Z = 向外张开，右腿 +Z = 向内收；
 * - LeftLeg / RightLeg（小腿）：−X = 屈膝；LeftFoot / RightFoot：+X = 勾脚尖，−X = 绷脚尖。
 *
 * **左右镜像**：名字左右对调，(x, y, z) → (x, −y, −z)。四个模型的 rest 局部轴都满足这条
 * （躯干与四肢偏差在 3.2° 以内，手指最多约 11°；13 根末端骨与两只眼睛不带蒙皮权重），DRT-21 的「左右镜像」照此做。
 */

// 只用类型（转译后整行消失）：构建脚本按 data URL 单独加载本文件，不能有运行时的相对导入。
import type { DirectorBodyCode, DirectorModelCode } from "./directorCatalog";

/** 有骨骼表的素模（与 `api/fixtures/director/catalog.json` 的 `models` 一致，`directorRig.test.ts` 核对）。 */
export const DIRECTOR_RIG_MODELS: readonly DirectorModelCode[] = Object.freeze([
  "adult_male",
  "adult_female",
  "youth_male",
  "youth_female"
]);

const FINGERS = ["Thumb", "Index", "Middle", "Ring", "Pinky"] as const;

function handBones(side: "Left" | "Right"): string[] {
  return FINGERS.flatMap((finger) => [1, 2, 3, 4].map((n) => `${side}Hand${finger}${n}`));
}

/** 67 根标准骨骼，按骨架顺序（父在子前）。 */
export const DIRECTOR_STANDARD_BONES: readonly string[] = Object.freeze([
  "Hips", "Spine", "Spine1", "Spine2", "Neck", "Head", "HeadTop_End", "LeftEye", "RightEye",
  "LeftShoulder", "LeftArm", "LeftForeArm", "LeftHand", ...handBones("Left"),
  "RightShoulder", "RightArm", "RightForeArm", "RightHand", ...handBones("Right"),
  "LeftUpLeg", "LeftLeg", "LeftFoot", "LeftToeBase", "LeftToe_End",
  "RightUpLeg", "RightLeg", "RightFoot", "RightToeBase", "RightToe_End"
]);

function parentTable(): Record<string, string | null> {
  const parent: Record<string, string | null> = {
    Hips: null, Spine: "Hips", Spine1: "Spine", Spine2: "Spine1", Neck: "Spine2", Head: "Neck",
    HeadTop_End: "Head", LeftEye: "Head", RightEye: "Head"
  };
  for (const side of ["Left", "Right"] as const) {
    parent[`${side}Shoulder`] = "Spine2";
    parent[`${side}Arm`] = `${side}Shoulder`;
    parent[`${side}ForeArm`] = `${side}Arm`;
    parent[`${side}Hand`] = `${side}ForeArm`;
    for (const finger of FINGERS) {
      parent[`${side}Hand${finger}1`] = `${side}Hand`;
      for (const n of [2, 3, 4]) parent[`${side}Hand${finger}${n}`] = `${side}Hand${finger}${n - 1}`;
    }
    parent[`${side}UpLeg`] = "Hips";
    parent[`${side}Leg`] = `${side}UpLeg`;
    parent[`${side}Foot`] = `${side}Leg`;
    parent[`${side}ToeBase`] = `${side}Foot`;
    parent[`${side}Toe_End`] = `${side}ToeBase`;
  }
  return parent;
}

/** 标准骨骼的父子关系（四个模型的层级完全相同，DRT-10 已核对）。根骨骼 Hips 的父为 null。 */
export const DIRECTOR_BONE_PARENT: Readonly<Record<string, string | null>> = Object.freeze(parentTable());

/** 绝大多数模型共用的末端骨编号（成年男、成年女、少年女）。 */
const LEAF_RENAMES_COMMON: Readonly<Record<string, string>> = Object.freeze({
  leaf: "HeadTop_End",
  "leaf.001": "LeftHandThumb4",
  "leaf.002": "LeftHandIndex4",
  "leaf.003": "LeftHandMiddle4",
  "leaf.004": "LeftHandRing4",
  "leaf.005": "LeftHandPinky4",
  "leaf.006": "RightHandThumb4",
  "leaf.007": "RightHandIndex4",
  "leaf.008": "RightHandMiddle4",
  "leaf.009": "RightHandRing4",
  "leaf.010": "RightHandPinky4",
  "leaf.011": "LeftToe_End",
  "leaf.012": "RightToe_End"
});

/**
 * 上游原名 → 标准名，只列名字不同的那些（其余 54 根上游就是标准名）。构建时用，发布的 GLB 里已是标准名。
 *
 * ⚠️ 少年男的编号是另一套：左手无名指末端是 `leaf.012`，之后整体前移一位。不按模型分开写的话，
 * 少年男的左手小指会被当成无名指、右手拇指当成左手小指，套姿势时手指扭成一团而不报错。
 */
export const DIRECTOR_UPSTREAM_BONE_RENAMES: Readonly<Record<DirectorModelCode, Readonly<Record<string, string>>>> =
  Object.freeze({
    adult_male: LEAF_RENAMES_COMMON,
    adult_female: LEAF_RENAMES_COMMON,
    youth_male: Object.freeze({
      leaf: "HeadTop_End",
      "leaf.001": "LeftHandThumb4",
      "leaf.002": "LeftHandIndex4",
      "leaf.003": "LeftHandMiddle4",
      "leaf.012": "LeftHandRing4",
      "leaf.004": "LeftHandPinky4",
      "leaf.005": "RightHandThumb4",
      "leaf.006": "RightHandIndex4",
      "leaf.007": "RightHandMiddle4",
      "leaf.008": "RightHandRing4",
      "leaf.009": "RightHandPinky4",
      "leaf.010": "LeftToe_End",
      "leaf.011": "RightToe_End"
    }),
    youth_female: LEAF_RENAMES_COMMON
  });

const STANDARD_SET = new Set(DIRECTOR_STANDARD_BONES);

/**
 * 上游原名 → 标准名；不是标准骨骼时返回 null。去掉可能带的 `mixamorig:` 前缀
 * （Mixamo 导出的命名，Blender 里常见；上游 GLB 本身没有）。
 */
export function directorStandardBoneName(model: DirectorModelCode, upstreamName: string): string | null {
  const name = upstreamName.replace(/^mixamorig:?/, "");
  const renamed = DIRECTOR_UPSTREAM_BONE_RENAMES[model][name] ?? name;
  return STANDARD_SET.has(renamed) ? renamed : null;
}

/** 上游自带的三个体型混合（morph target），名字与顺序以 GLB 里 `mesh.extras.targetNames` 为准。 */
export const DIRECTOR_MORPH_TARGETS = Object.freeze(["Skinny", "Muscular", "Obese"] as const);
export type DirectorMorphTarget = (typeof DIRECTOR_MORPH_TARGETS)[number];

/**
 * 体型三档 → morph 权重（DR-F-41）。没列出的 morph 权重为 0。
 *
 * - standard：上游默认体型（Mesomorph），全部为 0；
 * - slim：上游的 Ectomorph 原型（Skinny 1.0）；
 * - stocky：上游 README 给的「Stocky person」配方（Obese 0.5 + Muscular 0.5）。
 *
 * 四个模型同一套配方：少年模型的 morph 本身幅度小一些（腹部厚度 +27%，成年男 +42%），属于模型自身的体态，不另行放大。
 * 取值是设计建议值，DRT-12 的沙盒页目视核对后可调；改这里不影响已存的场景（场景只存档位码）。
 */
const BODY_RECIPE: Readonly<Record<DirectorBodyCode, Readonly<Partial<Record<DirectorMorphTarget, number>>>>> = Object.freeze({
  slim: Object.freeze({ Skinny: 1 }),
  standard: Object.freeze({}),
  stocky: Object.freeze({ Muscular: 0.5, Obese: 0.5 })
});

export const DIRECTOR_BODY_MORPH_WEIGHTS: Readonly<
  Record<DirectorModelCode, Readonly<Record<DirectorBodyCode, Readonly<Partial<Record<DirectorMorphTarget, number>>>>>>
> = Object.freeze({
  adult_male: BODY_RECIPE,
  adult_female: BODY_RECIPE,
  youth_male: BODY_RECIPE,
  youth_female: BODY_RECIPE
});

/**
 * 某个模型某档体型的**完整**权重（三个 morph 都有值）。切换体型时整组写入：
 * 只写配方里出现的那几个的话，从「偏壮」切到「偏瘦」会留下 Obese 0.5，而界面上显示的是偏瘦。
 */
export function directorBodyMorphWeights(model: DirectorModelCode, body: DirectorBodyCode): Record<DirectorMorphTarget, number> {
  const recipe = DIRECTOR_BODY_MORPH_WEIGHTS[model][body];
  return { Skinny: recipe.Skinny ?? 0, Muscular: recipe.Muscular ?? 0, Obese: recipe.Obese ?? 0 };
}

export type DirectorBoneRotation = { x: number; y: number; z: number };

/** 左右镜像后的骨骼名（中轴骨骼原样返回）。 */
export function directorMirrorBoneName(name: string): string {
  if (name.startsWith("Left")) return `Right${name.slice(4)}`;
  if (name.startsWith("Right")) return `Left${name.slice(5)}`;
  return name;
}

/** 左右镜像一组骨骼旋转：名字左右对调，(x, y, z) → (x, −y, −z)。镜像两次复原。不改入参。 */
export function directorMirrorBones(
  bones: Readonly<Record<string, DirectorBoneRotation>>
): Record<string, DirectorBoneRotation> {
  const out: Record<string, DirectorBoneRotation> = {};
  for (const [name, r] of Object.entries(bones)) {
    // `0 - v` 而不是 `-v`：避免产出 -0，序列化后与原值比较时不至于多出一个「-0」。
    out[directorMirrorBoneName(name)] = { x: r.x, y: 0 - r.y, z: 0 - r.z };
  }
  return out;
}
