/**
 * 素模骨骼与体型表的绊线（DRT-10，DR-F-40 / 41）。
 *
 * ⚠️ 直接读发布出去的 GLB（public/director/models/），不信任何中间产物：骨骼名、层级、morph、改名记录
 *    全部从文件本身取。表与文件对不上时，症状是套姿势时某根骨骼不动或扭成一团，而不报错。
 */
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import {
  DIRECTOR_BODY_MORPH_WEIGHTS,
  DIRECTOR_BONE_PARENT,
  DIRECTOR_MORPH_TARGETS,
  DIRECTOR_RIG_MODELS,
  DIRECTOR_STANDARD_BONES,
  DIRECTOR_UPSTREAM_BONE_RENAMES,
  directorBodyMorphWeights,
  directorMirrorBoneName,
  directorMirrorBones,
  directorStandardBoneName
} from "./directorRig";

const MODELS_DIR = fileURLToPath(new URL("../public/director/models/", import.meta.url));
const CATALOG = JSON.parse(
  readFileSync(fileURLToPath(new URL("../../api/fixtures/director/catalog.json", import.meta.url)), "utf8")
) as { models: { code: string }[]; bodies: { code: string }[] };

type GltfJson = {
  nodes: { name: string; children?: number[] }[];
  skins: { joints: number[] }[];
  meshes: { extras?: { targetNames?: string[] } }[];
  extras?: { directorAsset?: { boneRenames?: Record<string, string> } };
};

/** 读 GLB 的 JSON 块（12 字节文件头 + 8 字节块头之后就是 JSON）。 */
function glbJson(code: string): GltfJson {
  const bytes = readFileSync(`${MODELS_DIR}${code}.glb`);
  expect(bytes.readUInt32LE(0), `${code}.glb 不是 GLB`).toBe(0x46546c67);
  expect(bytes.readUInt32LE(16), `${code}.glb 的第一个块不是 JSON`).toBe(0x4e4f534a);
  return JSON.parse(bytes.subarray(20, 20 + bytes.readUInt32LE(12)).toString("utf8")) as GltfJson;
}

