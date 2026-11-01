import { describe, expect, it } from "vitest";
import type { StoredCanvasNode } from "~/types/canvas";
import { canvasNodeToStored, storedToCanvasNode } from "./canvasDocument";
import { actorLabel, createdByActor, isUserTouched, nodeOrigin, originLabel, ownedByActor, withTouched } from "./canvasOrigin";

/**
 * 来源字段的新旧兼容（MCPT-09）。与 Go `canvas/domain.NodeOrigin` 同语义：
 * 有 origin 以它为准，只有 agent 视为 kind=agent，userTouched 两处取「或」。
 */
describe("nodeOrigin", () => {
  it("旧agent与新origin解析一致", () => {
    const legacy = nodeOrigin({ agent: { sessionId: "42", stepId: "s" } });
    const modern = nodeOrigin({ origin: { kind: "agent", sessionId: "42" } });
    const both = nodeOrigin({ agent: { sessionId: "42" }, origin: { kind: "agent", sessionId: "42" } });
    expect(legacy).toEqual({ kind: "agent", sessionId: "42", userTouched: false });
    expect(modern).toMatchObject({ kind: "agent", sessionId: "42", userTouched: false });
    expect(both).toMatchObject({ kind: "agent", sessionId: "42", userTouched: false });
    const agent = { kind: "agent" as const, sessionId: "42" };
    for (const node of [{ agent: { sessionId: "42" } }, { origin: { kind: "agent", sessionId: "42" } }, { agent: { sessionId: 42 } }]) {
      expect(ownedByActor(node, agent)).toBe(true);
    }
    // 两个都在时以 origin 为准
    expect(nodeOrigin({ agent: { sessionId: "42" }, origin: { kind: "mcp", grantId: "5001", clientName: "Claude Code" } }))
      .toEqual({ kind: "mcp", sessionId: undefined, grantId: "5001", clientName: "Claude Code", userTouched: false });
    expect(nodeOrigin({})).toBeNull();
    expect(nodeOrigin({ agent: "坏值", origin: [1] })).toBeNull();
  });

  it("userTouched 两处取或", () => {
    expect(isUserTouched({ agent: { sessionId: "42", userTouched: true }, origin: { kind: "agent", sessionId: "42" } })).toBe(true);
    expect(isUserTouched({ agent: { sessionId: "42" }, origin: { kind: "mcp", grantId: "1", userTouched: true } })).toBe(true);
    expect(ownedByActor({ agent: { sessionId: "42", userTouched: true }, origin: { kind: "agent", sessionId: "42" } }, { kind: "agent", sessionId: "42" })).toBe(false);
  });

  it("Agent 与 MCP 互不可改；link 只看主体不看 userTouched", () => {
    const agent = { kind: "agent" as const, sessionId: "42" };
    const mcp = { kind: "mcp" as const, grantId: "5001" };
    const mcpNode = { origin: { kind: "mcp", grantId: "5001" } };
    const agentNode = { agent: { sessionId: "42" } };
    expect(ownedByActor(mcpNode, mcp)).toBe(true);
    expect(ownedByActor(mcpNode, agent)).toBe(false);
    expect(ownedByActor(agentNode, mcp)).toBe(false);
    expect(ownedByActor(mcpNode, { kind: "mcp", grantId: "5002" })).toBe(false);
    expect(ownedByActor({}, mcp)).toBe(false);
    const touched = { origin: { kind: "mcp", grantId: "5001", userTouched: true } };
    expect(ownedByActor(touched, mcp)).toBe(false);
    expect(createdByActor(touched, mcp)).toBe(true);
    expect(createdByActor(touched, { kind: "mcp", grantId: null })).toBe(false);
  });

  it("往返不丢未知字段", () => {
    const stored: StoredCanvasNode = {
      id: "a_m1",
      type: "image",
      title: "外部",
      position: { x: 1, y: 2 },
      size: { w: 330, h: 180 },
      params: { seed: 1 },
      agent: { sessionId: "42", futureAgentKey: "keep" } as StoredCanvasNode["agent"],
      origin: { kind: "mcp", grantId: "5001", clientName: "Claude Code", future: { nested: [1, { deep: true }] } },
      unknownTop: "x"
    };
    const node = storedToCanvasNode(JSON.parse(JSON.stringify(stored)), { index: 0, width: 330, params: { seed: 1 } });
    expect(node.data.origin).toEqual(stored.origin);
    expect(node.data.stored).toEqual({ unknownTop: "x" });
    const back = canvasNodeToStored(node, { w: 330, h: 180 });
    expect(JSON.parse(JSON.stringify(back))).toEqual(stored);
    // 只有旧 agent 的节点：不凭空多出 origin（不在打开画布时迁移）
    const legacy = canvasNodeToStored(storedToCanvasNode({ id: "a_1", type: "text", position: { x: 0, y: 0 }, params: {}, agent: { sessionId: "42" } }, { index: 0, width: 330, params: {} }), { w: 330, h: 180 });
    expect(legacy.origin).toBeUndefined();
    expect(legacy.agent).toEqual({ sessionId: "42" });
    // 非对象的 origin 不被认领，但原样往返
    const odd = canvasNodeToStored(storedToCanvasNode({ id: "x", type: "text", position: { x: 0, y: 0 }, params: {}, origin: "future-string" as unknown as StoredCanvasNode["origin"] }, { index: 0, width: 330, params: {} }), { w: 330, h: 180 });
    expect(odd.origin).toBe("future-string");
  });

  it("markTouched对两种来源都生效", () => {
    expect(withTouched({ agent: { sessionId: "42" } })).toEqual({ agent: { sessionId: "42", userTouched: true } });
    expect(withTouched({ origin: { kind: "mcp", grantId: "1", future: 1 } })).toEqual({ origin: { kind: "mcp", grantId: "1", future: 1, userTouched: true } });
    expect(withTouched({ agent: { sessionId: "42" }, origin: { kind: "mcp", grantId: "1" } })).toEqual({
      agent: { sessionId: "42", userTouched: true },
      origin: { kind: "mcp", grantId: "1", userTouched: true }
    });
    // 一处已标、另一处没标：补齐没标的那处
    expect(withTouched({ agent: { sessionId: "42", userTouched: true }, origin: { kind: "mcp", grantId: "1" } })?.origin).toEqual({ kind: "mcp", grantId: "1", userTouched: true });
    expect(withTouched({ agent: { sessionId: "42", userTouched: true } })).toBeNull();
    expect(withTouched({})).toBeNull();
  });

  it("角标与提示文案按来源", () => {
    expect(originLabel(nodeOrigin({ agent: { sessionId: "42" } }))).toEqual({ mark: "✦", text: "", title: "由 Agent 创建" });
    expect(originLabel(nodeOrigin({ agent: { sessionId: "42", userTouched: true } }))?.mark).toBe("✧");
    expect(originLabel(nodeOrigin({ origin: { kind: "mcp", grantId: "1", clientName: "Claude Code", userTouched: true } })))
      .toEqual({ mark: "⌘", text: "Claude Code", title: "由外部 AI 客户端创建（Claude Code） · 已被你修改" });
    expect(originLabel(nodeOrigin({ origin: { kind: "mcp", grantId: "1" } }))?.text).toBe("外部 AI 客户端");
    expect(originLabel(null)).toBeNull();
    expect(actorLabel("mcp", "Claude Code")).toBe("Claude Code");
    expect(actorLabel("mcp", null)).toBe("外部 AI 客户端");
    expect(actorLabel("agent")).toBe("Agent");
    expect(actorLabel("user")).toBe("协作者");
  });
});
