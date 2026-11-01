/**
 * 首批 36 个姿势的角度表（DRT-10，DR-F-42，DR-PARAM-15）。本仓库自编：不取自任何第三方姿势库
 * （Storyboarder / Shot Generator 的姿势库是 EULA，没有开源许可，DRR-2）。
 *
 * 由 `node scripts/director-assets.mjs poses` 编译成 `public/director/poses.json`：
 * 这里按**部位 + 解剖动作**写（「手臂放下 38°、向前 4°」），编译时换算成每根骨骼相对 rest 的局部欧拉角
 * （度，XYZ，口径见 utils/directorRig.ts）。直接手写局部欧拉角的话，同一个「向前抬手」在左右两侧符号相反，
 * 而且转动顺序一变意思就变，评审时看不出写得对不对。
 *
 * 写法：
 * - 中轴部位（hips / spine / head）直接写；
 * - 四肢部位写在顶层 = 左右对称（左侧照写，右侧按镜像规则生成）；只有左右不同时写进 `l` / `r`，
 *   `l` / `r` 里的同名部位覆盖顶层那一份；
 * - 未写的部位保持 rest（A 字姿势：手臂向外下方约 45°）；
 * - `lift`：腾空姿势离地的高度，占身高的比例。其余姿势一律「蒙皮后最低点落在地面」
 *   （坐、跪、躺都是这样落地的，DRT-12 套姿势时照做）。
 *
 * 部位与方向（全部是度，正方向如下；左右两侧的解剖含义相同）：
 * | 部位 | 参数 |
 * | --- | --- |
 * | hips（骨盆，带动全身） | yaw 向左转 · pitch 向前俯 · roll 向右侧倒 |
 * | spine（三节脊柱平分） | turn 向左扭 · bend 向前弯 · tilt 向右侧弯 |
 * | head（颈 40% + 头 60%） | turn 向左转 · nod 低头 · tilt 向右歪 |
 * | shoulder（锁骨） | raise 耸肩 · forward 前探 |
 * | arm（上臂，先放下、再前摆、最后内旋） | lower 从 rest 往下放（45 ≈ 贴身下垂，-45 = 水平侧平举，-135 = 竖直上举）· forward 向前摆 · twist 内旋 |
 * | forearm（前臂） | flex 屈肘 · twist 内旋（掌心转向下 / 向后） |
 * | hand（手腕） | flex 向掌心屈 · deviate 向拇指侧偏 · twist 内旋 |
 * | fingers（四指 + 拇指） | curl 握拳程度 0~1 · thumb 拇指内收 0~1 · point 食指伸直 |
 * | leg（大腿，先前抬、再外展、最后内旋） | flex 向前抬 · abduct 向外张 · twist 内旋（脚尖向内） |
 * | knee（膝） | flex 屈膝 |
 * | foot（踝） | flex 勾脚尖（负值 = 绷脚尖） |
 * | toe（脚趾） | flex 向上翘 |
 */
import { Euler, MathUtils, Quaternion, Vector3 } from "three";

const STAND = { arm: { lower: 38, forward: 4 }, forearm: { flex: 10 }, fingers: { curl: 0.15 }, leg: { abduct: 3 } };

