/**
 * 文字与字幕排版的前端部分（docs/agent/smartcut SCT-21，SCD-46）：缓存键、响应头解析、输入时的 DOM 临时显示。
 *
 * 两段式：输入时用 DOM 临时显示（同样的字号百分比、颜色、描边、位置），停手 400ms 后请求 `POST /smartcut/typeset`
 * 换成服务端 PNG —— 那一张就是成片里叠上去的那一张（SC-N-06）。所以临时显示只求「差不多」，不求逐像素一致；
 * 缺省值与 Go `typeset.defaultStyle` 同值，否则停手那一下文字会明显跳一下。
 *
 * ⚠️ 不碰 DOM（只给样式对象）；导出带 `smartcut` 前缀（Nuxt 自动导入 `utils/` 的全部导出，重名时悄悄顶替）。
 */

export type SmartcutTypesetInput = {
  kind: "subtitle" | "text";
  text: string;
  style?: Record<string, unknown> | null;
  box?: Record<string, unknown> | null;
};

/** 键按字典序递归排好再序列化：同样的样式不管键的先后都得到同一个缓存键。 */
function stable(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(stable);
  if (value && typeof value === "object") {
    return Object.fromEntries(Object.keys(value as Record<string, unknown>).sort().map(key => [key, stable((value as Record<string, unknown>)[key])]));
  }
  return value;
}

/**
 * 排版结果的缓存键：种类、文字、样式、位置框与画面尺寸。字幕不带位置框（它按 `bottomPct` 贴底）；
 * 画面尺寸要在键里 —— 换了画幅之后同样的文字会换行在别处。
 */
export function smartcutTypesetKey(input: SmartcutTypesetInput, width: number, height: number): string {
  return JSON.stringify(stable({
    k: input.kind,
    t: input.text,
    s: input.style ?? null,
    b: input.kind === "text" ? input.box ?? null : null,
    w: width,
    h: height
  }));
}

/** 解析 `X-Typeset-Box`（`x,y,w,h`，像素）；不合法时为 null。 */
export function smartcutParseTypesetBox(header: string | null | undefined): { x: number; y: number; w: number; h: number } | null {
  if (!header) return null;
  const parts = header.split(",").map(part => Number(part.trim()));
  if (parts.length !== 4 || parts.some(n => !Number.isFinite(n))) return null;
  const [x, y, w, h] = parts as [number, number, number, number];
  return w > 0 && h > 0 ? { x, y, w, h } : null;
}

/** `X-Typeset-Missing`（URL 编码的字符串）→ 缺字列表（去重，保持出现顺序）。 */
export function smartcutParseTypesetMissing(header: string | null | undefined): string[] {
  if (!header) return [];
  let text = "";
  try {
    text = decodeURIComponent(header);
  } catch {
    return [];
  }
  return [...new Set([...text])];
}

const num = (v: unknown, lo: number, hi: number, fallback: number): number =>
  typeof v === "number" && Number.isFinite(v) ? Math.min(hi, Math.max(lo, v)) : fallback;
const color = (v: unknown, fallback: string): string =>
  typeof v === "string" && /^#([0-9a-fA-F]{3}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$/.test(v) ? v : fallback;
const obj = (v: unknown): Record<string, unknown> | null => (v && typeof v === "object" && !Array.isArray(v) ? v as Record<string, unknown> : null);

/**
 * 输入时的 DOM 临时显示的样式（百分比都相对画面高度 `frameHeightPx` 换成像素）。规则与 Go `typeset.ParseStyle` 一致：
 * 字幕默认 4.5% 字号、贴底 8%、白字黑描边 0.3%、一行约 16 个字宽；文字层默认 8% 字号、正中。
 */
export function smartcutTypesetFallbackStyle(input: SmartcutTypesetInput, frameHeightPx: number): Record<string, string> {
  const style = obj(input.style) ?? {};
  const box = obj(input.box) ?? {};
  const subtitle = input.kind === "subtitle";
  const sizePct = num(style.fontSizePct, 2, 20, subtitle ? 4.5 : 8);
  const px = (pct: number) => `${(pct / 100) * frameHeightPx}px`;
  const out: Record<string, string> = {
    position: "absolute",
    fontSize: px(sizePct),
    lineHeight: "1.25",
    color: color(style.color, "#ffffff"),
    textAlign: typeof style.align === "string" && ["left", "center", "right"].includes(style.align) ? style.align : "center",
    whiteSpace: "pre-wrap",
    wordBreak: "break-word",
    pointerEvents: "none"
  };
  const maxWidth = num(style.maxWidthPct, 10, 100, 90);
  out.maxWidth = subtitle && style.maxWidthPct === undefined ? `min(90%, ${px(sizePct * 16)})` : `${maxWidth}%`;
  const strokeRaw = style.stroke === undefined ? (subtitle ? { widthPct: 0.3, color: "#000000" } : null) : obj(style.stroke);
  if (strokeRaw) {
    const w = num(strokeRaw.widthPct, 0, 5, 0.3);
    if (w > 0) out.webkitTextStroke = `${px(w)} ${color(strokeRaw.color, "#000000")}`;
    out.paintOrder = "stroke fill";
  }
  const bg = obj(style.background);
  if (bg) {
    const opacity = num(bg.opacity, 0, 1, 0.6);
    const base = color(bg.color, "#000000");
    out.backgroundColor = `color-mix(in srgb, ${base} ${Math.round(opacity * 100)}%, transparent)`;
    out.padding = px(num(bg.paddingPct, 0, 10, 1));
  }
  if (subtitle) {
    out.left = "50%";
    out.bottom = `${num(style.bottomPct, 0, 50, 8)}%`;
    out.transform = "translateX(-50%)";
    return out;
  }
  const x = num(box.xPct, 0, 100, 50);
  const y = num(box.yPct, 0, 100, 50);
  const anchor = typeof box.anchor === "string" && ["top", "center", "bottom"].includes(box.anchor) ? box.anchor : "center";
  out.left = `${x}%`;
  out.top = `${y}%`;
  out.transform = `translate(-50%, ${anchor === "top" ? "0" : anchor === "bottom" ? "-100%" : "-50%"})`;
  return out;
}
