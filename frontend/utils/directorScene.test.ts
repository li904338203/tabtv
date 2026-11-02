/**
 * 场景规则（DRT-11，DR-AC-50，DR-F-10 / 13 / 14 / 24 / 105）。
 *
 * ⚠️ 校验用例直接读 `api/fixtures/director/scenes/`（Go 的 `TestValidate_非法场景报出字段路径` 读同一批），
 *    不复制一份；每个非法 fixture 的 `path` 两边必须报得一模一样。
 */
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import { DIRECTOR_LIMITS } from "./directorCatalog";
import {
  type DirectorObject,
  type DirectorScene,
  directorAddObject,
  directorCrowdSizeError,
  directorDuplicateObject,
  directorMigrateScene,
  directorNewObjectId,
  directorNextName,
  directorRemoveObject,
  directorRenameObject,
  directorSceneSizeError,
  directorSetLocked,
  directorSetOutputCamera,
  directorSetVisible,
  directorTouch,
  directorValidateScene
} from "./directorScene";

const SCENES_DIR = fileURLToPath(new URL("../../api/fixtures/director/scenes/", import.meta.url));
const readScene = (name: string) => JSON.parse(readFileSync(join(SCENES_DIR, name), "utf8")) as DirectorScene;
const DEFAULT_SCENE = readScene("valid-default.json");

/** 固定随机源：每次调用依次给出 0,1,2…，ID 可预期。 */
function seqRandom() {
  let next = 0;
  return (n: number) => Uint8Array.from({ length: n }, () => next++ % 256);
}

function deepFreeze<T>(value: T): T {
  if (value && typeof value === "object") {
    for (const child of Object.values(value)) deepFreeze(child);
    Object.freeze(value);
  }
  return value;
}

const clone = <T>(v: T): T => JSON.parse(JSON.stringify(v)) as T;

function prop(id: string, name = "道具"): DirectorObject {
  return {
    id,
    name,
    kind: "prop",
    transform: { position: { x: 0, y: 0, z: 0 }, rotation: { x: 0, y: 0, z: 0 }, scale: { x: 1, y: 1, z: 1 } },
    prop: { shape: "box", size: { x: 1, y: 1, z: 1 } }
  };
}

function camera(id: string, name: string): DirectorObject {
  return {
    id,
    name,
    kind: "camera",
    transform: { position: { x: 0, y: 1.5, z: 3 }, rotation: { x: 0, y: 0, z: 0 }, scale: { x: 1, y: 1, z: 1 } },
    camera: { focalLengthMm: 35, aspect: "16:9" }
  };
}

