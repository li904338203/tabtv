/**
 * 目录常量与姿势数据的绊线（DRT-11 / DRT-10，DRD-29，DR-N-16）。
 *
 * ⚠️ 直接读 `api/fixtures/director/catalog.json`（Go 的 `TestCatalog_与共享fixture一致` 读同一份），不复制一份：
 *    复制的那份迟早和 Go 侧那份不一致，而两边各自都是绿的。
 */
import { existsSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import {
  DIRECTOR_ANGLES,
  DIRECTOR_ASPECTS,
  DIRECTOR_BODIES,
  DIRECTOR_LIMITS,
  DIRECTOR_MODELS,
  DIRECTOR_OBJECT_DEFAULTS,
  DIRECTOR_POSE_CATEGORIES,
  DIRECTOR_POSES,
  DIRECTOR_PROP_COLORS,
  DIRECTOR_PROP_SHAPES,
  DIRECTOR_RELATIONS,
  DIRECTOR_SCHEMA_VERSION,
  DIRECTOR_SHOT_SIZES,
  DIRECTOR_SIDES,
  directorAspectOf,
  directorModelOf
} from "./directorCatalog";
import { DIRECTOR_STANDARD_BONES } from "./directorRig";

const read = (rel: string) => JSON.parse(readFileSync(fileURLToPath(new URL(rel, import.meta.url)), "utf8")) as unknown;
const CATALOG = read("../../api/fixtures/director/catalog.json") as { poses: { code: string; name: string; category: string }[] };
type PoseEntry = {
  code: string;
  name: string;
  category: string;
  bones: Record<string, { x: number; y: number; z: number }>;
  lift?: number;
  thumbnail: string;
};
const POSES = read("../public/director/poses.json") as PoseEntry[];

describe("directorCatalog", () => {
  it("与 catalog.json 逐项相等（含 limits 与 defaults）", () => {
    expect({
      schemaVersion: DIRECTOR_SCHEMA_VERSION,
      models: DIRECTOR_MODELS,
      bodies: DIRECTOR_BODIES,
      poseCategories: DIRECTOR_POSE_CATEGORIES,
      poses: DIRECTOR_POSES,
      propShapes: DIRECTOR_PROP_SHAPES,
      propColors: DIRECTOR_PROP_COLORS,
      shotSizes: DIRECTOR_SHOT_SIZES,
      angles: DIRECTOR_ANGLES,
      sides: DIRECTOR_SIDES,
      relations: DIRECTOR_RELATIONS,
      aspects: DIRECTOR_ASPECTS,
      limits: DIRECTOR_LIMITS,
      defaults: DIRECTOR_OBJECT_DEFAULTS
    }).toEqual(CATALOG);
  });

  it("常量深冻结：改不动默认值", () => {
    expect(Object.isFrozen(DIRECTOR_OBJECT_DEFAULTS.prop.size)).toBe(true);
    expect(Object.isFrozen(DIRECTOR_POSES[0])).toBe(true);
    expect(() => {
      (DIRECTOR_LIMITS as { maxObjects: number }).maxObjects = 1;
    }).toThrow();
  });

  it("按码查素模与画幅；不认识的码返回 undefined", () => {
    expect(directorModelOf("adult_female")?.heightM).toBe(1.625);
    expect(directorModelOf("robot")).toBeUndefined();
    expect(directorAspectOf("21:9")).toEqual({ code: "21:9", width: 1680, height: 720 });
    expect(directorAspectOf("2:1")).toBeUndefined();
  });

  it("poses.json 的姿势码集合等于 catalog.json（含顺序、名称与分类）", () => {
    expect(POSES.map((p) => p.code)).toEqual(CATALOG.poses.map((p) => p.code));
    for (const pose of POSES) {
      const entry = CATALOG.poses.find((p) => p.code === pose.code);
      expect(pose.name, pose.code).toBe(entry?.name);
      expect(pose.category, pose.code).toBe(entry?.category);
      // DRT-21 回填：每个姿势都有缩略图，文件真在（DR-F-42）
      expect(pose.thumbnail, pose.code).toBe(`/director/poses/${pose.code}.webp`);
      expect(existsSync(fileURLToPath(new URL(`../public${pose.thumbnail}`, import.meta.url))), `${pose.code}：缩略图文件不存在`).toBe(true);
    }
  });

  it("每个姿势只用标准骨骼名", () => {
    const standard = new Set(DIRECTOR_STANDARD_BONES);
    for (const pose of POSES) {
      const bad = Object.keys(pose.bones).filter((b) => !standard.has(b));
      expect(bad, `${pose.code} 用了非标准骨骼名`).toEqual([]);
      expect(Object.keys(pose.bones).length).toBeLessThanOrEqual(DIRECTOR_LIMITS.maxBoneOverrides);
      for (const r of Object.values(pose.bones)) {
        for (const v of [r.x, r.y, r.z]) expect(Number.isFinite(v) && Math.abs(v) <= 360).toBe(true);
      }
    }
  });

  it("首批不少于 30 个姿势、覆盖六类；腾空高度在 0~1 之间", () => {
    expect(POSES.length).toBeGreaterThanOrEqual(30);
    expect(new Set(POSES.map((p) => p.category))).toEqual(new Set(DIRECTOR_POSE_CATEGORIES.map((c) => c.code)));
    for (const pose of POSES.filter((p) => p.lift !== undefined)) {
      expect(pose.lift, pose.code).toBeGreaterThan(0);
      expect(pose.lift, pose.code).toBeLessThan(1);
    }
  });
});
