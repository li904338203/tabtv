import type { components } from "@tabtv/api-client";
import { describe, expect, it } from "vitest";
import {
  directorCaptureSummaries,
  directorConvertToPlain,
  directorMirrorParams,
  directorPublishPositions,
  directorPublishedNode,
  directorReplacePublished,
  directorSourceOf,
  directorKindNote
} from "./directorPublish";
import type { DirectorScene } from "./directorScene";

type MediaRecord = components["schemas"]["DirectorMediaRecord"];

const record = (id: string, cameraId: string | null, mediaId: string, extra: Partial<MediaRecord> = {}): MediaRecord => ({
  id, sceneId: "9001", kind: "capture", mediaId, width: 1280, height: 720, durationMs: null, fps: null,
  cameraId, cameraName: cameraId ? `机位 ${cameraId.slice(-1)}` : null, camera: null, sceneVersion: 4,
  renderFingerprint: `fp-${id}`, hidden: false, createdAt: "2026-10-26T03:00:00Z", ...extra
});
const camera = (id: string, name: string) => ({
  id, name, kind: "camera" as const,
  transform: { position: { x: 0, y: 1.6, z: 4 }, rotation: { x: 0, y: 0, z: 0 }, scale: { x: 1, y: 1, z: 1 } }
});
const scene: DirectorScene = {
  schemaVersion: 1,
  outputCameraId: "cam1",
  objects: [camera("cam2", "机位 2"), camera("cam1", "机位 1"), {
    id: "char1", name: "角色 1", kind: "character",
    transform: { position: { x: 0, y: 0, z: 0 }, rotation: { x: 0, y: 0, z: 0 }, scale: { x: 1, y: 1, z: 1 } }
  }]
};

describe("截图回写节点（DR-F-74）", () => {
  it("directorMirrorParams：截输出机位更新封面与输出", () => {
    const params = { sceneId: "9001", prompt: "留着" };
    const next = directorMirrorParams(params, [record("801", "cam1", "7001")], scene);
    expect(next).toEqual({
      sceneId: "9001",
      prompt: "留着",
      outputMediaId: "7001",
      cover: "/api/v1/media/7001",
      captures: [{ cameraId: "cam1", cameraName: "机位 1", recordId: "801", mediaId: "7001", width: 1280, height: 720, renderFingerprint: "fp-801" }]
    });
    // 入参不变
    expect(params).toEqual({ sceneId: "9001", prompt: "留着" });
  });

  it("directorMirrorParams：截其他机位只更新 captures", () => {
    const before = directorMirrorParams({}, [record("801", "cam1", "7001")], scene);
    const next = directorMirrorParams(before, [record("802", "cam2", "7002")], scene, { shotTextOf: id => (id === "cam2" ? "中景 · 平视" : undefined) });
    expect(next.outputMediaId).toBe("7001");
    expect(next.cover).toBe("/api/v1/media/7001");
    // 按场景里机位的顺序（cam2 在前），没截到的机位沿用原来那条
    expect((next.captures as Array<{ cameraId: string; shotText?: string }>).map(c => [c.cameraId, c.shotText])).toEqual([["cam2", "中景 · 平视"], ["cam1", undefined]]);
  });

  it("directorMirrorParams：每个机位只留最新一条", () => {
    const next = directorMirrorParams({}, [
      record("803", "cam1", "7003", { createdAt: "2026-10-26T03:00:02Z" }),
      record("801", "cam1", "7001", { createdAt: "2026-10-26T03:00:00Z" }),
      record("804", "cam1", "7004", { createdAt: "2026-10-26T03:00:02Z" })
    ], scene);
    // 时间相同时排在后面的赢；输出与封面跟着最新那张
    expect(directorCaptureSummaries(next).map(c => c.recordId)).toEqual(["804"]);
    expect(next.outputMediaId).toBe("7004");
  });

  it("只认截图记录；场景里已删掉的机位去掉；一条截图都没有时原样返回", () => {
    const params = { captures: [{ cameraId: "gone", cameraName: "旧机位", recordId: "1", mediaId: "1", width: 1, height: 1, renderFingerprint: "x" }] };
    expect(directorMirrorParams(params, [record("805", "cam1", "7005", { kind: "depth" })], scene)).toBe(params);
    const next = directorMirrorParams(params, [record("806", "cam2", "7006")], scene);
    expect(directorCaptureSummaries(next).map(c => c.cameraId)).toEqual(["cam2"]);
    expect(next.outputMediaId).toBeUndefined();
  });

  it("读 captures 时丢掉形状不对的条目", () => {
    expect(directorCaptureSummaries({ captures: [null, "x", { cameraId: "c" }, { cameraId: "c", mediaId: "abc" }, { cameraId: "c", mediaId: "9" }] }))
      .toEqual([{ cameraId: "c", cameraName: "", recordId: "", mediaId: "9", width: null, height: null, renderFingerprint: null }]);
    expect(directorCaptureSummaries({})).toEqual([]);
  });
});