describe("directorValidateScene", () => {
  it("非法 fixture 报与 Go 相同的 path", () => {
    const files = readdirSync(SCENES_DIR).filter((f) => /^invalid-.*\.json$/.test(f)).sort();
    expect(files.length).toBeGreaterThanOrEqual(8);
    for (const file of files) {
      const fixture = JSON.parse(readFileSync(join(SCENES_DIR, file), "utf8")) as { scene: unknown; path: string };
      const result = directorValidateScene(fixture.scene);
      expect(result.ok, `${file} 应当不合法`).toBe(false);
      if (!result.ok) expect(result.path, `${file}：${result.message}`).toBe(fixture.path);
    }
  });

  it("合法 fixture 全部通过", () => {
    for (const file of readdirSync(SCENES_DIR).filter((f) => /^valid-.*\.json$/.test(f))) {
      expect(directorValidateScene(readScene(file)), file).toEqual({ ok: true });
    }
  });

  it("未知字段不报错，且原样保留（校验只读不改）", () => {
    const scene = readScene("valid-unknown-fields.json");
    const before = JSON.stringify(scene);
    expect(directorValidateScene(deepFreeze(scene))).toEqual({ ok: true });
    expect(JSON.stringify(scene)).toBe(before);
    for (const key of ["futureTopLevel", "futureStage", "futureSnap", "futureLight", "futureObject", "futureTransform", "futureSub", "futureTimeline", `"w":1`]) {
      expect(before).toContain(key);
    }
  });

  it("NaN 与 Infinity 被拒（JSON 里写不出来，内存里的对象可以有）", () => {
    const nan = clone(DEFAULT_SCENE);
    (nan.objects?.[1] as DirectorObject).transform.position.x = Number.NaN;
    expect(directorValidateScene(nan)).toMatchObject({ ok: false, path: "objects[1].transform.position.x" });
    const inf = clone(DEFAULT_SCENE);
    (inf.objects?.[0] as DirectorObject & { camera: { focalLengthMm: number } }).camera.focalLengthMm = Number.POSITIVE_INFINITY;
    expect(directorValidateScene(inf)).toMatchObject({ ok: false, path: "objects[0].camera.focalLengthMm" });
  });

  it("正文超过 512 KiB 被拒并带上限", () => {
    const big = { ...clone(DEFAULT_SCENE), padding: "x".repeat(DIRECTOR_LIMITS.maxSceneBytes) };
    expect(directorValidateScene(big)).toMatchObject({ ok: false, path: "$", limit: 524288 });
    expect(directorSceneSizeError(big)).toEqual({ error: "limit", what: "sceneBytes", limit: 524288 });
    expect(directorSceneSizeError(DEFAULT_SCENE)).toBeNull();
  });

  it("对象超过 200 个、单阵超过 100 人都带上限值", () => {
    const many = { schemaVersion: 1, objects: Array.from({ length: 201 }, (_, i) => prop(`obj_${i}`)) };
    expect(directorValidateScene(many)).toMatchObject({ ok: false, path: "objects", limit: 200 });
    const crowd = clone(DEFAULT_SCENE);
    crowd.objects?.push({
      ...prop("obj_crowd", "群众 1"),
      kind: "crowd",
      prop: undefined,
      crowd: { model: "adult_male", rows: 10, cols: 10, spacing: 1 }
    });
    expect(directorValidateScene(crowd)).toEqual({ ok: true });
    const crowdObj = crowd.objects?.[2] as DirectorObject & { crowd: { rows: number } };
    crowdObj.crowd.rows = 11;
    expect(directorValidateScene(crowd)).toMatchObject({ ok: false, path: "objects[2].crowd.rows" });
    expect(directorCrowdSizeError(10, 11)).toEqual({ error: "limit", what: "crowd", limit: 100 });
    expect(directorCrowdSizeError(10, 10)).toBeNull();
  });

  it("不是对象的正文报 $", () => {
    for (const v of [null, [], "scene", 3]) expect(directorValidateScene(v)).toMatchObject({ ok: false, path: "$" });
  });
});

describe("directorMigrateScene", () => {
  it("v1 原样返回同一个对象；更高版本返回 newer", () => {
    expect(directorMigrateScene(DEFAULT_SCENE)).toEqual({ scene: DEFAULT_SCENE });
    const migrated = directorMigrateScene(DEFAULT_SCENE);
    expect("scene" in migrated && migrated.scene).toBe(DEFAULT_SCENE);
    expect(directorMigrateScene({ schemaVersion: 2 })).toEqual({ error: "newer" });
  });
});

