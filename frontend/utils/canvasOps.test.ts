import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import type { CanvasOp, JsonObject, OpActor } from "~/types/canvas";
import { applyInverse, applyOps, inverseOf } from "./canvasOps";

/**
 * 前端重放与 Go `canvas/domain.Apply` 用**同一批** fixture（AGT-10 / AGT-11）。
 *
 * ⚠️ 直接读仓库里的 `api/fixtures/canvas-ops/`，不复制一份 —— 复制的那份迟早和 Go 侧那份
 *    不一致，而两边各自都是绿的。
 */
const FIXTURE_DIR = fileURLToPath(new URL("../../api/fixtures/canvas-ops/", import.meta.url));

type Fixture = {
  description: string;
  actor: { kind: "agent" | "user" | "mcp"; userId?: string; sessionId?: string; grantId?: string; clientName?: string };
  newIds?: string[];
  before: JsonObject;
  ops: Array<JsonObject>;
  applied?: CanvasOp[];
  after?: JsonObject;
  nodeIds?: string[];
  expectError?: string;
};

const fixtures = readdirSync(FIXTURE_DIR)
  .filter(name => /^\d{3}-.*\.json$/.test(name))
  .sort()
  .map(name => ({ name, fixture: JSON.parse(readFileSync(join(FIXTURE_DIR, name), "utf8")) as Fixture }));

const actorOf = (fixture: Fixture): OpActor => ({
  kind: fixture.actor.kind,
  userId: fixture.actor.userId,
  sessionId: fixture.actor.sessionId,
  grantId: fixture.actor.grantId,
  clientName: fixture.actor.clientName
});

/**
 * 把未解析的 ops（期望整批被拒的用例只有这一份）按 `newIds` 分配 ID，变成可重放的形状。
 * 期望拒绝的用例不带批内引用以外的东西，这里只补 ID 与来源（服务端也会这样写入：
 * Agent 写 `agent`、MCP 写 `origin`，MCPT-09）。
 */
function resolveForReplay(fixture: Fixture): CanvasOp[] {
  const ids = [...(fixture.newIds ?? [])];
  return fixture.ops.map((raw) => {
    const op = JSON.parse(JSON.stringify(raw)) as CanvasOp & { ref?: string };
    delete op.ref;
    if (op.kind === "add_node" && op.node) {
      const { agent: _agent, origin: _origin, ...node } = op.node;
      op.node = fixture.actor.kind === "mcp"
        ? { ...node, id: ids.shift(), origin: { kind: "mcp", grantId: fixture.actor.grantId, clientName: fixture.actor.clientName } }
        : { ...node, id: ids.shift(), agent: { sessionId: fixture.actor.sessionId } };
      if (raw.position) op.node.position = raw.position;
      delete op.position;
    }
    return op;
  });
}

describe("共享 fixture（api/fixtures/canvas-ops）", () => {
  it("目录里至少有 AGT-10 的 6 个用例", () => {
    expect(fixtures.length).toBeGreaterThanOrEqual(6);
  });

  it("MCPT-09 的 MCP 用例都在（与 Go 读同一目录）", () => {
    const names = fixtures.map(item => item.name);
    for (const prefix of ["009-", "010-", "011-", "012-", "013-", "014-", "015-"]) {
      expect(names.some(name => name.startsWith(prefix)), prefix).toBe(true);
    }
  });

  it("共享 fixture 全部一致", () => {
    for (const { name, fixture } of fixtures) {
      if (fixture.expectError) continue;
      const result = applyOps(fixture.before, fixture.applied ?? [], actorOf(fixture));
      expect(result.error, name).toBeUndefined();
      expect(result.doc, name).toEqual(fixture.after);
      expect(result.nodeIds, name).toEqual(fixture.nodeIds ?? []);
      expect(result.skipped, name).toBe(0);
    }
  });

  it("not_owned 时不修改文档", () => {
    const cases = fixtures.filter(({ fixture }) => fixture.expectError === "not_owned");
    expect(cases.length).toBeGreaterThan(0);
    for (const { name, fixture } of cases) {
      const before = JSON.parse(JSON.stringify(fixture.before));
      const result = applyOps(fixture.before, resolveForReplay(fixture), actorOf(fixture));
      expect(result.error, name).toBe("not_owned");
      expect(result.nodeIds, name).toEqual([]);
      // 整批拒绝：前面那条 add_node 也不生效，输入文档一个字节都没被改
      expect(result.doc, name).toEqual(before);
      expect(fixture.before, name).toEqual(before);
    }
  });

  it("逆操作还原批次前的文档", () => {
    for (const { name, fixture } of fixtures) {
      if (!fixture.after) continue;
      const inverse = inverseOf(fixture.before, fixture.after);
      expect(inverse.length, name).toBeGreaterThan(0);
      expect(applyInverse(fixture.after, inverse), name).toEqual(fixture.before);
    }
  });
});

