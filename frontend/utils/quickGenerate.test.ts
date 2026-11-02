import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import type { StoredCanvasNode } from "~/types/canvas";
import { canvasNodeToStored, storedToCanvasNode } from "./canvasDocument";
import {
  LAST_MODE_KEY,
  agentCanvasPath,
  agentProjectName,
  buildAgentSessionRequest,
  buildQuickRequest,
  modeNodeType,
  quickCanvasPath,
  quickErrorText,
  quickOperation,
  quickOperationNote,
  quickQuantities,
  quickVideoOperation,
  readLastMode,
  writeLastMode
} from "./quickGenerate";

function memoryStorage(initial: Record<string, string> = {}) {
  const data = new Map(Object.entries(initial));
  return {
    getItem: (key: string) => data.get(key) ?? null,
    setItem: (key: string, value: string) => { data.set(key, value); }
  };
}

describe("模式映射", () => {
  it("模式即节点类型", () => {
    expect(modeNodeType("video")).toBe("video");
    expect(modeNodeType("image")).toBe("image");
    expect(modeNodeType("audio")).toBe("audio");
  });

  it("视频按参考图张数推断操作：0 文生、1 图生、≥2 全能参考（不再推首尾帧）", () => {
    expect(quickVideoOperation(0)).toBe("text2video");
    expect(quickVideoOperation(1)).toBe("img2video");
    expect(quickVideoOperation(2)).toBe("ref2video");
    expect(quickVideoOperation(6)).toBe("ref2video");
    // 脏值按 0 张处理，不抛
    expect(quickVideoOperation(-1)).toBe("text2video");
    expect(quickVideoOperation(Number.NaN)).toBe("text2video");
    expect(quickVideoOperation(1.7)).toBe("img2video");
  });

  it("各模式的操作码", () => {
    expect(quickOperation("video", 0)).toBe("text2video");
    expect(quickOperation("video", 2)).toBe("ref2video");
    expect(quickOperation("image", 0)).toBe("text2img");
    expect(quickOperation("image", 2)).toBe("img2img");
    expect(quickOperation("audio", 0)).toBe("text2audio");
  });

  it("浮层底部说清当前按哪种操作生成", () => {
    expect(quickOperationNote("video", 0)).toBe("当前：文生视频（无参考图，按参考图自动判定）");
    expect(quickOperationNote("video", 3)).toBe("当前：全能参考（3 张参考图，按参考图自动判定）");
    expect(quickOperationNote("image", 1)).toBe("当前：图生图（1 张参考图，按参考图自动判定）");
    expect(quickOperationNote("audio", 0)).toBe("当前：文本转语音");
  });
});

