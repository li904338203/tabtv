/**
 * 图片节点工具条（docs/agent/imagetools，IMGT-13，IMGD-24/26/30）的纯函数：选项默认值、预设 ↔ 三元组、景别名、
 * 情绪格互查、只发 touched 角色、显示坐标 ↔ 原图坐标、禁用理由排序、受理请求组装。
 *
 * 方案（2026-10-31 变更）：四个工具都用现有图片大模型走**图生图**（img2img），服务端按 `params.tool` 拼提示词；
 * 前端请求的 `operation` 一律发 `img2img`，工具身份只在 `params.tool` + `params.toolOptions` 里。
 *
 * ⚠️ **不在前端拼提示词**（CLAUDE.md §5.9）：这里只组装结构化选项（`params.tool` + `params.toolOptions`），
 *    方向 / 情绪 / 光照翻译成上游参数与提示词全在服务端 `imagetool/domain`。
 * ⚠️ 导出名一律带 `imageTool` 前缀，调用处**显式 import**：Nuxt 自动导入 `utils/` 的全部导出，
 *    重名时只打一条 WARN 就悄悄顶替成另一个实现（CLAUDE.md §5.28）。
 * ⚠️ 表（多角度预设、景别分档、情绪 25 格、高清增强程度）来自 `/canvas/config.imageTools`，函数一律**收表做参数**；
 *    `IMAGE_TOOL_FALLBACK_CONFIG` 只在配置缺省（服务端还没下发 / 老服务端）时兜底，值与服务端
 *    `imagetool/domain` 同一份设计建议值（IMG-PARAM-*）。
 */
import type { components } from "@tabtv/api-client";

export type ImageToolCode = "multi_angle" | "relight" | "expression_edit" | "upscale";
export type ImageToolInfo = components["schemas"]["ImageTool"];
export type ImageToolMultiAngleOptions = components["schemas"]["MultiAngleOptions"];
export type ImageToolRelightOptions = components["schemas"]["RelightOptions"];
export type ImageToolPixelBox = components["schemas"]["PixelBox"];

/** 高清增强程度（标准 / 精细 / 极致）。 */
export type ImageToolUpscaleLevel = "standard" | "fine" | "ultra";
export type ImageToolUpscaleOptions = components["schemas"]["UpscaleOptions"];
/** 情绪调节：只有手动框选的角色（框是原图像素），输出即原图分辨率，所以**没有** resolution。 */
export type ImageToolExpressionOptions = Omit<components["schemas"]["ExpressionOptions"], "count"> & { count: number };
export type ImageToolsConfig = components["schemas"]["ImageToolsConfig"];
export type ImageToolOptions = ImageToolMultiAngleOptions | ImageToolRelightOptions | ImageToolExpressionOptions | ImageToolUpscaleOptions;

/** 顺序即工具条与 `/canvas/config.imageTools.tools` 的顺序。 */
export const IMAGE_TOOL_CODES: readonly ImageToolCode[] = ["expression_edit", "multi_angle", "relight", "upscale"];

export function isImageToolCode(value: unknown): value is ImageToolCode {
  return typeof value === "string" && (IMAGE_TOOL_CODES as readonly string[]).includes(value);
}

/** 节点上的工具码（`params.tool`）；不是工具节点返回 undefined。 */
export function imageToolOfParams(params: Record<string, unknown> | undefined): ImageToolCode | undefined {
  const tool = params?.tool;
  return isImageToolCode(tool) ? tool : undefined;
}

// ---------------------------------------------------------------------------
// 配置（/canvas/config.imageTools）与兜底
// ---------------------------------------------------------------------------

type Required2<T> = { [K in keyof T]-?: NonNullable<T[K]> };
export type ImageToolsResolvedConfig = Required2<ImageToolsConfig>;

