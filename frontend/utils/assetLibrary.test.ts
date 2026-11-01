import { describe, expect, it } from "vitest";
import { libraryCanvasResource, libraryThumbMediaId, libraryTree, toggleTag } from "./assetLibrary";

const item = (id: string, folderId: string | null, extra: Record<string, unknown> = {}) => ({
  id, folderId, generationId: "1", outputIndex: 0, kind: "image", title: id, tags: [], rating: 0, projectId: null, projectName: "",
  nodeId: "", creatorName: "", modelCode: "", modelName: "", prompt: "", generatedAt: "", savedAt: "", ...extra
}) as never;

describe("资产树", () => {
  it("顶层在前、文件夹在后；文件夹已不在的资产当顶层", () => {
    const folders = [{ id: "f1", name: "历史资产", itemCount: 1, createdAt: "" }];
    const tree = libraryTree(folders, [item("a", null), item("b", "f1"), item("c", "gone")]);
    expect(tree.root.map(i => (i as { id: string }).id)).toEqual(["a", "c"]);
    expect(tree.folders[0]!.items.map(i => (i as { id: string }).id)).toEqual(["b"]);
  });
  it("标签选中 / 取消", () => {
    expect(toggleTag(["角色"], "场景")).toEqual(["角色", "场景"]);
    expect(toggleTag(["角色", "场景"], "角色")).toEqual(["场景"]);
  });
  it("缩略图与添加到画布", () => {
    expect(libraryThumbMediaId({ kind: "video", mediaId: "1", posterMediaId: "2" })).toBe("2");
    expect(libraryThumbMediaId({ kind: "text" })).toBeUndefined();
    expect(libraryCanvasResource(item("t", null, { kind: "text", text: "# 剧本", title: "剧本" }))).toEqual({ type: "text", text: "# 剧本", title: "剧本" });
    expect(libraryCanvasResource(item("v", null, { kind: "video", mediaId: "9", title: "片" }))).toEqual({ type: "video", mediaId: "9", title: "片" });
    expect(libraryCanvasResource(item("x", null, { kind: "image" }))).toBeNull();
  });
});
