import { Euler, Quaternion } from "three";
import { describe, expect, it } from "vitest";
import { DIRECTOR_LIMITS } from "./directorCatalog";
import {
  directorArcLengthTable,
  directorCanShrink,
  directorDeleteKeyframe,
  directorEulerFromQuat,
  directorFramesToSeconds,
  directorMoveKeyframe,
  directorParamAtLength,
  directorPoseAt,
  directorQuatFromEuler,
  directorSampleTrack,
  directorSecondsToFrames,
  directorSetInterpolation,
  directorShrinkRejectedText,
  directorSlerpEuler,
  directorTimelineErrorText,
  directorUpsertKeyframe,
  type DirectorKeyframe,
  type DirectorTimeline,
  type DirectorTimelineVec3,
  type DirectorTrack
} from "./directorTimeline";

const v = (x: number, y: number, z: number): DirectorTimelineVec3 => ({ x, y, z });
const key = (frame: number, position: DirectorTimelineVec3, rotation = v(0, 0, 0), interpolation: DirectorKeyframe["interpolation"] = "linear", extra = {}) =>
  ({ frame, interpolation, value: { position, rotation, ...extra } });
const dist = (a: DirectorTimelineVec3, b: DirectorTimelineVec3) => Math.hypot(a.x - b.x, a.y - b.y, a.z - b.z);
const track = (keyframes: DirectorKeyframe[], channel: DirectorTrack["channel"] = "camera", objectId = "cam1"): DirectorTrack => ({ objectId, channel, keyframes });
const sample = (t: DirectorTrack, frame: number) => directorSampleTrack(t, frame)!;

describe("欧拉角与四元数（与 three 同一约定）", () => {
  it("欧拉 XYZ → 四元数与 three 的 setFromEuler 一致，往返回到同一个朝向", () => {
    for (const r of [v(0, 0, 0), v(30, 45, 60), v(-120, 10, 170), v(90, 89, -45), v(0, 180, 0)]) {
      const ours = directorQuatFromEuler(r);
      const theirs = new Quaternion().setFromEuler(new Euler(r.x * Math.PI / 180, r.y * Math.PI / 180, r.z * Math.PI / 180, "XYZ"));
      expect(ours.x).toBeCloseTo(theirs.x, 10);
      expect(ours.y).toBeCloseTo(theirs.y, 10);
      expect(ours.z).toBeCloseTo(theirs.z, 10);
      expect(ours.w).toBeCloseTo(theirs.w, 10);
      // 往返：欧拉值可以换一种写法，但朝向相同（四元数点积的绝对值为 1）
      const back = directorQuatFromEuler(directorEulerFromQuat(ours));
      expect(Math.abs(back.x * ours.x + back.y * ours.y + back.z * ours.z + back.w * ours.w)).toBeCloseTo(1, 9);
      const expected = new Euler().setFromQuaternion(theirs, "XYZ");
      const got = directorEulerFromQuat(ours);
      expect(got.x).toBeCloseTo(expected.x * 180 / Math.PI, 6);
      expect(got.y).toBeCloseTo(expected.y * 180 / Math.PI, 6);
      expect(got.z).toBeCloseTo(expected.z * 180 / Math.PI, 6);
    }
  });

  it("旋转插值走最短路径：170° → -170° 的中点是 180° 而不是 0°", () => {
    const mid = directorSlerpEuler(v(0, 170, 0), v(0, -170, 0), 0.5);
    const q = directorQuatFromEuler(mid);
    const expected = directorQuatFromEuler(v(0, 180, 0));
    expect(Math.abs(q.x * expected.x + q.y * expected.y + q.z * expected.z + q.w * expected.w)).toBeCloseTo(1, 9);
    // 端点原样返回
    expect(directorSlerpEuler(v(1, 2, 3), v(4, 5, 6), 0)).toEqual(v(1, 2, 3));
    expect(directorSlerpEuler(v(1, 2, 3), v(4, 5, 6), 1)).toEqual(v(4, 5, 6));
  });
});

describe("弧长", () => {
  it("directorArcLengthTable 直线时弧长等于距离", () => {
    const a = v(0, 0, 0);
    const b = v(3, 4, 12);
    // 两端复制端点的 Catmull-Rom 退化成直线
    const table = directorArcLengthTable([a, a, b, b]);
    expect(table.total).toBeCloseTo(13, 6);
    expect(table.lengths).toHaveLength(257);
    // 反查：一半的弧长落在参数 0.5（直线上对称）
    expect(directorParamAtLength(table, 6.5)).toBeCloseTo(0.5, 6);
    expect(directorParamAtLength(table, -1)).toBe(0);
    expect(directorParamAtLength(table, 99)).toBe(1);
  });
});

