/**
 * 智能剪辑时间线的纯计算（docs/agent/smartcut SCT-18；移植自 `.backup/web-rewrite/app/features/timeline/timeline.ts`）。
 *
 * ⚠️ 这一层**只算时间与像素的换算**，不碰 DOM、不发请求。时间线的错误几乎都"错得很安静"
 *    （拖动差半秒、播放头对不上片段），只有摊成纯函数才验得住。
 * ⚠️ 所有导出一律带 `smartcut` 前缀并由调用方**显式 import**：Nuxt 会自动导入 `utils/` 的全部导出，
 *    重名时悄悄顶替（CLAUDE.md §5.28），备份版里的 `clipAt` / `ticks` 这类名字在这里会撞上别的模块。
 * ⚠️ 时间一律是**成片时间**（SCK-15）：片段的 `startMs` 由服务端按顺序与转场算好（画面轨扣掉了转场重叠），
 *    这里只按它换算，不自己累加。
 */
import type { components } from "@tabtv/api-client";

type TimelineClip = components["schemas"]["TimelineClip"];
type TimelineSubtitle = components["schemas"]["TimelineSubtitle"];

/** 计算用到的片段字段（生成类型的子集：调用方传完整片段或手写的最小对象都行）。 */
export type SmartcutClipTiming = Pick<TimelineClip, "id" | "startMs" | "durationMs" | "sourceInMs" | "speed">;
/** 计算用到的字幕字段（`trackId` 可缺省：缺省的都算在同一条字幕轨上）。 */
export type SmartcutSubtitleTiming = Pick<TimelineSubtitle, "id" | "startMs" | "endMs" | "text" | "trackId">;

// ---------------------------------------------------------------- 缩放

/**
 * 每秒占多少像素。
 *
 * ⚠️ 档位固定而不是连续缩放：连续缩放下"同一条时间线在两台机器上看起来不一样"，
 *    而用户之间是靠"第几秒那一段"沟通的。固定档位还让吸附阈值（见 smartcutSnapMs）有确定含义。
 */
export const smartcutZoomLevels = [10, 20, 40, 80, 160, 320] as const;
export const smartcutDefaultZoom = 2; // 40 px/s

export function smartcutClampZoom(zoom: number): number {
  if (!Number.isFinite(zoom)) return smartcutDefaultZoom;
  return Math.min(smartcutZoomLevels.length - 1, Math.max(0, Math.round(zoom)));
}

export function smartcutPxPerSecond(zoom: number): number {
  return smartcutZoomLevels[smartcutClampZoom(zoom)]!;
}

export function smartcutMsToPx(ms: number, zoom: number): number {
  return (ms / 1000) * smartcutPxPerSecond(zoom);
}

export function smartcutPxToMs(px: number, zoom: number): number {
  return Math.round((px / smartcutPxPerSecond(zoom)) * 1000);
}

/**
 * 刻度间隔：保证刻度之间至少 60px，否则标签会叠在一起。
 *
 * ⚠️ 用固定的"好看"间隔（1/2/5/10/30/60 秒）而不是 `总长/10` —— 后者会出现"每 3.7 秒一格"这种刻度，
 *    用户没法据此估时间。
 */
const TICK_STEPS_MS = [1000, 2000, 5000, 10000, 30000, 60000, 300000];

export function smartcutTickStepMs(zoom: number, minGapPx = 60): number {
  for (const step of TICK_STEPS_MS) {
    if (smartcutMsToPx(step, zoom) >= minGapPx) return step;
  }
  return TICK_STEPS_MS[TICK_STEPS_MS.length - 1]!;
}

export function smartcutTicks(totalMs: number, zoom: number): number[] {
  const step = smartcutTickStepMs(zoom);
  const out: number[] = [];
  // 末尾多给一格：不给的话最后一段片段右侧是一片没有刻度的空白。
  for (let t = 0; t <= totalMs + step; t += step) out.push(t);
  return out;
}

// ---------------------------------------------------------------- 播放头

/** 把播放头夹在 [0, total] 内。 */
export function smartcutClampPlayhead(ms: number, totalMs: number): number {
  if (!Number.isFinite(ms) || ms < 0) return 0;
  return Math.min(totalMs, Math.round(ms));
}

/**
 * 播放头落在哪一段上（按成片时间）。
 *
 * ⚠️ 用**左闭右开**区间：相邻两段在接缝处不能都命中，否则点在接缝上时选中的是哪一段取决于遍历顺序 —— 时灵时不灵。
 * ⚠️ 画面轨有转场时相邻两段在成片里**重叠**：重叠区间里返回前一段（左闭右开的自然结果）；
 *    预览要同时拿两段时用 {@link smartcutClipsAt}。
 */
