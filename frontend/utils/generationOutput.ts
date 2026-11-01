export type GenerationOutputKind = "text" | "image" | "video" | "audio" | "binary";

export type DecodedGenerationOutput =
  | { kind: "text"; text: string }
  | { kind: Exclude<GenerationOutputKind, "text">; blob: Blob };

export function generationOutputKind(declaredType?: string, contentType?: string | null): GenerationOutputKind {
  const declared = declaredType?.toLowerCase();
  if (declared === "text" || declared === "script") return "text";
  if (declared === "image" || declared === "video" || declared === "audio") return declared;

  const mediaType = contentType?.split(";", 1)[0]?.trim().toLowerCase() ?? "";
  if (mediaType.startsWith("text/") || mediaType === "application/json") return "text";
  if (mediaType.startsWith("image/")) return "image";
  if (mediaType.startsWith("video/")) return "video";
  if (mediaType.startsWith("audio/")) return "audio";
  return "binary";
}

/** `GET /generations/{id}/outputs` 的一个元素（只取渲染需要的字段）。 */
export type GenerationOutputRef = {
  type?: string | null;
  text?: string | null;
  url?: string | null;
};

/** 拿到一个产物之后该怎么渲染。 */
export type GenerationOutputPlan =
  | { kind: "text"; text: string }
  | { kind: "fetch"; url: string; declaredType?: string }
  | { kind: "none" };

/**
 * ⚠️ **文本产物直接带正文、没有 `url`**；图片/视频/音频反过来 —— 只有 `url`。
 *
 * 老前端只认 `url`（老后端连文本也给一个可下载的地址），于是在新后端上
 * **文本节点这一整类永远拿不到结果**：任务成功、积分扣了、节点上空白一片，
 * 而图片/视频恰好有 url，看起来像"只有文本节点坏了"。
 *
 * ⚠️ **有 text 就先用 text**，不要先看 `type` 再决定 —— `type` 可能缺失或是
 * 服务端没见过的新值，而正文已经在手上了。
 */
export function generationOutputPlan(output?: GenerationOutputRef | null): GenerationOutputPlan {
  if (!output) return { kind: "none" };
  if (typeof output.text === "string" && output.text !== "") {
    return { kind: "text", text: output.text };
  }
  if (typeof output.url === "string" && output.url !== "") {
    return { kind: "fetch", url: output.url, declaredType: output.type ?? undefined };
  }
  return { kind: "none" };
}

export async function decodeGenerationOutput(response: Response, declaredType?: string): Promise<DecodedGenerationOutput> {
  const kind = generationOutputKind(declaredType, response.headers.get("content-type"));
  if (kind === "text") return { kind, text: await response.text() };
  return { kind, blob: await response.blob() };
}