const EMOTION_NAMES = [
  ["欣喜若狂", "兴高采烈", "激动万分", "愤愤不平", "怒不可遏"],
  ["喜笑颜开", "热情洋溢", "惊讶", "焦躁不安", "怒目而视"],
  ["温柔亲切", "和颜悦色", "淡然自若", "冷淡", "冷若冰霜"],
  ["含情脉脉", "恬静安然", "若有所思", "落寞", "漠然"],
  ["安详满足", "宁静祥和", "平静如水", "黯然神伤", "心灰意冷"]
];

/** 情绪示意图地址（IMGD-28，`public/imagetools/emotion/{arousal}_{warmth}.webp`）。 */
export function imageToolEmotionImage(arousal: number, warmth: number) {
  return `/imagetools/emotion/${arousal}_${warmth}.svg`;
}

/**
 * 兜底配置：与服务端 `imagetool/domain` 的表逐项一致（IMG-PARAM-04/06/20/21/30/31）。
 * ⚠️ 只在 `/canvas/config` 没有下发对应字段时用；服务端下发了就以服务端为准（配置可以比前端新）。
 */
export const IMAGE_TOOL_FALLBACK_CONFIG: ImageToolsResolvedConfig = {
  tools: [
    { code: "expression_edit", name: "情绪调节", spawn: "情绪调节", isNew: true },
    { code: "multi_angle", name: "多角度", spawn: "多角度", isNew: false },
    { code: "relight", name: "打光", spawn: "打光", isNew: false },
    { code: "upscale", name: "高清", spawn: "高清", isNew: false }
  ],
  portraitPreset: "portrait_tuning",
  panoramaPreset: "panorama_720",
  multiAnglePresets: [
    { code: "custom", name: "自定义", horizontal: 0, vertical: 0, zoom: 5 },
    { code: "fisheye", name: "鱼眼视角", horizontal: 0, vertical: 0, zoom: 6 },
    { code: "dutch", name: "倾斜视角", horizontal: 30, vertical: 10, zoom: 5 },
    { code: "high_front", name: "正面俯拍", horizontal: 0, vertical: 45, zoom: 5 },
    { code: "low_front", name: "正面仰拍", horizontal: 0, vertical: -30, zoom: 5 },
    { code: "birds_eye", name: "全景俯拍", horizontal: 0, vertical: 75, zoom: 1 },
    { code: "back", name: "背面视角", horizontal: 180, vertical: 0, zoom: 5 }
  ],
  zoomBands: [
    { max: 1, name: "大远景" },
    { max: 3, name: "远景" },
    { max: 6, name: "中景" },
    { max: 8, name: "近景" },
    { max: 10, name: "特写" }
  ],
  lightPresets: [
    { code: "left", name: "左侧", azimuth: 270, elevation: 0 },
    { code: "top", name: "顶部", azimuth: 0, elevation: 80 },
    { code: "right", name: "右侧", azimuth: 90, elevation: 0 },
    { code: "front", name: "前方", azimuth: 0, elevation: 0 },
    { code: "bottom", name: "底部", azimuth: 0, elevation: -80 },
    { code: "back", name: "后方", azimuth: 180, elevation: 0 }
  ],
  namedColors: [
    { name: "暖白", hex: "#FFF1DC" }, { name: "冷白", hex: "#E6F0FF" }, { name: "琥珀", hex: "#FFB347" }, { name: "橙", hex: "#FF8C1A" },
    { name: "红", hex: "#FF3B30" }, { name: "品红", hex: "#FF2D95" }, { name: "粉", hex: "#FF9EC4" }, { name: "紫", hex: "#8E5CFF" },
    { name: "蓝", hex: "#2F6BFF" }, { name: "青", hex: "#24C8DB" }, { name: "绿", hex: "#34C759" }, { name: "黄绿", hex: "#B4E04B" },
    { name: "金黄", hex: "#FFD23F" }, { name: "月光蓝", hex: "#9DB8FF" }, { name: "霓虹青", hex: "#00FFE1" }, { name: "夕阳红", hex: "#FF6A3D" }
  ],
  emotions: EMOTION_NAMES.flatMap((row, r) => row.map((name, c) => ({
    arousal: 2 - r,
    warmth: 2 - c,
    name,
    image: imageToolEmotionImage(2 - r, 2 - c)
  }))),
  upscaleLevels: [
    { code: "standard", name: "标准" },
    { code: "fine", name: "精细" },
    { code: "ultra", name: "极致" }
  ],
  maxRoles: 6
};

