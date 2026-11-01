import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import {
  smartcutDefaultAudioRole,
  smartcutDisconnectedClipIds,
  smartcutInboundSources,
  smartcutInputKindOf,
  smartcutResolveInputs,
  smartcutUnusedCount,
  type SmartcutCanvasDoc,
  type SmartcutCanvasNode,
  type SmartcutInputsLookup
} from "./smartcutInputs";

// 手写用例按 SCT-25 的解析规则；与 Go 共用的 fixture 在文件末尾（「与 Go 读同一批 fixture」）。

const SC = "sc1";
const node = (id: string, type: string, extra: Partial<SmartcutCanvasNode> = {}): SmartcutCanvasNode => ({ id, type, title: `节点 ${id}`, ...extra });
const edge = (source: string, order: number, target = SC, operation?: string) => ({ id: `e-${source}-${target}`, source, target, order, ...(operation ? { operation } : {}) });
const done = (mediaId: string, id = `g-${mediaId}`) => ({ id, status: "success", mediaId, outputUrl: `/api/v1/media/${mediaId}` });

describe("入边（与 Go 的 InboundSources 一致）", () => {
  it("按 order 排，相同按源节点 ID；跳过文档、画框与悬空边", () => {
    const doc: SmartcutCanvasDoc = {
      nodes: [node("b", "video"), node("a", "video"), node("doc", "document"), node("f", "frame"), node(SC, "smartcut")],
      edges: [edge("b", 1), edge("a", 1), edge("doc", 2), edge("f", 3), edge("ghost", 4), edge("a", 1, "other")]
    };
    expect(smartcutInboundSources(doc, SC).map(s => s.node.id)).toEqual(["a", "b"]);
  });
});