describe("对象操作", () => {
  it("directorAddObject：新对象按类命名、取默认子结构，位置取视线落点或原点", () => {
    const r = directorAddObject(DEFAULT_SCENE, "character", { x: 1, y: 0, z: -2 }, seqRandom());
    if ("error" in r) throw new Error("不该超上限");
    expect(r.object).toMatchObject({
      name: "角色 2",
      kind: "character",
      source: "user",
      visible: true,
      locked: false,
      transform: { position: { x: 1, y: 0, z: -2 } },
      character: { model: "adult_male", body: "standard", posePreset: "stand_neutral", height: 1.8 }
    });
    expect(r.object.id).toMatch(/^obj_[a-z0-9]{12}$/);
    expect(r.scene.objects).toHaveLength(3);
    expect(directorValidateScene(r.scene)).toEqual({ ok: true });
    const p = directorAddObject(DEFAULT_SCENE, "prop");
    if ("error" in p) throw new Error("不该超上限");
    expect(p.object.transform.position).toEqual({ x: 0, y: 0, z: 0 });
    expect(p.object.name).toBe("道具 1");
    expect(directorValidateScene(p.scene)).toEqual({ ok: true });
  });

  it("directorAddObject：超过 200 个返回 limit", () => {
    const full: DirectorScene = { schemaVersion: 1, objects: Array.from({ length: 200 }, (_, i) => prop(`obj_${i}`)) };
    expect(directorAddObject(full, "prop")).toEqual({ error: "limit", what: "objects", limit: 200 });
    expect(directorDuplicateObject(full, "obj_0")).toEqual({ error: "limit", what: "objects", limit: 200 });
  });

  it("directorAddObject：没有输出机位时，新加的机位成为输出机位", () => {
    const noCamera: DirectorScene = { schemaVersion: 1, objects: [prop("obj_p")], outputCameraId: null };
    const r = directorAddObject(noCamera, "camera");
    if ("error" in r) throw new Error("不该超上限");
    expect(r.scene.outputCameraId).toBe(r.object.id);
    expect(directorValidateScene(r.scene)).toEqual({ ok: true });
    const again = directorAddObject(r.scene, "camera");
    if ("error" in again) throw new Error("不该超上限");
    expect(again.scene.outputCameraId).toBe(r.object.id);
  });

  it("directorAddObject：不修改入参（深冻结入参）", () => {
    const frozen = deepFreeze(clone(DEFAULT_SCENE));
    expect(() => directorAddObject(frozen, "crowd")).not.toThrow();
    expect(() => directorDuplicateObject(frozen, "obj_character_1")).not.toThrow();
    expect(() => directorRemoveObject(frozen, "obj_camera_1")).not.toThrow();
    expect(() => directorRenameObject(frozen, "obj_character_1", "林澈")).not.toThrow();
    expect(() => directorSetVisible(frozen, "obj_character_1", false)).not.toThrow();
    expect(() => directorSetLocked(frozen, "obj_character_1", true)).not.toThrow();
    expect(JSON.stringify(frozen)).toBe(JSON.stringify(DEFAULT_SCENE));
  });

  it("directorDuplicateObject：新 id、名称自增、x 偏移 0.5", () => {
    const r = directorDuplicateObject(DEFAULT_SCENE, "obj_character_1", seqRandom());
    if ("error" in r) throw new Error("应当复制成功");
    const original = DEFAULT_SCENE.objects?.[1] as DirectorObject;
    expect(r.object.id).not.toBe(original.id);
    expect(r.object.name).toBe("角色 2");
    expect(r.object.transform.position).toEqual({ x: 0.5, y: 0, z: 0 });
    expect(r.object.character).toEqual(original.character);
    expect(r.object.character).not.toBe(original.character);
    expect(directorValidateScene(r.scene)).toEqual({ ok: true });

    const named = directorRenameObject(DEFAULT_SCENE, "obj_character_1", "林澈");
    const first = directorDuplicateObject(named, "obj_character_1");
    if ("error" in first) throw new Error("应当复制成功");
    expect(first.object.name).toBe("林澈 2");
    const second = directorDuplicateObject(first.scene, first.object.id);
    if ("error" in second) throw new Error("应当复制成功");
    expect(second.object.name).toBe("林澈 3");
    expect(directorDuplicateObject(DEFAULT_SCENE, "obj_missing")).toEqual({ error: "not_found" });
  });

  it("directorDuplicateObject：副本是用户对象、不锁定，未知字段一并复制", () => {
    const scene = clone(DEFAULT_SCENE);
    Object.assign(scene.objects?.[1] as DirectorObject, { source: "ai", locked: true, futureField: { a: 1 } });
    const r = directorDuplicateObject(scene, "obj_character_1");
    if ("error" in r) throw new Error("应当复制成功");
    expect(r.object).toMatchObject({ source: "user", locked: false, futureField: { a: 1 } });
  });

  it("directorRemoveObject：删输出机位自动改选剩下的第一个机位", () => {
    const scene: DirectorScene = {
      ...clone(DEFAULT_SCENE),
      objects: [...(clone(DEFAULT_SCENE).objects ?? []), camera("obj_cam_b", "机位 2"), camera("obj_cam_c", "机位 3")]
    };
    const next = directorRemoveObject(scene, "obj_camera_1");
    expect(next.outputCameraId).toBe("obj_cam_b");
    expect(directorValidateScene(next)).toEqual({ ok: true });
    expect(directorRemoveObject(next, "obj_character_1").outputCameraId).toBe("obj_cam_b");
  });

  it("directorRemoveObject：删光机位后输出为 null；挂在对象上的轨道一并删除", () => {
    const scene = clone(DEFAULT_SCENE);
    scene.timeline = {
      ...scene.timeline,
      tracks: [{ objectId: "obj_camera_1", channel: "camera", keyframes: [] }, { objectId: "obj_character_1", channel: "transform", keyframes: [] }]
    };
    const next = directorRemoveObject(scene, "obj_camera_1");
    expect(next.outputCameraId).toBeNull();
    expect(next.timeline?.tracks?.map((t) => t.objectId)).toEqual(["obj_character_1"]);
    expect(directorValidateScene(next)).toEqual({ ok: true });
    expect(directorRemoveObject(scene, "obj_missing")).toBe(scene);
  });

  it("directorRenameObject：截断到 20 字，空名称不改", () => {
    const long = "一二三四五六七八九十一二三四五六七八九十多出来的";
    const renamed = directorRenameObject(DEFAULT_SCENE, "obj_character_1", `  ${long}  `);
    expect(renamed.objects?.[1]?.name).toBe("一二三四五六七八九十一二三四五六七八九十");
    expect(directorValidateScene(renamed)).toEqual({ ok: true });
    expect(directorRenameObject(DEFAULT_SCENE, "obj_character_1", "   ")).toBe(DEFAULT_SCENE);
    // 按字符数截断，不切开代理对（emoji 是一个字符）。
    expect(directorRenameObject(DEFAULT_SCENE, "obj_character_1", "😀".repeat(25)).objects?.[1]?.name).toBe("😀".repeat(20));
  });

  it("directorSetVisible / directorSetLocked / directorSetOutputCamera：只改目标字段，非机位不能设为输出", () => {
    const hidden = directorSetVisible(DEFAULT_SCENE, "obj_character_1", false);
    expect(hidden.objects?.[1]?.visible).toBe(false);
    expect(hidden.objects?.[0]).toBe(DEFAULT_SCENE.objects?.[0]);
    expect(directorSetVisible(DEFAULT_SCENE, "obj_character_1", true)).toBe(DEFAULT_SCENE);
    expect(directorSetLocked(DEFAULT_SCENE, "obj_character_1", true).objects?.[1]?.locked).toBe(true);
    expect(directorSetOutputCamera(DEFAULT_SCENE, "obj_character_1")).toBe(DEFAULT_SCENE);
    expect(directorSetOutputCamera(DEFAULT_SCENE, null).outputCameraId).toBeNull();
  });

  it("directorTouch：ai 对象被改后变 user", () => {
    const ai = { ...prop("obj_a"), source: "ai" as const };
    expect(directorTouch(ai).source).toBe("user");
    expect(ai.source).toBe("ai");
    const user = prop("obj_u");
    expect(directorTouch(user)).toBe(user);
  });

  it("directorNextName：取同类最大号加一", () => {
    const scene: DirectorScene = {
      schemaVersion: 1,
      objects: [prop("a", "道具 1"), prop("b", "道具 7"), prop("c", "林澈"), camera("d", "机位 3"), prop("e", "道具 x")]
    };
    expect(directorNextName(scene, "prop")).toBe("道具 8");
    expect(directorNextName(scene, "camera")).toBe("机位 4");
    expect(directorNextName(scene, "crowd")).toBe("群众 1");
  });

  it("directorNewObjectId：obj_ + 12 位，可注入随机源", () => {
    expect(directorNewObjectId(seqRandom())).toBe("obj_abcdefghijkl");
    expect(directorNewObjectId()).toMatch(/^obj_[a-z0-9]{12}$/);
    expect(directorNewObjectId()).not.toBe(directorNewObjectId());
  });
});