/**
 * 逐字段合并：服务端下发的字段优先，缺的字段用兜底。
 * ⚠️ 按字段而不是整份替换：老服务端可能只下发了一部分（例如还没有 `namedColors`），整份替换会让那部分控件空着。
 */
export function imageToolsConfigOf(config: ImageToolsConfig | null | undefined): ImageToolsResolvedConfig {
  const result = { ...IMAGE_TOOL_FALLBACK_CONFIG } as Record<string, unknown>;
  for (const [key, value] of Object.entries(config ?? {})) {
    if (value === undefined || value === null) continue;
    if (Array.isArray(value) && !value.length) continue;
    result[key] = value;
  }
  return result as ImageToolsResolvedConfig;
}

/** 工具的展示名与派生子节点默认标题。 */
export function imageToolLabel(config: ImageToolsResolvedConfig, tool: ImageToolCode): { name: string; spawn: string } {
  const entry = config.tools.find(item => item.code === tool)
    ?? IMAGE_TOOL_FALLBACK_CONFIG.tools.find(item => item.code === tool);
  return { name: entry?.name ?? tool, spawn: entry?.spawn ?? entry?.name ?? tool };
}

/**
 * 工具条按钮自己的 NEW 角标（IMG-F-01：不再借九宫格预设的 `isNew`）。
 * ⚠️ 契约的 `ImageToolsConfig.tools[]` 目前没有 `isNew` 字段（回报里已列为契约缺口），这里按未知字段宽松读取，
 *    服务端补上之后不用改前端。
 */
export function imageToolIsNew(config: ImageToolsResolvedConfig, tool: ImageToolCode): boolean {
  const entry = config.tools.find(item => item.code === tool) as { isNew?: unknown } | undefined;
  return entry?.isNew === true;
}

/** 预设已下线时按钮上的说明（IMG-F-06）。 */
export const IMAGE_TOOL_PRESET_OFFLINE = "该玩法已下线";

/**
 * 工具条上复用九宫格预设的按钮（人物调节 / 全景）：按码在目录里找预设；找不到（运营下线）时
 * 按钮**照样显示并说明原因**，不隐藏、不报错（IMG-F-06）。
 */
export function imageToolPresetLookup<T>(presets: readonly T[], code: string, codeOf: (preset: T) => string): { preset?: T; reason: string | null } {
  const preset = code ? presets.find(item => codeOf(item) === code) : undefined;
  return preset ? { preset, reason: null } : { reason: IMAGE_TOOL_PRESET_OFFLINE };
}

// ---------------------------------------------------------------------------
// 默认选项
// ---------------------------------------------------------------------------

export const IMAGE_TOOL_MULTI_ANGLE_PROMPT_MAX = 200;

export function imageToolDefaultOptions(tool: "multi_angle", context?: ImageToolDefaultsContext): ImageToolMultiAngleOptions;
export function imageToolDefaultOptions(tool: "relight", context?: ImageToolDefaultsContext): ImageToolRelightOptions;
export function imageToolDefaultOptions(tool: "expression_edit", context?: ImageToolDefaultsContext): ImageToolExpressionOptions;
export function imageToolDefaultOptions(tool: "upscale", context?: ImageToolDefaultsContext): ImageToolUpscaleOptions;
export function imageToolDefaultOptions(tool: ImageToolCode, context?: ImageToolDefaultsContext): ImageToolOptions;
/**
 * 各工具的默认选项（IMG-F-34 / F-44 / F-17 / F-51 的「重置参数」也回到这里）。
 * 多角度的默认值取预设表里的 `custom`，表里没有时回落到 0° / 0° / 景别 5。
 */
