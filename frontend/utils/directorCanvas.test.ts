import { describe, expect, it } from "vitest";
import type { CanvasEdge, CanvasNode } from "~/types/canvas";
import {
  DIRECTOR_CHARACTER_TEXT,
  DIRECTOR_FIRST_FRAME_TEXT,
  DIRECTOR_RECREATED_NOTICE,
  directorAddMention,
  directorCaptureCount,
  directorCoverUrl,
  directorMentionCandidates,
  directorOpenPlan,
  directorSceneIdOf,
  directorSuggestionKey,
  directorSuggestions,
  directorSummariesAfterEvent,
  directorSummaryOf,
  directorUpstreamImages,
  type DirectorSceneSummary
} from "./directorCanvas";

const node = (id: string, nodeType: string, params: Record<string, unknown> = {}, extra: Partial<CanvasNode["data"]> = {}): CanvasNode => ({
  id,
  type: "aitv",
  position: { x: 0, y: 0 },
  data: { nodeType, title: `${nodeType} ${id}`, params, ...extra }
});
const edge = (source: string, target: string, order: number): CanvasEdge => ({ id: `${source}-${target}`, source, target, data: { order } });

describe("打开导演台时的自愈判定（DRD-43）", () => {
  it("directorOpenPlan：缺 sceneId 新建", () => {
    expect(directorOpenPlan(node("n1", "director"), null)).toEqual({ action: "create" });
    // 形状不对的 sceneId 等于没有：拿它去 GET 只会 400
    expect(directorOpenPlan(node("n1", "director", { sceneId: "abc" }), null)).toEqual({ action: "create" });
  });

  it("directorOpenPlan：404 新建并提示", () => {
    // 本项目里查不到（别的项目粘贴来的）：先按它分叉，分叉不了再新建空场景并提示
    expect(directorOpenPlan(node("n1", "director", { sceneId: "77" }), { status: "missing" }))
      .toEqual({ action: "recreate", sourceSceneId: "77", notice: DIRECTOR_RECREATED_NOTICE });
    expect(DIRECTOR_RECREATED_NOTICE).toBe("原场景不在本项目，已新建");
  });

  it("directorOpenPlan：canvasNodeId 不符时分叉", () => {
    expect(directorOpenPlan(node("copy", "director", { sceneId: "77" }), { status: "found", canvasNodeId: "orig" }))
      .toEqual({ action: "fork", sourceSceneId: "77" });
  });

  it("directorOpenPlan：一致直接打开", () => {
    expect(directorOpenPlan(node("n1", "director", { sceneId: " 77 " }), { status: "found", canvasNodeId: "n1" }))
      .toEqual({ action: "open", sceneId: "77" });
    expect(directorSceneIdOf({ sceneId: 77 })).toBe("77");
  });
});

describe("节点卡", () => {
  it("截图数取 params.captures 的条数；没有或形状不对时为 0", () => {
    expect(directorCaptureCount({ captures: [{ cameraId: "c1" }, { cameraId: "c2" }] })).toBe(2);
    expect(directorCaptureCount({ captures: "2" })).toBe(0);
    expect(directorCaptureCount({})).toBe(0);
  });

  it("封面取 640 档：cover 优先，可以是受保护地址或媒体 ID；没有时用输出截图；都没有时为空串（画占位图）", () => {
    expect(directorCoverUrl({ cover: "/api/v1/media/12", outputMediaId: "34" })).toBe("/api/v1/media/12?w=640");
    expect(directorCoverUrl({ cover: "/api/v1/media/12?w=320" })).toBe("/api/v1/media/12?w=320");
    expect(directorCoverUrl({ cover: "12" })).toBe("/api/v1/media/12?w=640");
    expect(directorCoverUrl({ outputMediaId: "34" })).toBe("/api/v1/media/34?w=640");
    // 不是受保护媒体的地址不用（不把任意 URL 当成已授权的图）
    expect(directorCoverUrl({ cover: "https://cdn.example.com/a.png" })).toBe("");
    expect(directorCoverUrl({})).toBe("");
  });
});

describe("全景上游（DR-F-02）", () => {
  it("directorUpstreamImages：取不到产出时 mediaId 为 null", () => {
    const nodes = [
      node("dir", "director"),
      node("ok", "image", {}, { generation: { id: "1", status: "success", outputType: "image", mediaId: "501" } }),
      node("copied", "image", {}, { generation: { status: "success", outputUrl: "/api/v1/media/502" } }),
      node("running", "image", {}, { generation: { id: "2", status: "running", mediaId: "503" } }),
      node("uploaded", "image", { reference: [{ id: "9", type: "image" }] }),
      node("vid", "video", {}, { generation: { id: "3", status: "success", outputType: "video", mediaId: "504" } })
    ];
    const edges = [edge("running", "dir", 3), edge("ok", "dir", 1), edge("copied", "dir", 2), edge("uploaded", "dir", 4), edge("vid", "dir", 5)];
    expect(directorUpstreamImages(nodes, edges, "dir")).toEqual([
      { sourceNodeId: "ok", title: "image ok", order: 1, mediaId: "501" },
      { sourceNodeId: "copied", title: "image copied", order: 2, mediaId: "502" },
      { sourceNodeId: "running", title: "image running", order: 3, mediaId: null },
      // 上传在节点上的参考不是产出
      { sourceNodeId: "uploaded", title: "image uploaded", order: 4, mediaId: null },
      { sourceNodeId: "vid", title: "video vid", order: 5, mediaId: null }
    ]);
    // 上游节点被删了（连线还在）：照样列出，标题回落
    expect(directorUpstreamImages([], [edge("gone", "dir", 1)], "dir")).toEqual([
      { sourceNodeId: "gone", title: "上游节点", order: 1, mediaId: null }
    ]);
  });
});

