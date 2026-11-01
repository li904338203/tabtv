/**
 * 机位换算（DRT-11，DR-F-23，DR-PARAM-08）。
 *
 * ⚠️ 视角用例读 `api/fixtures/director/camera/fov.json`，Go 求解器（DRT-18 的 `TestVerticalFOV_横幅与竖幅`）读同一份：
 *    两边按同一个「传感器长边」口径算视角，AI 按中景摆的机位截出来才是中景。
 */
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import type { DirectorScene } from "./directorScene";
import { Euler, Group, PerspectiveCamera, Vector3 } from "three";
import { describe, expect, it } from "vitest";
import {
  DIRECTOR_FOCAL_PRESETS,
  DIRECTOR_PREVIEW_MAX_BYTES,
  DIRECTOR_WALK_SPEED,
  directorAspectRatio,
  directorCameraMotion,
  directorCaptureSize,
  directorClampFocal,
  directorDepthRange,
  directorFocalForVerticalFov,
  directorFocalLabel,
  directorLetterbox,
  directorLightPosition,
  directorLookAtRotation,
  directorNearestFocalPreset,
  directorPreviewBitrate,
  directorPreviewEstimateBytes,
  directorPreviewSize,
  directorProjectionCameraOf,
  directorProjectPoint,
  directorShotAngle,
  directorShotSize,
  directorShotTarget,
  directorShotText,
  directorVerticalFov,
  directorWalkStep
} from "./directorCamera";
import { DIRECTOR_ASPECTS } from "./directorCatalog";

type FovCase = { focalMm: number; sensorMm: number; aspect: string; vfovDeg: number };
const FOV_CASES = JSON.parse(
  readFileSync(fileURLToPath(new URL("../../api/fixtures/director/camera/fov.json", import.meta.url)), "utf8")
) as FovCase[];

describe("directorCamera", () => {
  it("directorVerticalFov：与 fov.json 一致（误差小于 1e-6 度），横幅与竖幅都覆盖", () => {
    expect(FOV_CASES.length).toBeGreaterThanOrEqual(12);
    expect(new Set(FOV_CASES.map((c) => c.aspect))).toEqual(new Set(DIRECTOR_ASPECTS.map((a) => a.code)));
    for (const c of FOV_CASES) {
      expect(Math.abs(directorVerticalFov(c.focalMm, c.sensorMm, c.aspect) - c.vfovDeg), JSON.stringify(c)).toBeLessThan(1e-6);
    }
  });

  it("directorVerticalFov：竖幅时传感器长边就是画面高", () => {
    // 同一焦距下，9:16 的竖直视角等于 1:1 的（长边都是画面高），且大于 16:9 的。
    expect(directorVerticalFov(35, 36, "9:16")).toBeCloseTo(directorVerticalFov(35, 36, "1:1"), 12);
    expect(directorVerticalFov(35, 36, "9:16")).toBeGreaterThan(directorVerticalFov(35, 36, "16:9"));
    expect(directorVerticalFov(35, 36, 16 / 9)).toBeCloseTo(directorVerticalFov(35, 36, "16:9"), 12);
  });

  it("directorFocalForVerticalFov：与 directorVerticalFov 互逆", () => {
    for (const c of FOV_CASES) {
      expect(directorFocalForVerticalFov(c.vfovDeg, c.sensorMm, c.aspect)).toBeCloseTo(c.focalMm, 9);
    }
  });

  it("不认识的画幅、非正数一律抛错，不猜", () => {
    expect(() => directorVerticalFov(35, 36, "2:1")).toThrow();
    expect(() => directorVerticalFov(0, 36, "16:9")).toThrow();
    expect(() => directorVerticalFov(35, Number.NaN, "16:9")).toThrow();
    expect(() => directorFocalForVerticalFov(180, 36, "16:9")).toThrow();
    expect(() => directorAspectRatio(-1)).toThrow();
    expect(directorAspectRatio("21:9")).toBeCloseTo(1680 / 720, 12);
  });

  it("directorClampFocal 与快捷档", () => {
    expect(directorClampFocal(5)).toBe(12);
    expect(directorClampFocal(500)).toBe(200);
    expect(directorClampFocal(50)).toBe(50);
    expect(directorClampFocal(Number.NaN)).toBe(35);
    expect([...DIRECTOR_FOCAL_PRESETS]).toEqual([14, 18, 24, 35, 50, 85, 135]);
    expect(directorNearestFocalPreset(30)).toBe(35);
    expect(directorNearestFocalPreset(21)).toBe(18);
    expect(directorNearestFocalPreset(200)).toBe(135);
    expect(directorNearestFocalPreset(12)).toBe(14);
  });

  it("directorCaptureSize：六种画幅", () => {
    expect(Object.fromEntries(["16:9", "9:16", "1:1", "4:3", "3:4", "21:9"].map((a) => [a, directorCaptureSize(a)]))).toEqual({
      "16:9": { width: 1280, height: 720 },
      "9:16": { width: 720, height: 1280 },
      "1:1": { width: 1024, height: 1024 },
      "4:3": { width: 1152, height: 864 },
      "3:4": { width: 864, height: 1152 },
      "21:9": { width: 1680, height: 720 }
    });
  });

  it("directorCaptureSize：未知画幅抛错", () => {
    expect(() => directorCaptureSize("2:1")).toThrow(/不认识的画幅/);
  });
});

