import { describe, expect, it } from "vitest";
import { decodeGenerationOutput, generationOutputKind, generationOutputPlan } from "./generationOutput";

describe("generation output helpers", () => {
  it("uses the declared output type before the response content type", () => {
    expect(generationOutputKind("script", "application/octet-stream")).toBe("text");
    expect(generationOutputKind("video", "application/octet-stream")).toBe("video");
  });

  it("infers output types from response content types", () => {
    expect(generationOutputKind(undefined, "text/plain; charset=utf-8")).toBe("text");
    expect(generationOutputKind(undefined, "image/png")).toBe("image");
    expect(generationOutputKind(undefined, "video/mp4")).toBe("video");
    expect(generationOutputKind(undefined, "audio/mpeg")).toBe("audio");
    expect(generationOutputKind(undefined, "application/octet-stream")).toBe("binary");
  });

  it("decodes text results instead of treating them as media previews", async () => {
    const result = await decodeGenerationOutput(new Response("生成后的文本", {
      headers: { "content-type": "text/plain; charset=utf-8" }
    }));

    expect(result).toEqual({ kind: "text", text: "生成后的文本" });
  });

  it("keeps image results as blobs", async () => {
    const result = await decodeGenerationOutput(new Response(new Uint8Array([1, 2, 3]), {
      headers: { "content-type": "image/png" }
    }), "image");

    expect(result.kind).toBe("image");
    if (result.kind !== "text") {
      expect(result.blob.type).toBe("image/png");
      expect(result.blob.size).toBe(3);
    }
  });
});

/**
 * ⚠️ 起因：文本节点**生成成功却什么都不显示**。
 *
 * 新后端的产物形状分两种：文本直接带 `text`（**没有 url**），
 * 图片/视频/音频只有 `url`（没有 text）。老前端一律 `if (!output.url) return`，
 * 于是文本这一整类静默落空 —— 而图片恰好有 url，看起来像"只有文本节点坏了"。
 */
describe("产物渲染方式的判定", () => {
  it("有正文就直接用正文，不去 fetch", () => {
    expect(generationOutputPlan({ type: "text", text: "一段结果", url: null }))
      .toEqual({ kind: "text", text: "一段结果" });
  });

  it("⚠️ 文本产物没有 url —— 只认 url 的话这一类永远落空", () => {
    const plan = generationOutputPlan({ type: "text", text: "正文", url: null });
    expect(plan.kind).not.toBe("none");
    expect(plan.kind).toBe("text");
  });

  it("媒体产物走 fetch，并把声明类型带上", () => {
    expect(generationOutputPlan({ type: "image", url: "/api/v1/media/42", text: null }))
      .toEqual({ kind: "fetch", url: "/api/v1/media/42", declaredType: "image" });
  });

  it("有 text 时优先用 text，不先看 type", () => {
    // type 可能缺失、也可能是前端没见过的新值，而正文已经在手上了。
    expect(generationOutputPlan({ text: "正文" }).kind).toBe("text");
    expect(generationOutputPlan({ type: "brand_new_kind", text: "正文" }).kind).toBe("text");
  });

  it("空正文不当作文本产物", () => {
    // 空串是"这一项没有正文"，落到 fetch 分支或 none，而不是渲染一段空白。
    expect(generationOutputPlan({ type: "text", text: "", url: "/api/v1/media/7" }).kind).toBe("fetch");
    expect(generationOutputPlan({ type: "text", text: "" }).kind).toBe("none");
  });

  it("两样都没有就什么都不做，不抛", () => {
    expect(generationOutputPlan(undefined).kind).toBe("none");
    expect(generationOutputPlan(null).kind).toBe("none");
    expect(generationOutputPlan({}).kind).toBe("none");
    expect(generationOutputPlan({ type: "image", url: "" }).kind).toBe("none");
  });
});