describe("节点面板的 @ 参考（DRT-20）", () => {
  const nodes = [
    node("old", "image", {}, { title: "城墙", generation: { id: "1", status: "success", outputType: "image", mediaId: "601" } }),
    node("dir", "director", { outputMediaId: "602" }, { title: "导演台" }),
    node("pending", "image", {}, { title: "还在跑", generation: { id: "2", status: "running" } }),
    node("vid", "video", {}, { title: "视频", generation: { id: "3", status: "success", outputType: "video", mediaId: "603" } }),
    // 从导演台发布出来的节点：图片节点 + 成功的 generation（DRT-17）
    node("pub", "image", { directorSource: { directorNodeId: "dir" } }, { title: "机位 1 截图", generation: { status: "success", outputType: "image", mediaId: "604" } }),
    node("txt", "text", {}, { title: "文本" })
  ];

  it("directorMentionCandidates：只列有产出的图片类节点（含发布节点），最近的在前，不含自己", () => {
    expect(directorMentionCandidates(nodes, "dir")).toEqual([
      { nodeId: "pub", title: "机位 1 截图", mediaId: "604" },
      { nodeId: "old", title: "城墙", mediaId: "601" }
    ]);
    expect(directorMentionCandidates(nodes, "dir", "城")).toEqual([{ nodeId: "old", title: "城墙", mediaId: "601" }]);
    expect(directorMentionCandidates(nodes, "dir", "", 1)).toHaveLength(1);
  });

  it("directorAddMention：与上传图同一个形状、多带 fromNodeId；重复与超上限不加并说明", () => {
    const candidate = { nodeId: "old", title: "城墙", mediaId: "601" };
    const upload = { id: "700", type: "image", contentType: "image/png", name: "a.png", size: 2048 };
    const added = directorAddMention([upload], candidate, 4);
    expect(added.notice).toBeUndefined();
    expect(added.references.map(item => item.id)).toEqual(["700", "601"]);
    expect(added.references[1]).toMatchObject({ id: "601", type: "image", name: "城墙", fromNodeId: "old", url: "/media/601" });
    expect(directorAddMention(added.references, candidate, 4).notice).toBe("「城墙」已经在参考图里了");
    const full = directorAddMention([upload], candidate, 1);
    expect(full.notice).toBe("参考图最多 1 张");
    expect(full.references.map(item => item.id)).toEqual(["700"]);
  });
});

