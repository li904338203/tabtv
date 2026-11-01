import { describe, expect, it } from "vitest";
import { directorIsStale, directorNodeStale, directorPublishedStale, directorStaleText } from "./directorStale";

const summary = (renderFingerprint: string, outputCameraId: string | null = "cam1") => ({ renderFingerprint, outputCameraId });
const capture = (cameraId: string, renderFingerprint: string | null) => ({ cameraId, cameraName: cameraId, recordId: "1", mediaId: "7001", width: 1, height: 1, renderFingerprint });

describe("截图过期（DR-F-83）", () => {
  it("directorPublishedStale：指纹不同为过期", () => {
    expect(directorPublishedStale({ renderFingerprint: "fp-1" }, summary("fp-2"))).toBe(true);
    expect(directorPublishedStale({ renderFingerprint: "fp-2" }, summary("fp-2"))).toBe(false);
    // 拿不准时不报过期：没有摘要、记录上没有指纹、没有来源
    expect(directorPublishedStale({ renderFingerprint: "fp-1" }, undefined)).toBe(false);
    expect(directorPublishedStale({ renderFingerprint: null }, summary("fp-2"))).toBe(false);
    expect(directorPublishedStale(null, summary("fp-2"))).toBe(false);
    expect(directorIsStale("a", { renderFingerprint: "" })).toBe(false);
  });

  it("directorNodeStale：只看输出机位", () => {
    const params = { outputMediaId: "7001", captures: [capture("cam1", "fp-new"), capture("cam2", "fp-old")] };
    // 别的机位的截图旧了不影响下游
    expect(directorNodeStale(params, summary("fp-new"))).toBe(false);
    expect(directorNodeStale(params, summary("fp-newer"))).toBe(true);
    // 输出机位换成了还没截过图的机位：节点上挂的是旧机位的画面
    expect(directorNodeStale(params, summary("fp-new", "cam3"))).toBe(true);
    // 从没截过图：不算过期（卡片显示「还没有截图」）
    expect(directorNodeStale({}, summary("fp-new", "cam3"))).toBe(false);
    // 场景没有输出机位、没有摘要时不报
    expect(directorNodeStale(params, summary("x", null))).toBe(false);
    expect(directorNodeStale(params, undefined)).toBe(false);
  });

  it("directorStaleText：机位已删除时给改用输出机位", () => {
    expect(directorStaleText({ stale: true, cameraExists: false })).toEqual({ text: "该机位已删除", action: "use_output" });
    expect(directorStaleText({ stale: true })).toEqual({ text: "场景已更新，截图可能过期", action: "recapture" });
    expect(directorStaleText({ stale: true, sourceDeleted: true })).toEqual({ text: "来源导演台已删除", action: null });
    expect(directorStaleText({ stale: false })).toBeNull();
  });

  it("directorStaleText：预演过期不给「重新截图」，说回导演台重新导出（DRT-25）", () => {
    expect(directorStaleText({ stale: true, preview: true })).toEqual({ text: "场景已更新，预演可能过期：回到导演台重新导出", action: null });
    expect(directorStaleText({ stale: false, preview: true })).toBeNull();
    expect(directorStaleText({ stale: true, preview: true, sourceDeleted: true })).toEqual({ text: "来源导演台已删除", action: null });
  });
});