export const POSE_TABLE = {
  // ---------------------------------------------------------------- 站立
  stand_neutral: { ...STAND },
  stand_arms_crossed: {
    // 左前臂在下、贴着胸口；右前臂在上、更靠前，两条前臂上下错开不穿插；手掌扣住对侧上臂。
    hand: { flex: 35 },
    fingers: { curl: 0.5 },
    leg: { abduct: 3 },
    l: { arm: { lower: 35, forward: 31, twist: 75 }, forearm: { flex: 70 } },
    r: { arm: { lower: 36, forward: 43, twist: 68 }, forearm: { flex: 69 } }
  },
  stand_hands_on_hips: {
    // 手肘向外、略向后，手腕落在髂骨两侧。
    arm: { lower: 1, forward: -30, twist: 69 },
    forearm: { flex: 62 },
    hand: { flex: -20 },
    fingers: { curl: 0.2 },
    leg: { abduct: 4 }
  },
  stand_hands_in_pockets: {
    // 手腕落在裤兜口，手掌向下插进兜里（素模没有衣服，手掌没入大腿前侧就是插兜的样子）。
    shoulder: { raise: 5 },
    arm: { lower: 38, forward: -10, twist: 25 },
    forearm: { flex: 27 },
    hand: { flex: 15 },
    fingers: { curl: 0.5 },
    leg: { abduct: 3 }
  },
  stand_hands_behind: {
    // 上臂向后、内旋，屈肘把两只手带到腰后交叠。
    arm: { lower: 34, forward: -30, twist: 93 },
    forearm: { flex: 61 },
    fingers: { curl: 0.4 },
    leg: { abduct: 2 }
  },
  stand_look_back: {
    ...STAND,
    spine: { turn: 40 },
    head: { turn: 60, nod: 5 }
  },

  // ---------------------------------------------------------------- 行走奔跑
  walk: {
    spine: { turn: -5 },
    forearm: { flex: 20 },
    fingers: { curl: 0.2 },
    l: { arm: { lower: 38, forward: -20 }, leg: { flex: 25 }, knee: { flex: 10 }, foot: { flex: 10 } },
    r: { arm: { lower: 38, forward: 20 }, leg: { flex: -15 }, knee: { flex: 20 }, foot: { flex: -10 } }
  },
  walk_fast: {
    spine: { bend: 8, turn: -8 },
    forearm: { flex: 50 },
    fingers: { curl: 0.35 },
    l: { arm: { lower: 38, forward: -35 }, leg: { flex: 35 }, knee: { flex: 15 }, foot: { flex: 10 } },
    r: { arm: { lower: 38, forward: 35 }, leg: { flex: -20 }, knee: { flex: 30 }, foot: { flex: -15 } }
  },
  run: {
    spine: { bend: 15, turn: -10 },
    forearm: { flex: 90 },
    fingers: { curl: 0.6 },
    l: { arm: { lower: 38, forward: -35 }, leg: { flex: 55 }, knee: { flex: 80 }, foot: { flex: 0 } },
    r: { arm: { lower: 38, forward: 50 }, leg: { flex: -25 }, knee: { flex: 60 }, foot: { flex: -25 } }
  },
  sprint: {
    hips: { pitch: 10 },
    spine: { bend: 20, turn: -12 },
    head: { nod: -15 },
    forearm: { flex: 90 },
    fingers: { curl: 0.7 },
    l: { arm: { lower: 38, forward: -60 }, leg: { flex: 75 }, knee: { flex: 100 }, foot: { flex: 5 } },
    r: { arm: { lower: 38, forward: 70 }, leg: { flex: -30 }, knee: { flex: 40 }, foot: { flex: -35 } }
  },
  jump: {
    arm: { lower: -100, forward: 30 },
    forearm: { flex: 15 },
    fingers: { curl: 0.1 },
    leg: { flex: 20 },
    knee: { flex: 35 },
    foot: { flex: -30 },
    head: { nod: -10 },
    lift: 0.15
  },
  sneak: {
    hips: { pitch: 25 },
    spine: { bend: 20 },
    head: { nod: -35 },
    arm: { lower: 20, forward: 40 },
    forearm: { flex: 70 },
    hand: { flex: 20 },
    fingers: { curl: 0.2 },
    l: { leg: { flex: 55 }, knee: { flex: 70 } },
    r: { leg: { flex: 15 }, knee: { flex: 55 }, foot: { flex: -10 } }
  },

  // ---------------------------------------------------------------- 坐卧
  sit_chair: {
    // 大腿水平、小腿竖直，手掌搭在膝盖上方的大腿上。
    spine: { bend: 5 },
    arm: { lower: 49, forward: 42, twist: 16 },
    forearm: { flex: -13 },
    fingers: { curl: 0.2 },
    leg: { flex: 90, abduct: 4 },
    knee: { flex: 90 }
  },
  sit_floor_cross: {
    // 大腿外展并外旋 90°，屈膝时小腿在水平面里向内收，两条小腿在身前交叉、臀部着地；脚背绷直免得脚尖戳进地面。
    arm: { lower: 20, forward: 35 },
    forearm: { flex: -13 },
    fingers: { curl: 0.2 },
    leg: { flex: 90, abduct: 45, twist: -90 },
    knee: { flex: 130 },
    foot: { flex: -40 }
  },
  squat: {
    // 前臂搭在膝盖上，手垂在膝前。
    spine: { bend: 20 },
    arm: { lower: 30, forward: 72, twist: 46 },
    forearm: { flex: 10 },
    fingers: { curl: 0.2 },
    leg: { flex: 120, abduct: 15 },
    knee: { flex: 140 },
    foot: { flex: 30 }
  },
  kneel_one: {
    ...STAND,
    l: { leg: { flex: 90 }, knee: { flex: 90 } },
    r: { leg: { flex: -5 }, knee: { flex: 90 }, toe: { flex: 60 } }
  },
  lie_back: {
    // rest 的前臂自带约 15° 弯曲，躺平时要伸直，否则两只手会翘离地面。
    hips: { pitch: -90 },
    arm: { lower: 40 },
    forearm: { flex: -15 },
    fingers: { curl: 0.2 }
  },
  lie_side: {
    hips: { roll: 90 },
    leg: { flex: 30 },
    knee: { flex: 40 },
    fingers: { curl: 0.2 },
    l: { arm: { lower: 40, forward: 20 }, forearm: { flex: 10 }, leg: { flex: 40 } },
    r: { arm: { lower: -120 }, forearm: { flex: 90 } }
  },

  // ---------------------------------------------------------------- 手势交流
  wave: {
    ...STAND,
    r: { arm: { lower: -75, forward: 20, twist: -80 }, forearm: { flex: 45 }, fingers: { curl: 0 } }
  },
  point_forward: {
    ...STAND,
    r: { arm: { lower: 72, forward: 80, twist: 30 }, forearm: { flex: -14 }, fingers: { curl: 1, thumb: 1, point: true } }
  },
  phone_call: {
    ...STAND,
    head: { tilt: 8 },
    r: { arm: { lower: 31, forward: 47, twist: 20 }, forearm: { flex: 134 }, fingers: { curl: 0.4 } }
  },
  thinking: {
    // 左前臂横在胸下，右肘支在左手上，右手托住下巴。
    ...STAND,
    head: { nod: 10 },
    l: { arm: { lower: 33, forward: 25, twist: 73 }, forearm: { flex: 74 }, fingers: { curl: 0.4 } },
    r: { arm: { lower: 51, forward: 11, twist: 41 }, forearm: { flex: 103 }, fingers: { curl: 0.6 } }
  },
  cheer: {
    arm: { lower: -125, forward: 5 },
    forearm: { flex: 10 },
    fingers: { curl: 0 },
    leg: { abduct: 4 },
    head: { nod: -10 }
  },
  bow: {
    // 骨盆前俯 30° 带着上身弯下去，大腿前抬同样的角度把双腿留在竖直位置。
    hips: { pitch: 30 },
    spine: { bend: 15 },
    head: { nod: 10 },
    arm: { lower: 40 },
    fingers: { curl: 0.1 },
    leg: { flex: 30 }
  },

  // ---------------------------------------------------------------- 动作打斗
  fight_stance: {
    // 左脚在前；双拳护在下巴前，左拳在前、右拳靠后。
    spine: { bend: 5 },
    head: { nod: 10 },
    fingers: { curl: 1, thumb: 1 },
    l: { arm: { lower: 42, forward: 41, twist: 25 }, forearm: { flex: 86 }, leg: { flex: 20 }, knee: { flex: 20 } },
    r: { arm: { lower: 38, forward: 26, twist: 28 }, forearm: { flex: 107 }, leg: { flex: -15 }, knee: { flex: 25 } }
  },
  punch: {
    // 右拳直拳打出，左拳收在下巴前；上身随出拳向左转。
    spine: { turn: 20, bend: 5 },
    fingers: { curl: 1, thumb: 1 },
    l: { arm: { lower: 42, forward: 41, twist: 25 }, forearm: { flex: 86 }, leg: { flex: 30 }, knee: { flex: 30 } },
    r: { arm: { lower: 72, forward: 80, twist: 30 }, forearm: { flex: -14 }, leg: { flex: -20 }, knee: { flex: 5 } }
  },
  kick_side: {
    // 左腿支撑，右腿向右侧伸直踢出，上身向左侧倾保持平衡。
    spine: { tilt: -25 },
    fingers: { curl: 1, thumb: 1 },
    l: { arm: { lower: 42, forward: 41, twist: 25 }, forearm: { flex: 86 } },
    r: { arm: { lower: 38, forward: 26, twist: 28 }, forearm: { flex: 107 }, leg: { abduct: 75 }, foot: { flex: -20 } }
  },
  flying_kick: {
    spine: { bend: -10 },
    fingers: { curl: 1, thumb: 1 },
    l: { arm: { lower: 42, forward: 41, twist: 25 }, forearm: { flex: 86 }, leg: { flex: 85 }, foot: { flex: -20 } },
    r: { arm: { lower: 38, forward: 26, twist: 28 }, forearm: { flex: 107 }, leg: { flex: 40 }, knee: { flex: 110 } },
    lift: 0.3
  },
  block: {
    // 两条前臂竖在脸前。
    arm: { lower: 65, forward: 61, twist: 31 },
    forearm: { flex: 97 },
    fingers: { curl: 1, thumb: 1 },
    leg: { flex: 15, abduct: 5 },
    knee: { flex: 25 }
  },
  fall_back: {
    hips: { pitch: -35 },
    spine: { bend: -15 },
    head: { nod: 20 },
    arm: { lower: -50, forward: -10 },
    forearm: { flex: 20 },
    fingers: { curl: 0 },
    l: { leg: { flex: 40 }, knee: { flex: 20 } },
    r: { leg: { flex: 15 }, knee: { flex: 20 } }
  },

  // ---------------------------------------------------------------- 互动
  handshake: {
    ...STAND,
    r: { arm: { lower: 40, forward: 12, twist: 9 }, forearm: { flex: 49 }, fingers: { curl: 0.3 } }
  },
  hug: {
    // 双臂向前张开、在身前合拢，中间留出一个人的厚度。
    spine: { bend: 10 },
    arm: { lower: -14, forward: 51, twist: 21 },
    forearm: { flex: 72 },
    fingers: { curl: 0.3 },
    leg: { abduct: 3 }
  },
  hold_hands: {
    ...STAND,
    r: { arm: { lower: 20 }, forearm: { flex: 0 }, fingers: { curl: 0.4 } }
  },
  push: {
    // 双臂向前推到肩高，手腕上翻、掌心朝前。
    spine: { bend: 15 },
    arm: { lower: -2, forward: 76, twist: -2 },
    forearm: { flex: 18 },
    hand: { flex: -70 },
    fingers: { curl: 0 },
    l: { leg: { flex: 35 }, knee: { flex: 40 } },
    r: { leg: { flex: -20 }, knee: { flex: 5 } }
  },
  hand_over: {
    arm: { lower: 43, forward: 22, twist: 20 },
    forearm: { flex: 45 },
    fingers: { curl: 0.3 },
    leg: { abduct: 3 }
  },
  carry_box: {
    // 上臂下垂，前臂水平向前，双手在身前托住箱底；上身微微后仰。
    spine: { bend: -5 },
    arm: { lower: 39, forward: 4, twist: 3 },
    forearm: { flex: 54 },
    fingers: { curl: 0.3 },
    leg: { abduct: 4 }
  },
};