describe("directorLetterbox / directorLightPosition / directorWalkStep（DRT-12）", () => {
  it("directorLetterbox：宽屏视口放竖幅机位留左右黑边", () => {
    const r = directorLetterbox(1600, 900, "9:16");
    expect(r.height).toBe(900);
    expect(r.y).toBe(0);
    expect(Math.abs(r.width - (900 * 9) / 16)).toBeLessThanOrEqual(1);
    expect(r.x).toBe(Math.floor((1600 - r.width) / 2));
    // 竖视口放横幅机位：上下黑边。
    const t = directorLetterbox(600, 1000, "16:9");
    expect(t.width).toBe(600);
    expect(t.x).toBe(0);
    expect(Math.abs(t.height - (600 * 9) / 16)).toBeLessThanOrEqual(1);
    expect(t.y).toBe(Math.floor((1000 - t.height) / 2));
  });

  it("directorLetterbox：比例一致时不留黑边", () => {
    expect(directorLetterbox(1280, 720, "16:9")).toEqual({ x: 0, y: 0, width: 1280, height: 720 });
    expect(directorLetterbox(0, 720, "16:9")).toEqual({ x: 0, y: 0, width: 0, height: 0 });
    expect(() => directorLetterbox(100, 100, "2:1")).toThrow();
  });

  it("directorLightPosition：方位 0 俯仰 90 在正上方", () => {
    const top = directorLightPosition(0, 90, 10);
    expect(top.x).toBeCloseTo(0, 9);
    expect(top.y).toBeCloseTo(10, 9);
    expect(top.z).toBeCloseTo(0, 9);
    // 方位 0 俯仰 0 在 +Z；方位 90 在 +X。
    const front = directorLightPosition(0, 0, 10);
    expect(front.z).toBeCloseTo(10, 9);
    const right = directorLightPosition(90, 0, 10);
    expect(right.x).toBeCloseTo(10, 9);
    expect(right.z).toBeCloseTo(0, 9);
  });

  it("directorWalkStep：W 沿视线前进、Shift 三倍速、无按键时位移为 0", () => {
    // 偏航 0 朝 −Z 看：按住 W 1 秒前移 1.5 米。
    const w = directorWalkStep(["KeyW"], 0, DIRECTOR_WALK_SPEED, 1);
    expect(w.z).toBeCloseTo(-1.5, 9);
    expect(w.x).toBeCloseTo(0, 9);
    expect(w.y).toBe(0);
    const boosted = directorWalkStep(["KeyW", "ShiftLeft"], 0, DIRECTOR_WALK_SPEED, 1);
    expect(boosted.z).toBeCloseTo(-4.5, 9);
    expect(directorWalkStep([], 30, DIRECTOR_WALK_SPEED, 1)).toEqual({ x: 0, y: 0, z: 0 });
    // 偏航 90（朝 −X 看）：W 往 −X，D 往 −Z。
    const turned = directorWalkStep(["KeyW"], 90, 1, 1);
    expect(turned.x).toBeCloseTo(-1, 9);
    expect(turned.z).toBeCloseTo(0, 9);
    const strafe = directorWalkStep(["KeyD"], 90, 1, 1);
    expect(strafe.z).toBeCloseTo(-1, 9);
    // 斜着走速度不变；E / Q 升降。
    const diagonal = directorWalkStep(["KeyW", "KeyD"], 0, 1, 1);
    expect(Math.hypot(diagonal.x, diagonal.y, diagonal.z)).toBeCloseTo(1, 9);
    expect(directorWalkStep(["KeyE"], 0, 1, 0.5).y).toBeCloseTo(0.5, 9);
    expect(directorWalkStep(["KeyQ"], 0, 1, 0.5).y).toBeCloseTo(-0.5, 9);
    // 同时按前后：抵消为 0。
    expect(directorWalkStep(["KeyW", "KeyS"], 0, 1, 1)).toEqual({ x: 0, y: 0, z: 0 });
  });
});