describe("素材条（SC-F-10~18）", () => {
  it("连线顺序即编号；类型按节点；生成成功的就绪", () => {
    const doc: SmartcutCanvasDoc = {
      nodes: [
        node("v1", "video", { result: done("71") }),
        node("i1", "image", { result: done("72") }),
        node("t1", "text", { params: { text: "  新品上市  " } }),
        node(SC, "smartcut")
      ],
      edges: [edge("i1", 2), edge("v1", 1), edge("t1", 3)]
    };
    const { items, omitted } = smartcutResolveInputs(doc, SC);
    expect(items.map(i => [i.no, i.kind, i.status, i.mediaId ?? i.text])).toEqual([
      ["1", "video", "ready", "71"],
      ["2", "image", "ready", "72"],
      ["3", "text", "ready", "新品上市"]
    ]);
    expect(items[0]).toMatchObject({ sourceNodeId: "v1", sourceNodeType: "video", title: "节点 v1", used: false });
    expect(omitted).toBe(0);
  });

  // SC-AC-05：一个出了 4 张图的图片节点 → 3-1~3-4；单个上游最多取前 9 个。
  it("多产物按产物顺序编号 3-1、3-2……，最多 9 个", () => {
    const outputs = Array.from({ length: 11 }, (_, i) => ({ mediaId: String(900 + i) }));
    const lookup: SmartcutInputsLookup = {
      generation: id => (id === "g-many" ? { status: "success", outputs } : id === "g-four" ? { status: "success", outputs: outputs.slice(0, 4) } : undefined)
    };
    const doc: SmartcutCanvasDoc = {
      nodes: [node("v1", "video", { result: done("71") }), node("v2", "video"), node("img", "image", { result: { id: "g-four", status: "success" } }),
        node("big", "image", { result: { id: "g-many", status: "success" } }), node(SC, "smartcut")],
      edges: [edge("v1", 1), edge("v2", 2), edge("img", 3), edge("big", 4)]
    };
    const { items } = smartcutResolveInputs(doc, SC, { lookup });
    expect(items.filter(i => i.sourceNodeId === "img").map(i => [i.no, i.mediaId])).toEqual([
      ["3-1", "900"], ["3-2", "901"], ["3-3", "902"], ["3-4", "903"]
    ]);
    expect(items.filter(i => i.sourceNodeId === "big").map(i => i.no)).toEqual(
      ["4-1", "4-2", "4-3", "4-4", "4-5", "4-6", "4-7", "4-8", "4-9"]);
  });

  it("素材条最多 50 条，超出的只报个数", () => {
    const nodes = [node(SC, "smartcut"), ...Array.from({ length: 53 }, (_, i) => node(`v${i}`, "video", { result: done(String(100 + i)) }))];
    const edges = Array.from({ length: 53 }, (_, i) => edge(`v${i}`, i + 1));
    const { items, omitted } = smartcutResolveInputs({ nodes, edges }, SC);
    expect(items).toHaveLength(50);
    expect(items[49]!.no).toBe("50");
    expect(omitted).toBe(3);
  });

  // 导演台节点给的是输出机位截图；发布节点（导演台预演、成片）按媒体声明，生成 ID 不存在。
  it("导演台与发布节点、上传节点、复制出来的节点", () => {
    const doc: SmartcutCanvasDoc = {
      nodes: [
        node("dir", "director", { params: { outputMediaId: "501" } }),
        node("dir2", "director", { params: {} }),
        node("pub", "video", { params: { directorSource: { kind: "preview", mediaId: "502" } }, result: { status: "success", mediaId: "502", outputUrl: "/api/v1/media/502" } }),
        node("cut", "video", { params: { smartcutSource: { timelineId: "9", renderId: "8", mediaId: "503", timelineVersion: 4 } } }),
        node("up", "audio", { result: { status: "success", outputUrl: "/api/v1/media/504" } }),
        node("copy", "image", { result: { sourceGenerationId: "777", status: "success", mediaId: "505" } }),
        node(SC, "smartcut")
      ],
      edges: ["dir", "dir2", "pub", "cut", "up", "copy"].map((id, i) => edge(id, i + 1))
    };
    const { items } = smartcutResolveInputs(doc, SC);
    expect(items.map(i => [i.no, i.kind, i.status, i.mediaId ?? i.reason])).toEqual([
      ["1", "image", "ready", "501"],
      ["2", "image", "pending", "请先在导演台截图"],
      ["3", "video", "ready", "502"],
      ["4", "video", "ready", "503"],
      ["5", "audio", "ready", "504"],
      ["6", "image", "ready", "505"]
    ]);
    // 上传的音频默认是配乐（不是文字转语音来的）
    expect(items[4]!.audioRole).toBe("music");
  });

  it("未完成：排队 / 运行中为 pending，失败带原因，完成却没有产物也算失败", () => {
    const lookup: SmartcutInputsLookup = { generation: id => (id === "g-empty" ? { status: "success", outputs: [] } : undefined) };
    const doc: SmartcutCanvasDoc = {
      nodes: [
        node("q", "video", { result: { id: "g-q", status: "queued" } }),
        node("r", "video", { result: { id: "g-r", status: "running" } }),
        node("x", "video", { result: { id: "g-x", status: "failed", error: "上游超时" } }),
        node("e", "video", { result: { id: "g-empty", status: "success" } }),
        node("n", "video"),
        node(SC, "smartcut")
      ],
      edges: ["q", "r", "x", "e", "n"].map((id, i) => edge(id, i + 1))
    };
    const { items } = smartcutResolveInputs(doc, SC, { lookup });
    expect(items.map(i => [i.status, i.reason])).toEqual([
      ["pending", "上游还在生成"],
      ["pending", "上游还在生成"],
      ["failed", "上游超时"],
      ["failed", "上游任务完成但没有产物"],
      ["pending", "还没有产出"]
    ]);
  });

  // 生成节点以回查的事实为准（result 只是 UI 快照）：快照说成功、回查说跨项目 → invalid，不进轨。
  it("回查结果优先于快照；跨工作区与平台素材为 invalid", () => {
    const lookup: SmartcutInputsLookup = {
      generation: id => (id === "g-other" ? { status: "invalid", reason: "不是本项目的生成" } : undefined),
      media: id => (id === "600" ? { status: "invalid", reason: "平台素材不能直接使用" } : { status: "ready", durationMs: 4000, hasAudio: true })
    };
    const doc: SmartcutCanvasDoc = {
      nodes: [
        node("g", "video", { result: done("71", "g-other") }),
        node("p", "image", { result: { status: "success", mediaId: "600" } }),
        node("u", "video", { result: { status: "success", mediaId: "601" } }),
        node(SC, "smartcut")
      ],
      edges: [edge("g", 1), edge("p", 2), edge("u", 3)]
    };
    const { items } = smartcutResolveInputs(doc, SC, { lookup });
    expect(items.map(i => [i.no, i.status, i.reason])).toEqual([
      ["1", "invalid", "不是本项目的生成"],
      ["2", "invalid", "平台素材不能直接使用"],
      ["3", "ready", undefined]
    ]);
    expect(items[2]).toMatchObject({ durationMs: 4000, hasAudio: true });
  });

  it("脚本与视频组是镜头组；其它组与不认的节点为 invalid", () => {
    const doc: SmartcutCanvasDoc = {
      nodes: [
        node("s", "script", { params: { scriptId: "31" } }),
        node("s0", "script", { params: {} }),
        node("vg", "group", { params: { groupType: "video_generator", groupId: "41", scriptId: "31" } }),
        node("ag", "group", { params: { groupType: "asset", groupId: "42" } }),
        node("st", "style"),
        node(SC, "smartcut")
      ],
      edges: ["s", "s0", "vg", "ag", "st"].map((id, i) => edge(id, i + 1))
    };
    const { items } = smartcutResolveInputs(doc, SC);
    expect(items.map(i => [i.kind, i.status])).toEqual([
      ["shotGroup", "ready"], ["shotGroup", "pending"], ["shotGroup", "ready"], ["video", "invalid"], ["video", "invalid"]
    ]);
    expect(smartcutInputKindOf({ type: "director" })).toBe("image");
  });

  it("文本：有成功的生成时取产出，否则取节点上写的；超长截到 2000 字（按码点）", () => {
    const doc: SmartcutCanvasDoc = {
      nodes: [
        node("gen", "text", { params: { prompt: "润色一下" }, result: { id: "g-t", status: "success", outputText: "润色后的稿子" } }),
        node("long", "text", { params: { text: "字".repeat(2001) } }),
        node("empty", "text", { params: { text: "  " } }),
        node(SC, "smartcut")
      ],
      edges: [edge("gen", 1), edge("long", 2), edge("empty", 3)]
    };
    const { items } = smartcutResolveInputs(doc, SC);
    expect(items[0]!.text).toBe("润色后的稿子");
    expect([...items[1]!.text!]).toHaveLength(2000);
    expect(items[2]).toMatchObject({ status: "pending", reason: "文本还是空的" });
  });

  it("used：时间线里有片段用到（同来源节点，多产物时还要同媒体）", () => {
    const lookup: SmartcutInputsLookup = { generation: () => ({ status: "success", outputs: [{ mediaId: "1" }, { mediaId: "2" }] }) };
    const doc: SmartcutCanvasDoc = { nodes: [node("m", "image", { result: { id: "g", status: "success" } }), node(SC, "smartcut")], edges: [edge("m", 1)] };
    const { items } = smartcutResolveInputs(doc, SC, { lookup, clips: [{ sourceNodeId: "m", mediaId: "2" }] });
    expect(items.map(i => [i.no, i.used])).toEqual([["1-1", false], ["1-2", true]]);
    expect(smartcutUnusedCount(items)).toBe(1);
  });
});