export function smartcutClipAt<T extends Pick<SmartcutClipTiming, "startMs" | "durationMs">>(clips: T[], ms: number): T | null {
  for (const c of clips) {
    if (ms >= c.startMs && ms < c.startMs + c.durationMs) return c;
  }
  // 正好落在末尾时归最后一段：否则拖到最右边会"没有选中任何片段"。
  const last = clips[clips.length - 1];
  if (last && ms === last.startMs + last.durationMs) return last;
  return null;
}

/**
 * 某一时刻正在播的全部片段（转场重叠区间里是两段）。左闭右开；末尾同 {@link smartcutClipAt} 归最后一段。
 */
export function smartcutClipsAt<T extends Pick<SmartcutClipTiming, "startMs" | "durationMs">>(clips: T[], ms: number): T[] {
  const out = clips.filter(c => ms >= c.startMs && ms < c.startMs + c.durationMs);
  if (out.length > 0) return out;
  const last = clips[clips.length - 1];
  return last && ms === last.startMs + last.durationMs ? [last] : [];
}

/**
 * 播放头在某一段里对应**素材内**的偏移。
 *
 * ⚠️ 必须带上变速换算：2 倍速时时间线上的 1 秒对应素材里的 2 秒。不换算的话预览里播的位置和时间线上的
 *    播放头对不上 —— 而两边各自看都是"在播"。
 */
export function smartcutSourceOffsetMs(c: Pick<SmartcutClipTiming, "startMs" | "sourceInMs" | "speed">, playheadMs: number): number {
  const into = Math.max(0, playheadMs - c.startMs);
  const speed = c.speed > 0 ? c.speed : 100;
  return c.sourceInMs + Math.round((into * speed) / 100);
}

// ---------------------------------------------------------------- 吸附

/**
 * 拖动时吸附到附近的关键点（片段边界、播放头、0、节拍）。
 *
 * ⚠️ 阈值按**像素**算再换回毫秒，不是固定毫秒：放大到 320px/s 时 8px 只有 25ms，而缩小到 10px/s 时 8px 是
 *    800ms —— 用固定毫秒的话，缩小之后吸附会把片段拽出去老远。
 */
export const smartcutSnapPx = 8;

export function smartcutSnapMs(ms: number, anchors: number[], zoom: number): number {
  const tolerance = smartcutPxToMs(smartcutSnapPx, zoom);
  let best = ms;
  let bestDist = tolerance + 1;
  for (const a of anchors) {
    const d = Math.abs(a - ms);
    if (d <= tolerance && d < bestDist) {
      best = a;
      bestDist = d;
    }
  }
  return best;
}

/**
 * 一条轨道上的吸附锚点：每段的首尾 + 0，可选播放头与配乐的节拍（SC-F-24、SC-F-27）。
 *
 * ⚠️ 节拍只是**额外的**锚点：吸附仍按最近点取，不会因为开了节拍就只吸节拍 —— 片段边界对齐同样重要。
 */
export function smartcutAnchorsOf(
  clips: Pick<SmartcutClipTiming, "startMs" | "durationMs">[],
  playheadMs?: number,
  beatsMs?: readonly number[]
): number[] {
  const out = [0];
  for (const c of clips) {
    out.push(c.startMs, c.startMs + c.durationMs);
  }
  if (playheadMs !== undefined) out.push(playheadMs);
  if (beatsMs) out.push(...beatsMs);
  return out;
}

// ---------------------------------------------------------------- 拖放排序

/**
 * 把"拖到了哪个时刻"翻成"插到第几位"（画面轨）。
 *
 * ⚠️ 判据是**片段的中点**而不是左边界：按左边界判的话，把 A 往右挪一点点就会越过 B 的左边界、
 *    直接跳到 B 后面 —— 而用户只是想微调。
 */
export function smartcutDropPosition(clips: Pick<SmartcutClipTiming, "startMs" | "durationMs">[], dropMs: number): number {
  let pos = 1;
  for (const c of clips) {
    if (dropMs >= c.startMs + c.durationMs / 2) pos++;
  }
  return Math.min(pos, Math.max(1, clips.length));
}

// ---------------------------------------------------------------- 展示

export function smartcutFormatTime(ms: number): string {
  const total = Math.max(0, Math.round(ms / 1000));
  const m = Math.floor(total / 60);
  const s = total % 60;
  return `${String(m).padStart(2, "0")}:${String(s).padStart(2, "0")}`;
}

/** 带毫秒的精确时间，给播放头与片段属性用。 */
export function smartcutFormatTimeMs(ms: number): string {
  const safe = Math.max(0, Math.round(ms));
  return `${smartcutFormatTime(safe)}.${String(safe % 1000).padStart(3, "0")}`;
}