describe("机位对准选中对象（DR-F-26）", () => {
  it("directorLookAtRotation 与 three 的 camera.lookAt 一致", async () => {
    const { PerspectiveCamera, Vector3 } = await import("three");
    const { directorLookAtRotation } = await import("./directorCamera");
    const cases: Array<[[number, number, number], [number, number, number]]> = [
      [[0, 1.6, 4], [0, 1, 0]],
      [[3, 2, 3], [0, 1, 0]],
      [[-2, 0.5, -5], [1, 1.7, 2]],
      [[0, 5, 0.001], [0, 0, 0]]
    ];
    for (const [from, to] of cases) {
      const camera = new PerspectiveCamera();
      camera.position.set(...from);
      camera.lookAt(new Vector3(...to));
      const ours = directorLookAtRotation({ x: from[0], y: from[1], z: from[2] }, { x: to[0], y: to[1], z: to[2] })!;
      // 比朝向而不是比欧拉值：同一个朝向可以有两种欧拉写法
      const dir = (r: { x: number; y: number; z: number }) => {
        const c = new PerspectiveCamera();
        c.rotation.set(r.x * Math.PI / 180, r.y * Math.PI / 180, r.z * Math.PI / 180, "XYZ");
        return c.getWorldDirection(new Vector3());
      };
      const expected = camera.getWorldDirection(new Vector3());
      const got = dir(ours);
      expect(got.distanceTo(expected)).toBeLessThan(1e-6);
      // 不带横滚：机位的上方向不朝下
      const up = new Vector3(0, 1, 0).applyEuler(camera.rotation.clone().set(ours.x * Math.PI / 180, ours.y * Math.PI / 180, ours.z * Math.PI / 180, "XYZ"));
      expect(up.y).toBeGreaterThan(0);
    }
    expect(directorLookAtRotation({ x: 1, y: 1, z: 1 }, { x: 1, y: 1, z: 1 })).toBeNull();
  });
});