// DRT-14：编辑权限只有一份判断（CLAUDE.md §5.17）。这里按动态 import 取，追加时不改上面的 import 块。
describe("directorCanEdit", () => {
  it("directorCanEdit：与 roleAllowsScope 的编辑列逐角色一致", async () => {
    const { directorCanEdit } = await import("./directorRole");
    const { roleAllowsScope } = await import("./roleScope");
    for (const role of ["owner", "admin", "editor", "viewer", undefined] as const) {
      expect(directorCanEdit(role), String(role)).toBe(roleAllowsScope(role, "seeaitv:edit"));
    }
    expect(directorCanEdit("editor")).toBe(true);
    expect(directorCanEdit("viewer")).toBe(false);
    expect(directorCanEdit(null)).toBe(false);
  });
});

// DRT-13：视口编辑的两个判断。同上按动态 import 取，追加时不改上面的 import 块。
describe("director viewport editing", () => {
  it("directorUniformScale：三轴不等时统一为等比", async () => {
    const { directorUniformScale } = await import("./directorScene");
    expect(directorUniformScale({ x: 1, y: 2, z: 1 })).toEqual({ x: 2, y: 2, z: 2 });
    expect(directorUniformScale({ x: 0.5, y: 1, z: 1.2 })).toEqual({ x: 0.5, y: 0.5, z: 0.5 });
    // 偏离一样多时取 x、y、z 里的第一个。
    expect(directorUniformScale({ x: 1.5, y: 0.5, z: 1 })).toEqual({ x: 1.5, y: 1.5, z: 1.5 });
    expect(directorUniformScale({ x: 1.3, y: 1.3, z: 1.3 })).toEqual({ x: 1.3, y: 1.3, z: 1.3 });
    expect(directorUniformScale({ x: 1, y: 1, z: 1 })).toEqual({ x: 1, y: 1, z: 1 });
    // 坏值不参与：不让一个 NaN 或 0 把角色缩没。
    expect(directorUniformScale({ x: Number.NaN, y: 0, z: 1.1 })).toEqual({ x: 1.1, y: 1.1, z: 1.1 });
    expect(directorUniformScale({ x: -2, y: Number.POSITIVE_INFINITY, z: 0 })).toEqual({ x: 1, y: 1, z: 1 });
  });

  it("directorIsPickable：锁定或隐藏的对象不可选", async () => {
    const { directorIsPickable } = await import("./directorScene");
    expect(directorIsPickable({})).toBe(true);
    expect(directorIsPickable({ locked: false, visible: true })).toBe(true);
    expect(directorIsPickable({ locked: true })).toBe(false);
    expect(directorIsPickable({ visible: false })).toBe(false);
    expect(directorIsPickable({ locked: true, visible: false })).toBe(false);
  });
});