export function imageToolDefaultOptions(tool: ImageToolCode, context: ImageToolDefaultsContext = {}): ImageToolOptions {
  const config = context.config ?? IMAGE_TOOL_FALLBACK_CONFIG;
  switch (tool) {
    case "multi_angle": {
      const custom = config.multiAnglePresets.find(item => item.code === "custom");
      return { preset: "custom", horizontal: custom?.horizontal ?? 0, vertical: custom?.vertical ?? 0, zoom: custom?.zoom ?? 5 };
    }
    case "relight": {
      const front = config.lightPresets.find(item => item.code === "front");
      return { smart: false, preset: "front", azimuth: front?.azimuth ?? 0, elevation: front?.elevation ?? 0, brightness: 50, color: null, rim: false };
    }
    case "expression_edit":
      return { sourceMediaId: context.sourceMediaId ?? "", count: 1, roles: [] };
    case "upscale":
      // 默认「标准」；配置里没有 standard（运营改了表）时取表头
      return { level: (config.upscaleLevels.some(item => item.code === "standard") ? "standard" : (config.upscaleLevels[0]?.code ?? "standard")) as ImageToolUpscaleLevel };
  }
}

export type ImageToolDefaultsContext = {
  config?: ImageToolsResolvedConfig;
  /** 情绪调节必填：人物框所属的源图（IMGD-11）。 */
  sourceMediaId?: string;
};

// ---------------------------------------------------------------------------
// 多角度
// ---------------------------------------------------------------------------

type MultiAnglePresetRow = ImageToolsResolvedConfig["multiAnglePresets"][number];

/**
 * 点预设 = 把三根滑杆设成预设值（IMG-F-30）；补充提示词保持用户写的那段。
 * 预设码不在表里（配置下线了它）时原样返回当前值，不报错。
 */
export function imageToolPresetToOptions(
  presets: readonly MultiAnglePresetRow[],
  code: string,
  current: ImageToolMultiAngleOptions = {}
): ImageToolMultiAngleOptions {
  const preset = presets.find(item => item.code === code);
  if (!preset) return { ...current };
  return {
    ...current,
    preset: preset.code as ImageToolMultiAngleOptions["preset"],
    horizontal: preset.horizontal,
    vertical: preset.vertical,
    zoom: preset.zoom
  };
}

/** 动任何一根滑杆 → 页签切回「自定义」（IMG-F-30）。 */
export function imageToolSetAngle(
  current: ImageToolMultiAngleOptions,
  key: "horizontal" | "vertical" | "zoom",
  value: number
): ImageToolMultiAngleOptions {
  const range = { horizontal: [0, 360], vertical: [-30, 90], zoom: [0, 10] }[key] as [number, number];
  const clamped = Math.min(range[1], Math.max(range[0], Math.round(Number.isFinite(value) ? value : 0)));
  return { ...current, preset: "custom", [key]: clamped };
}

/**
 * 球形机位的箭头步进（IMG-F-32）：环绕 ±45° 取模到 0~359，俯仰 ±15° 夹在 -30~90。
 */
export function imageToolStepAngle(current: ImageToolMultiAngleOptions, direction: "left" | "right" | "up" | "down"): ImageToolMultiAngleOptions {
  const horizontal = current.horizontal ?? 0;
  const vertical = current.vertical ?? 0;
  if (direction === "left" || direction === "right") {
    const next = (((horizontal + (direction === "right" ? 45 : -45)) % 360) + 360) % 360;
    return { ...current, preset: "custom", horizontal: next };
  }
  return imageToolSetAngle(current, "vertical", vertical + (direction === "up" ? 15 : -15));
}

/** 景别名（IMG-PARAM-21）：`zoom ≤ max` 的第一档；超出最后一档按最后一档。 */
export function imageToolZoomLabel(zoom: number, bands: ImageToolsResolvedConfig["zoomBands"] = IMAGE_TOOL_FALLBACK_CONFIG.zoomBands): string {
  const sorted = [...bands].sort((a, b) => a.max - b.max);
  return sorted.find(band => zoom <= band.max)?.name ?? sorted[sorted.length - 1]?.name ?? "";
}

