import type { components } from "@tabtv/api-client";
import { describe, expect, it } from "vitest";
import type { CanvasEdge, CanvasNode } from "~/types/canvas";
import { acceptInputsOf, generationInputs } from "./canvas";
import {
  smartcutMediaUrl,
  smartcutPublishedGeneration,
  smartcutPublishedNode,
  smartcutPublishPosition,
  smartcutSourceBadge,
  smartcutSourceOf
} from "./smartcutPublish";

type RenderJob = components["schemas"]["RenderJob"];

const render = (over: Partial<RenderJob> = {}): RenderJob => ({
  id: "8001", timelineId: "9", timelineVersion: 12, status: "success", progress: 100, creditsCost: 0, outputMediaId: "7001", ...over
});

describe("发布节点（SC-F-77、SC-D-08）", () => {
  it("视频节点 + params.smartcutSource + 没有 id 的「已有产出」，两处媒体 ID 一致", () => {
    const node = smartcutPublishedNode(render(), { timelineId: "9", title: "智能剪辑 1" })!;
    expect(node).toEqual({
      nodeType: "video",
      title: "智能剪辑 1 成片",
      params: { smartcutSource: { timelineId: "9", renderId: "8001", mediaId: "7001", timelineVersion: 12 } },
      generation: { status: "success", progress: 100, outputType: "video", mediaId: "7001", outputUrl: "/api/v1/media/7001" }
    });
    expect(node.generation).not.toHaveProperty("id");
    expect(smartcutSourceOf(node.params)!.mediaId).toBe(node.generation.mediaId);
  });

  it("没有产出时不建节点（还没导完、失败、产出 ID 缺失）", () => {
    expect(smartcutPublishedNode(render({ status: "running" }), { timelineId: "9" })).toBeNull();
    expect(smartcutPublishedNode(render({ status: "failed" }), { timelineId: "9" })).toBeNull();
    expect(smartcutPublishedNode(render({ outputMediaId: undefined }), { timelineId: "9" })).toBeNull();
    expect(smartcutPublishedNode(render(), { timelineId: "" })).toBeNull();
  });

  it("摘要里的导出简报也能直接发布；标题缺省「智能剪辑 成片」", () => {
    const brief: components["schemas"]["SmartcutRenderBrief"] = { id: "8002", status: "success", progress: 100, outputMediaId: "7002", timelineVersion: 3 };
    expect(smartcutPublishedNode(brief, { timelineId: "9", title: "  " })!.title).toBe("智能剪辑 成片");
  });

  // SC-AC-28（前端部分）：发布节点连到视频节点作参考视频时，下游按媒体 ID 声明「参考」，不按不存在的生成 ID。
  it("下游 generationInputs 把发布节点当作参考（reference）", () => {
    const published = smartcutPublishedNode(render(), { timelineId: "9", title: "智能剪辑 1" })!;
    const nodes = [
      { id: "pub", type: "aitv", position: { x: 0, y: 0 }, data: { nodeType: "video", title: published.title, params: published.params, generation: published.generation } },
      { id: "v2", type: "aitv", position: { x: 400, y: 0 }, data: { nodeType: "video", title: "视频", params: { mode: "video2video" } } }
    ] as CanvasNode[];
    const edges = [{ id: "e1", source: "pub", target: "v2", data: { order: 1 } }] as CanvasEdge[];
    const inputs = generationInputs(nodes, edges, "v2");
    expect(inputs).toEqual([{ kind: "video", sourceNodeId: "pub", order: 1, referenceId: "7001" }]);
    expect(acceptInputsOf(inputs)).toEqual([{ kind: "reference", order: 1, sourceNodeId: "pub", referenceId: "7001" }]);
  });
});

describe("来源读取与角标", () => {
  it("smartcutSourceOf：形状不对一律 null", () => {
    expect(smartcutSourceOf({ smartcutSource: { timelineId: "9", renderId: "8", mediaId: "7", timelineVersion: 2 } }))
      .toEqual({ timelineId: "9", renderId: "8", mediaId: "7", timelineVersion: 2 });
    expect(smartcutSourceOf({})).toBeNull();
    expect(smartcutSourceOf(undefined)).toBeNull();
    expect(smartcutSourceOf({ smartcutSource: [] })).toBeNull();
    expect(smartcutSourceOf({ smartcutSource: { timelineId: "9", renderId: "8", mediaId: "x", timelineVersion: 2 } })).toBeNull();
    expect(smartcutSourceOf({ smartcutSource: { timelineId: "9", renderId: "8", mediaId: "7", timelineVersion: "2" } })).toBeNull();
    expect(smartcutSourceOf({ smartcutSource: { timelineId: "9", renderId: "8", mediaId: "7", timelineVersion: 0 } })).toBeNull();
  });

  it("角标：来自谁 · 第几版；时间线更新过就说「剪辑已更新」；来源删了说清楚", () => {
    const source = { timelineId: "9", renderId: "8", mediaId: "7", timelineVersion: 12 };
    expect(smartcutSourceBadge(source, { title: "智能剪辑 1", version: 12 })).toEqual({ text: "来自 智能剪辑 1 · v12", missing: false, updated: false });
    expect(smartcutSourceBadge(source, { title: "智能剪辑 1", version: 13 })).toEqual({ text: "来自 智能剪辑 1 · v12 · 剪辑已更新", missing: false, updated: true });
    expect(smartcutSourceBadge(source, null)).toEqual({ text: "来源智能剪辑已删除", missing: true, updated: false });
  });

  it("落点在智能剪辑节点右侧，多次发布纵向排开", () => {
    expect(smartcutPublishPosition({ x: 100, y: 50 })).toEqual({ x: 520, y: 50 });
    expect(smartcutPublishPosition({ x: 100, y: 50 }, 2)).toEqual({ x: 520, y: 650 });
  });

  it("媒体地址是受保护端点", () => {
    expect(smartcutMediaUrl("7001")).toBe("/api/v1/media/7001");
    expect(smartcutPublishedGeneration("7001").outputUrl).toBe("/api/v1/media/7001");
  });
});