// DRT-15：属性编辑。同上按动态 import 取。
describe("director property editing", () => {
  const base = (): DirectorScene => clone({
    schemaVersion: 1,
    outputCameraId: "c1",
    objects: [
      { ...camera("c1", "机位 1"), transform: { position: { x: 0, y: 1.6, z: 4 }, rotation: { x: 0, y: 0, z: 0 }, scale: { x: 1, y: 1, z: 1 }, extra: 1 } },
      prop("p1")
    ]
  } as DirectorScene);

  it("directorSetTransform：换掉变换且保留 transform 的未知字段；值没变时返回同一个场景", async () => {
    const { directorSetTransform, directorFindObject } = await import("./directorScene");
    const scene = base();
    const t = { position: { x: 1, y: 1.6, z: 4 }, rotation: { x: 0, y: 30, z: 0 }, scale: { x: 1, y: 1, z: 1 } };
    const next = directorSetTransform(scene, "c1", t);
    expect(directorFindObject(next, "c1")?.transform).toEqual({ ...t, extra: 1 });
    expect(directorFindObject(scene, "c1")?.transform.position.x).toBe(0);
    expect(directorSetTransform(next, "c1", t)).toBe(next);
    expect(directorSetTransform(scene, "nope", t)).toBe(scene);
  });

  it("directorPatchObject：浅合并子结构，缺失时按默认值补齐，没变化时返回同一个场景", async () => {
    const { directorPatchObject, directorFindObject } = await import("./directorScene");
    const scene = base();
    const next = directorPatchObject(scene, "c1", "camera", { focalLengthMm: 85 });
    expect((directorFindObject(next, "c1")?.camera as Record<string, unknown>).focalLengthMm).toBe(85);
    expect(directorPatchObject(next, "c1", "camera", { focalLengthMm: 85 })).toBe(next);
    // 道具没有 prop 子结构时按默认值补齐再合并
    const bare = clone(scene);
    delete (bare.objects![1] as Record<string, unknown>).prop;
    const patched = directorPatchObject(bare, "p1", "prop", { shape: "sphere" });
    expect(directorFindObject(patched, "p1")?.prop).toEqual({ color: "#9aa0a6", shape: "sphere", size: { x: 1, y: 1, z: 1 } });
  });

  it("directorPatchSettings / directorPatchLight：缺失时按默认值补齐，只改给的那几项", async () => {
    const { directorPatchSettings, directorPatchLight } = await import("./directorScene");
    const scene = base();
    const ground = directorPatchSettings(scene, "ground", { visible: false });
    expect(ground.ground).toEqual({ height: 0, opacity: 1, visible: false });
    expect(directorPatchSettings(ground, "ground", { visible: false })).toBe(ground);
    const lit = directorPatchLight(scene, "key", { intensity: 0 });
    expect((lit.lights as Record<string, Record<string, unknown>>).key!.intensity).toBe(0);
    expect((lit.lights as Record<string, Record<string, unknown>>).fill!.intensity).toBe(0.5);
    expect(directorPatchLight(lit, "key", { intensity: 0 })).toBe(lit);
  });
});

