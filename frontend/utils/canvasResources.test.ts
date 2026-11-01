import { describe, expect, it } from "vitest";
import { canvasAddMenuBadge, canvasAddMenuTypes, existingOutputGeneration, historyNodeTitle, historyOutputs, resourceNodePositions } from "./canvasResources";

describe("canvasAddMenuTypes", () => {
  it("去掉组 / 风格 / 特效 / 文档，按参考次序排，新类型排最后", () => {
    const types = ["text", "image", "video", "audio", "script", "group", "style", "effect", "document", "director", "smartcut", "future"].map(code => ({ code }));
    expect(canvasAddMenuTypes(types).map(t => t.code)).toEqual(["text", "script", "image", "video", "director", "smartcut", "audio", "future"]);
  });
  it("没有配置时是空列表", () => {
    expect(canvasAddMenuTypes(undefined)).toEqual([]);
  });
  it("只有导演台挂标签", () => {
    expect(["text", "script", "director", "smartcut"].map(canvasAddMenuBadge)).toEqual(["", "", "3D", ""]);
  });
});

describe("historyOutputs", () => {
  const items = [
    { id: "1", projectId: "9", status: "success", modelCode: "m", createdAt: "t1", outputs: [{ index: 0, type: "image", mediaId: "11" }, { index: 1, type: "image", mediaId: "12" }] },
    { id: "2", projectId: "9", status: "failed", createdAt: "t2", outputs: [{ index: 0, type: "image", mediaId: "21" }] },
    { id: "3", projectId: "8", status: "success", createdAt: "t3", outputs: [{ index: 0, type: "text" }, { index: 1, type: "video", mediaId: "31" }, { index: 2, type: "audio" }] }
  ];
  it("成功的媒体产物每个一项，不只取第一个", () => {
    expect(historyOutputs(items).map(o => o.key)).toEqual(["1:0", "1:1", "3:1"]);
  });
  it("按类型筛", () => {
    expect(historyOutputs(items, "video").map(o => o.mediaId)).toEqual(["31"]);
    expect(historyOutputs(items, "audio")).toEqual([]);
  });
});

describe("资源节点", () => {
  it("已有产出不带生成 ID", () => {
    const g = existingOutputGeneration("image", "77");
    expect(g).toEqual({ status: "success", progress: 100, outputType: "image", mediaId: "77", outputUrl: "/api/v1/media/77" });
    expect(g.id).toBeUndefined();
  });
  it("落点每行 3 个", () => {
    expect(resourceNodePositions({ x: 0, y: 0 }, 4)).toEqual([{ x: 0, y: 0 }, { x: 430, y: 0 }, { x: 860, y: 0 }, { x: 0, y: 380 }]);
  });
  it("标题", () => {
    expect(historyNodeTitle({ type: "video", modelCode: "seedance" })).toBe("视频 · seedance");
    expect(historyNodeTitle({ type: "audio" })).toBe("音频");
  });
});
