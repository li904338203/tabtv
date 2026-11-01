import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import {
  DIRECTOR_SAMPLED_LIFT,
  directorAutoKeyframe,
  directorDisplayedObject,
  directorEditTimeline,
  directorKeyObjectAt,
  directorMotionChannel,
  directorProjectObject,
  directorSetTimelineSettings,
  directorTimelineOf,
  directorTimelineRows,
  directorTrackOf
} from "./directorAnimation";
import { directorFindObject, directorSetTransform, directorValidateScene, type DirectorScene } from "./directorScene";
import { directorFrameAtX, directorFrameTimeText, directorKeyframeLabel, directorMoveKeyframe, directorTimelineTicks } from "./directorTimeline";

const DEFAULT_SCENE = JSON.parse(readFileSync(fileURLToPath(new URL("../../api/fixtures/director/scenes/valid-default.json", import.meta.url)), "utf8")) as DirectorScene;
const CAM = "obj_camera_1";
const HERO = "obj_character_1";
const at = (x: number, y = 1.5, z = 4) => ({ position: { x, y, z }, rotation: { x: 0, y: 0, z: 0 } });
const poses = (code: string) => ({
  sit_chair: { bones: { LeftUpLeg: { x: -90, y: 0, z: 0 } } },
  jump: { bones: { LeftLeg: { x: 40, y: 0, z: 0 } }, lift: 0.15 }
} as Record<string, { bones: Record<string, { x: number; y: number; z: number }>; lift?: number }>)[code];

function withCameraTrack(scene: DirectorScene = DEFAULT_SCENE): DirectorScene {
  return {
    ...scene,
    timeline: {
      fps: 30, durationFrames: 150, loop: false,
      tracks: [{ objectId: CAM, channel: "camera", keyframes: [
        { frame: 0, interpolation: "linear", value: { ...at(0), focalLengthMm: 35 } },
        { frame: 90, interpolation: "linear", value: { ...at(3), focalLengthMm: 50 } }
      ] }]
    }
  };
}

describe("对象在某一帧的样子（DRT-24）", () => {
  it("运动通道：机位是 camera，其余是 transform", () => {
    expect(directorMotionChannel("camera")).toBe("camera");
    expect(directorMotionChannel("character")).toBe("transform");
    expect(directorMotionChannel("prop")).toBe("transform");
  });

  it("有轨道的部分换成采样值（位置、朝向、焦距），其余原样", () => {
    const scene = withCameraTrack();
    const shown = directorDisplayedObject(scene, CAM, 45)!;
    expect(shown.transform.position).toEqual({ x: 1.5, y: 1.5, z: 4 });
    expect((shown.camera as { focalLengthMm: number }).focalLengthMm).toBeCloseTo(42.5, 6);
    // 没有轨道的对象就是基础值
    expect(directorDisplayedObject(scene, HERO, 45)).toEqual(directorFindObject(scene, HERO));
  });

  it("姿势轨道：两个姿势之间逐骨骼插值，离地比例也插值（只在投影里）", () => {
    const hero = directorFindObject(DEFAULT_SCENE, HERO)!;
    const tracks = [{ objectId: HERO, channel: "pose" as const, keyframes: [
      { frame: 0, value: { posePreset: "sit_chair", boneOverrides: {} } },
      { frame: 30, value: { posePreset: "jump", boneOverrides: {} } }
    ] }];
    const mid = directorProjectObject(hero, tracks, 15, poses).character as Record<string, unknown>;
    expect(mid.posePreset).toBe("");
    expect((mid.boneOverrides as Record<string, { x: number }>).LeftUpLeg.x).toBeCloseTo(-45, 6);
    expect((mid.boneOverrides as Record<string, { x: number }>).LeftLeg.x).toBeCloseTo(20, 6);
    expect(mid[DIRECTOR_SAMPLED_LIFT]).toBeCloseTo(0.075, 6);
    // 正好落在关键帧上：带预设码
    expect((directorProjectObject(hero, tracks, 30, poses).character as Record<string, unknown>).posePreset).toBe("jump");
  });
});