// ---------------------------------------------------------------- 编译

const AXES = { x: new Vector3(1, 0, 0), y: new Vector3(0, 1, 0), z: new Vector3(0, 0, 1) };
const LIMB_PARTS = ["shoulder", "arm", "forearm", "hand", "fingers", "leg", "knee", "foot", "toe"];
const FINGER_CURL = { 1: 70, 2: 95, 3: 60 };

/** 依次绕（已转过的）局部轴转动，角度为 0 的步骤跳过。 */
function turns(...steps) {
  const out = new Quaternion();
  for (const [axis, deg] of steps) {
    if (deg) out.multiply(new Quaternion().setFromAxisAngle(AXES[axis], MathUtils.degToRad(deg)));
  }
  return out;
}

const n = (v) => v ?? 0;

/** 中轴部位 → { 骨骼名: 四元数 }。 */
const CENTER = {
  hips: (p) => ({ Hips: turns(["y", n(p.yaw)], ["x", n(p.pitch)], ["z", n(p.roll)]) }),
  spine: (p) =>
    Object.fromEntries(["Spine", "Spine1", "Spine2"].map((b) => [b, turns(["y", n(p.turn) / 3], ["x", n(p.bend) / 3], ["z", n(p.tilt) / 3])])),
  head: (p) => ({
    Neck: turns(["y", 0.4 * n(p.turn)], ["x", 0.4 * n(p.nod)], ["z", 0.4 * n(p.tilt)]),
    Head: turns(["y", 0.6 * n(p.turn)], ["x", 0.6 * n(p.nod)], ["z", 0.6 * n(p.tilt)])
  })
};

