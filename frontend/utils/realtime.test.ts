import { describe, expect, it } from "vitest";
import { eventTarget, projectTopic, workspaceTopic } from "./realtime";

describe("实时事件的 topic", () => {
  it("生成事件按工作区投递，画布协作按项目", () => {
    expect(workspaceTopic("96209082733559810")).toBe("workspace:96209082733559810");
    expect(projectTopic("7")).toBe("project:7");
  });
});

describe("生成事件的处理", () => {
  const tracked = new Map([["701", "node-a"]]);
  const update = (over: Record<string, unknown> = {}) => ({
    type: "generation.update", generationId: "701", projectId: "p1", nodeId: "node-from-event", status: "success", ...over,
  });

  it("本页在跟踪的任务：落到本页记下的那个节点（不信事件里的 nodeId）", () => {
    expect(eventTarget(update(), "p1", tracked)).toEqual({ generationId: "701", nodeId: "node-a" });
  });

  it("已经落定、不再跟踪的任务：晚到的旧事件丢掉，不把完成的节点打回运行中", () => {
    expect(eventTarget(update({ generationId: "700", status: "running" }), "p1", tracked)).toBeNull();
  });

  it("同一工作区别的项目的事件：丢掉", () => {
    expect(eventTarget(update({ projectId: "p2" }), "p1", tracked)).toBeNull();
  });

  it("不是生成事件、或缺 generationId：丢掉", () => {
    expect(eventTarget({ type: "workflow.update", generationId: "701", projectId: "p1" }, "p1", tracked)).toBeNull();
    expect(eventTarget({ type: "generation.update", projectId: "p1" }, "p1", tracked)).toBeNull();
    expect(eventTarget(null, "p1", tracked)).toBeNull();
  });
});