// ---------------------------------------------------------------------------
// 情绪
// ---------------------------------------------------------------------------

type EmotionRow = ImageToolsResolvedConfig["emotions"][number];

/** 坐标 → 格子（含名称与示意图）；越界或表里没有返回 undefined。 */
export function imageToolEmotionAt(emotions: readonly EmotionRow[], arousal: number, warmth: number): EmotionRow | undefined {
  return emotions.find(item => item.arousal === arousal && item.warmth === warmth);
}

/** 名称 → 坐标（「情绪定位：{名}」的反查）。 */
export function imageToolEmotionByName(emotions: readonly EmotionRow[], name: string): { arousal: number; warmth: number } | undefined {
  const hit = emotions.find(item => item.name === name);
  return hit ? { arousal: hit.arousal, warmth: hit.warmth } : undefined;
}

/**
 * 5×5 坐标盘的行列顺序（IMG-F-14）：行 = 激动(+2) → 平静(-2)，列 = 亲近(+2) → 疏离(-2)。
 * 缺格（配置不全）时那一格为 undefined，界面照样占位。
 */
export function imageToolEmotionGrid(emotions: readonly EmotionRow[]): Array<Array<EmotionRow | undefined>> {
  return [2, 1, 0, -1, -2].map(arousal => [2, 1, 0, -1, -2].map(warmth => imageToolEmotionAt(emotions, arousal, warmth)));
}

/** 编辑器里的角色：比受理的 `ExpressionRole` 多两样只在界面上用的东西。 */
export type ImageToolEditorRole = {
  id: string;
  box: ImageToolPixelBox;
  arousal: number;
  warmth: number;
  /** 被调整过（点过坐标盘）。只有它为 true 的进请求（IMG-F-15）。 */
  touched: boolean;
};

/** 角色显示名「角色1、角色2…」按当前列表的从左到右顺序（手动框选的也按位置排）。 */
export function imageToolRoleLabels(roles: readonly ImageToolEditorRole[]): Record<string, string> {
  return Object.fromEntries([...roles]
    .sort((a, b) => a.box.x - b.box.x || a.box.y - b.box.y)
    .map((role, index) => [role.id, `角色${index + 1}`]));
}

/** 只发被调整过的角色（IMG-F-15）；`touched` 是界面状态，不进请求。 */
export function imageToolTouchedRoles(roles: readonly ImageToolEditorRole[]): ImageToolExpressionOptions["roles"] {
  return roles.filter(role => role.touched).map(role => ({
    id: role.id,
    box: { ...role.box },
    arousal: role.arousal,
    warmth: role.warmth
  }));
}

/**
 * 再加一个角色是否超上限（IMG-F-22）。返回说明文案；没超返回 null。
 */
export function imageToolRoleLimitReason(count: number, maxRoles: number = IMAGE_TOOL_FALLBACK_CONFIG.maxRoles): string | null {
  return count >= maxRoles ? `最多同时调节 ${maxRoles} 个角色` : null;
}

// ---------------------------------------------------------------------------
// 显示坐标 ↔ 原图像素坐标（IMGD-30，同 CLAUDE.md §5.9 蒙版那三条）
// ---------------------------------------------------------------------------

/** 图片在容器里按 `object-fit: contain` 摆放的几何关系。 */
export type ImageToolFit = {
  /** 图片左上角相对容器左上角的偏移（居中留白）。 */
  offsetX: number;
  offsetY: number;
  /** 显示像素 / 原图像素。 */
  scale: number;
  naturalWidth: number;
  naturalHeight: number;
};

/** 框选的最小边（原图像素）：再小的话脸都看不清，服务端同样会丢弃（IMG-F-64）。 */
export const IMAGE_TOOL_MIN_BOX_SIDE = 24;