describe("directorSampleTrack", () => {
  it("directorSampleTrack linear 取中点", () => {
    const t = track([key(0, v(0, 0, 0), v(0, 0, 0), "linear", { focalLengthMm: 24 }), key(90, v(10, 2, -4), v(0, 90, 0), "linear", { focalLengthMm: 50 })]);
    const s = sample(t, 45);
    expect(s.position).toEqual(v(5, 1, -2));
    expect(s.focalLengthMm).toBe(37);
    const q = directorQuatFromEuler(s.rotation);
    const expected = directorQuatFromEuler(v(0, 45, 0));
    expect(Math.abs(q.x * expected.x + q.y * expected.y + q.z * expected.z + q.w * expected.w)).toBeCloseTo(1, 9);
  });

  it("directorSampleTrack hold 保持前值", () => {
    const t = track([key(0, v(0, 0, 0), v(0, 0, 0), "hold"), key(30, v(9, 9, 9), v(0, 45, 0), "linear")]);
    expect(sample(t, 29.9).position).toEqual(v(0, 0, 0));
    expect(sample(t, 15).rotation).toEqual(v(0, 0, 0));
    // 到下一个关键帧那一帧才跳变
    expect(sample(t, 30).position).toEqual(v(9, 9, 9));
  });

  it("directorSampleTrack smooth 弧长中点误差小于1%", () => {
    // DR-AC-35：第 0 帧与第 90 帧两个关键帧，第 45 帧在路径的弧长中点
    const two = track([key(0, v(0, 1.6, 4), v(0, 0, 0), "smooth"), key(90, v(6, 1.6, -2), v(0, 60, 0), "smooth")]);
    const mid = sample(two, 45).position;
    const length = dist(v(0, 1.6, 4), v(6, 1.6, -2));
    expect(dist(mid, v(3, 1.6, 1)) / length).toBeLessThan(0.01);

    // 弯曲的路径（三个关键帧拐一个直角）：段内恒速 —— 第 45 帧走过的弧长是这一段的一半
    const bent = track([key(0, v(0, 0, 0), v(0, 0, 0), "smooth"), key(90, v(10, 0, 0), v(0, 0, 0), "smooth"), key(180, v(10, 0, 10))]);
    const points = Array.from({ length: 91 }, (_, f) => sample(bent, f).position);
    const cumulative = [0];
    for (let f = 1; f < points.length; f += 1) cumulative.push(cumulative[f - 1]! + dist(points[f - 1]!, points[f]!));
    const total = cumulative[90]!;
    expect(Math.abs(cumulative[45]! / total - 0.5)).toBeLessThan(0.01);
    // 每一帧走的距离几乎一样（不重参数化的话两端慢、中间快，这个比值会明显大于 1）
    const steps = points.slice(1).map((p, f) => dist(points[f]!, p));
    expect(Math.max(...steps) / Math.min(...steps)).toBeLessThan(1.02);
    // 关键帧那一帧正好在关键帧的位置上，而且路径真的是弯的（不是直线）
    expect(dist(sample(bent, 90).position, v(10, 0, 0))).toBeLessThan(1e-9);
    expect(sample(bent, 45).position.z).not.toBeCloseTo(0, 3);
  });

  it("directorSampleTrack 首帧之前与末帧之后取端点", () => {
    const t = track([key(30, v(1, 1, 1)), key(60, v(2, 2, 2))]);
    expect(sample(t, 0).position).toEqual(v(1, 1, 1));
    expect(sample(t, 400).position).toEqual(v(2, 2, 2));
    // 只有一个关键帧时恒等于它；没有关键帧时为 null
    const one = track([key(10, v(5, 0, 0))]);
    expect(sample(one, 0).position).toEqual(v(5, 0, 0));
    expect(sample(one, 99).position).toEqual(v(5, 0, 0));
    expect(directorSampleTrack(track([]), 0)).toBeNull();
    // 关键帧顺序乱也照帧号算
    const shuffled = track([key(60, v(2, 2, 2)), key(30, v(1, 1, 1))]);
    expect(sample(shuffled, 45).position).toEqual(v(1.5, 1.5, 1.5));
  });
});