describe("发布到画布（DR-F-80）", () => {
  it("directorPublishedNode：result 没有 id 且 outputUrl 为媒体地址", () => {
    const node = directorPublishedNode(record("801", "cam1", "7001"))!;
    expect(node.nodeType).toBe("image");
    expect(node.title).toBe("机位 1 截图");
    expect(node.generation).toEqual({ status: "success", progress: 100, outputType: "image", mediaId: "7001", outputUrl: "/api/v1/media/7001" });
    expect("id" in node.generation).toBe(false);
  });

  it("directorPublishedNode：result 与 directorSource 的媒体 ID 一致", () => {
    const node = directorPublishedNode(record("807", "cam2", "7007", { kind: "depth" }))!;
    expect(node.title).toBe("机位 2 深度图");
    expect(node.params.directorSource).toEqual({ kind: "depth", sceneId: "9001", recordId: "807", mediaId: "7007", cameraId: "cam2", cameraName: "机位 2", renderFingerprint: "fp-807" });
    expect(directorSourceOf(node.params)?.mediaId).toBe(node.generation.mediaId);
    // 预演建视频节点；泼溅文件不发布；没有机位名时写「机位」
    const preview = directorPublishedNode(record("808", "cam1", "7008", { kind: "preview", cameraName: null }))!;
    expect([preview.nodeType, preview.title, preview.generation.outputType]).toEqual(["video", "机位 预演", "video"]);
    expect(directorPublishedNode(record("809", null, "7009", { kind: "splat" }))).toBeNull();
  });

  it("directorReplacePublished：只替换目标节点", () => {
    const data = {
      params: { prompt: "用户写的", directorSource: { kind: "capture", sceneId: "9001", recordId: "801", mediaId: "7001" } },
      generation: { status: "success", mediaId: "7001", outputUrl: "/api/v1/media/7001", previewUrl: "blob:old" }
    };
    const next = directorReplacePublished(data, record("810", "cam1", "7010"));
    expect(next.params.prompt).toBe("用户写的");
    expect(directorSourceOf(next.params)?.recordId).toBe("810");
    // 旧的 previewUrl 指着旧图，不能留
    expect(next.generation).toEqual({ status: "success", progress: 100, outputType: "image", mediaId: "7010", outputUrl: "/api/v1/media/7010" });
    expect(data.params.directorSource.recordId).toBe("801");
  });

  it("directorConvertToPlain：去掉 directorSource 且不动其他参数", () => {
    const params = { prompt: "保留", preset: "whitebox_to_real", directorSource: { kind: "capture", mediaId: "7001" } };
    expect(directorConvertToPlain(params)).toEqual({ prompt: "保留", preset: "whitebox_to_real" });
    expect(params.directorSource).toBeDefined();
  });

  it("directorPublishPositions：在来源右侧纵向排开", () => {
    expect(directorPublishPositions({ x: 100.4, y: 50 }, 3)).toEqual([{ x: 520, y: 50 }, { x: 520, y: 350 }, { x: 520, y: 650 }]);
    expect(directorPublishPositions({ x: 0, y: 0 }, 0)).toEqual([]);
  });

  it("来源读取丢掉形状不对的", () => {
    expect(directorSourceOf({})).toBeNull();
    expect(directorSourceOf({ directorSource: { kind: "splat", mediaId: "1" } })).toBeNull();
    expect(directorSourceOf({ directorSource: { kind: "capture", mediaId: "x" } })).toBeNull();
  });
  it("mirrorParams 截图摘要带 shotText；发布节点的来源带发布那张图的镜头文字", () => {
    const next = directorMirrorParams({}, [record("801", "cam1", "7001")], scene, { shotTextOf: () => "全景 · 低角度仰拍 · 35mm" });
    expect((next.captures as Array<{ shotText?: string }>)[0]?.shotText).toBe("全景 · 低角度仰拍 · 35mm");
    const node = directorPublishedNode(record("801", "cam1", "7001"), "近景 · 平视 · 85mm");
    expect(directorSourceOf(node!.params)?.shotText).toBe("近景 · 平视 · 85mm");
    // 没有文字时不带这个字段（不写空串）
    expect("shotText" in (directorSourceOf(directorPublishedNode(record("802", "cam1", "7002"))!.params) ?? {})).toBe(false);
    const replaced = directorReplacePublished({ params: { prompt: "x" } }, record("803", "cam1", "7003"), "特写 · 平视 · 50mm");
    expect(directorSourceOf(replaced.params)?.shotText).toBe("特写 · 平视 · 50mm");
  });
  it("控制图的发布节点注明类型（当参考图使用），截图与预演不注明", () => {
    expect(directorKindNote("depth")).toContain("深度图（当参考图使用）");
    expect(directorKindNote("pose")).toContain("骨架图（当参考图使用）");
    expect([directorKindNote("capture"), directorKindNote("preview")]).toEqual(["", ""]);
    const depth = directorPublishedNode(record("810", "cam1", "7010", { kind: "depth" }));
    expect([depth?.nodeType, depth?.title]).toEqual(["image", "机位 1 深度图"]);
  });
});