describe("音频用途与来源已断开", () => {
  it("文字转语音来的默认人声，其余默认配乐；回查给了操作码时以它为准", () => {
    const tts = node("tts", "audio", { result: done("80") });
    const music = node("mus", "audio", { result: done("81") });
    const doc: SmartcutCanvasDoc = {
      nodes: [node("txt", "text"), tts, music, node(SC, "smartcut")],
      edges: [edge("txt", 1, "tts", "text2audio"), edge("txt", 1, "mus", "text2music")]
    };
    expect(smartcutDefaultAudioRole(doc, tts)).toBe("voice");
    expect(smartcutDefaultAudioRole(doc, music)).toBe("music");
    // 没有入边但有生成：音频节点的默认操作就是 text2audio
    expect(smartcutDefaultAudioRole({ nodes: [tts] }, tts)).toBe("voice");
    expect(smartcutDefaultAudioRole(doc, tts, "video2audio")).toBe("music");
  });

  // SC-AC-04：断开连线后，用到它的片段保留，标「来源已断开」。
  it("片段的来源节点不在入边里时记为断开；没有来源节点的不算", () => {
    const doc: SmartcutCanvasDoc = { nodes: [node("v1", "video"), node(SC, "smartcut")], edges: [edge("v1", 1)] };
    const tracks = [
      { clips: [{ id: "c1", sourceNodeId: "v1" }, { id: "c2", sourceNodeId: "gone" }, { id: "c3" }] },
      { clips: [{ id: "c4", sourceNodeId: "gone2" }, { id: "c5", sourceNodeId: null }] }
    ];
    expect(smartcutDisconnectedClipIds(tracks, smartcutInboundSources(doc, SC))).toEqual(["c2", "c4"]);
  });
});