describe("镜头文字（DRT-26，DR-F-130~132）", () => {
  it("directorShotSize 八档边界：阈值是相邻两档的几何平均", () => {
    const cases: Array<[number, string]> = [
      [10, "大远景"], [4.25, "大远景"], [4.23, "远景"], [1.94, "远景"], [1.93, "全景"], [1.25, "全景"], [1.04, "全景"], [1.03, "中全景"],
      [0.72, "中全景"], [0.71, "中景"], [0.49, "中景"], [0.48, "近景"], [0.32, "近景"], [0.31, "特写"], [0.18, "特写"], [0.17, "大特写"], [0.05, "大特写"]
    ];
    for (const [coverage, name] of cases) expect(directorShotSize(coverage).name, String(coverage)).toBe(name);
    expect(directorShotSize(0.6).code).toBe("medium");
  });

  it("directorShotAngle 仰角与斜角", () => {
    const cases: Array<[number, number, string]> = [
      [0, 0, "平视"], [7.9, 0, "平视"], [-7.9, 0, "平视"], [-8, 0, "仰拍"], [-14.9, 0, "仰拍"], [-15, 0, "低角度仰拍"], [-40, 0, "低角度仰拍"],
      [8, 0, "俯拍"], [59.9, 0, "俯拍"], [60, 0, "顶视"], [89, 0, "顶视"], [0, 10, "平视"], [0, 11, "斜角平视"], [30, -15, "斜角俯拍"]
    ];
    for (const [e, roll, text] of cases) expect(directorShotAngle(e, roll), `${e}/${roll}`).toBe(text);
  });

  it("directorFocalLabel 就近取整", () => {
    expect([12, 16, 30, 31.5, 33, 40, 70, 120, 190].map(directorFocalLabel)).toEqual(["14mm", "14mm", "28mm", "28mm", "35mm", "35mm", "85mm", "135mm", "200mm"]);
  });

  // 一个 1.8 m 的人站在原点，机位看向他的中心
  const person = (extra: Record<string, unknown> = {}) => ({
    id: "hero", name: "角色 1", kind: "character", source: "user", visible: true, locked: false,
    transform: { position: { x: 0, y: 0, z: 0 }, rotation: { x: 0, y: 0, z: 0 }, scale: { x: 1, y: 1, z: 1 } },
    character: { model: "adult_male", height: 1.8, body: "standard", posePreset: "stand_neutral" }, ...extra
  });
  function shot(distance: number, elevationDeg: number, focal = 35, roll = 0, tracks: unknown[] = []): DirectorScene {
    const center = { x: 0, y: 0.9, z: 0 };
    const e = elevationDeg * Math.PI / 180;
    const position = { x: 0, y: center.y + distance * Math.sin(e), z: distance * Math.cos(e) };
    const rotation = directorLookAtRotation(position, center)!;
    return {
      schemaVersion: 1,
      objects: [
        person(),
        { id: "cam", name: "机位 1", kind: "camera", source: "user", visible: true, locked: false,
          transform: { position, rotation, scale: { x: 1, y: 1, z: 1 } },
          camera: { focalLengthMm: focal, sensorWidthMm: 36, aspect: "16:9", near: 0.1, far: 1000, roll } }
      ],
      timeline: { fps: 30, durationFrames: 150, loop: false, tracks }
    } as unknown as DirectorScene;
  }
  // 35mm、16:9 时纵向视角约 32.3°；全身入画（coverage 1.25 → 画面高 2.25 m）的距离约 3.89 m
  const FULL_BODY = 2.25 / (2 * Math.tan((directorVerticalFov(35, 36, 16 / 9) * Math.PI / 180) / 2));

  it("directorShotText 全身入画仰角负20度35mm（DR-AC-38 原例）", () => {
    expect(directorShotText({ scene: shot(FULL_BODY, -20), cameraId: "cam" })).toBe("全景 · 低角度仰拍 · 35mm");
    expect(directorShotTarget(shot(FULL_BODY, -20), "cam")).toMatchObject({ ids: ["hero"], height: 1.8 });
    // 同样的距离与角度换 85mm：画面里只剩上半身附近 → 景别变近
    expect(directorShotText({ scene: shot(FULL_BODY, 0, 85), cameraId: "cam" })).toBe("中景 · 平视 · 85mm");
    expect(directorShotText({ scene: shot(FULL_BODY, 30, 35, 15), cameraId: "cam" })).toBe("全景 · 斜角俯拍 · 35mm");
  });

  it("画面里没有对象时没有景别、角度按机位自己的俯仰；机位不存在时为空串", () => {
    const scene = shot(FULL_BODY, 30);
    scene.objects = (scene.objects ?? []).filter(o => o.kind === "camera");
    expect(directorShotTarget(scene, "cam")).toBeNull();
    expect(directorShotText({ scene, cameraId: "cam" })).toBe("俯拍 · 35mm");
    expect(directorShotText({ scene, cameraId: "nope" })).toBe("");
  });

  const key = (frame: number, position: { x: number; y: number; z: number }, rotation = { x: 0, y: 0, z: 0 }) => ({ frame, value: { position, rotation } });

  it("directorCameraMotion 推近 / 拉远 / 左摇 / 右移 / 升 / 环绕", () => {
    const gaze = { x: 0, y: 1, z: 0 };
    // 机位在 z = 4 看向 −Z（注视点在 z = 0）
    expect(directorCameraMotion([key(0, { x: 0, y: 1, z: 4 }), key(150, { x: 0, y: 1, z: 3 })], 30, gaze)).toBe("缓慢推近");
    expect(directorCameraMotion([key(0, { x: 0, y: 1, z: 3 }), key(30, { x: 0, y: 1, z: 5 })], 30, gaze)).toBe("快速拉远");
    expect(directorCameraMotion([key(0, { x: 0, y: 1, z: 4 }), key(30, { x: 0, y: 1, z: 4 }, { x: 0, y: 25, z: 0 })], 30, gaze)).toBe("快速左摇");
    expect(directorCameraMotion([key(0, { x: 0, y: 1, z: 4 }), key(90, { x: 0, y: 1, z: 4 }, { x: 0, y: -20, z: 0 })], 30, gaze)).toBe("缓慢右摇");
    expect(directorCameraMotion([key(0, { x: 0, y: 1, z: 4 }), key(60, { x: 0.6, y: 1, z: 4 })], 30, gaze)).toBe("缓慢右移");
    expect(directorCameraMotion([key(0, { x: 0, y: 1, z: 4 }), key(30, { x: 0, y: 2.2, z: 4 })], 30, gaze)).toBe("快速上升");
    expect(directorCameraMotion([key(0, { x: 0, y: 1, z: 4 }), key(150, { x: 4, y: 1, z: 0 }, { x: 0, y: 90, z: 0 })], 30, gaze)).toBe("快速环绕");
  });

  it("directorCameraMotion 速度分快慢", () => {
    const gaze = { x: 0, y: 1, z: 0 };
    expect(directorCameraMotion([key(0, { x: 0, y: 1, z: 4 }), key(150, { x: 0, y: 1, z: 3 })], 30, gaze)).toBe("缓慢推近");
    expect(directorCameraMotion([key(0, { x: 0, y: 1, z: 4 }), key(30, { x: 0, y: 1, z: 3 })], 30, gaze)).toBe("快速推近");
  });

  it("directorCameraMotion 无关键帧返回空", () => {
    expect(directorCameraMotion([], 30)).toBe("");
    expect(directorCameraMotion([key(0, { x: 0, y: 1, z: 4 })], 30)).toBe("");
    expect(directorCameraMotion([key(0, { x: 0, y: 1, z: 4 }), key(60, { x: 0, y: 1, z: 4 })], 30)).toBe("");
  });

  it("有机位轨道时文字末尾追加运镜（DR-F-131：向目标前进 1 m、用时 5 秒）", () => {
    const base = shot(FULL_BODY, 0);
    const cam = (base.objects ?? []).find(o => o.id === "cam")!;
    const start = cam.transform.position;
    const tracks = [{ objectId: "cam", channel: "camera", keyframes: [
      { frame: 0, value: { position: start, rotation: cam.transform.rotation, focalLengthMm: 35 } },
      { frame: 150, value: { position: { ...start, z: start.z - 1 }, rotation: cam.transform.rotation, focalLengthMm: 35 } }
    ] }];
    expect(directorShotText({ scene: shot(FULL_BODY, 0, 35, 0, tracks), cameraId: "cam" })).toBe("全景 · 平视 · 35mm · 缓慢推近");
  });
});