describe("连线建议条（DR-F-93~95）", () => {
  const defaults = { presetCode: "whitebox_to_real", presetName: "按白模构图出图", videoMode: "ref2video", videoModeName: "全能参考" };
  const director = node("dir", "director");
  const character = node("role", "image");

  it("directorSuggestions：预设建议、首帧提示、角色图建议三种", () => {
    // 图片节点：预设不是配置的默认值 → 预设建议（带「使用」）；只有截图 → 角色图建议
    expect(directorSuggestions(node("t", "image", { preset: "storyboard" }), [director], defaults)).toEqual([
      { kind: "preset", text: "建议使用『按白模构图出图』", apply: { param: "preset", value: "whitebox_to_real", label: "使用" }, dismissible: true },
      { kind: "character", text: DIRECTOR_CHARACTER_TEXT, dismissible: true }
    ]);
    // 视频节点在图生视频模式：首帧提示（跟着模式走，不可关闭），配置了默认模式时给「改用全能参考」
    expect(directorSuggestions(node("t", "video", { mode: "img2video" }), [director, character], defaults)).toEqual([
      { kind: "first_frame", text: DIRECTOR_FIRST_FRAME_TEXT, apply: { param: "mode", value: "ref2video", label: "改用全能参考" }, dismissible: false }
    ]);
    expect(directorSuggestions(node("t", "video", { mode: "first_last_frame" }), [director, character], defaults)[0]?.kind).toBe("first_frame");
  });

  it("用的已经是配置的预设 / 全能参考模式 / 连了别的图时不提示", () => {
    expect(directorSuggestions(node("t", "image", { preset: "whitebox_to_real" }), [director, character], defaults)).toEqual([]);
    expect(directorSuggestions(node("t", "video", { mode: "ref2video" }), [director, character], defaults)).toEqual([]);
  });

  it("上传的参考、角色库里的角色也算「别的图片」", () => {
    const withUpload = node("t", "image", { preset: "whitebox_to_real", reference: [{ id: "8", type: "image", url: "/api/v1/media/8" }] });
    expect(directorSuggestions(withUpload, [director], defaults)).toEqual([]);
    const withRole = node("t", "video", { mode: "ref2video", roleLib: [{ kind: "marketing_character", id: "c1", name: "小美", mediaId: "9" }] });
    expect(directorSuggestions(withRole, [director], defaults)).toEqual([]);
  });

  it("上游没有导演台、或目标不是图片 / 视频时一条都没有；没配默认值时不给预设建议", () => {
    expect(directorSuggestions(node("t", "image"), [character], defaults)).toEqual([]);
    expect(directorSuggestions(node("t", "text"), [director], defaults)).toEqual([]);
    expect(directorSuggestions(node("t", "image"), [director]).map(tip => tip.kind)).toEqual(["character"]);
    // 没配默认模式：首帧提示照常出现，只是没有「改用」按钮
    expect(directorSuggestions(node("t", "video", { mode: "img2video" }), [director, character])[0]).toEqual(
      { kind: "first_frame", text: DIRECTOR_FIRST_FRAME_TEXT, dismissible: false });
  });

  it("directorSuggestions：发布的截图节点连图生视频时有首帧提示", () => {
    const published = node("pub", "image", { directorSource: { kind: "capture", sceneId: "9", recordId: "8", mediaId: "7" } });
    const tips = directorSuggestions(node("t", "video", { mode: "img2video" }), [published], defaults);
    expect(tips.map(tip => tip.kind)).toEqual(["first_frame", "character"]);
    // 发布出来的截图不算角色图；预设建议只针对导演台节点本身（DR-F-93）
    expect(directorSuggestions(node("t", "image"), [published], defaults).map(tip => tip.kind)).toEqual(["character"]);
    // 深度图 / 骨架图不是截图：单独连着时不提示
    const depth = node("depth", "image", { directorSource: { kind: "depth", mediaId: "7" } });
    expect(directorSuggestions(node("t", "video", { mode: "img2video" }), [depth], defaults)).toEqual([]);
    // 截图 + 深度图：深度图也不算角色图
    expect(directorSuggestions(node("t", "video", { mode: "ref2video" }), [published, depth], defaults).map(tip => tip.kind)).toEqual(["character"]);
  });

  it("关闭状态的键按项目 + 节点 + 种类区分", () => {
    expect(directorSuggestionKey("p1", "n1", "character")).toBe("seeaitv:director-tip:p1:n1:character");
    expect(directorSuggestionKey("p1", "n2", "character")).not.toBe(directorSuggestionKey("p1", "n1", "character"));
  });
});

describe("项目场景摘要按事件更新", () => {
  const summary: DirectorSceneSummary = {
    id: "77", canvasNodeId: "n1", version: 3, renderFingerprint: "fp3", outputCameraId: "cam1",
    lastEditorName: "我", editorKind: "user", updatedAt: "2026-10-26T01:00:00Z"
  };
  const frame = (extra: Record<string, unknown>) => ({
    type: "director.scene_updated", occurredAt: "2026-10-26T02:00:00Z", projectId: "p1", sceneId: "77",
    canvasNodeId: "n1", version: 4, renderFingerprint: "fp4", editorKind: "agent", editorId: "5", editorName: "同事", ...extra
  });

  it("新版本按帧更新版本与指纹，输出机位沿用原值", () => {
    const { summaries, unknown } = directorSummariesAfterEvent({ 77: summary }, frame({}), "p1");
    expect(unknown).toBe(false);
    expect(summaries["77"]).toEqual({ ...summary, version: 4, renderFingerprint: "fp4", lastEditorName: "同事", editorKind: "agent", updatedAt: "2026-10-26T02:00:00Z" });
  });

  it("旧版本、自己的回声、别的项目、形状不对的帧原样返回", () => {
    const base = { 77: summary };
    for (const extra of [{ version: 3 }, { version: 2 }, { projectId: "p2" }, { version: "x" }, { sceneId: "" }, { type: "canvas.ops_applied" }]) {
      const result = directorSummariesAfterEvent(base, frame(extra), "p1");
      expect(result.summaries).toBe(base);
      expect(result.unknown).toBe(false);
    }
  });

  it("不认识的场景要重新拉列表（事件不带输出机位，拼不出完整摘要）", () => {
    const base = { 77: summary };
    const result = directorSummariesAfterEvent(base, frame({ sceneId: "88" }), "p1");
    expect(result).toEqual({ summaries: base, unknown: true });
  });

  it("整份场景转摘要", () => {
    expect(directorSummaryOf({
      id: "77", projectId: "p1", canvasNodeId: "n1", schemaVersion: 1, version: 1, renderFingerprint: "fp1",
      scene: { schemaVersion: 1, outputCameraId: "cam9" } as never, records: [], lastEditorId: null, lastEditorName: "",
      editorKind: "user", createdAt: "2026-10-26T00:00:00Z", updatedAt: "2026-10-26T00:00:00Z"
    })).toEqual({ id: "77", canvasNodeId: "n1", version: 1, renderFingerprint: "fp1", outputCameraId: "cam9", lastEditorName: "", editorKind: "user", updatedAt: "2026-10-26T00:00:00Z" });
  });
});
