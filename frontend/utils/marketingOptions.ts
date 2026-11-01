/**
 * 营销选项（创意 / Hook / 风格）弹层用到的纯函数（MKT-T-13，dev/03 §3.4；MKT-F-19、MKT-UX-01/03/06/07）。
 *
 * 组件只调用这里：可用性判定、列表查询参数、公开取图地址、网格键盘移动。
 * 悬停播放开关用 `utils/mediaGrid.canHoverPlay`（与模板墙同一份判定，不另写）。
 * 用户端 Vitest 没有 DOM，规则一律放这里测。
 *
 * ⚠️ 几条容易做反的：
 * - **不可用的照样列出并说明原因**（MKT-F-19）：隐藏的话用户以为没有这个选项，而他其实只差一张图或换个模型。
 * - **语气按 `userFixable` 这个分类定，不匹配文案**（CD-48、MKT-UX-07）：文案改一个字匹配就失效，
 *   失效的表现是所有原因都退回默认那句。
 * - **智能匹配时不判出声**：那时用哪个模型由 Agent 决定，它会挑能出声的；前端此刻置灰等于替它做了一个错误判断。
 *   当前模型的出声标记**未知**（目录还没取到）同样不判 —— 服务端受理时兜底（MKT-T-20）。
 * - **缺商品图只提示、不挡选择**（MKT-D-29）：输入要求不满足时禁用的是**发送键**（`marketingSendGate`），
 *   挡住选择的话用户得先传图才能挑创意，顺序被倒过来了。
 * - 片段（prompt）**不在任何类型里**：契约里的 `MarketingOption` 本来就没有这个字段（MKT-AC-06）。
 */
import type { components } from "@tabtv/api-client";

export type MarketingOption = components["schemas"]["MarketingOption"];
export type MarketingKind = components["schemas"]["MarketingKind"];
export type MarketingCategory = components["schemas"]["MarketingCategory"];
export type MarketingModelRef = components["schemas"]["MarketingModelRef"];

/** 可用性判定的原因码。前两类用户自己能解决，`unavailable` 按服务端给的分类。 */
export type OptionBlockCode = "needs_audio_model" | "needs_product" | "output_mismatch" | "unavailable";

export type OptionBlock = {
  code: OptionBlockCode;
  /** true = 用户自己能解决（换模型、传图）；false = 运营要解决（暂不可用）。语气只看它。 */
  userFixable: boolean;
  text: string;
  /**
   * 还能不能点选。只有 `needs_product` 可以：缺的输入由发送键拦（MKT-D-29），先选后补是正常顺序。
   * 其余三类选了也不生效（不出声的模型放不出音效、产出对不上、已下线），置灰不可选。
   */
  selectable: boolean;
};

/** 服务端 42215 对某个选项的结论（建会话时回的），按 `code` 存进上下文。 */
export type ServerOptionBlock = { message: string; userFixable: boolean };

export type OptionBlockCtx = {
  /**
   * 当前视频模型；null / undefined = 智能匹配（不判出声）。
   * `hasNativeAudio` 为 undefined（目录还没取到这个模型）同样不判。
   */
  videoModel?: { code: string; hasNativeAudio?: boolean } | null;
  /** 商品槽已有几张图（含上传中的不算）。 */
  productCount?: number;
  /** 源视频槽已放了视频（素材裂变）：源视频本身就是商品视频，「需要商品图」算满足。 */
  sourceVideo?: boolean;
  /** 当前技能的产出；undefined / null = 还没定（不判产出）。 */
  outputType?: "video" | "image" | null;
  /** 服务端 42215 回过的选项（key = 选项 code）。 */
  serverBlocks?: Readonly<Record<string, ServerOptionBlock>>;
};

export const KIND_LABEL: Record<MarketingKind, string> = { creative: "创意", hook: "Hook", style: "风格" };

const OUTPUT_LABEL: Record<string, string> = { video: "视频", image: "图片" };

/**
 * 一个选项此刻能不能用；能用返回 null。
 *
 * 多个原因同时成立时给**最根本**的那一条（CD-59 / CD-60 同一条）：服务端说不行 > 产出对不上 > 模型不出声 > 缺商品图。
 * 反过来的话，一个已下线的风格会提示「需要商品图」，用户传完图发现还是用不了。
 */