/**
 * 总时长说明。
 *
 * ⚠️ 设了转场之后 `totalMs` 与 `effectiveMs` 会不一样，此时**两个都要显示**：用户按"放了多少素材"理解前者，
 *    但配乐与字幕要对齐的是后者 —— 只显示一个的话，设了转场之后字幕整体偏移，而偏多少没人说得清。
 */
export function smartcutDescribeDuration(totalMs: number, effectiveMs: number): string {
  if (effectiveMs === totalMs) return smartcutFormatTime(totalMs);
  return `${smartcutFormatTime(effectiveMs)}（素材合计 ${smartcutFormatTime(totalMs)}，转场重叠 ${
    smartcutFormatTime(totalMs - effectiveMs)}）`;
}

// ---------------------------------------------------------------- 字幕

/**
 * 当前播放头下该显示哪几条字幕。
 *
 * ⚠️ 返回**数组**不是单条：允许时间重叠（两个人同时说话是正当的），只取第一条的话第二个人的台词永远不显示。
 */
export function smartcutSubtitlesAt<T extends Pick<SmartcutSubtitleTiming, "startMs" | "endMs">>(subs: T[], ms: number): T[] {
  return subs.filter(s => ms >= s.startMs && ms < s.endMs);
}

/** 字幕时间轴的一条问题（只提示不拦截，SC-F-54）。 */
export type SmartcutSubtitleIssue = { index: number; message: string };

/** 一条字幕最长显示多久：超过这个时长观众会觉得它"卡住了"。 */
const MAX_SUBTITLE_MS = 10000;
/** 一条字幕最短显示多久：再短观众来不及看完（设计建议值，未定稿）。 */
const MIN_SUBTITLE_MS = 700;
/** 每行最多约 16 个汉字（SC-PARAM-31，按宽度算：半角字符算半个）。 */
const MAX_LINE_UNITS = 16;

/** 一行的宽度（汉字 = 1，半角 = 0.5）：「每行约 16 个汉字」是按宽度说的，英文字母窄一半。 */
function lineUnits(line: string): number {
  let units = 0;
  for (const ch of line) units += ch.codePointAt(0)! < 0x80 ? 0.5 : 1;
  return units;
}

/**
 * 字幕时间轴的问题清单（只提示不拦截，SC-F-54）：超出片长、过长、没有内容（移植自备份），
 * 以及过短、与同一轨的前一条重叠、单行字数过多（智能剪辑补上的三条）。
 *
 * ⚠️ 服务端只拦"结束不晚于开始"、"没有内容"与"同一轨里时间完全相同"这几条硬错误。其余是**质量问题不是错误** ——
 *    拦下来用户就走不下去了（与资产质量检查同一条规则）；重叠尤其如此：两个人同时说话是正当的。
 */
export function smartcutSubtitleIssues(subs: SmartcutSubtitleTiming[], totalMs: number): SmartcutSubtitleIssue[] {
  const out: SmartcutSubtitleIssue[] = [];
  const sorted = [...subs].sort((a, b) => a.startMs - b.startMs);
  const lastEndByTrack = new Map<string, number>();
  sorted.forEach((s, i) => {
    if (totalMs > 0 && s.endMs > totalMs) {
      out.push({ index: i, message: `第 ${i + 1} 条超出片长，渲染时会被截断` });
    }
    if (s.endMs - s.startMs > MAX_SUBTITLE_MS) {
      out.push({ index: i, message: `第 ${i + 1} 条显示超过 ${MAX_SUBTITLE_MS / 1000} 秒，考虑拆成两条` });
    }
    if (!s.text.trim()) {
      out.push({ index: i, message: `第 ${i + 1} 条没有内容` });
    }
    if (s.endMs > s.startMs && s.endMs - s.startMs < MIN_SUBTITLE_MS) {
      out.push({ index: i, message: `第 ${i + 1} 条显示不到 ${MIN_SUBTITLE_MS / 1000} 秒，观众可能来不及看完` });
    }
    const track = s.trackId ?? "";
    const lastEnd = lastEndByTrack.get(track);
    if (lastEnd !== undefined && s.startMs < lastEnd) {
      out.push({ index: i, message: `第 ${i + 1} 条与前一条重叠（两个人同时说话时可以不管）` });
    }
    lastEndByTrack.set(track, Math.max(lastEnd ?? 0, s.endMs));
    if (s.text.split("\n").some(line => lineUnits(line) > MAX_LINE_UNITS)) {
      out.push({ index: i, message: `第 ${i + 1} 条有一行超过约 ${MAX_LINE_UNITS} 个字，会自动换行` });
    }
  });
  return out;
}