describe("directorPoseAt", () => {
  const poses = (code: string) => ({
    stand: { bones: { LeftArm: v(40, 0, 0), RightArm: v(40, 0, 0) } },
    wave: { bones: { LeftArm: v(0, 0, 120) }, lift: 0.1 }
  } as Record<string, { bones: Record<string, DirectorTimelineVec3>; lift?: number }>)[code];
  const poseKey = (frame: number, posePreset: string, boneOverrides?: Record<string, DirectorTimelineVec3>, interpolation: DirectorKeyframe["interpolation"] = "smooth") =>
    ({ frame, interpolation, value: { posePreset, ...(boneOverrides ? { boneOverrides } : {}) } });

  it("directorPoseAt 两个姿势中间逐骨骼插值", () => {
    const t = track([poseKey(0, "stand"), poseKey(60, "wave", { RightArm: v(10, 0, 0) })], "pose", "char1");
    const mid = directorPoseAt(t, 30, poses)!;
    // 两边预设不同：都先展开成完整骨骼值再插值；中间那一刻不属于任何一个预设
    expect(mid.posePreset).toBeUndefined();
    expect(mid.bones.RightArm!.x).toBeCloseTo(25, 6);
    const left = directorQuatFromEuler(mid.bones.LeftArm!);
    const expected = directorQuatFromEuler(directorSlerpEuler(v(40, 0, 0), v(0, 0, 120), 0.5));
    expect(Math.abs(left.x * expected.x + left.y * expected.y + left.z * expected.z + left.w * expected.w)).toBeCloseTo(1, 9);
    expect(mid.lift).toBeCloseTo(0.05, 9);
    // 端点就是关键帧本身（覆盖值取代预设值，不叠加）
    const end = directorPoseAt(t, 60, poses)!;
    expect(end).toEqual({ posePreset: "wave", bones: { LeftArm: v(0, 0, 120), RightArm: v(10, 0, 0) }, lift: 0.1 });
  });

  it("hold 段保持段首姿势；没有关键帧时为 null", () => {
    const t = track([poseKey(0, "stand", undefined, "hold"), poseKey(60, "wave")], "pose", "char1");
    expect(directorPoseAt(t, 59, poses)?.posePreset).toBe("stand");
    expect(directorPoseAt(track([], "pose"), 0, poses)).toBeNull();
  });
});

