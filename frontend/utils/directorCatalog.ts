/**
 * 3D 导演台的目录常量（DRT-11，DRD-29，DR-N-16）：素模、体型、姿势码、道具形状与色板、景别、角度、方位、关系、画幅、上限与默认值。
 *
 * ⚠️ 这是 `api/fixtures/director/catalog.json` 的**逐项镜像**，不是第二个定义：Go 侧（`director/domain/catalog.go`）
 * 与这里各持一份是必然的（Go 不能 embed 模块外的文件，前端也不该在运行时去读仓库里的 fixture），
 * 由 `directorCatalog.test.ts` 与 Go 的 `TestCatalog_与共享fixture一致` 两条绊线保证三方一致。
 * 只改一边的话，表现是「AI 用了一个前端不认识的姿势」或「前端选得出、保存时被拒」，而不报错。
 * 改目录的顺序：先改 Go 常量 → 重写 fixture → 改这里与 `public/director/poses.json`（见 fixture 的 README）。
 *
 * 全部常量深冻结：store 的撤销栈按快照工作（DRD-36），谁顺手改了一个默认值，之后每个新对象都会带上它。
 */

export type DirectorModelCode = "adult_male" | "adult_female" | "youth_male" | "youth_female";
export type DirectorBodyCode = "slim" | "standard" | "stocky";
export type DirectorPoseCategoryCode = "stand" | "move" | "rest" | "gesture" | "action" | "interact";
export type DirectorPropShape = "box" | "cylinder" | "sphere" | "plane";
export type DirectorShotSizeCode =
  | "extreme_long"
  | "long"
  | "full"
  | "medium_long"
  | "medium"
  | "medium_close"
  | "close_up"
  | "extreme_close_up";
export type DirectorAngleCode = "eye_level" | "low" | "high" | "top" | "dutch";
export type DirectorSideCode = "front" | "front_left" | "front_right" | "left" | "right" | "back";
export type DirectorRelation = "left_of" | "right_of" | "in_front_of" | "behind" | "near" | "facing" | "center";
export type DirectorAspectCode = "16:9" | "9:16" | "1:1" | "4:3" | "3:4" | "21:9";

export type DirectorLabeled<C extends string> = { readonly code: C; readonly name: string };
export type DirectorModel = DirectorLabeled<DirectorModelCode> & { readonly heightM: number };
export type DirectorPose = DirectorLabeled<string> & { readonly category: DirectorPoseCategoryCode; readonly description: string };
export type DirectorShotSize = DirectorLabeled<DirectorShotSizeCode> & { readonly coverage: number };
export type DirectorAspect = { readonly code: DirectorAspectCode; readonly width: number; readonly height: number };

function deepFreeze<T>(value: T): T {
  if (value && typeof value === "object") {
    for (const child of Object.values(value)) deepFreeze(child);
    Object.freeze(value);
  }
  return value;
}

/** 正文结构版本（与 Go 的 `CurrentSchemaVersion` 一致）。 */
export const DIRECTOR_SCHEMA_VERSION = 1;

/** 内置素模（DRK-2：统一素模外观，只有体型不同）。身高是模型原始身高，新角色默认取它。 */
export const DIRECTOR_MODELS: readonly DirectorModel[] = deepFreeze([
  { code: "adult_male", name: "成年男", heightM: 1.8 },
  { code: "adult_female", name: "成年女", heightM: 1.625 },
  { code: "youth_male", name: "少年男", heightM: 1.6 },
  { code: "youth_female", name: "少年女", heightM: 1.6 }
]);

export const DIRECTOR_BODIES: readonly DirectorLabeled<DirectorBodyCode>[] = deepFreeze([
  { code: "slim", name: "偏瘦" },
  { code: "standard", name: "标准" },
  { code: "stocky", name: "偏壮" }
]);

export const DIRECTOR_POSE_CATEGORIES: readonly DirectorLabeled<DirectorPoseCategoryCode>[] = deepFreeze([
  { code: "stand", name: "站立" },
  { code: "move", name: "行走奔跑" },
  { code: "rest", name: "坐卧" },
  { code: "gesture", name: "手势交流" },
  { code: "action", name: "动作打斗" },
  { code: "interact", name: "互动" }
]);

