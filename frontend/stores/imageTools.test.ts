import { ref } from "vue";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { imageToolRequest } from "~/utils/imageTools";

/**
 * 图片工具 store（IMGT-13）：缓存（源图尺寸 / 工具列表）与编辑器报价。
 * vitest 里没有 Nuxt / Pinia 的自动导入，按 store 用到的全局函数注入替身（同 stores/canvas.test.ts）。
 */
type Call = { path: string; method: string; body?: unknown };

function setup(handler: (call: Call) => unknown) {
  const calls: Call[] = [];
  vi.stubGlobal("ref", ref);
  vi.stubGlobal("defineStore", (_id: string, setupFn: () => unknown) => () => setupFn());
  vi.stubGlobal("useApi", () => async (path: string, options: { method?: string; body?: unknown } = {}) => {
    const call = { path, method: options.method ?? "GET", body: options.body };
    calls.push(call);
    return handler(call);
  });
  return calls;
}

async function store(handler: (call: Call) => unknown) {
  const calls = setup(handler);
  const { useImageToolsStore } = await import("./imageTools");
  // ⚠️ 测试里的 defineStore 替身不像 Pinia 那样解包 ref：状态字段要按 `.value` 读
  type Raw = Omit<ReturnType<typeof useImageToolsStore>, "quotes"> & {
    quotes: { value: Record<string, { loading: boolean; ready?: boolean; error?: string } | undefined> };
  };
  return { store: useImageToolsStore() as unknown as Raw, calls };
}

beforeEach(() => {
  vi.resetModules();
  vi.useFakeTimers();
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("缓存", () => {
  it("工具列表不带源图尺寸与倍数；同一模型第二次不再请求；只认编辑器条目", async () => {
    const { store: s, calls } = await store(() => [
      { code: "upscale", name: "高清", presetGroup: "tool", available: true, userFixable: true, credits: 1 },
      { code: "upscale", name: "高清", presetGroup: "editor", available: true, userFixable: true, credits: 8 }
    ]);
    await s.tools();
    await s.tools();
    expect(calls.map(call => call.path)).toEqual(["/image-tools?imageCount=1&hasText=false"]);
    expect(s.toolOf("upscale")?.credits).toBe(8);
    await s.tools({ modelCode: "img_a" });
    expect(calls[1]?.path).toBe("/image-tools?imageCount=1&hasText=false&modelCode=img_a");
  });

  it("源图尺寸按媒体缓存；失败回 0×0 且不缓存", async () => {
    let fail = true;
    const { store: s, calls } = await store(() => {
      if (fail) throw new Error("网络错误");
      return { width: 1024, height: 768 };
    });
    expect(await s.sourceSize("m1")).toEqual({ width: 0, height: 0 });
    expect(s.sizeOf("m1")).toBeUndefined();
    fail = false;
    expect(await s.sourceSize("m1")).toEqual({ width: 1024, height: 768 });
    await s.sourceSize("m1");
    expect(calls).toHaveLength(2);
    expect(calls[1]).toMatchObject({ path: "/media/m1/info", method: "GET" });
  });

  it("不再有人物识别", async () => {
    const { store: s } = await store(() => ({}));
    expect("detectPeople" in s).toBe(false);
  });
});

describe("编辑器报价", () => {
  const request = imageToolRequest({
    projectId: "p1", nodeId: "n1", tool: "upscale", modelCode: "img_a",
    options: { level: "fine" },
    inputs: [{ sourceNodeId: "n0", kind: "image", order: 1, generationId: "g1" }]
  });

  it("防抖后走 /generations/estimate：operation=img2img、带 tool / toolOptions、不带源图尺寸，inputs 是 {image:1}", async () => {
    const { store: s, calls } = await store(() => ({ credits: 34 }));
    s.quote("k", request);
    s.quote("k", request);
    expect(s.quotes.value.k?.loading).toBe(true);
    await vi.advanceTimersByTimeAsync(350);
    expect(calls).toHaveLength(1);
    expect(calls[0]).toMatchObject({
      path: "/generations/estimate",
      method: "POST",
      body: {
        modelCode: "img_a", operation: "img2img", count: 1, inputs: { image: 1 },
        tool: "upscale", toolOptions: { level: "fine" }
      }
    });
    expect(calls[0]!.body).not.toHaveProperty("sourceWidth");
    expect(s.quotes.value.k).toEqual({ loading: false, ready: true });
  });

  it("失败显示服务端原话（422 / 400 带字段路径）", async () => {
    const tooLarge = Object.assign(new Error("该模型不支持图生图"), { statusCode: 422, data: { code: 42201 } });
    const badField = Object.assign(new Error("景别应在 0~10"), { statusCode: 400, data: { code: 40001, details: { path: "toolOptions.zoom" } } });
    let next: unknown = tooLarge;
    const { store: s } = await store(() => { throw next; });
    await s.quoteNow("k", request);
    expect(s.quotes.value.k).toEqual({ loading: false, error: "该模型不支持图生图" });
    next = badField;
    await s.quoteNow("k", request);
    expect(s.quotes.value.k?.error).toBe("景别应在 0~10（toolOptions.zoom）");
    s.clearQuote("k");
    expect(s.quotes.value.k).toBeUndefined();
  });
});
