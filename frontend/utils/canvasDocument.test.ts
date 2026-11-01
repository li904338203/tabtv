import { describe, expect, it } from "vitest";
import type { StoredCanvasEdge, StoredCanvasNode } from "~/types/canvas";
import { canvasEdgeToStored, canvasNodeToStored, documentExtras, storedToCanvasEdge, storedToCanvasNode } from "./canvasDocument";

const json = <T>(value: T): T => JSON.parse(JSON.stringify(value)) as T;

function roundTripNode(node: StoredCanvasNode) {
  const params = { ...(node.params ?? {}) };
  const hydrated = storedToCanvasNode(json(node), { index: 0, params, width: node.size?.w ?? 330 });
  return json(canvasNodeToStored(hydrated, { w: Number(hydrated.width), h: node.size?.h ?? 180 }));
}

describe("落库形状 ↔ 运行时形状", () => {
  it("节点的未知字段与 agent 原样往返", () => {
    const node: StoredCanvasNode = {
      id: "a_x_1",
      type: "image",
      title: "封面",
      position: { x: 1, y: 2 },
      size: { w: 330, h: 180 },
      params: { prompt: "猫", nested: { keep: [1, 2] } },
      result: { id: "g1", status: "success", mediaId: "m1" },
      agent: { sessionId: "s1", stepId: "st", sourceDocId: "d1", userTouched: false },
      foo: 1,
      future: { deep: { list: ["a"] } }
    };
    expect(roundTripNode(node)).toEqual(node);
  });

  it("已知键以运行时为准，未知键不覆盖它们", () => {
    const hydrated = storedToCanvasNode(
      { id: "n1", type: "text", title: "旧", position: { x: 0, y: 0 }, foo: 1 },
      { index: 0, params: {}, width: 330 }
    );
    hydrated.data.title = "新";
    const stored = canvasNodeToStored(hydrated, { w: 330, h: 180 });
    expect(stored.title).toBe("新");
    expect(stored.foo).toBe(1);
  });

  it("result 仍剥掉 previewUrl / outputText", () => {
    const hydrated = storedToCanvasNode(
      { id: "n1", type: "image", position: { x: 0, y: 0 }, result: { id: "g", previewUrl: "blob:x", outputText: "正文" } },
      { index: 0, params: {}, width: 330 }
    );
    expect(json(canvasNodeToStored(hydrated, { w: 330, h: 180 })).result).toEqual({ id: "g" });
  });

  it("没有未知字段的节点不背空对象", () => {
    const hydrated = storedToCanvasNode({ id: "n1", type: "text", position: { x: 0, y: 0 } }, { index: 0, params: {}, width: 330 });
    expect("stored" in hydrated.data).toBe(false);
    expect("agent" in hydrated.data).toBe(false);
  });

  it("连线的未知字段原样往返", () => {
    const edge: StoredCanvasEdge = { id: "e1", source: "a", sourceHandle: "out", target: "b", targetHandle: "in", order: 2, operation: "img2video", bar: 2 };
    expect(json(canvasEdgeToStored(storedToCanvasEdge(json(edge))))).toEqual(edge);
  });

  it("文档顶层只取未知键", () => {
    expect(documentExtras({ schemaVersion: 1, viewport: {}, nodes: [], edges: [], baz: 3 })).toEqual({ baz: 3 });
    expect(documentExtras(null)).toEqual({});
  });
});
