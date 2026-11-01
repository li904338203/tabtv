/**
 * rest 姿势表的绊线（DRT-23）：`utils/directorRestPose.ts` 是从素模导出的生成文件，这里直接读 GLB 的节点表逐项核对。
 * ⚠️ 重建素模（`director-assets.mjs models`）而忘了重跑 `rest` 的话，参考图转姿势按旧骨骼换算，
 *    角色摆出来的姿势歪掉却不报错 —— 这条测试让它在门禁上红。
 */
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import { DIRECTOR_REST_BONES, DIRECTOR_REST_POSE } from "./directorRestPose";
import { DIRECTOR_RIG_MODELS } from "./directorRig";

type GltfNode = { name?: string; rotation?: number[]; translation?: number[]; scale?: number[]; matrix?: number[]; children?: number[] };

function glbNodes(model: string): GltfNode[] {
  const bytes = readFileSync(fileURLToPath(new URL(`../public/director/models/${model}.glb`, import.meta.url)));
  const length = bytes.readUInt32LE(12);
  return (JSON.parse(bytes.subarray(20, 20 + length).toString("utf8")) as { nodes: GltfNode[] }).nodes;
}

describe("素模 rest 姿势表（DRT-23）", () => {
  it("四个素模都有表，每根骨骼的局部旋转与位置与 GLB 一致", () => {
    expect(Object.keys(DIRECTOR_REST_POSE).sort()).toEqual([...DIRECTOR_RIG_MODELS].sort());
    for (const model of DIRECTOR_RIG_MODELS) {
      const nodes = glbNodes(model);
      for (const name of DIRECTOR_REST_BONES) {
        const node = nodes.find(n => n.name === name);
        const rest = DIRECTOR_REST_POSE[model][name];
        expect(node, `${model} 缺少 ${name}`).toBeDefined();
        (node?.rotation ?? [0, 0, 0, 1]).forEach((v, i) => expect(rest?.q[i], `${model}.${name}.q[${i}]`).toBeCloseTo(v, 5));
        (node?.translation ?? [0, 0, 0]).forEach((v, i) => expect(rest?.p[i], `${model}.${name}.p[${i}]`).toBeCloseTo(v, 5));
      }
    }
  });

  it("Hips 以上的节点是单位变换（换算直接在模型坐标里做）", () => {
    for (const model of DIRECTOR_RIG_MODELS) {
      const nodes = glbNodes(model);
      const parentOf = new Map<number, number>();
      nodes.forEach((node, index) => (node.children ?? []).forEach(child => parentOf.set(child, index)));
      for (let i = parentOf.get(nodes.findIndex(n => n.name === "Hips")); i !== undefined; i = parentOf.get(i)) {
        const node = nodes[i] as GltfNode;
        expect(node.matrix, `${model}：${node.name} 用了 matrix`).toBeUndefined();
        (node.rotation ?? [0, 0, 0, 1]).forEach((v, k) => expect(v).toBeCloseTo([0, 0, 0, 1][k] as number, 6));
        (node.scale ?? [1, 1, 1]).forEach(v => expect(v).toBeCloseTo(1, 6));
        (node.translation ?? [0, 0, 0]).forEach(v => expect(v).toBeCloseTo(0, 6));
      }
    }
  });
});