describe("重放细节", () => {
  const doc = {
    schemaVersion: 1,
    nodes: [
      { id: "n_user", type: "image", title: "用户的", position: { x: 0, y: 0 }, params: {} },
      { id: "a_1", type: "text", title: "Agent 的", position: { x: 0, y: 0 }, params: { a: 1 }, agent: { sessionId: "7", userTouched: true } }
    ],
    edges: []
  };
  const agent: OpActor = { kind: "agent", sessionId: "7" };

  it("严格模式：用户改过的 Agent 节点不再归 Agent 所有", () => {
    const result = applyOps(doc, [{ kind: "update_node", nodeId: "a_1", patch: { title: "x" } }], agent);
    expect(result.error).toBe("not_owned");
  });

  it("宽松模式：本地已删 / 用户刚改过的节点跳过，重复新增跳过，其余照常", () => {
    const result = applyOps(doc, [
      { kind: "update_node", nodeId: "a_1", patch: { title: "被跳过" } },
      { kind: "move_node", nodeId: "gone", position: { x: 1, y: 1 } },
      { kind: "add_node", node: { id: "n_user", type: "text", title: "重复" } },
      { kind: "add_edge", edge: { id: "e_x", source: "n_user", target: "gone" } },
      { kind: "add_node", node: { id: "a_2", type: "text", title: "新", params: {}, position: { x: 5, y: 5 } } }
    ], agent, { lenient: true });
    expect(result.error).toBeUndefined();
    expect(result.skipped).toBe(4);
    expect(result.nodeIds).toEqual(["a_2"]);
    const nodes = result.doc.nodes as JsonObject[];
    expect(nodes.find(node => node.id === "a_1")?.title).toBe("Agent 的");
    expect(result.doc.edges).toEqual([]);
  });

  it("未知操作整批拒绝", () => {
    expect(applyOps(doc, [{ kind: "delete_everything" }], agent).error).toBe("invalid");
    expect(applyOps(doc, [], agent).error).toBe("invalid");
  });

  it("逆操作删节点时连带删掉连到它的线并从画框成员里摘掉", () => {
    const before = { nodes: [{ id: "u", type: "image", position: { x: 0, y: 0 } }, { id: "f", type: "frame", position: { x: 0, y: 0 }, members: ["u"] }], edges: [] };
    const after = { nodes: [...before.nodes, { id: "a", type: "text", position: { x: 0, y: 0 } }], edges: [] };
    const inverse = inverseOf(before, after);
    // 批次之后用户把线连到了 Agent 节点、并把它加进了画框
    const later = {
      nodes: [before.nodes[0], { ...before.nodes[1], members: ["u", "a"] }, after.nodes[2]],
      edges: [{ id: "e", source: "u", target: "a" }]
    };
    const reverted = applyInverse(later, inverse);
    expect((reverted.nodes as JsonObject[]).map(node => node.id)).toEqual(["u", "f"]);
    expect(reverted.edges).toEqual([]);
    expect((reverted.nodes as JsonObject[])[1]!.members).toEqual(["u"]);
  });

  it("link_generation：挂上任务并报给调用方；同一任务已挂着不动；宽松模式下用户改过的照样挂、已删的跳过", () => {
    const base = {
      nodes: [
        { id: "a_1", type: "video", title: "v", position: { x: 0, y: 0 }, params: { x: 1 }, agent: { sessionId: "7", userTouched: true } },
        { id: "a_2", type: "image", title: "i", position: { x: 0, y: 0 }, params: {}, result: { id: "5", status: "success", mediaId: "m" }, agent: { sessionId: "7" } }
      ]
    };
    const result = applyOps(base, [
      { kind: "link_generation", nodeId: "a_1", patch: { generationId: "9007199254740993" } },
      { kind: "link_generation", nodeId: "a_2", patch: { generationId: "5" } },
      { kind: "link_generation", nodeId: "gone", patch: { generationId: "6" } }
    ], agent, { lenient: true });
    expect(result.error).toBeUndefined();
    expect(result.skipped).toBe(1);
    expect(result.linked).toEqual([{ nodeId: "a_1", generationId: "9007199254740993" }]);
    const nodes = result.doc.nodes as JsonObject[];
    expect(nodes[0]!.result).toEqual({ id: "9007199254740993", status: "queued", progress: 0 });
    expect(nodes[0]!.params).toEqual({ x: 1 });
    expect(nodes[1]!.result).toEqual({ id: "5", status: "success", mediaId: "m" });
  });

  it("link_generation：严格模式下非法 patch / 别的会话 / 画框拒绝", () => {
    const base = {
      nodes: [
        { id: "a_1", type: "video", position: { x: 0, y: 0 }, agent: { sessionId: "7" } },
        { id: "a_o", type: "video", position: { x: 0, y: 0 }, agent: { sessionId: "8" } },
        { id: "a_f", type: "frame", position: { x: 0, y: 0 }, members: [], agent: { sessionId: "7" } }
      ]
    };
    const link = (nodeId: string, patch: JsonObject) => applyOps(base, [{ kind: "link_generation", nodeId, patch }], agent).error;
    expect(link("a_o", { generationId: "1" })).toBe("not_owned");
    expect(link("a_f", { generationId: "1" })).toBe("invalid");
    expect(link("a_1", { generationId: 1 })).toBe("invalid");
    expect(link("a_1", { generationId: "0" })).toBe("invalid");
    expect(link("a_1", { generationId: "1", status: "success" })).toBe("invalid");
    expect(link("a_1", { generationId: "1" })).toBeUndefined();
  });
});