export function optionBlock(option: Pick<MarketingOption, "kind" | "code" | "isAudioRequired" | "outputTypes" | "inputRequirements">, ctx: OptionBlockCtx = {}): OptionBlock | null {
  // 「这个 Hook 不生效」「这个风格只适用于视频」：英文名两侧留空格，中文名不留。
  const raw = KIND_LABEL[option.kind] ?? "选项";
  const label = /[A-Za-z]/.test(raw) ? ` ${raw} ` : raw;
  const server = ctx.serverBlocks?.[option.code];
  if (server) {
    return { code: "unavailable", userFixable: server.userFixable, text: server.message || `这个${label}暂不可用`, selectable: false };
  }
  const outputs = option.outputTypes ?? [];
  if (ctx.outputType && outputs.length && !outputs.includes(ctx.outputType)) {
    const names = outputs.map(item => OUTPUT_LABEL[item] ?? item).join("、");
    return { code: "output_mismatch", userFixable: true, text: `这个${label}只适用于${names}`, selectable: false };
  }
  if (option.isAudioRequired && ctx.videoModel && ctx.videoModel.hasNativeAudio === false) {
    return {
      code: "needs_audio_model",
      userFixable: true,
      text: `当前模型不出声，这个${label}不生效 · 换一个能出声的模型`,
      selectable: false
    };
  }
  // 素材裂变的源视频本身就是商品视频，也算有商品（04 文档 P-47，与服务端同一条）。
  if (option.inputRequirements?.product === "required" && !(ctx.productCount && ctx.productCount > 0) && !ctx.sourceVideo) {
    return { code: "needs_product", userFixable: true, text: "需要商品图", selectable: true };
  }
  return null;
}

/**
 * 从建会话失败的错误里取出 42215 的选项结论（`details{kind,code,reason,userFixable}`）；不是这一类返回 null。
 * 错误形状见 `composables/useApi`（`data.code` / `data.details`）。
 */
export function serverOptionBlock(error: unknown): { kind: string; code: string; block: ServerOptionBlock } | null {
  if (typeof error !== "object" || error === null) return null;
  const data = (error as { data?: unknown }).data;
  if (typeof data !== "object" || data === null) return null;
  const { code, details } = data as { code?: unknown; details?: unknown };
  if (code !== 42215 || typeof details !== "object" || details === null) return null;
  const d = details as Record<string, unknown>;
  if (typeof d.code !== "string" || !d.code) return null;
  const message = (error as { message?: unknown }).message;
  return {
    kind: typeof d.kind === "string" ? d.kind : "",
    code: d.code,
    block: { message: typeof message === "string" ? message : "", userFixable: d.userFixable === true }
  };
}

/**
 * 弹层左栏的筛选：全部 / 某个分类 / 某个模型（仅创意）。
 * 分类与模型是两组互斥的筛选（服务端同时给两个也能筛，但界面上同一时刻只选中一项，避免「为什么是空的」）。
 */
export type OptionFilter = { type: "all" } | { type: "category"; code: string } | { type: "model"; code: string };

export const ALL_FILTER: OptionFilter = { type: "all" };

export function sameFilter(a: OptionFilter, b: OptionFilter): boolean {
  if (a.type !== b.type) return false;
  return a.type === "all" || (a as { code: string }).code === (b as { code: string }).code;
}

export const OPTION_PAGE_SIZE = 24;

/** `GET /marketing/options` 的查询参数：分类与搜索都交给服务端（MKT-F-34），空值不发。 */
export function optionListQuery(input: { kind: MarketingKind; filter: OptionFilter; q?: string; cursor?: string; limit?: number }): Record<string, string | number> {
  const query: Record<string, string | number> = { kind: input.kind, limit: input.limit ?? OPTION_PAGE_SIZE };
  if (input.filter.type === "category") query.category = input.filter.code;
  if (input.filter.type === "model" && input.kind === "creative") query.model = input.filter.code;
  const q = (input.q ?? "").trim();
  if (q) query.q = q;
  if (input.cursor) query.cursor = input.cursor;
  return query;
}

