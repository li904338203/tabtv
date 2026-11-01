import { describe, expect, it } from "vitest";
import type { DirectorScene } from "./directorScene";
import {
  DIRECTOR_SPLAT_MAX_BYTES,
  directorFitGroundPlane,
  directorPatchSplat,
  directorRemoveSplat,
  directorSetSplat,
  directorSplatFileCheck,
  directorSplatGroundBand,
  directorSplatOf,
  directorSplatUploadErrorText
} from "./directorSplat";

/** 半径 0.5 m 圆盘上的规则点阵（泼溅贴地的取点范围），高度按 `y(x, z)`。 */
function disc(y: (x: number, z: number) => number, step = 0.05, center = { x: 0, z: 0 }) {
  const out: Array<{ x: number; y: number; z: number }> = [];
  for (let x = -0.5; x <= 0.5 + 1e-9; x += step) {
    for (let z = -0.5; z <= 0.5 + 1e-9; z += step) {
      if (x * x + z * z <= 0.25) out.push({ x: center.x + x, y: y(center.x + x, center.z + z), z: center.z + z });
    }
  }
  return out;
}

describe("泼溅贴地的平面拟合（DRT-28，DR-F-141）", () => {
  it("directorFitGroundPlane 水平面高度正确", () => {
    const fit = directorFitGroundPlane(disc(() => 0.3), { x: 0.1, z: -0.2 });
    expect(fit?.height).toBeCloseTo(0.3, 6);
    expect(fit?.normal.y).toBeCloseTo(1, 6);
    expect(fit?.residual).toBeLessThan(1e-6);
  });

  it("directorFitGroundPlane 倾斜面法线正确", () => {
    // y = 0.3 + 0.05x − 0.1z：法线 ∝ (−0.05, 1, 0.1)
    const fit = directorFitGroundPlane(disc((x, z) => 0.3 + 0.05 * x - 0.1 * z, 0.05, { x: 1, z: 1 }), { x: 1, z: 1 });
    expect(fit?.height).toBeCloseTo(0.3 + 0.05 - 0.1, 6);
    const l = Math.hypot(0.05, 1, 0.1);
    expect(fit?.normal.x).toBeCloseTo(-0.05 / l, 6);
    expect(fit?.normal.z).toBeCloseTo(0.1 / l, 6);
  });

  it("directorFitGroundPlane 剔掉飘在空中的点", () => {
    // 地面 y=0.3 上混进 8 个高 1 m 的离群点：剔掉之后高度仍是 0.3
    const points = [...disc(() => 0.3), ...Array.from({ length: 8 }, (_, i) => ({ x: i * 0.05 - 0.2, y: 1.3, z: 0.1 }))];
    const fit = directorFitGroundPlane(points, { x: 0, z: 0 });
    expect(fit?.height).toBeCloseTo(0.3, 3);
    expect(fit?.residual).toBeLessThan(0.01);
  });

  it("directorSplatGroundBand 脚边的石头不混进地面", () => {
    // 地面 y=0.25 的圆盘 + 一块球心在 0.6 m 处、半径 0.3 m 的石头（120 个点，高 0.3~0.9 m）
    const floor = disc(() => 0.25);
    const rock = Array.from({ length: 120 }, (_, i) => {
      const u = (i / 119) * 2 - 1;
      const phi = i * 2.399963;
      const s = Math.sqrt(1 - u * u);
      return { x: 0.35 + 0.3 * s * Math.cos(phi), y: 0.6 + 0.3 * u, z: 0.1 + 0.3 * s * Math.sin(phi) };
    });
    const band = directorSplatGroundBand([...floor, ...rock]);
    expect(band.length).toBeGreaterThanOrEqual(floor.length);
    expect(band.every(p => p.y < 0.34)).toBe(true);
    expect(directorFitGroundPlane(band, { x: 0, z: 0 })?.height).toBeCloseTo(0.25, 3);
    // 两层一样密时取高的那层
    const twoLayers = [...disc(() => 0.2), ...disc(() => 0.8)];
    expect(directorSplatGroundBand(twoLayers).every(p => p.y === 0.8)).toBe(true);
    expect(directorSplatGroundBand([])).toEqual([]);
  });

  it("directorFitGroundPlane 点太少返回空", () => {
    expect(directorFitGroundPlane(disc(() => 0.3, 0.25), { x: 0, z: 0 })).toBeNull();
    expect(directorFitGroundPlane([], { x: 0, z: 0 })).toBeNull();
    // 点够多但排成一条线：定不出平面
    const line = Array.from({ length: 40 }, (_, i) => ({ x: i * 0.02, y: 0.3, z: 0 }));
    expect(directorFitGroundPlane(line, { x: 0, z: 0 })).toBeNull();
  });
});

describe("泼溅文件与场景（DRT-28，DR-F-140 / 143）", () => {
  it("directorSplatFileCheck 扩展名与大小", () => {
    expect(directorSplatFileCheck({ name: "room.spz", size: 1024 })).toEqual({ ok: true });
    expect(directorSplatFileCheck({ name: "ROOM.PLY", size: DIRECTOR_SPLAT_MAX_BYTES })).toEqual({ ok: true });
    expect(directorSplatFileCheck({ name: "room.splat", size: 10 })).toMatchObject({ ok: false, reason: "extension" });
    expect(directorSplatFileCheck({ name: "room", size: 10 })).toMatchObject({ ok: false, reason: "extension" });
    const big = directorSplatFileCheck({ name: "room.ply", size: DIRECTOR_SPLAT_MAX_BYTES + 1 });
    expect(big).toMatchObject({ ok: false, reason: "size" });
    expect(big.ok ? "" : big.text).toContain("超过 100 MB");
  });

  it("directorSplatUploadErrorText 按错误码说明", () => {
    expect(directorSplatUploadErrorText({ data: { code: 41301 } })).toContain("大小上限");
    expect(directorSplatUploadErrorText({ data: { code: 41501 } })).toContain("不支持的文件类型");
    expect(directorSplatUploadErrorText({ data: { code: 41502 } })).toContain("内容与扩展名不符");
    expect(directorSplatUploadErrorText({ message: "断网了" })).toBe("导入失败：断网了");
  });

  it("导入、替换、改变换、移除", () => {
    const scene = { schemaVersion: 1, objects: [] } as unknown as DirectorScene;
    const added = directorSetSplat(scene, "7001");
    expect(directorSplatOf(added)).toEqual({ mediaId: "7001", position: { x: 0, y: 0, z: 0 }, rotation: { x: 0, y: 0, z: 0 }, scale: 1, visible: true });
    const moved = directorPatchSplat(added, { position: { x: 1, y: -0.2, z: 0 }, scale: 2, visible: false });
    // 替换文件保留变换与显示状态
    const replaced = directorSetSplat(moved, "7002");
    expect(directorSplatOf(replaced)).toEqual({ mediaId: "7002", position: { x: 1, y: -0.2, z: 0 }, rotation: { x: 0, y: 0, z: 0 }, scale: 2, visible: false });
    expect(directorPatchSplat(replaced, { scale: 2 })).toBe(replaced);
    expect(directorPatchSplat(scene, { scale: 2 })).toBe(scene);
    const removed = directorRemoveSplat(replaced);
    expect("splat" in removed).toBe(false);
    expect(directorSplatOf(removed)).toBeNull();
  });
});