/** 首批姿势（DR-PARAM-15）。骨骼数据在 `public/director/poses.json`，这里只有码、名称、分类与说明。 */
export const DIRECTOR_POSES: readonly DirectorPose[] = deepFreeze([
  { code: "stand_neutral", name: "自然站立", category: "stand", description: "双脚与肩同宽站直，双臂自然下垂" },
  { code: "stand_arms_crossed", name: "双臂交叉", category: "stand", description: "站立，双臂在胸前交叉抱住" },
  { code: "stand_hands_on_hips", name: "叉腰", category: "stand", description: "站立，双手叉在腰间，手肘向外" },
  { code: "stand_hands_in_pockets", name: "插兜", category: "stand", description: "站立，双手插在裤兜里，肩膀放松" },
  { code: "stand_hands_behind", name: "背手", category: "stand", description: "站直，双手背在身后相握" },
  { code: "stand_look_back", name: "回头", category: "stand", description: "站立，上身与头向一侧后方扭转回望" },
  { code: "walk", name: "行走", category: "move", description: "正常步速行走，一腿迈出，双臂前后自然摆动" },
  { code: "walk_fast", name: "快走", category: "move", description: "步子更大的快步走，上身略前倾，摆臂明显" },
  { code: "run", name: "奔跑", category: "move", description: "跑步，一腿蹬地一腿屈膝前提，双臂屈肘摆动" },
  { code: "sprint", name: "冲刺", category: "move", description: "全力冲刺，上身大幅前倾，大步蹬地，用力摆臂" },
  { code: "jump", name: "起跳", category: "move", description: "双脚离地向上跳起，双臂上扬，膝盖微屈" },
  { code: "sneak", name: "蹑手蹑脚", category: "move", description: "弯腰屈膝，小步前移，双手在身前微抬" },
  { code: "sit_chair", name: "坐椅子", category: "rest", description: "坐在椅子上，大腿水平，小腿垂直，双手放在膝上" },
  { code: "sit_floor_cross", name: "盘腿坐地", category: "rest", description: "盘腿坐在地上，上身直立，双手搭在膝上" },
  { code: "squat", name: "蹲下", category: "rest", description: "双脚着地深蹲，膝盖弯曲，双臂搭在膝上" },
  { code: "kneel_one", name: "单膝跪地", category: "rest", description: "一膝着地一脚踏地，上身直立" },
  { code: "lie_back", name: "仰躺", category: "rest", description: "身体平躺在地，面朝上，双臂放在身体两侧" },
  { code: "lie_side", name: "侧躺", category: "rest", description: "侧身躺在地上，一臂枕在头下，双腿微屈" },
  { code: "wave", name: "挥手", category: "gesture", description: "站立，一只手臂举过肩向外挥动" },
  { code: "point_forward", name: "指向前方", category: "gesture", description: "站立，一只手臂向前伸直，食指指向前方" },
  { code: "phone_call", name: "打电话", category: "gesture", description: "站立，一只手把手机贴在耳边，另一只手自然下垂" },
  { code: "thinking", name: "托腮思考", category: "gesture", description: "站立，一手抱胸，另一手托住下巴，头微低" },
  { code: "cheer", name: "双手举起欢呼", category: "gesture", description: "站立，双臂高举过头张开" },
  { code: "bow", name: "鞠躬", category: "gesture", description: "双脚并拢，上身向前弯约四十五度，双手贴在身侧" },
  { code: "fight_stance", name: "格斗架势", category: "action", description: "两脚前后分开微屈膝，双拳举在下巴前" },
  { code: "punch", name: "出拳", category: "action", description: "前腿弓步，一臂向前直拳打出，另一拳收在下巴前" },
  { code: "kick_side", name: "侧踢", category: "action", description: "单腿支撑，另一腿向侧面伸直踢出，上身侧倾" },
  { code: "flying_kick", name: "飞身踢", category: "action", description: "腾空，一腿向前伸直踢出，另一腿屈膝收起" },
  { code: "block", name: "格挡", category: "action", description: "前臂竖起挡在头部与身体前方，膝盖微屈" },
  { code: "fall_back", name: "后仰倒地", category: "action", description: "身体向后仰倒，双臂向两侧张开，单脚离地" },
  { code: "handshake", name: "握手", category: "interact", description: "站立，右手向前伸出到腰高，准备与对方握手" },
  { code: "hug", name: "拥抱", category: "interact", description: "站立，双臂向前张开环抱，上身微前倾" },
  { code: "hold_hands", name: "牵手", category: "interact", description: "站立，一只手向侧下方伸出，牵住身旁的人" },
  { code: "push", name: "推", category: "interact", description: "弓步向前，双臂向前伸直推出" },
  { code: "hand_over", name: "递东西", category: "interact", description: "站立，双手在胸前向前递出一件物品" },
  { code: "carry_box", name: "搬箱子", category: "interact", description: "站立，双手在身前托住一个箱子，手肘弯曲" }
]);

export const DIRECTOR_PROP_SHAPES: readonly DirectorPropShape[] = deepFreeze(["box", "cylinder", "sphere", "plane"]);

/** 道具的灰阶色板（8 档，道具一律白模外观）。校验不限定在色板内。 */
export const DIRECTOR_PROP_COLORS: readonly string[] = deepFreeze([
  "#f1f3f4",
  "#dadce0",
  "#bdc1c6",
  "#9aa0a6",
  "#80868b",
  "#5f6368",
  "#3c4043",
  "#202124"
]);

/**
 * 8 档景别，coverage = 画面高 ÷ 目标高（dev/01 §4.4）。⚠️ 与求解器、镜头文字是**一张表**：
 * 只改一边的话，「AI 按特写摆的机位，镜头文字却说近景」，而两边各自看都没错。
 */