describe("控制图的投影与深度范围（DRT-27）", () => {
  const camera = { position: { x: 0.5, y: 1.4, z: 3 }, rotation: { x: -8, y: 12, z: 0 }, focalLengthMm: 35, sensorWidthMm: 36, aspect: "16:9", roll: 0 };
  const size = { width: 1280, height: 720 };

  it("directorProjectPoint 视线中心点投影到画面中心", () => {
    // 视线前方 4 m 的点
    const r = new Euler(camera.rotation.x * Math.PI / 180, camera.rotation.y * Math.PI / 180, 0, "XYZ");
    const ahead = new Vector3(0, 0, -4).applyEuler(r).add(new Vector3(camera.position.x, camera.position.y, camera.position.z));
    const p = directorProjectPoint(ahead, camera, size);
    expect(p.visible).toBe(true);
    expect(p.x).toBeCloseTo(640, 6);
    expect(p.y).toBeCloseTo(360, 6);
    expect(p.depth).toBeCloseTo(4, 6);
  });

  it("directorProjectPoint 与 three 的透视相机同一结果（含横滚）", () => {
    for (const roll of [0, 17]) {
      const rig = new Group();
      rig.position.set(camera.position.x, camera.position.y, camera.position.z);
      rig.rotation.set(camera.rotation.x * Math.PI / 180, camera.rotation.y * Math.PI / 180, 0, "XYZ");
      const roller = new Group();
      roller.rotation.set(0, 0, roll * Math.PI / 180);
      const cam = new PerspectiveCamera(directorVerticalFov(35, 36, 16 / 9), 1280 / 720, 0.1, 1000);
      rig.add(roller);
      roller.add(cam);
      rig.updateMatrixWorld(true);
      for (const point of [new Vector3(0, 1, 0), new Vector3(1.2, 0.3, -1), new Vector3(-0.4, 1.9, 0.5)]) {
        const ndc = point.clone().project(cam);
        const ours = directorProjectPoint(point, { ...camera, roll }, size);
        expect(ours.x).toBeCloseTo(((ndc.x + 1) / 2) * 1280, 4);
        expect(ours.y).toBeCloseTo(((1 - ndc.y) / 2) * 720, 4);
      }
    }
  });

  it("directorProjectPoint 画面外的点标记为不可见", () => {
    expect(directorProjectPoint({ x: 0.5, y: 1.4, z: 10 }, camera, size).visible).toBe(false); // 机位背后
    expect(directorProjectPoint({ x: 30, y: 1.4, z: 2 }, camera, size).visible).toBe(false); // 画面右侧外
  });

  it("directorDepthRange 取包围盒最近最远并留余量", () => {
    const front = { position: { x: 0, y: 1, z: 5 }, rotation: { x: 0, y: 0, z: 0 } };
    const range = directorDepthRange([
      { min: { x: -0.5, y: 0, z: 1 }, max: { x: 0.5, y: 2, z: 2 } }, // 深度 3 ~ 4
      { min: { x: 2, y: 0, z: -6 }, max: { x: 3, y: 1, z: -5 } }, // 深度 10 ~ 11
      { min: { x: 0, y: 0, z: 8 }, max: { x: 1, y: 1, z: 9 } } // 在机位背后，不算
    ], front);
    expect(range!.near).toBeCloseTo(3 * 0.95, 6);
    expect(range!.far).toBeCloseTo(11 * 1.05, 6);
    expect(directorDepthRange([], front)).toBeNull();
  });

  it("directorProjectionCameraOf 取机位对象的位置、朝向与镜头参数（缺的用默认值）", () => {
    const object = { id: "c", name: "c", kind: "camera", transform: { position: { x: 1, y: 2, z: 3 }, rotation: { x: 0, y: 0, z: 0 }, scale: { x: 1, y: 1, z: 1 } }, camera: { focalLengthMm: 50 } };
    expect(directorProjectionCameraOf(object as never)).toMatchObject({ position: { x: 1, y: 2, z: 3 }, focalLengthMm: 50, sensorWidthMm: 36, aspect: "16:9", roll: 0 });
  });
});

describe("预演视频（DRT-25）", () => {
  it("directorPreviewSize 六种画幅为偶数且短边720", () => {
    const sizes = ["16:9", "9:16", "1:1", "4:3", "3:4", "21:9"].map(directorPreviewSize);
    expect(sizes).toEqual([
      { width: 1280, height: 720 }, { width: 720, height: 1280 }, { width: 720, height: 720 },
      { width: 960, height: 720 }, { width: 720, height: 960 }, { width: 1680, height: 720 }
    ]);
    for (const size of sizes) {
      expect(size.width % 2 + size.height % 2).toBe(0);
      expect(Math.min(size.width, size.height)).toBe(720);
    }
  });

  it("码率按像素数等比，最长 15 秒的 21:9 也在 50 MiB 以内", () => {
    expect(directorPreviewBitrate(1280, 720)).toBe(6_000_000);
    expect(directorPreviewBitrate(720, 720)).toBe(3_375_000);
    expect(directorPreviewEstimateBytes(1280, 720, 90)).toBe(Math.round((6_000_000 * 3) / 8 * 1.05));
    expect(directorPreviewEstimateBytes(1680, 720, 450)).toBeLessThan(DIRECTOR_PREVIEW_MAX_BYTES);
  });
});