// DRT-22：群众阵列布局与上限。同上按动态 import 取。
describe("director crowds and limits", () => {
  it("directorCrowdLayout 10x10 为 100 个实例", async () => {
    const { directorCrowdLayout } = await import("./directorScene");
    const slots = directorCrowdLayout({ rows: 10, cols: 10, spacing: 1, jitter: false, seed: 1 });
    expect(slots).toHaveLength(100);
    // 行优先：第 0 个在左后角，第 9 个是同一行最右，第 10 个换到下一行
    expect(slots[0]).toEqual({ x: -4.5, z: -4.5, yaw: 0 });
    expect(slots[9]).toEqual({ x: 4.5, z: -4.5, yaw: 0 });
    expect(slots[10]).toEqual({ x: -4.5, z: -3.5, yaw: 0 });
    // 坏值夹回范围：行列超过 10 当 10，间距不是正数按默认值
    expect(directorCrowdLayout({ rows: 99, cols: 3, spacing: -1 })).toHaveLength(30);
  });

  it("directorCrowdLayout 同种子结果相同", async () => {
    const { directorCrowdLayout } = await import("./directorScene");
    const a = directorCrowdLayout({ rows: 4, cols: 5, spacing: 2, jitter: true, seed: 42 });
    const b = directorCrowdLayout({ rows: 4, cols: 5, spacing: 2, jitter: true, seed: 42 });
    expect(a).toEqual(b);
    // 错位在 ±0.25 × 间距、朝向在 ±30° 之内
    const grid = directorCrowdLayout({ rows: 4, cols: 5, spacing: 2, jitter: false });
    for (const [i, slot] of a.entries()) {
      expect(Math.abs(slot.x - grid[i]!.x)).toBeLessThanOrEqual(0.5);
      expect(Math.abs(slot.z - grid[i]!.z)).toBeLessThanOrEqual(0.5);
      expect(Math.abs(slot.yaw)).toBeLessThanOrEqual(30);
    }
    expect(a.some(slot => slot.yaw !== 0)).toBe(true);
  });

  it("directorCrowdLayout 换种子结果不同", async () => {
    const { directorCrowdLayout } = await import("./directorScene");
    const a = directorCrowdLayout({ rows: 3, cols: 3, spacing: 1, jitter: true, seed: 1 });
    const b = directorCrowdLayout({ rows: 3, cols: 3, spacing: 1, jitter: true, seed: 2 });
    expect(a).not.toEqual(b);
    // 关掉错位时种子不起作用
    expect(directorCrowdLayout({ rows: 3, cols: 3, spacing: 1, jitter: false, seed: 1 }))
      .toEqual(directorCrowdLayout({ rows: 3, cols: 3, spacing: 1, jitter: false, seed: 2 }));
  });

  it("directorCrowdLayout 居中于原点", async () => {
    const { directorCrowdLayout } = await import("./directorScene");
    for (const [rows, cols] of [[1, 1], [2, 3], [10, 7]] as const) {
      const slots = directorCrowdLayout({ rows, cols, spacing: 1.5, jitter: false });
      const sx = slots.reduce((sum, s) => sum + s.x, 0);
      const sz = slots.reduce((sum, s) => sum + s.z, 0);
      expect(Math.abs(sx)).toBeLessThan(1e-9);
      expect(Math.abs(sz)).toBeLessThan(1e-9);
      const xs = slots.map(s => s.x);
      expect(Math.min(...xs)).toBeCloseTo(-Math.max(...xs), 9);
    }
  });

  it("directorCanAddObject 第101人被拒", async () => {
    const { directorCanAddObject } = await import("./directorScene");
    const scene = clone(DEFAULT_SCENE);
    expect(directorCanAddObject(scene, "crowd", 100)).toEqual({ ok: true });
    expect(directorCanAddObject(scene, "crowd", 101)).toEqual({
      ok: false, reason: "单阵最多 100 人", error: { error: "limit", what: "crowd", limit: DIRECTOR_LIMITS.maxCrowdSize }
    });
    // 不是群众时不看人数
    expect(directorCanAddObject(scene, "prop", 999)).toEqual({ ok: true });
  });

  it("directorCanAddObject 对象满200被拒", async () => {
    const { directorCanAddObject, directorLimitText } = await import("./directorScene");
    const full: DirectorScene = { schemaVersion: 1, objects: Array.from({ length: DIRECTOR_LIMITS.maxObjects }, (_, i) => prop(`p${i}`)) };
    for (const kind of ["character", "crowd", "prop", "camera"] as const) {
      const result = directorCanAddObject(full, kind, 1);
      expect(result.ok).toBe(false);
      if (!result.ok) expect(result.reason).toBe("场景里最多 200 个对象");
    }
    expect(directorCanAddObject({ ...full, objects: full.objects!.slice(1) }, "prop")).toEqual({ ok: true });
    expect(directorLimitText({ error: "limit", what: "sceneBytes", limit: 524288 })).toBe("场景内容已到上限（512 KiB），请先删掉一些对象");
  });

  it("directorSeededRandom 同一种子序列相同、落在 [0, 1)", async () => {
    const { directorSeededRandom } = await import("./directorScene");
    const a = directorSeededRandom(7);
    const b = directorSeededRandom(7);
    const xs = Array.from({ length: 50 }, () => a());
    expect(Array.from({ length: 50 }, () => b())).toEqual(xs);
    expect(xs.every(x => x >= 0 && x < 1)).toBe(true);
    expect(new Set(xs).size).toBe(50);
  });
});