describe("关键帧编辑", () => {
  const base = (): DirectorTimeline => ({
    fps: 30,
    durationFrames: 150,
    loop: false,
    tracks: [track([key(0, v(0, 0, 0)), key(90, v(1, 0, 0))])]
  });

  it("directorCanShrink 拒绝并返回范围外数量", () => {
    // DR-AC-35：第 90 帧有关键帧，缩到 2 秒（60 帧）被拒，范围外 1 个
    expect(directorCanShrink(base(), directorSecondsToFrames(2))).toEqual({ ok: false, outside: 1 });
    expect(directorShrinkRejectedText(1, 60)).toBe("有 1 个关键帧落在 2 秒之后，请先删除或移动它们");
    expect(directorCanShrink(base(), 90)).toEqual({ ok: true, outside: 0 });
    expect(directorCanShrink({}, 30)).toEqual({ ok: true, outside: 0 });
  });

  it("directorUpsertKeyframe 同帧覆盖", () => {
    const result = directorUpsertKeyframe(base(), "cam1", "camera", 90, { position: v(5, 5, 5), rotation: v(0, 0, 0) });
    expect("timeline" in result).toBe(true);
    if (!("timeline" in result)) return;
    const keys = result.timeline.tracks![0]!.keyframes;
    expect(keys).toHaveLength(2);
    expect(keys[1]).toEqual({ frame: 90, interpolation: "linear", value: { position: v(5, 5, 5), rotation: v(0, 0, 0) } });
    // 新对象 / 新通道：建一条轨道，帧号取整并夹在时长之内；入参不变（撤销栈按快照工作）
    const original = base();
    const added = directorUpsertKeyframe(original, "char1", "transform", 999.6, { position: v(0, 0, 0), rotation: v(0, 0, 0) }, "smooth");
    if (!("timeline" in added)) throw new Error("应当成功");
    expect(added.timeline.tracks).toHaveLength(2);
    expect(added.timeline.tracks![1]).toEqual({ objectId: "char1", channel: "transform", keyframes: [{ frame: 150, interpolation: "smooth", value: { position: v(0, 0, 0), rotation: v(0, 0, 0) } }] });
    expect(original.tracks).toHaveLength(1);
  });

  it("directorUpsertKeyframe 超上限拒绝", () => {
    const full = track(Array.from({ length: DIRECTOR_LIMITS.maxKeyframesPerTrack }, (_, i) => key(i, v(i, 0, 0))));
    const timeline: DirectorTimeline = { durationFrames: 450, tracks: [full] };
    expect(directorUpsertKeyframe(timeline, "cam1", "camera", 300, { position: v(0, 0, 0), rotation: v(0, 0, 0) }))
      .toEqual({ error: "limit", what: "track", limit: DIRECTOR_LIMITS.maxKeyframesPerTrack });
    // 覆盖已有的那一帧不受上限影响
    expect("timeline" in directorUpsertKeyframe(timeline, "cam1", "camera", 5, { position: v(0, 0, 0), rotation: v(0, 0, 0) })).toBe(true);
    // 全场景上限
    const many = Array.from({ length: Math.ceil(DIRECTOR_LIMITS.maxKeyframes / 100) }, (_, t) =>
      track(Array.from({ length: 100 }, (_, i) => key(i, v(0, 0, 0))), "transform", `obj${t}`));
    const crowded: DirectorTimeline = { durationFrames: 450, tracks: many };
    const rejected = directorUpsertKeyframe(crowded, "objX", "transform", 0, { position: v(0, 0, 0), rotation: v(0, 0, 0) });
    expect(rejected).toEqual({ error: "limit", what: "scene", limit: DIRECTOR_LIMITS.maxKeyframes });
    expect(directorTimelineErrorText(rejected as never)).toBe(`整个场景的关键帧已到上限（${DIRECTOR_LIMITS.maxKeyframes} 个）`);
  });

  it("directorMoveKeyframe 不与已有帧重合", () => {
    expect(directorMoveKeyframe(base(), "cam1", "camera", 90, 0)).toEqual({ error: "occupied", frame: 0 });
    const moved = directorMoveKeyframe(base(), "cam1", "camera", 90, 45);
    if (!("timeline" in moved)) throw new Error("应当成功");
    expect(moved.timeline.tracks![0]!.keyframes.map(k => k.frame)).toEqual([0, 45]);
    // 夹在 [0, durationFrames] 并取整；挪到原地不产生变化
    const clamped = directorMoveKeyframe(base(), "cam1", "camera", 90, 400.2);
    if (!("timeline" in clamped)) throw new Error("应当成功");
    expect(clamped.timeline.tracks![0]!.keyframes.map(k => k.frame)).toEqual([0, 150]);
    const same = base();
    expect(directorMoveKeyframe(same, "cam1", "camera", 90, 90)).toEqual({ timeline: same });
    expect(directorMoveKeyframe(base(), "cam1", "camera", 7, 8)).toEqual({ error: "missing" });
  });

  it("删除最后一个关键帧时连轨道一起去掉；改插值方式", () => {
    const one = directorDeleteKeyframe(base(), "cam1", "camera", 0);
    if (!("timeline" in one)) throw new Error("应当成功");
    expect(one.timeline.tracks![0]!.keyframes.map(k => k.frame)).toEqual([90]);
    const none = directorDeleteKeyframe(one.timeline, "cam1", "camera", 90);
    if (!("timeline" in none)) throw new Error("应当成功");
    expect(none.timeline.tracks).toEqual([]);
    const smooth = directorSetInterpolation(base(), "cam1", "camera", 0, "smooth");
    if (!("timeline" in smooth)) throw new Error("应当成功");
    expect(smooth.timeline.tracks![0]!.keyframes[0]!.interpolation).toBe("smooth");
    expect(directorSetInterpolation(base(), "cam1", "camera", 1, "hold")).toEqual({ error: "missing" });
  });

  it("帧与秒（固定 30 fps）", () => {
    expect(directorFramesToSeconds(45)).toBe(1.5);
    expect(directorSecondsToFrames(1.5)).toBe(45);
    expect(directorShrinkRejectedText(2, 45)).toBe("有 2 个关键帧落在 1.5 秒之后，请先删除或移动它们");
  });
});