/**
 * 计算 contain 摆放：取两方向**较小**的缩放比，剩余空间平均分到两侧。
 * 任一尺寸未知（0）时返回 null —— 尺寸未知不能换算，界面应当不让框选。
 */
export function imageToolFit(containerWidth: number, containerHeight: number, naturalWidth: number, naturalHeight: number): ImageToolFit | null {
  if (!(containerWidth > 0 && containerHeight > 0 && naturalWidth > 0 && naturalHeight > 0)) return null;
  const scale = Math.min(containerWidth / naturalWidth, containerHeight / naturalHeight);
  return {
    offsetX: (containerWidth - naturalWidth * scale) / 2,
    offsetY: (containerHeight - naturalHeight * scale) / 2,
    scale,
    naturalWidth,
    naturalHeight
  };
}

/** 显示坐标 → 原图像素坐标：减居中偏移、除缩放比、夹到图内。 */
export function imageToolDisplayToImage(point: { x: number; y: number }, fit: ImageToolFit): { x: number; y: number } {
  const x = (point.x - fit.offsetX) / fit.scale;
  const y = (point.y - fit.offsetY) / fit.scale;
  return {
    x: Math.min(fit.naturalWidth, Math.max(0, x)),
    y: Math.min(fit.naturalHeight, Math.max(0, y))
  };
}

/** 原图像素坐标 → 显示坐标（画角色框用）。 */
export function imageToolImageToDisplay(point: { x: number; y: number }, fit: ImageToolFit): { x: number; y: number } {
  return { x: fit.offsetX + point.x * fit.scale, y: fit.offsetY + point.y * fit.scale };
}

/**
 * 拖出的矩形（显示坐标的两个对角点，方向任意）→ 原图像素框；夹到图内后任一边 < 24px 返回 null（丢弃）。
 * 取整：起点向下、终点向上，保证框不会比用户拖出来的小。
 */
export function imageToolDisplayBoxToImage(
  start: { x: number; y: number },
  end: { x: number; y: number },
  fit: ImageToolFit,
  minSide: number = IMAGE_TOOL_MIN_BOX_SIDE
): ImageToolPixelBox | null {
  const a = imageToolDisplayToImage(start, fit);
  const b = imageToolDisplayToImage(end, fit);
  const x0 = Math.floor(Math.min(a.x, b.x));
  const y0 = Math.floor(Math.min(a.y, b.y));
  const x1 = Math.min(fit.naturalWidth, Math.ceil(Math.max(a.x, b.x)));
  const y1 = Math.min(fit.naturalHeight, Math.ceil(Math.max(a.y, b.y)));
  const w = x1 - x0;
  const h = y1 - y0;
  if (w < minSide || h < minSide) return null;
  return { x: x0, y: y0, w, h };
}

/** 原图像素框 → 显示矩形（`left/top/width/height`，画框用）。 */
export function imageToolImageBoxToDisplay(box: ImageToolPixelBox, fit: ImageToolFit) {
  const origin = imageToolImageToDisplay({ x: box.x, y: box.y }, fit);
  return { left: origin.x, top: origin.y, width: box.w * fit.scale, height: box.h * fit.scale };
}

// ---------------------------------------------------------------------------
// 发送按钮的禁用理由（CLAUDE.md §5.17：给最根本的那一条）
// ---------------------------------------------------------------------------

/**
 * 阻塞的种类，排序即优先级：权限 > 工具不可用 > 编辑器里还缺东西 > 能力校验失败。
 * tabtv 不计价（TT-T-14）：没有余额这一类。
 *
 * ⚠️ 权限必须排第一：真正的原因是没有权限时，先说别的只会让人去修不相干的问题。
 */
export type ImageToolBlockerKind = "permission" | "unavailable" | "input" | "quote";
const BLOCKER_ORDER: readonly ImageToolBlockerKind[] = ["permission", "unavailable", "input", "quote"];

export type ImageToolBlocker = {
  kind: ImageToolBlockerKind;
  text: string;
  /** 用户自己能不能解决（不可用那一类由服务端 `userFixable` 给，决定语气）。 */
  userFixable?: boolean;
};