describe("请求体", () => {
  it("附件只带 referenceId，不含任何 URL", () => {
    const body = buildQuickRequest({
      mode: "image",
      modelCode: "m1",
      text: "  水墨江南  ",
      params: {
        aspect_ratio: "16:9", count: "2", prompt: "旧词", model: "旧模型",
        reference: [{ id: "1", url: "/api/v1/media/references/1/content" }], empty: ""
      },
      attachments: [{ referenceId: "1001", name: "a.png" }, { referenceId: "1002" }]
    });
    expect(body).toEqual({
      mode: "image",
      modelCode: "m1",
      text: "水墨江南",
      params: { aspect_ratio: "16:9", count: "2" },
      inputs: [
        { kind: "reference", referenceId: "1001", order: 1, name: "a.png" },
        { kind: "reference", referenceId: "1002", order: 2 }
      ]
    });
    expect(JSON.stringify(body)).not.toMatch(/url|https?:|\/api\//i);
    expect(body.operation).toBeUndefined();
  });

  it("视频把按张数推断的操作放进 operation，params 里的旧 mode 不起作用", () => {
    const body = buildQuickRequest({
      mode: "video", modelCode: "v", text: "x", params: { mode: "first_last_frame", duration: "10" },
      attachments: [{ referenceId: "1" }, { referenceId: "2" }]
    });
    expect(body.operation).toBe("ref2video");
    expect(body.params).toEqual({ duration: "10" });
    const one = buildQuickRequest({ mode: "video", modelCode: "v", text: "x", params: {}, attachments: [{ referenceId: "1" }] });
    expect(one.operation).toBe("img2video");
    const none = buildQuickRequest({ mode: "video", modelCode: "v", text: "x", params: { mode: "img2video" }, attachments: [] });
    expect(none.operation).toBe("text2video");
  });

  it("数量与时长与画布同规则", () => {
    expect(quickQuantities("video", { count: "1", duration: "10" })).toEqual({ count: 1, durationSec: 10 });
    expect(quickQuantities("image", { count: "4", duration: "10" })).toEqual({ count: 4 });
    expect(quickQuantities("audio", {})).toEqual({ count: 1 });
  });

  it("跳转到画布", () => {
    expect(quickCanvasPath("123")).toBe("/video/canvas/123");
  });
});

describe("上次模式", () => {
  it("默认 Agent；读写往返（切到视频后刷新仍是视频）", () => {
    const storage = memoryStorage();
    expect(readLastMode(storage)).toBe("agent");
    writeLastMode(storage, "video");
    expect(readLastMode(storage)).toBe("video");
    writeLastMode(storage, "agent");
    expect(readLastMode(storage)).toBe("agent");
  });

  it("脏值时退回默认", () => {
    expect(readLastMode(memoryStorage({ [LAST_MODE_KEY]: "xx" }), "image")).toBe("image");
    expect(readLastMode(memoryStorage({ [LAST_MODE_KEY]: "xx" }))).toBe("agent");
    expect(readLastMode(undefined)).toBe("agent");
  });

  it("存储抛错不影响使用", () => {
    const broken = { getItem: () => { throw new Error("denied"); }, setItem: () => { throw new Error("denied"); } };
    expect(readLastMode(broken)).toBe("agent");
    expect(() => writeLastMode(broken, "image")).not.toThrow();
  });
});

describe("Agent 模式发送体（buildAgentSessionRequest）", () => {
  it("项目名取正文前 20 字（按码点，不切坏 emoji），空白折叠", () => {
    expect(agentProjectName("三打白骨精，悟空视角，悲壮一点，十五秒竖屏，配旁白讲述")).toBe("三打白骨精，悟空视角，悲壮一点，十五秒竖");
    expect(Array.from(agentProjectName("😀".repeat(30)))).toHaveLength(20);
    expect(agentProjectName("  第一行\n\n  第二行  ")).toBe("第一行 第二行");
    expect(agentProjectName("   ")).toBe("未命名创作");
  });

  it("建项目 + 首条消息 + 技能 + 偏好；引用只有 {kind,id}", () => {
    const body = buildAgentSessionRequest({
      text: "  三打白骨精  ",
      refs: [{ kind: "media", id: "101", label: "参考图", thumbMediaId: "101" } as never],
      skillCode: "story_topic",
      prefs: { reasoning: { model: "m-reason" } }
    });
    expect(body).toEqual({
      createProject: { name: "三打白骨精" },
      message: { text: "三打白骨精", refs: [{ kind: "media", id: "101" }] },
      skillCode: "story_topic",
      prefs: { reasoning: { model: "m-reason" } }
    });
  });

  it("不选技能也能发送：不带 skillCode；空偏好不带", () => {
    const body = buildAgentSessionRequest({ text: "x", refs: [] });
    expect(body).not.toHaveProperty("skillCode");
    expect(body).not.toHaveProperty("prefs");
  });

  it("跳画布带上会话", () => {
    expect(agentCanvasPath("12", "34")).toBe("/video/canvas/12?session=34");
  });
});

describe("失败文案", () => {

  it("断网说断网，其余带服务端原话", () => {
    expect(quickErrorText(new Error("模型暂时不可用"), false)).toBe("网络已断开，恢复后再发送");
    expect(quickErrorText(new Error("模型暂时不可用：已下线"), true)).toBe("模型暂时不可用：已下线");
    expect(quickErrorText(new Error("请求失败"), true)).toBe("发送没有成功，请稍后重试");
  });
});

describe("服务端建的节点与前端 document() 同形", () => {
  // 与 Go 侧 TestBuildQuickNode_形状与前端document一致 读同一份 fixture。
  const fixture = JSON.parse(readFileSync(fileURLToPath(new URL(
    "../../backend/internal/canvas/domain/testdata/quick-node.json", import.meta.url)), "utf8")) as StoredCanvasNode;

  it("经前端往返一字不变（前端认识它的每一个键）", () => {
    const runtime = storedToCanvasNode(fixture, { index: 0, params: fixture.params ?? {}, width: fixture.size?.w ?? 0 });
    expect(runtime.data.nodeType).toBe("video");
    expect(runtime.data.stored).toBeUndefined();
    const back = canvasNodeToStored(runtime, fixture.size!);
    expect(JSON.parse(JSON.stringify(back))).toEqual(fixture);
  });
});