/** 四肢部位（按左侧骨骼的局部轴）→ { 去掉 Left 前缀的骨骼名: 四元数 }。 */
const LIMB = {
  shoulder: (p) => ({ Shoulder: turns(["x", -n(p.raise)], ["z", n(p.forward)]) }),
  arm: (p) => ({ Arm: turns(["x", n(p.lower)], ["z", n(p.forward)], ["y", n(p.twist)]) }),
  forearm: (p) => ({ ForeArm: turns(["z", n(p.flex)], ["y", n(p.twist)]) }),
  hand: (p) => ({ Hand: turns(["x", n(p.flex)], ["z", n(p.deviate)], ["y", n(p.twist)]) }),
  fingers: (p) => {
    const out = {};
    for (const finger of ["Index", "Middle", "Ring", "Pinky"]) {
      const curl = finger === "Index" && p.point ? 0 : n(p.curl);
      for (const k of [1, 2, 3]) out[`Hand${finger}${k}`] = turns(["x", FINGER_CURL[k] * curl]);
    }
    const thumb = n(p.thumb);
    out.HandThumb1 = turns(["z", 25 * thumb]);
    out.HandThumb2 = turns(["x", 25 * thumb]);
    out.HandThumb3 = turns(["x", 35 * thumb]);
    return out;
  },
  leg: (p) => ({ UpLeg: turns(["x", n(p.flex)], ["z", n(p.abduct)], ["y", n(p.twist)]) }),
  knee: (p) => ({ Leg: turns(["x", -n(p.flex)]) }),
  foot: (p) => ({ Foot: turns(["x", n(p.flex)]) }),
  toe: (p) => ({ ToeBase: turns(["x", n(p.flex)]) })
};

