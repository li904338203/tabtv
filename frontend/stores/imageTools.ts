import type { components } from "@tabtv/api-client";
import { estimateRejectionText } from "~/utils/canvas";
import type { ImageToolInfo, ImageToolRequest } from "~/utils/imageTools";

/**
 * ⚠️ 服务端实际回的是 `credits`（`catalog/app.Quote`），契约里写的是 `creditsCost` —— 两个都认（同 stores/generation）。
 */
type QuoteResponse = Partial<components["schemas"]["GenerationQuote"]> & { credits?: number };

/** 一次编辑器报价的状态（IMG-UX-02：报价中显示加载，失败显示原因并禁用发送）。 */
export type ImageToolQuote = {
  loading: boolean;
  credits?: number;
  /** 报价失败的原因（服务端原话；400 带 details.path / 422 类原样显示，其余回落到通用文案）。 */
  error?: string;
};

/** 源图像素尺寸（`GET /media/{id}/info`）；0 表示未知。 */
export type ImageToolSourceSize = { width: number; height: number };

/**
 * `GET /image-tools` 的查询条件：只有模型。
 * ⚠️ 2026-10-31 方案变更后不再带源图尺寸与倍数：工具都是 img2img，单价 = 模型的 img2img 单价，与源图无关。
 */
export type ImageToolsAsk = { modelCode?: string };

type MediaInfoResponse = { width?: number; height?: number };

/**
 * 图片节点工具条的数据（IMGT-13，IMGD-20/29）：
 *
 * - `GET /media/{id}/info`：源图像素尺寸，按媒体缓存（只给情绪调节的框选做「显示 ↔ 原图」坐标换算）；
 * - `GET /image-tools`：按模型缓存 —— 打开工具条时取一次，之后再选中图片不再请求；
 * - 编辑器报价：防抖后走 `/generations/estimate`（与 ParamPanel 同一个接口），按调用方给的 key 存结果。
 *
 * ⚠️ 工具列表与报价只做**显示**：真正扣费在受理时由服务端按工具与模型算。
 * ⚠️ 失败**不缓存**：一次网络抖动不该让本次会话里再也取不到工具列表 / 源图尺寸。
 */
