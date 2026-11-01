import { computed, reactive, ref } from "vue";
import { afterEach, describe, expect, it, vi } from "vitest";

import { useOutputPreview } from "./useOutputPreview";

/**
 * 产物预览的状态是「创作资产」与画布图片节点**共用**的，两处的行为都靠这一份。
 * vitest 里没有 Nuxt 的自动导入，按调用时才用到的几个全局函数注入替身。
 */
type Deferred = { promise: Promise<Response>; resolve: (value: Response) => void };
function deferred(): Deferred {
  let resolve!: (value: Response) => void;
  return { promise: new Promise<Response>((r) => { resolve = r; }), resolve };
}

function setup(options: { outputs?: unknown[] | Error; hold?: Record<string, Deferred> } = {}) {
  const apiCalls: string[] = [];
  const mediaCalls: string[] = [];
  const created: string[] = [];
  const revoked: string[] = [];
  vi.stubGlobal("ref", ref);
  vi.stubGlobal("computed", computed);
  vi.stubGlobal("reactive", reactive);
  vi.stubGlobal("onBeforeUnmount", () => {});
  vi.stubGlobal("useApi", () => async (path: string) => {
    apiCalls.push(path);
    if (options.outputs instanceof Error) throw options.outputs;
    return options.outputs ?? [];
  });
  vi.stubGlobal("useAuthenticatedMedia", () => ({
    fetchMedia: (url: string) => {
      mediaCalls.push(url);
      return options.hold?.[url]?.promise ?? Promise.resolve(new Response(new Blob(["x"], { type: "image/png" })));
    }
  }));
  vi.spyOn(URL, "createObjectURL").mockImplementation(() => {
    const url = `blob:${created.length}`;
    created.push(url);
    return url;
  });
  vi.spyOn(URL, "revokeObjectURL").mockImplementation((url) => { revoked.push(String(url)); });
  return { apiCalls, mediaCalls, created, revoked };
}

const IMAGES = [
  { index: 0, type: "image", url: "/api/v1/media/1" },
  { index: 1, type: "image", url: "/api/v1/media/2" }
];
const base = { title: "图片节点 6", filename: "未命名-图片节点 6" };

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("产物预览", () => {
  it("节点上正显示的那张直接用内存里的字节，不再请求；其余逐张取、切回来走缓存", async () => {
    const env = setup({ outputs: IMAGES });
    const preview = useOutputPreview();
    await preview.open({ ...base, generationId: "9", fallback: { type: "image", url: "/api/v1/media/1" }, seed: new Blob(["seed"]), types: ["image"] });

    expect(env.apiCalls).toEqual(["/generations/9/outputs"]);
    expect(env.mediaCalls).toEqual([]);
    expect(preview.currentSrc.value).toBe("blob:0");
    expect(preview.outputs.value).toHaveLength(2);

    preview.step(1);
    await vi.waitFor(() => expect(preview.currentSrc.value).toBe("blob:1"));
    expect(env.mediaCalls).toEqual(["/api/v1/media/2"]);
    preview.step(1); // 绕回第一张：已在缓存里
    await vi.waitFor(() => expect(preview.index.value).toBe(0));
    expect(env.mediaCalls).toEqual(["/api/v1/media/2"]);
  });

  it("种子只挂在**同一个地址**那一项上，并从那一项开始看", async () => {
    const env = setup({ outputs: IMAGES });
    const preview = useOutputPreview();
    await preview.open({ ...base, generationId: "9", fallback: { type: "image", url: "/api/v1/media/2" }, seed: new Blob(["seed"]) });
    expect(preview.index.value).toBe(1);
    expect(env.mediaCalls).toEqual([]);
  });

  it("列不出产物时退回节点上那一张，而不是整个预览报错", async () => {
    setup({ outputs: new Error("网络断了") });
    const preview = useOutputPreview();
    await preview.open({ ...base, generationId: "9", fallback: { type: "image", url: "/api/v1/media/1" } });
    expect(preview.error.value).toBe("");
    expect(preview.outputs.value).toHaveLength(1);
    expect(preview.currentSrc.value).toBeTruthy();
  });

  it("没有兜底又列不出来：说出原因，不是一片空白", async () => {
    setup({ outputs: new Error("网络断了") });
    const preview = useOutputPreview();
    await preview.open({ ...base, generationId: "9" });
    expect(preview.error.value).toBe("网络断了");
    expect(preview.loading.value).toBe(false);
  });

  it("只看指定的类型：文本与别的类型不进预览", async () => {
    setup({ outputs: [
      { index: 0, type: "text", text: "旁白" },
      { index: 1, type: "video", url: "/api/v1/media/3" },
      { index: 2, type: "image", url: "/api/v1/media/4" }
    ] });
    const preview = useOutputPreview();
    await preview.open({ ...base, generationId: "9", types: ["image"] });
    expect(preview.outputs.value.map(output => output.url)).toEqual(["/api/v1/media/4"]);
  });

  it("关闭时释放全部 object URL；关闭之后才回来的响应不再挂上去", async () => {
    const slow = deferred();
    const env = setup({ outputs: IMAGES, hold: { "/api/v1/media/2": slow } });
    const preview = useOutputPreview();
    await preview.open({ ...base, generationId: "9" });
    expect(env.created).toEqual(["blob:0"]);

    preview.step(1);            // 第二张还在路上……
    preview.close();            // ……用户已经关了
    slow.resolve(new Response(new Blob(["late"])));
    await new Promise(resolve => setTimeout(resolve, 0));

    expect(env.revoked).toEqual(["blob:0"]);
    expect(env.created).toEqual(["blob:0"]); // 晚到的那张没有再建 object URL（建了就是一份没人释放的原图）
    expect(preview.opened.value).toBe(false);
  });
});