const round = (deg) => Math.round(deg * 10) / 10 + 0;

function toEuler(q) {
  const e = new Euler().setFromQuaternion(q, "XYZ");
  return [e.x, e.y, e.z].map((r) => round(MathUtils.radToDeg(r)));
}

/** 把一个姿势编译成 { 标准骨骼名: [x, y, z] }（相对 rest 的局部欧拉角，度，XYZ）。只输出不是 0 的骨骼。 */
export function compilePose(spec) {
  const bones = {};
  const put = (name, q) => {
    const e = toEuler(q);
    if (e.some((v) => v !== 0)) bones[name] = e;
  };
  for (const [part, build] of Object.entries(CENTER)) {
    if (spec[part]) for (const [bone, q] of Object.entries(build(spec[part]))) put(bone, q);
  }
  for (const side of ["Left", "Right"]) {
    const own = side === "Left" ? spec.l : spec.r;
    for (const part of LIMB_PARTS) {
      const params = own?.[part] ?? spec[part];
      if (!params) continue;
      for (const [bone, q] of Object.entries(LIMB[part](params))) {
        // 右侧 = 左侧的镜像：(x, y, z) → (x, −y, −z)（utils/directorRig.ts 的 directorMirrorBones 同一条规则）。
        const [x, y, z] = toEuler(q);
        const e = side === "Left" ? [x, y, z] : [x, round(-y), round(-z)];
        if (e.some((v) => v !== 0)) bones[`${side}${bone}`] = e;
      }
    }
  }
  return bones;
}

export function compilePoses() {
  return Object.fromEntries(
    Object.entries(POSE_TABLE).map(([code, spec]) => [code, { bones: compilePose(spec), lift: spec.lift }])
  );
}