/**
 * 与 Go 的 `canvas/domain.InboundSources` + `smartcut/domain.ResolveInputs` 读**同一批** fixture（SCT-25，SCR-11）。
 * ⚠️ 直接读仓库里的 `api/fixtures/smartcut/inputs/`，不复制一份。期望值是 fixture 里手算写死的。
 */
const INPUTS_DIR = fileURLToPath(new URL("../../api/fixtures/smartcut/inputs/", import.meta.url));

type InputsFixture = {
  why: string;
  nodeId: string;
  canvas: SmartcutCanvasDoc;
  lookup?: {
    generations?: Record<string, ReturnType<NonNullable<SmartcutInputsLookup["generation"]>>>;
    media?: Record<string, ReturnType<NonNullable<SmartcutInputsLookup["media"]>>>;
  };
  clips?: Array<{ id: string; sourceNodeId?: string; mediaId?: string }>;
  expect: { items: unknown[]; omitted: number; disconnected: string[]; unusedCount: number };
};

describe("与 Go 读同一批 fixture（api/fixtures/smartcut/inputs）", () => {
  const files = readdirSync(INPUTS_DIR).filter(name => name.endsWith(".json")).sort();
  it("fixture 齐全", () => expect(files.length).toBeGreaterThanOrEqual(10));
  for (const file of files) {
    it(file, () => {
      const f = JSON.parse(readFileSync(join(INPUTS_DIR, file), "utf8")) as InputsFixture;
      const lookup: SmartcutInputsLookup = {
        generation: id => f.lookup?.generations?.[id],
        media: id => f.lookup?.media?.[id]
      };
      const clips = f.clips ?? [];
      const { items, omitted } = smartcutResolveInputs(f.canvas, f.nodeId, { lookup, clips });
      expect(items, f.why).toEqual(f.expect.items);
      expect(omitted).toBe(f.expect.omitted);
      expect(smartcutDisconnectedClipIds([{ clips }], smartcutInboundSources(f.canvas, f.nodeId))).toEqual(f.expect.disconnected);
      expect(smartcutUnusedCount(items)).toBe(f.expect.unusedCount);
    });
  }
});

