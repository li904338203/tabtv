import { describe, expect, it } from "vitest";
import { addManualRef, nodeCandidates, removeRef, serializeRefs, syncSelectedRefs, type ComposerRef } from "./composerRefs";

const nodes: Record<string, { label: string; thumbMediaId?: string }> = {
  a: { label: "shot_01", thumbMediaId: "m1" },
  b: { label: "shot_02" },
  c: { label: "角色图" }
};
const lookup = (id: string) => (nodes[id] ? { id, ...nodes[id] } : undefined);

describe("选中即引用", () => {
  it("选中节点生成自动胶囊", () => {
    const refs = syncSelectedRefs([], ["a", "b", "c"], lookup);
    expect(refs.map(ref => [ref.id, ref.auto])).toEqual([["a", true], ["b", true], ["c", true]]);
    expect(refs[0]).toMatchObject({ kind: "node", label: "shot_01", thumbMediaId: "m1" });
  });

  it("取消选中只移除自动胶囊", () => {
    let refs = syncSelectedRefs([], ["a", "b", "c"], lookup);
    refs = addManualRef(refs, { kind: "media", id: "900", label: "参考图" });
    refs = syncSelectedRefs(refs, ["a", "c"], lookup);
    expect(refs.map(ref => ref.id)).toEqual(["a", "c", "900"]);
    refs = syncSelectedRefs(refs, [], lookup);
    expect(refs.map(ref => ref.id)).toEqual(["900"]);
  });

  it("手动与自动重复时保留手动", () => {
    let refs = syncSelectedRefs([], ["a"], lookup);
    refs = addManualRef(refs, { kind: "node", id: "a", label: "shot_01" });
    expect(refs).toHaveLength(1);
    expect(refs[0]!.auto).toBe(false);
    // 取消选中后手动的那一个还在；再选中也不重复加
    refs = syncSelectedRefs(refs, [], lookup);
    refs = syncSelectedRefs(refs, ["a"], lookup);
    expect(refs).toHaveLength(1);
    expect(refs[0]!.auto).toBe(false);
  });

  it("查不到的节点不加", () => {
    expect(syncSelectedRefs([], ["ghost"], lookup)).toEqual([]);
  });
});

describe("serializeRefs", () => {
  it("序列化只含 kind 与 id", () => {
    const refs: ComposerRef[] = [
      { kind: "node", id: "a", label: "shot_01", auto: true, thumbMediaId: "m1" },
      { kind: "media", id: "900", label: "参考图", auto: false, thumbMediaId: "900" },
      { kind: "media", id: "900", label: "重复", auto: false }
    ];
    const out = serializeRefs(refs);
    expect(out).toEqual([{ kind: "node", id: "a" }, { kind: "media", id: "900" }]);
    for (const ref of out) expect(Object.keys(ref).sort()).toEqual(["id", "kind"]);
    expect(JSON.stringify(out)).not.toMatch(/url|objectKey|label|thumb/i);
  });

  it("不带 slot 时输出与原来逐字一致（创造页回归，MKT-T-12）", () => {
    const refs: ComposerRef[] = [
      { kind: "node", id: "a", label: "shot_01", auto: true },
      { kind: "media", id: "900", label: "参考图", auto: false },
      { kind: "asset", id: "5", label: "唐僧", auto: false, hint: "character" }
    ];
    expect(JSON.stringify(serializeRefs(refs))).toBe('[{"kind":"node","id":"a"},{"kind":"media","id":"900"},{"kind":"asset","id":"5"}]');
  });

  it("带 slot 时另带 slot 与 order，去重按 slot 分开", () => {
    const refs: ComposerRef[] = [
      { kind: "media", id: "1", label: "主图", auto: false, slot: "product", order: 1 },
      { kind: "media", id: "1", label: "重复", auto: false, slot: "product", order: 2 },
      { kind: "media", id: "1", label: "普通附件", auto: false },
      { kind: "asset", id: "5", label: "唐僧", auto: false, slot: "character", order: 1, thumbMediaId: "77" }
    ];
    expect(serializeRefs(refs)).toEqual([
      { kind: "media", id: "1", slot: "product", order: 1 },
      { kind: "media", id: "1" },
      { kind: "asset", id: "5", slot: "character", order: 1 }
    ]);
    expect(JSON.stringify(serializeRefs(refs))).not.toMatch(/url|label|thumb/i);
  });

  it("removeRef 按 kind+id 移除", () => {
    const refs = addManualRef([], { kind: "asset", id: "5", label: "唐僧" });
    expect(removeRef(refs, { kind: "asset", id: "5" })).toEqual([]);
  });
});

describe("nodeCandidates", () => {
  it("按最近创建排序、排除画框、按标题过滤", () => {
    const list = [
      { id: "n1", data: { nodeType: "image", title: "古树" } },
      { id: "f1", data: { nodeType: "frame", title: "画框" } },
      { id: "n2", data: { nodeType: "video", title: "", generation: { mediaId: "77" } } }
    ];
    expect(nodeCandidates(list, "").map(item => item.id)).toEqual(["n2", "n1"]);
    expect(nodeCandidates(list, "").at(0)).toMatchObject({ label: "视频节点", thumbMediaId: "77" });
    expect(nodeCandidates(list, "古").map(item => item.id)).toEqual(["n1"]);
  });
});