/** 排序后的阻塞列表（稳定排序：同类保持传入顺序）。 */
export function imageToolSortBlockers(blockers: readonly (ImageToolBlocker | null | undefined | false)[]): ImageToolBlocker[] {
  return blockers
    .filter((item): item is ImageToolBlocker => Boolean(item))
    .map((item, index) => ({ item, index }))
    .sort((a, b) => BLOCKER_ORDER.indexOf(a.item.kind) - BLOCKER_ORDER.indexOf(b.item.kind) || a.index - b.index)
    .map(entry => entry.item);
}

/** 最根本的那一条；没有阻塞返回 null。 */
export function imageToolDisabledReason(blockers: readonly (ImageToolBlocker | null | undefined | false)[]): ImageToolBlocker | null {
  return imageToolSortBlockers(blockers)[0] ?? null;
}

/**
 * 工具不可用的说明：按 `userFixable` 分两种语气（IMG-F-08、CD-48），**不匹配文案**。
 */
export function imageToolUnavailableBlocker(tool: Pick<ImageToolInfo, "available" | "blockedReason" | "userFixable"> | undefined): ImageToolBlocker | null {
  if (!tool || tool.available) return null;
  const reason = tool.blockedReason?.trim();
  if (tool.userFixable) return { kind: "unavailable", text: reason || "当前图片还不满足这个工具的条件", userFixable: true };
  return { kind: "unavailable", text: `${reason || "该工具暂未开放"}（需要运营配置）`, userFixable: false };
}

/** 只读成员（没有生成权限）的说明（IMG-UX-05）。 */
export function imageToolPermissionBlocker(canGenerate: boolean, roleIsViewer: boolean): ImageToolBlocker | null {
  if (canGenerate) return null;
  return {
    kind: "permission",
    text: roleIsViewer ? "需要生成权限，你在该工作区是只读成员" : "需要生成权限，请联系工作区管理员"
  };
}

// ---------------------------------------------------------------------------
// 子节点位置（IMG-UX-06、IMGD-26）
// ---------------------------------------------------------------------------

/** 子节点之间的竖向间隙（像素）。 */
export const IMAGE_TOOL_SPAWN_GAP = 40;

/**
 * 派生子节点的位置：源节点右侧 +420px（与九宫格派生同一个身位），
 * 同一源节点已有 n 个派生子节点时向下错开 n × (节点高 + 40)。
 */
export function imageToolSpawnPosition(source: { x: number; y: number }, existingChildren: number, nodeHeight: number) {
  const n = Math.max(0, Math.floor(existingChildren));
  return { x: Math.round(source.x + 420), y: Math.round(source.y + n * (Math.max(0, nodeHeight) + IMAGE_TOOL_SPAWN_GAP)) };
}

// ---------------------------------------------------------------------------
// 受理请求
// ---------------------------------------------------------------------------

/**
 * 发出去之前把选项规范成契约形状（不拼任何提示词）：
 * - 情绪：角色带 `touched` 字段的只留 touched 的，并剥掉界面字段；
 * - 打光：智能模式只发 `{smart:true}`（IMG-F-43：其余字段置灰**且不发送**）；
 * - 多角度：补充提示词去首尾空白，空串不发；
 * - 高清：只发 `level`（老节点上残留的 provider / model / factor 一并丢掉，未知值按「标准」）。
 */