export const DIRECTOR_SHOT_SIZES: readonly DirectorShotSize[] = deepFreeze([
  { code: "extreme_long", name: "大远景", coverage: 6 },
  { code: "long", name: "远景", coverage: 3 },
  { code: "full", name: "全景", coverage: 1.25 },
  { code: "medium_long", name: "中全景", coverage: 0.85 },
  { code: "medium", name: "中景", coverage: 0.6 },
  { code: "medium_close", name: "近景", coverage: 0.4 },
  { code: "close_up", name: "特写", coverage: 0.25 },
  { code: "extreme_close_up", name: "大特写", coverage: 0.12 }
]);

export const DIRECTOR_ANGLES: readonly DirectorLabeled<DirectorAngleCode>[] = deepFreeze([
  { code: "eye_level", name: "平视" },
  { code: "low", name: "低机位仰拍" },
  { code: "high", name: "俯拍" },
  { code: "top", name: "顶视" },
  { code: "dutch", name: "斜角" }
]);

export const DIRECTOR_SIDES: readonly DirectorLabeled<DirectorSideCode>[] = deepFreeze([
  { code: "front", name: "正面" },
  { code: "front_left", name: "左前" },
  { code: "front_right", name: "右前" },
  { code: "left", name: "左侧" },
  { code: "right", name: "右侧" },
  { code: "back", name: "背面" }
]);

/** AI 搭景草案里的关系类型（左右按观看者算，DRD-27）。 */
export const DIRECTOR_RELATIONS: readonly DirectorRelation[] = deepFreeze([
  "left_of",
  "right_of",
  "in_front_of",
  "behind",
  "near",
  "facing",
  "center"
]);

/** 画幅与截图像素（DR-PARAM-08，设计建议值）。截图尺寸与屏幕分辨率、缩放无关（DR-F-71）。 */
export const DIRECTOR_ASPECTS: readonly DirectorAspect[] = deepFreeze([
  { code: "16:9", width: 1280, height: 720 },
  { code: "9:16", width: 720, height: 1280 },
  { code: "1:1", width: 1024, height: 1024 },
  { code: "4:3", width: 1152, height: 864 },
  { code: "3:4", width: 864, height: 1152 },
  { code: "21:9", width: 1680, height: 720 }
]);

/** 场景上限（DR-PARAM-09，设计建议值）。超出时拒绝并说明上限（DR-F-13）。 */
export const DIRECTOR_LIMITS = deepFreeze({
  maxObjects: 200,
  maxCrowdSize: 100,
  maxCrowdRows: 10,
  maxCrowdCols: 10,
  maxKeyframesPerTrack: 120,
  maxKeyframes: 2000,
  maxBoneOverrides: 70,
  /** 场景正文 `JSON.stringify` 后的 UTF-8 字节数上限（512 KiB）。 */
  maxSceneBytes: 524288,
  maxNameChars: 20,
  maxIdChars: 64,
  /** 坐标分量的绝对值上限（米）。超出不是「场景很大」，而是数据坏了。 */
  maxCoord: 10000
});

/**
 * 新对象与默认场景各段的默认值（catalog.json 的 `defaults` 段）。
 *
 * ⚠️ 角色的默认身高**不在这里**：取所选素模的 `heightM`（换模型时身高跟着换）。
 * ⚠️ 前端不生成默认场景：新建场景的正文由服务端给（`director/domain.DefaultScene()`），这里只用于新加的对象。
 */
export const DIRECTOR_OBJECT_DEFAULTS = deepFreeze({
  camera: { aspect: "16:9" as DirectorAspectCode, far: 1000, focalLengthMm: 35, near: 0.1, roll: 0, sensorWidthMm: 36 },
  character: { body: "standard" as DirectorBodyCode, model: "adult_male" as DirectorModelCode, posePreset: "stand_neutral" },
  crowd: {
    cols: 3,
    jitter: false,
    model: "adult_male" as DirectorModelCode,
    posePreset: "stand_neutral",
    rows: 3,
    seed: 1,
    spacing: 1
  },
  prop: { color: "#9aa0a6", shape: "box" as DirectorPropShape, size: { x: 1, y: 1, z: 1 } },
  scene: {
    background: { color: "#060608", mode: "color" },
    ground: { height: 0, opacity: 1, visible: true },
    lights: {
      fill: { azimuth: 225, color: "#ffffff", elevation: 20, intensity: 0.5, visible: true },
      key: { azimuth: 45, color: "#ffffff", elevation: 50, intensity: 1.2, visible: true }
    },
    stage: { offset: { x: 0, y: 0, z: 0 }, rotation: { x: 0, y: 0, z: 0 }, scale: 1 },
    timeline: { durationFrames: 150, fps: 30, loop: false, tracks: [] as readonly unknown[] },
    view: {
      angleSnap: { enabled: false, step: 15 },
      gridSnap: { enabled: false, step: 0.5 },
      groundSnap: true,
      showLabels: true
    }
  }
});

/** 按码取素模；不认识的码返回 undefined。 */
export function directorModelOf(code: string): DirectorModel | undefined {
  return DIRECTOR_MODELS.find((m) => m.code === code);
}

/** 按码取画幅；不认识的码返回 undefined。 */
export function directorAspectOf(code: string): DirectorAspect | undefined {
  return DIRECTOR_ASPECTS.find((a) => a.code === code);
}
