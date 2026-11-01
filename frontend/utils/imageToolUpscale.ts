/**
 * 高清面板（docs/agent/imagetools IMGT-18；2026-10-31 方案变更后为「高清重绘」）的专用纯函数：
 * 增强程度归一、增强程度的键盘移动、目录模型挑选。
 *
 * 高清就是用图片大模型做一次图生图重绘，
 * 输出尺寸由模型决定，前端只选「模型」与「增强程度」（标准 / 精细 / 极致），提示词由服务端按程度拼。
 *
 * ⚠️ 导出名一律带 `imageToolUpscale` 前缀并在调用处显式 import（Nuxt 自动导入 `utils/` 的全部导出，重名时悄悄顶替，CLAUDE.md §5.28）。
 */
import type {
  ImageToolInfo,
  ImageToolsResolvedConfig,
  ImageToolUpscaleLevel,
  ImageToolUpscaleOptions
} from "~/utils/imageTools";

type UpscaleModel = NonNullable<ImageToolInfo["models"]>[number];
type UpscaleLevelRow = ImageToolsResolvedConfig["upscaleLevels"][number];

/** 契约里的三档（`UpscaleOptions.level`）；配置表只决定显示名与顺序，码不能超出这三个。 */
const LEVEL_CODES: readonly ImageToolUpscaleLevel[] = ["standard", "fine", "ultra"];
export const IMAGE_TOOL_UPSCALE_DEFAULT_LEVEL: ImageToolUpscaleLevel = "standard";

function isLevel(value: unknown): value is ImageToolUpscaleLevel {
  return typeof value === "string" && (LEVEL_CODES as readonly string[]).includes(value);
}

/**
 * 分段按钮的选项：取配置表里码合法的那几档（顺序随配置）；一个都没有时给固定三档。
 * ⚠️ 表外的码（配置比契约新、或写错）不显示：发出去就是 400，显示一个点了必失败的按钮比不显示更糟。
 */
export function imageToolUpscaleLevels(levels: readonly UpscaleLevelRow[] | undefined): Array<{ code: ImageToolUpscaleLevel; name: string }> {
  const rows = (levels ?? []).filter((row): row is { code: ImageToolUpscaleLevel; name: string } => isLevel(row.code));
  return rows.length
    ? rows.map(row => ({ code: row.code, name: row.name || row.code }))
    : [{ code: "standard", name: "标准" }, { code: "fine", name: "精细" }, { code: "ultra", name: "极致" }];
}

/**
 * 节点 params 里的 `toolOptions` → 合法的高清选项：只认 `level`，非法或缺省回「标准」。
 * ⚠️ 老节点里可能存着旧版本的 `provider / model / factor`：一律丢掉、不报错（它们在现方案里没有含义）。
 */
export function imageToolUpscaleOptionsOf(raw: unknown): Required<ImageToolUpscaleOptions> {
  const value = (raw && typeof raw === "object" ? raw : {}) as Record<string, unknown>;
  return { level: isLevel(value.level) ? value.level : IMAGE_TOOL_UPSCALE_DEFAULT_LEVEL };
}

/**
 * 分段按钮的方向键（IMG-N-08）：左右在几档之间移动，到头绕回；Home / End 到第一 / 最后一档。
 * 不是这几个键返回 null（调用方不 preventDefault，Tab 等照常工作）。
 */
export function imageToolUpscaleMoveLevel(
  current: ImageToolUpscaleLevel,
  key: string,
  levels: readonly { code: ImageToolUpscaleLevel }[]
): ImageToolUpscaleLevel | null {
  if (!levels.length) return null;
  const index = Math.max(0, levels.findIndex(row => row.code === current));
  switch (key) {
    case "ArrowRight":
    case "ArrowDown": return levels[(index + 1) % levels.length]!.code;
    case "ArrowLeft":
    case "ArrowUp": return levels[(index - 1 + levels.length) % levels.length]!.code;
    case "Home": return levels[0]!.code;
    case "End": return levels[levels.length - 1]!.code;
    default: return null;
  }
}

/**
 * 发给受理 / 报价的目录模型码（`/image-tools` 高清条目的 `models`，现在是支持 img2img 的图片模型）：
 * 节点上已有且仍在可选列表里就沿用；否则取列表第一个；列表还没取到时沿用节点上的（不清空，免得报价闪一下）。
 */
export function imageToolUpscaleModelCode(current: unknown, models: readonly UpscaleModel[] | undefined): string {
  const code = typeof current === "string" ? current : "";
  if (!models?.length) return code;
  return models.some(model => model.code === code) ? code : models[0]!.code;
}