// ---------------------------------------------------------------- 画幅

/**
 * 画幅 → 1080p 下的推荐分辨率。宽高**必须是偶数**（H.264 的 yuv420p 要求）。
 * 与服务端 `timeline/domain.AspectPresets` 同一份取值，由 `api/fixtures/smartcut/content/aspects.json` 绊住
 * （`smartcutContent.test.ts` 逐项比对）；加一个画幅要 fixture、Go、这里一起改（SC-F-21 加了 3:4）。
 */
export const smartcutAspectPresets: { ratio: string; label: string; width: number; height: number }[] = [
  { ratio: "16:9", label: "横屏 16:9", width: 1920, height: 1080 },
  { ratio: "9:16", label: "竖屏 9:16", width: 1080, height: 1920 },
  { ratio: "1:1", label: "方形 1:1", width: 1080, height: 1080 },
  { ratio: "4:3", label: "4:3", width: 1440, height: 1080 },
  { ratio: "3:4", label: "3:4", width: 1080, height: 1440 },
  { ratio: "21:9", label: "宽银幕 21:9", width: 2560, height: 1080 }
];

export function smartcutPresetFor(ratio: string) {
  return smartcutAspectPresets.find(p => p.ratio === ratio) ?? smartcutAspectPresets[0]!;
}

/** 偶数取整（H.264 的 yuv420p 要求宽高都能被 2 整除）。 */
function even(n: number): number {
  return Math.max(2, Math.round(n / 2) * 2);
}

/**
 * 导出规格的宽高（SCK-11、SC-F-71）：按画幅取 1080p 的推荐尺寸，再按短边缩放到 720 / 1080，宽高取偶数。
 * 例：16:9 → 1920×1080 / 1280×720；9:16 → 1080×1920 / 720×1280。认不出的画幅按 16:9。
 */
export function smartcutSpecSize(ratio: string, resolution: "720p" | "1080p"): { width: number; height: number } {
  const preset = smartcutPresetFor(ratio);
  const target = resolution === "720p" ? 720 : 1080;
  const short = Math.min(preset.width, preset.height);
  const scale = target / short;
  return { width: even(preset.width * scale), height: even(preset.height * scale) };
}

/**
 * 「跟随第一段素材」（SC-F-21）：按素材的宽高比、短边取 1080，换算出最接近的偶数尺寸；素材尺寸未知时回落 16:9。
 */
export function smartcutFollowSize(mediaWidth: number, mediaHeight: number): { width: number; height: number } {
  if (!(mediaWidth > 0 && mediaHeight > 0)) return { width: 1920, height: 1080 };
  const scale = 1080 / Math.min(mediaWidth, mediaHeight);
  return { width: even(mediaWidth * scale), height: even(mediaHeight * scale) };
}

/**
 * 预览框在给定容器里的尺寸。
 *
 * ⚠️ 按画幅**等比缩放**而不是直接填满：填满的话竖屏片子在横屏容器里会被拉扁，用户按那个画面调构图，
 *    导出来完全不是一回事。
 */
export function smartcutPreviewBox(
  width: number, height: number, box: { width: number; height: number }
): { width: number; height: number } {
  if (width <= 0 || height <= 0 || box.width <= 0 || box.height <= 0) {
    return { width: 0, height: 0 };
  }
  const scale = Math.min(box.width / width, box.height / height);
  return { width: Math.round(width * scale), height: Math.round(height * scale) };
}

// ---------------------------------------------------------------- 转场

export const smartcutTransitions = [
  { kind: "none", label: "无" },
  { kind: "fade", label: "淡入淡出" },
  { kind: "dissolve", label: "叠化" },
  { kind: "slide_left", label: "左滑" },
  { kind: "wipe", label: "擦除" }
] as const;

/**
 * 这一段能不能设转场、最长能设多久。
 *
 * ⚠️ 转场会吃掉两段各一半的时间，所以上限是**两侧较短那段的一半** —— 前端先算一遍是为了把滑块的范围限住；
 *    服务端仍会再判一次（前端挡一道是体验，不是安全边界）。
 */
export function smartcutMaxTransitionMs(clips: Pick<SmartcutClipTiming, "id" | "durationMs">[], clipId: string): number {
  const i = clips.findIndex(c => c.id === clipId);
  if (i < 0) return 0;
  const self = clips[i]!.durationMs;
  // 第一段是"从黑场淡入"，没有上一段可吃。
  const prev = i > 0 ? clips[i - 1]!.durationMs : Number.POSITIVE_INFINITY;
  const cap = Math.floor(Math.min(self, prev) / 2);
  return Math.max(0, Math.min(3000, cap));
}