export const useImageToolsStore = defineStore("image-tools", () => {
  const toolLists = ref<Record<string, ImageToolInfo[]>>({});
  const toolErrors = ref<Record<string, string>>({});
  const sizes = ref<Record<string, ImageToolSourceSize>>({});
  const quotes = ref<Record<string, ImageToolQuote>>({});
  const inflightTools = new Map<string, Promise<ImageToolInfo[]>>();
  const inflightSizes = new Map<string, Promise<ImageToolSourceSize>>();
  const quoteTimers = new Map<string, ReturnType<typeof setTimeout>>();
  const quoteSeq = new Map<string, number>();

  function toolsKey(ask: ImageToolsAsk = {}) {
    return ask.modelCode ?? "";
  }

  /** 源图尺寸（像素）。取不到时**不抛**、不缓存，回 0×0（未知）：尺寸未知只是暂时不能框选，不该让面板打不开。 */
  async function sourceSize(mediaId: string | undefined): Promise<ImageToolSourceSize> {
    if (!mediaId) return { width: 0, height: 0 };
    const cached = sizes.value[mediaId];
    if (cached) return cached;
    const pending = inflightSizes.get(mediaId);
    if (pending) return pending;
    const request = (async () => {
      try {
        const info = await useApi()<MediaInfoResponse>(`/media/${encodeURIComponent(mediaId)}/info`);
        const size = { width: Number(info?.width) || 0, height: Number(info?.height) || 0 };
        sizes.value[mediaId] = size;
        return size;
      } catch {
        return { width: 0, height: 0 };
      } finally {
        inflightSizes.delete(mediaId);
      }
    })();
    inflightSizes.set(mediaId, request);
    return request;
  }

  /** 已缓存的源图尺寸（同步读）。 */
  function sizeOf(mediaId: string | undefined): ImageToolSourceSize | undefined {
    return mediaId ? sizes.value[mediaId] : undefined;
  }

  /**
   * 工具列表（含可用性、阻塞原因、单价、可选模型）。
   * `imageCount=1`：工具条只在已出图的节点上出现，源图就是那一张。
   */
  async function tools(ask: ImageToolsAsk = {}, options: { force?: boolean } = {}) {
    const key = toolsKey(ask);
    if (!options.force && toolLists.value[key]) return toolLists.value[key]!;
    const pending = inflightTools.get(key);
    if (pending) return pending;
    const query = new URLSearchParams({ imageCount: "1", hasText: "false" });
    if (ask.modelCode) query.set("modelCode", ask.modelCode);
    const request = (async () => {
      try {
        const list = await useApi()<ImageToolInfo[]>(`/image-tools?${query.toString()}`);
        toolLists.value[key] = Array.isArray(list) ? list : [];
        toolErrors.value = withoutKey(toolErrors.value, key);
        return toolLists.value[key]!;
      } catch (cause) {
        toolErrors.value[key] = cause instanceof Error && cause.message ? cause.message : "工具列表加载失败";
        throw cause;
      } finally {
        inflightTools.delete(key);
      }
    })();
    inflightTools.set(key, request);
    return request;
  }

  /** 已缓存的某个工具条目（同步读，给模板用）；没取过返回 undefined。 */
  function toolOf(code: string, ask: ImageToolsAsk = {}) {
    // ⚠️ 只认编辑器工具（presetGroup=editor）：P09 的旧加工预设里有同名 code（multi_angle / relight / upscale），
    // 按 code 取第一条的话会拿到那条没有 models、按预设配方报价的旧预设。
    return toolLists.value[toolsKey(ask)]?.find(item => item.code === code && item.presetGroup === "editor");
  }

  /** 已缓存的工具列表的加载错误（同步读）。 */
  function toolsError(ask: ImageToolsAsk = {}) {
    return toolErrors.value[toolsKey(ask)] ?? "";
  }

  /**
   * 编辑器报价（防抖，默认 350ms，与 ParamPanel 同节奏）。结果写进 `quotes[key]`。
   *
   * 请求体：`operation` 是 `img2img`（`imageToolRequest` 给）、`modelCode` 传所选模型、`inputs: {image:1}`（报价的 inputs
   * 是「角色→条数」的 map，不是受理的数组，见 stores/generation 的 estimate 注释），另带 `tool` / `toolOptions` ——
   * 服务端用同一套工具翻译算张数后报价（= 模型 img2img 单价 × 张数）。
   * ⚠️ 晚到的旧响应不覆盖新结果（按序号丢弃）。
   */
  function quote(key: string, request: ImageToolRequest | null, delay = 350) {
    clearTimeout(quoteTimers.get(key));
    if (!request) {
      quoteSeq.set(key, (quoteSeq.get(key) ?? 0) + 1);
      quotes.value = withoutKey(quotes.value, key);
      return;
    }
    quotes.value[key] = { ...quotes.value[key], loading: true };
    quoteTimers.set(key, setTimeout(() => void quoteNow(key, request), delay));
  }

  async function quoteNow(key: string, request: ImageToolRequest) {
    const seq = (quoteSeq.get(key) ?? 0) + 1;
    quoteSeq.set(key, seq);
    quotes.value[key] = { ...quotes.value[key], loading: true };
    try {
      const response = await useApi()<QuoteResponse>("/generations/estimate", {
        method: "POST",
        body: {
          modelCode: request.modelCode,
          operation: request.operation,
          count: request.count,
          inputs: { image: 1 },
          tool: request.params.tool,
          toolOptions: request.params.toolOptions
        }
      });
      if (quoteSeq.get(key) !== seq) return quotes.value[key];
      quotes.value[key] = { loading: false, credits: response.credits ?? response.creditsCost ?? 0 };
    } catch (cause) {
      if (quoteSeq.get(key) !== seq) return quotes.value[key];
      quotes.value[key] = { loading: false, error: quoteErrorText(cause) };
    }
    return quotes.value[key];
  }

  /** 关闭编辑器时清掉它的报价（与在途请求）。 */
  function clearQuote(key: string) {
    clearTimeout(quoteTimers.get(key));
    quoteTimers.delete(key);
    quoteSeq.set(key, (quoteSeq.get(key) ?? 0) + 1);
    quotes.value = withoutKey(quotes.value, key);
  }

  return {
    toolLists, toolErrors, sizes, quotes,
    sourceSize, sizeOf, tools, toolOf, toolsError,
    quote, quoteNow, clearQuote
  };
});

/** 去掉一个键（新对象，保持响应式替换；不用 `delete obj[key]`）。 */
function withoutKey<T>(record: Record<string, T>, key: string): Record<string, T> {
  return Object.fromEntries(Object.entries(record).filter(([name]) => name !== key));
}

/**
 * 报价失败的说明：422 类（能力不支持等）与 400（toolOptions 不合法，带 details.path）都原样显示服务端那句话；
 * 其余回落到通用文案，不猜。
 */
function quoteErrorText(cause: unknown): string {
  const rejected = estimateRejectionText(cause);
  if (rejected) return rejected;
  const { statusCode, message, data } = (cause ?? {}) as { statusCode?: unknown; message?: unknown; data?: { details?: { path?: unknown } } };
  if (statusCode === 400 && typeof message === "string" && message.trim()) {
    const path = typeof data?.details?.path === "string" ? data.details.path : "";
    return path ? `${message.trim()}（${path}）` : message.trim();
  }
  return "报价失败，请稍后再试";
}