/** 公开封面（不鉴权，可直接 `<img>`）。下标从 0 起；`coverCount=0` 时调用方显示占位。 */
export function optionCoverUrl(apiBase: string, code: string, index = 0, width: 160 | 320 | 640 = 320): string {
  return `${apiBase}/marketing/options/${encodeURIComponent(code)}/covers/${index}?w=${width}`;
}

/** 公开示例视频（不鉴权，可直接 `<video>`）。 */
export function optionPreviewUrl(apiBase: string, code: string): string {
  return `${apiBase}/marketing/options/${encodeURIComponent(code)}/preview`;
}

/** 技能公开封面（MKT-T-05；不在登录组里）。 */
export function skillCoverUrl(apiBase: string, code: string, width: 160 | 320 | 640 = 320): string {
  return `${apiBase}/agent/skills/${encodeURIComponent(code)}/cover?w=${width}`;
}

/**
 * 封面网格的键盘移动（MKT-UX-06）：↑↓ 按行（跨 `cols` 个）、←→ 按格；越界停在边上（网格里循环会让人迷路），
 * 没有当前项时任何方向都落到第一项。
 */
export function gridMove(index: number, key: "up" | "down" | "left" | "right", count: number, cols: number): number {
  if (count <= 0) return -1;
  if (index < 0 || index >= count) return 0;
  const step = key === "up" ? -cols : key === "down" ? cols : key === "left" ? -1 : 1;
  const next = index + step;
  if (next < 0) return key === "up" ? index : 0;
  if (next >= count) {
    // 向下时最后一行不满：落到最后一项，而不是原地不动（否则按 ↓ 像是没反应）。
    return key === "down" && Math.floor(index / cols) < Math.floor((count - 1) / cols) ? count - 1 : index;
  }
  return next;
}

/** 详情抽屉「适用输入」那一行：商品图：必填 · 角色：可选。 */
const SLOT_LABEL: Record<string, string> = { product: "商品图", character: "角色", source_video: "源视频" };

export function inputRequirementText(requirements: Readonly<Record<string, string>> | null | undefined): string {
  const entries = Object.entries(requirements ?? {}).filter(([, value]) => value === "required" || value === "optional");
  if (!entries.length) return "不限";
  // 必填在前：用户最先要知道的是「不传这个发不出去」。
  entries.sort(([, a], [, b]) => Number(b === "required") - Number(a === "required"));
  return entries.map(([key, value]) => `${SLOT_LABEL[key] ?? key}：${value === "required" ? "必填" : "可选"}`).join(" · ");
}

/** 详情抽屉「推荐画幅时长」：`{"aspectRatio":"9:16","durationSec":[10,15]}` → 「9:16 · 10~15 秒」；没有返回 ""。 */
export function recommendedParamsText(params: Readonly<Record<string, unknown>> | null | undefined): string {
  const parts: string[] = [];
  const ratio = params?.aspectRatio;
  if (typeof ratio === "string" && ratio) parts.push(ratio);
  const duration = params?.durationSec;
  if (typeof duration === "number" && duration > 0) parts.push(`${duration} 秒`);
  else if (Array.isArray(duration)) {
    const nums = duration.filter((item): item is number => typeof item === "number" && item > 0);
    if (nums.length === 1) parts.push(`${nums[0]} 秒`);
    else if (nums.length >= 2) parts.push(nums[0] === nums[1] ? `${nums[0]} 秒` : `${Math.min(nums[0]!, nums[1]!)}~${Math.max(nums[0]!, nums[1]!)} 秒`);
  }
  return parts.join(" · ");
}

/**
 * 可用性判定要的「当前视频模型」：偏好里的视频模型 code（`prefModel(prefs, "video")`）+ 目录里的出声标记。
 * 没选（智能匹配）→ null；目录里找不到这个模型 → 出声未知（不判）。
 */
export function currentVideoModel(code: string | undefined | null, catalog: readonly { code: string; hasNativeAudio?: boolean }[]): OptionBlockCtx["videoModel"] {
  if (!code) return null;
  const found = catalog.find(item => item.code === code);
  return { code, hasNativeAudio: found?.hasNativeAudio };
}