describe("directorRig", () => {
  it("素模码与 catalog.json 一致", () => {
    expect([...DIRECTOR_RIG_MODELS]).toEqual(CATALOG.models.map((m) => m.code));
  });

  it("四个模型的映射覆盖全部标准骨骼", () => {
    expect(DIRECTOR_STANDARD_BONES).toHaveLength(67);
    expect(new Set(DIRECTOR_STANDARD_BONES).size).toBe(67);
    for (const model of DIRECTOR_RIG_MODELS) {
      const renames = DIRECTOR_UPSTREAM_BONE_RENAMES[model];
      // 上游原名 = 没改名的标准名 + 改名表的键；逐个映射回去，必须一一对应地覆盖全部标准骨骼。
      const targets = new Set(Object.values(renames));
      const upstream = [...DIRECTOR_STANDARD_BONES.filter((b) => !targets.has(b)), ...Object.keys(renames)];
      const mapped = upstream.map((raw) => directorStandardBoneName(model, raw));
      expect(mapped.every((b) => b !== null), `${model}：有上游骨骼映射不到标准名`).toBe(true);
      expect(new Set(mapped).size, `${model}：两根上游骨骼映射到了同一个标准名`).toBe(67);
      expect(new Set(mapped)).toEqual(new Set(DIRECTOR_STANDARD_BONES));
    }
  });

  it("发布的 GLB：骨骼名就是标准名，改名记录与表一致", () => {
    for (const model of DIRECTOR_RIG_MODELS) {
      const json = glbJson(model);
      expect(json.skins, `${model}：应只有一份骨骼`).toHaveLength(1);
      const names = (json.skins[0]?.joints ?? []).map((i) => json.nodes[i]?.name);
      expect(new Set(names), `${model}：GLB 里的骨骼名`).toEqual(new Set(DIRECTOR_STANDARD_BONES));
      expect(json.extras?.directorAsset?.boneRenames, `${model}：GLB 记录的改名`).toEqual(DIRECTOR_UPSTREAM_BONE_RENAMES[model]);
    }
  });

  it("发布的 GLB：骨骼层级与 DIRECTOR_BONE_PARENT 一致", () => {
    for (const model of DIRECTOR_RIG_MODELS) {
      const json = glbJson(model);
      const joints = new Set(json.skins[0]?.joints ?? []);
      const parentOf = new Map<string, string | null>();
      json.nodes.forEach((node, i) => {
        if (!joints.has(i)) return;
        const parent = json.nodes.findIndex((n) => n.children?.includes(i));
        parentOf.set(node.name, joints.has(parent) ? (json.nodes[parent]?.name ?? null) : null);
      });
      expect(Object.fromEntries(parentOf), `${model}：骨骼层级`).toEqual(DIRECTOR_BONE_PARENT);
    }
  });

  it("directorStandardBoneName：去掉 mixamorig 前缀，非标准骨骼返回 null", () => {
    expect(directorStandardBoneName("adult_male", "mixamorig:LeftUpLeg")).toBe("LeftUpLeg");
    expect(directorStandardBoneName("youth_male", "leaf.012")).toBe("LeftHandRing4");
    expect(directorStandardBoneName("adult_male", "leaf.012")).toBe("RightToe_End");
    expect(directorStandardBoneName("adult_male", "Armature")).toBeNull();
  });

  it("体型三档的权重都在 0~1，且 morph 名在每个 GLB 里都有", () => {
    for (const model of DIRECTOR_RIG_MODELS) {
      const targetNames = glbJson(model).meshes[0]?.extras?.targetNames;
      expect(targetNames, `${model}：GLB 的 morph`).toEqual([...DIRECTOR_MORPH_TARGETS]);
      expect(Object.keys(DIRECTOR_BODY_MORPH_WEIGHTS[model]).sort()).toEqual(CATALOG.bodies.map((b) => b.code).sort());
      for (const body of CATALOG.bodies.map((b) => b.code as "slim" | "standard" | "stocky")) {
        for (const [name, w] of Object.entries(DIRECTOR_BODY_MORPH_WEIGHTS[model][body])) {
          expect(DIRECTOR_MORPH_TARGETS as readonly string[]).toContain(name);
          expect(w, `${model}/${body}/${name}`).toBeGreaterThanOrEqual(0);
          expect(w, `${model}/${body}/${name}`).toBeLessThanOrEqual(1);
        }
      }
    }
  });

  it("directorBodyMorphWeights：三个 morph 都给值，换体型不残留上一档", () => {
    expect(directorBodyMorphWeights("adult_male", "stocky")).toEqual({ Skinny: 0, Muscular: 0.5, Obese: 0.5 });
    expect(directorBodyMorphWeights("adult_male", "slim")).toEqual({ Skinny: 1, Muscular: 0, Obese: 0 });
    expect(directorBodyMorphWeights("youth_female", "standard")).toEqual({ Skinny: 0, Muscular: 0, Obese: 0 });
  });

  it("directorMirrorBones：左右对调、y 与 z 取反，镜像两次复原且不改入参", () => {
    const pose = Object.freeze({
      LeftArm: Object.freeze({ x: 38, y: 10, z: 4 }),
      RightUpLeg: Object.freeze({ x: -15, y: 0, z: -3 }),
      Spine: Object.freeze({ x: 5, y: 12, z: -2 })
    });
    const mirrored = directorMirrorBones(pose);
    expect(mirrored).toEqual({
      RightArm: { x: 38, y: -10, z: -4 },
      LeftUpLeg: { x: -15, y: 0, z: 3 },
      Spine: { x: 5, y: -12, z: 2 }
    });
    expect(directorMirrorBones(mirrored)).toEqual(pose);
    expect(directorMirrorBoneName("LeftHandIndex4")).toBe("RightHandIndex4");
    expect(directorMirrorBoneName("HeadTop_End")).toBe("HeadTop_End");
  });
});