describe("自动打帧（DRT-24）", () => {
  it("有轨道的对象被改了：在当前帧写关键帧；没有轨道的只改基础值", () => {
    const before = withCameraTrack();
    const after = directorSetTransform(directorSetTransform(before, CAM, { ...at(1, 2, 3), scale: { x: 1, y: 1, z: 1 } }), HERO, {
      position: { x: 0.5, y: 0, z: 0 }, rotation: { x: 0, y: 0, z: 0 }, scale: { x: 1, y: 1, z: 1 }
    });
    const result = directorAutoKeyframe(before, after, 45);
    expect(result.keyed).toBe(1);
    const track = directorTrackOf(result.scene, CAM, "camera")!;
    expect(track.keyframes.map(k => k.frame)).toEqual([0, 45, 90]);
    expect(track.keyframes[1]!.value).toMatchObject({ position: { x: 1, y: 2, z: 3 }, focalLengthMm: 35 });
    // 角色没有轨道：不建轨道
    expect(directorTrackOf(result.scene, HERO, "transform")).toBeUndefined();
    expect(directorValidateScene(result.scene).ok).toBe(true);
  });

  it("同一帧已有关键帧时覆盖；改的只是轨道本身（挪关键帧）时不打帧", () => {
    const before = withCameraTrack();
    const after = directorSetTransform(before, CAM, { ...at(9), scale: { x: 1, y: 1, z: 1 } });
    const result = directorAutoKeyframe(before, after, 90);
    expect(directorTrackOf(result.scene, CAM, "camera")!.keyframes.map(k => [k.frame, (k.value as { position: { x: number } }).position.x])).toEqual([[0, 0], [90, 9]]);
    const moved = directorEditTimeline(before, t => directorMoveKeyframe(t, CAM, "camera", 90, 60));
    if ("error" in moved) throw new Error("挪关键帧失败");
    expect(directorAutoKeyframe(before, moved.scene, 30).keyed).toBe(0);
  });

  it("没有任何轨道时原样返回同一个场景", () => {
    const after = directorSetTransform(DEFAULT_SCENE, CAM, { ...at(1), scale: { x: 1, y: 1, z: 1 } });
    expect(directorAutoKeyframe(DEFAULT_SCENE, after, 10).scene).toBe(after);
  });
});

describe("打关键帧与时间轴设置（DRT-24）", () => {
  it("在当前帧打关键帧记录显示的状态（已有轨道时是采样值）", () => {
    const scene = withCameraTrack();
    const keyed = directorKeyObjectAt(scene, CAM, "camera", 30);
    if ("error" in keyed) throw new Error("打关键帧失败");
    const k = directorTrackOf(keyed.scene, CAM, "camera")!.keyframes.find(item => item.frame === 30)!;
    expect((k.value as { position: { x: number } }).position.x).toBeCloseTo(1, 6);
    // 没有轨道时新建一条，记基础值
    const hero = directorKeyObjectAt(DEFAULT_SCENE, HERO, "transform", 0);
    if ("error" in hero) throw new Error("打关键帧失败");
    expect(directorTrackOf(hero.scene, HERO, "transform")!.keyframes).toHaveLength(1);
    // 通道与对象不匹配（角色没有 camera 通道）
    expect(directorKeyObjectAt(DEFAULT_SCENE, HERO, "camera", 0)).toEqual({ error: "missing" });
  });

  it("改总时长与循环；值没变时返回同一个场景", () => {
    const scene = directorSetTimelineSettings(DEFAULT_SCENE, { durationFrames: 60, loop: true });
    expect(directorTimelineOf(scene)).toMatchObject({ durationFrames: 60, loop: true });
    expect(directorSetTimelineSettings(scene, { durationFrames: 60 })).toBe(scene);
  });
});

describe("时间轴界面（DRT-24）", () => {
  it("行：有关键帧的对象与选中的对象；机位一行，角色两行（位置、姿势）；机位在前", () => {
    const scene = withCameraTrack();
    expect(directorTimelineRows(scene, null).map(r => r.label)).toEqual(["机位 1"]);
    expect(directorTimelineRows(scene, HERO).map(r => [r.label, r.channel, r.keyframes.length])).toEqual([
      ["机位 1", "camera", 2], ["角色 1 · 位置", "transform", 0], ["角色 1 · 姿势", "pose", 0]
    ]);
  });

  it("刻度、帧号换算与文案", () => {
    expect(directorTimelineTicks(60).map(t => [t.frame, t.major, t.label])).toEqual([[0, true, "0s"], [15, false, ""], [30, true, "1s"], [45, false, ""], [60, true, "2s"]]);
    expect(directorFrameAtX(181, 4, 150)).toBe(45);
    expect(directorFrameAtX(-20, 4, 150)).toBe(0);
    expect(directorFrameAtX(10000, 4, 150)).toBe(150);
    expect(directorFrameTimeText(45)).toBe("帧 45 · 1.50 s");
    expect(directorKeyframeLabel("机位 1", 90, "smooth")).toBe("机位 1 · 第 90 帧 · 平滑");
    expect(directorKeyframeLabel("机位 1", 0, undefined)).toBe("机位 1 · 第 0 帧 · 线性");
  });
});