export function imageToolWireOptions(tool: ImageToolCode, options: ImageToolOptions): ImageToolOptions {
  if (tool === "expression_edit") {
    const value = options as ImageToolExpressionOptions;
    const roles = ((value.roles ?? []) as Array<Partial<ImageToolEditorRole>>)
      .filter(role => role.touched !== false)
      .map(role => ({ id: role.id, box: { ...(role.box as ImageToolPixelBox) }, arousal: Number(role.arousal ?? 0), warmth: Number(role.warmth ?? 0) }));
    return { sourceMediaId: value.sourceMediaId, count: value.count ?? 1, roles };
  }
  if (tool === "relight") {
    const value = options as ImageToolRelightOptions;
    if (value.smart) return { smart: true };
    const result: ImageToolRelightOptions = { ...value, smart: false };
    if (!result.color) result.color = null;
    return result;
  }
  if (tool === "multi_angle") {
    const value = { ...(options as ImageToolMultiAngleOptions) };
    const prompt = typeof value.prompt === "string" ? value.prompt.trim() : "";
    if (prompt) value.prompt = prompt;
    else delete value.prompt;
    return value;
  }
  const level = (options as ImageToolUpscaleOptions).level;
  return { level: level === "fine" || level === "ultra" ? level : "standard" };
}

/** 一次请求产出几张（报价与受理同一个数）：情绪按张数，其余固定 1。 */
export function imageToolCount(tool: ImageToolCode, options: ImageToolOptions): number {
  if (tool !== "expression_edit") return 1;
  const count = Number((options as ImageToolExpressionOptions).count ?? 1);
  return Number.isFinite(count) ? Math.min(4, Math.max(1, Math.round(count))) : 1;
}

/** 受理请求的输入（与 `utils/canvas` 的 `GenerationInput` 同形，不 import 以免两个工具模块互相依赖）。 */
export type ImageToolRequestInput = {
  sourceNodeId: string;
  kind: string;
  order: number;
  generationId?: string;
  referenceId?: string;
  text?: string;
  materialCode?: string;
};

/** 工具子节点受理时的操作码：四个工具都是图片模型的图生图（2026-10-31 方案变更）。 */
export const IMAGE_TOOL_OPERATION = "img2img";

export type ImageToolRequest = {
  projectId: string;
  nodeId: string;
  operation: typeof IMAGE_TOOL_OPERATION;
  modelCode: string;
  params: Record<string, unknown> & { tool: ImageToolCode; toolOptions: ImageToolOptions };
  inputs: ImageToolRequestInput[];
  count: number;
};

/**
 * 服务端写的键（IMGD-2 / R-2）：客户端传了也会被丢弃，这里先摘掉免得节点上残留的旧值混进快照对比；
 * `preset` / `presetOptions` 是九宫格的配方码，工具节点受理时服务端不拼它（R-2）。
 */
const SERVER_OWNED_PARAMS = ["upstream", "output", "preset", "presetOptions"];

/**
 * 组装 `POST /generations`（与 `/generations/estimate`）的请求体。
 *
 * ⚠️ `operation` 一律发 `img2img`（工具都是图生图），**工具身份以 `params.tool` 为准**，提示词由服务端按工具拼。
 * ⚠️ 字段名是 `modelCode`（不是 `model`，见 stores/generation 的注释）。
 * ⚠️ 高清不带 `prompt`：「高清重绘」的提示词完全由服务端按增强程度给，节点上残留的描述不能跟着发出去（R-2，服务端也会删）。
 */
export function imageToolRequest(args: {
  projectId: string;
  nodeId: string;
  tool: ImageToolCode;
  options: ImageToolOptions;
  modelCode: string;
  inputs: readonly ImageToolRequestInput[];
  /** 节点上的其它参数（会按上面的规则摘掉服务端键）；缺省为空。 */
  params?: Record<string, unknown>;
}): ImageToolRequest {
  const base = Object.fromEntries(Object.entries(args.params ?? {})
    .filter(([key]) => !SERVER_OWNED_PARAMS.includes(key) && !(args.tool === "upscale" && key === "prompt")));
  const toolOptions = imageToolWireOptions(args.tool, args.options);
  return {
    projectId: args.projectId,
    nodeId: args.nodeId,
    operation: IMAGE_TOOL_OPERATION,
    modelCode: args.modelCode,
    params: { ...base, model: args.modelCode, tool: args.tool, toolOptions },
    inputs: args.inputs.map(input => ({ ...input })),
    count: imageToolCount(args.tool, toolOptions)
  };
}
