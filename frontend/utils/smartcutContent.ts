/**
 * 整份内容（`TimelineContent`）的保存前校验、片段编辑计算与临时 ID 改写（docs/agent/smartcut SCT-18）。
 *
 * ⚠️ 保存前校验与 Go 的 `timeline/domain.ValidateContent` 是**同一份规则**：遍历顺序、每个字段的取值范围、
 *    路径格式（`tracks[2].clips[5].transform.scale`，整份不是对象时为 `$`）都照
 *    `api/fixtures/smartcut/README.md`「内容校验的规则与顺序」，遇到第一处错误就停 —— 同一份坏数据两边报同一个 path。
 *    两边由 `api/fixtures/smartcut/content/` 绊住（SC-N-15）：改规则时 fixture、Go、这里一起改，不要在这里另立规则。
 * ⚠️ 校验的是**发出去的那份 JSON**（先 `JSON.stringify` 再解析）：`undefined` 的键会被丢掉、`NaN` 会变成 `null`，
 *    服务端看到的就是这样 —— 直接校验内存里的对象会出现「前端说能存、服务端回 40001」。
 * ⚠️ 同轨重叠（音频轨、同一文字轨）也在这里判，判定与成片时间换算共用 `smartcutFirstOverlap`（Go 同在 `layout.go`）。
 * ⚠️ 片段编辑（分割 / 删左 / 删右 / 裁切 / 变速）照 Go 的整数算法换算变速；结果以保存后服务端返回的时间线为准。
 */
import type { components } from "@tabtv/api-client";
import { smartcutFirstOverlap, smartcutMinClipMs, smartcutPlayLen, smartcutTransitionActive, type SmartcutSpan } from "./smartcutLayout";
import { smartcutAspectPresets } from "./smartcutTimeline";

type Transition = components["schemas"]["Transition"];

/** 一处校验失败：`path` 与 Go 的 `ValidationError.Path` 逐字相同。 */
export type SmartcutContentError = { path: string; reason: string };

/** 时间线上限（03 SC-PARAM-21，与 Go `timeline/domain` 的常量同值）。 */
export const smartcutContentLimits = {
  clips: 500,
  audioTracks: 6,
  subtitleTracks: 4,
  textTracks: 4,
  textLayers: 100,
  subtitles: 2000,
  textChars: 500,
  alternates: 10,
  trackNameChars: 64,
  sourceNodeIdBytes: 64,
  langChars: 16,
  timelineMs: 30 * 60 * 1000
} as const;

const MIN_CLIP_MS = smartcutMinClipMs;
const MIN_TRANSITION_MS = 100;
const MAX_TRANSITION_MS = 3000;
const MIN_SIDE = 64;
const MAX_SIDE = 7680;
const MIN_FPS = 1;
const MAX_FPS = 120;
const MIN_INT32 = -2147483648;
const MAX_INT32 = 2147483647;
const MAX_INT64 = "9223372036854775807";

const ID_PATTERN = /^([0-9]{1,19}|tmp-[A-Za-z0-9_-]{1,40})$/;
const NUMERIC_ID_PATTERN = /^[0-9]{1,19}$/;
const LANG_PATTERN = /^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8}){0,3}$/;
const COLOR_PATTERN = /^#([0-9a-fA-F]{3}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$/;

const TRACK_KINDS = new Set(["video", "audio", "subtitle", "text"]);
const TRANSITION_KINDS = new Set(["none", "fade", "dissolve", "slide_left", "wipe"]);
const CLIP_ORIGINS = new Set(["ai", "user", "sync"]);
const ALTERNATE_KINDS = new Set(["original", "regen", "desubtitle"]);
const TEXT_ALIGNS = new Set(["left", "center", "right"]);
const BOX_ANCHORS = new Set(["top", "center", "bottom"]);
const CROP_SIDES = ["left", "top", "right", "bottom"] as const;
const TRACK_LABELS: Record<string, string> = { video: "画面轨", audio: "音频轨", subtitle: "字幕轨", text: "文字轨" };
const KIND_LIMITS = [
  { kind: "audio", max: smartcutContentLimits.audioTracks },
  { kind: "subtitle", max: smartcutContentLimits.subtitleTracks },
  { kind: "text", max: smartcutContentLimits.textTracks }
];

type JsonMap = Record<string, unknown>;

function isMap(v: unknown): v is JsonMap {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

/** 字段取值；不是自有属性时为 undefined（与 Go 的 `m[key]` 缺省同义）。 */
function own(m: JsonMap, key: string): unknown {
  return Object.prototype.hasOwnProperty.call(m, key) ? m[key] : undefined;
}

const isNil = (v: unknown) => v === undefined || v === null;

function at(base: string, key: string): string {
  return base ? `${base}.${key}` : key;
}

function idx(base: string, i: number): string {
  return `${base}[${i}]`;
}

/** 码点数（Go 的 `utf8.RuneCountInString`）：一个 emoji 在 UTF-16 里占 2，这里算 1。 */
function runeCount(s: string): number {
  return [...s].length;
}

/** UTF-8 字节数（Go 的 `len(s)`）：`sourceNodeId` 的上限按字节算。 */
function utf8Len(s: string): number {
  let n = 0;
  for (const ch of s) {
    const cp = ch.codePointAt(0)!;
    n += cp < 0x80 ? 1 : cp < 0x800 ? 2 : cp < 0x10000 ? 3 : 4;
  }
  return n;
}

/** Go 的 `unicode.IsSpace`（`strings.TrimSpace` 用它）：与 JS 的 trim 差 U+0085 与 U+FEFF 两个字符。 */
function isGoSpace(cp: number): boolean {
  return (cp >= 0x09 && cp <= 0x0d) || cp === 0x20 || cp === 0x85 || cp === 0xa0 || cp === 0x1680
    || (cp >= 0x2000 && cp <= 0x200a) || cp === 0x2028 || cp === 0x2029 || cp === 0x202f || cp === 0x205f || cp === 0x3000;
}

function blank(s: string): boolean {
  for (const ch of s) {
    if (!isGoSpace(ch.codePointAt(0)!)) return false;
  }
  return true;
}

/** 数字串能不能放进 int64 且大于 0（Go 的 `strconv.ParseInt` + `n > 0`）。 */
function positiveInt64(s: string): boolean {
  const digits = s.replace(/^0+/, "");
  if (!digits) return false;
  return digits.length < 19 || (digits.length === 19 && digits <= MAX_INT64);
}

// ---------------------------------------------------------------- 画幅

function presetOf(aspect: string) {
  return smartcutAspectPresets.find(p => p.ratio === aspect);
}

function aspectError(aspect: string): SmartcutContentError | null {
  return presetOf(aspect) || aspect === "custom" ? null : { path: "aspectRatio", reason: `不支持的画幅 ${aspect}` };
}

function fpsError(fps: number): SmartcutContentError | null {
  return fps < MIN_FPS || fps > MAX_FPS ? { path: "fps", reason: `帧率只支持 ${MIN_FPS}~${MAX_FPS}，当前 ${fps}` } : null;
}

function sideError(path: string, v: number): SmartcutContentError | null {
  if (v < MIN_SIDE || v > MAX_SIDE) return { path, reason: `宽高只支持 ${MIN_SIDE}~${MAX_SIDE} 像素，当前 ${v}` };
  if (v % 2 !== 0) return { path, reason: `宽高必须是偶数，当前 ${v}` };
  return null;
}

/** 固定画幅的宽高自洽（容差 2%）；custom 不判。错误记在 height 上。 */
function ratioError(aspect: string, w: number, h: number): SmartcutContentError | null {
  const preset = presetOf(aspect);
  if (!preset || h === 0) return null;
  const want = preset.width / preset.height;
  const got = w / h;
  return Math.abs(got - want) / want > 0.02 ? { path: "height", reason: `尺寸 ${w}×${h} 与画幅 ${aspect} 不符` } : null;
}

/**
 * 画幅、帧率与尺寸（与 Go 的 `ValidateFormat` 同一顺序：画幅 → 帧率 → 宽 → 高 → 宽高自洽）。合法时为 null。
 * 剪辑台改画幅、填自定义尺寸时直接用它，不必拼一整份内容。
 */
export function smartcutValidateFormat(aspect: string, fps: number, width: number, height: number): SmartcutContentError | null {
  return aspectError(aspect) ?? fpsError(fps) ?? sideError("width", width) ?? sideError("height", height) ?? ratioError(aspect, width, height);
}

// ---------------------------------------------------------------- 整份内容

class ContentChecker {
  err: SmartcutContentError | null = null;
  /** 已出现过的 ID → 路径（四类对象共用一张 idMap，ID 必须全局唯一）。 */
  private ids = new Map<string, string>();
  private trackKind = new Map<string, string>();
  private clipIds = new Set<string>();
  /** 片段 ID → 所在轨道的种类；音频片段的锚点在所有轨道看完之后再查（锚点指向的画面片段可能排在后面）。 */
  private clipKinds = new Map<string, string>();
  private clipAnchors: Array<{ path: string; v: unknown }> = [];
  private clipCount = 0;
  /** 文字轨的 ID，按内容里的顺序（文字层的同轨重叠按这个顺序查）。 */
  private textTracks: string[] = [];

  ok(): boolean {
    return this.err === null;
  }

  fail(path: string, reason: string): void {
    if (!this.err) this.err = { path, reason };
  }

  set(e: SmartcutContentError | null): void {
    if (!this.err && e) this.err = e;
  }

  root(m: JsonMap): void {
    this.integer(m, "", "version", true, 0, MAX_INT32);
    if (!this.ok()) return;
    const aspectRaw = own(m, "aspectRatio");
    const aspect = typeof aspectRaw === "string" ? aspectRaw : "";
    if (isNil(aspectRaw)) this.fail("aspectRatio", "必填");
    else if (typeof aspectRaw !== "string") this.fail("aspectRatio", "必须是字符串");
    else this.set(aspectError(aspect));
    const [fps] = this.integer(m, "", "fps", true, MIN_INT32, MAX_INT32);
    if (this.ok()) this.set(fpsError(fps));
    const [w] = this.integer(m, "", "width", true, MIN_INT32, MAX_INT32);
    if (this.ok()) this.set(sideError("width", w));
    const [h] = this.integer(m, "", "height", true, MIN_INT32, MAX_INT32);
    if (this.ok()) this.set(sideError("height", h));
    if (!this.ok()) return;
    this.set(ratioError(aspect, w, h));

    const [tracks] = this.array(m, "", "tracks", true);
    if (!this.ok()) return;
    const kinds = new Map<string, number>();
    for (let i = 0; i < tracks.length; i++) {
      this.track(idx("tracks", i), tracks[i]);
      if (!this.ok()) return;
      const kind = own(tracks[i] as JsonMap, "kind") as string;
      kinds.set(kind, (kinds.get(kind) ?? 0) + 1);
    }
    const videos = kinds.get("video") ?? 0;
    if (videos !== 1) {
      this.fail("tracks", `必须恰好有一条画面轨，当前 ${videos} 条`);
      return;
    }
    for (const limit of KIND_LIMITS) {
      const count = kinds.get(limit.kind) ?? 0;
      if (count > limit.max) {
        this.fail("tracks", `${TRACK_LABELS[limit.kind]}最多 ${limit.max} 条，当前 ${count} 条`);
        return;
      }
    }
    if (this.clipCount > smartcutContentLimits.clips) {
      this.fail("tracks", `片段最多 ${smartcutContentLimits.clips} 个，当前 ${this.clipCount} 个`);
      return;
    }
    // 音频片段的锚点（SC-F-56）：形状同字幕；指向这份内容里的片段时，那一段必须在画面轨上。
    for (const pa of this.clipAnchors) {
      this.anchor(pa.path, pa.v);
      if (!this.ok()) return;
      const target = own(pa.v as JsonMap, "clipId") as string;
      const kind = this.clipKinds.get(target);
      if (kind !== undefined && kind !== "video") {
        this.fail(at(pa.path, "clipId"), "只能跟随画面轨上的片段");
        return;
      }
    }

    const [subs] = this.array(m, "", "subtitles", false);
    if (!this.ok()) return;
    if (subs.length > smartcutContentLimits.subtitles) {
      this.fail("subtitles", `字幕最多 ${smartcutContentLimits.subtitles} 条，当前 ${subs.length} 条`);
      return;
    }
    // 同一条字幕轨上起止完全相同的两条（重叠允许，完全相同多半是重复提交）。
    const seenRange = new Map<string, string>();
    for (let i = 0; i < subs.length; i++) {
      const p = idx("subtitles", i);
      this.timedItem(p, subs[i], "subtitle");
      if (!this.ok()) return;
      const sm = subs[i] as JsonMap;
      this.textStyle(at(p, "style"), own(sm, "style"), true);
      this.anchor(at(p, "anchor"), own(sm, "anchor"));
      if (!this.ok()) return;
      const key = `${own(sm, "trackId")}|${own(sm, "startMs")}|${own(sm, "endMs")}`;
      const prev = seenRange.get(key);
      if (prev !== undefined) {
        this.fail(at(p, "startMs"), `与 ${prev} 的起止完全相同`);
        return;
      }
      seenRange.set(key, p);
    }

    const [texts] = this.array(m, "", "texts", false);
    if (!this.ok()) return;
    if (texts.length > smartcutContentLimits.textLayers) {
      this.fail("texts", `文字层最多 ${smartcutContentLimits.textLayers} 个，当前 ${texts.length} 个`);
      return;
    }
    const byTrack = new Map<string, SmartcutSpan[]>();
    for (let i = 0; i < texts.length; i++) {
      const p = idx("texts", i);
      this.timedItem(p, texts[i], "text");
      if (!this.ok()) return;
      const tm = texts[i] as JsonMap;
      this.textStyle(at(p, "style"), own(tm, "style"), false);
      this.box(at(p, "box"), own(tm, "box"));
      if (!this.ok()) return;
      const trackId = own(tm, "trackId") as string;
      const spans = byTrack.get(trackId) ?? [];
      spans.push({ start: own(tm, "startMs") as number, end: own(tm, "endMs") as number, index: i });
      byTrack.set(trackId, spans);
    }
    // 文字轨同轨不许重叠（SCD-15），按文字轨在内容里的顺序查；字幕允许重叠（两个人同时说话）。
    for (const trackId of this.textTracks) {
      const k = smartcutFirstOverlap(byTrack.get(trackId) ?? []);
      if (k >= 0) {
        this.fail(at(idx("texts", k), "startMs"), "与同一条文字轨上的另一个文字层重叠");
        return;
      }
    }
  }

  private track(p: string, v: unknown): void {
    if (!isMap(v)) {
      this.fail(p, "轨道必须是对象");
      return;
    }
    this.id(p, v);
    const kindRaw = own(v, "kind");
    const kind = typeof kindRaw === "string" ? kindRaw : "";
    if (this.ok() && !TRACK_KINDS.has(kind)) this.fail(at(p, "kind"), `不支持的轨道种类 ${JSON.stringify(kind)}`);
    if (!this.ok()) return;
    this.trackKind.set(own(v, "id") as string, kind);
    if (kind === "text") this.textTracks.push(own(v, "id") as string);
    const name = own(v, "name");
    if (!isNil(name)) {
      if (typeof name !== "string") this.fail(at(p, "name"), "必须是字符串");
      else if (runeCount(name) > smartcutContentLimits.trackNameChars) this.fail(at(p, "name"), `名称最多 ${smartcutContentLimits.trackNameChars} 个字`);
    }
    const role = this.optString(v, p, "role");
    if (role && role.value !== "") {
      const valid = kind === "audio" ? ["music", "voice", "sfx"].includes(role.value) : kind === "video" && role.value === "voice";
      if (!valid) this.fail(role.path, `${TRACK_LABELS[kind]}不能标用途 ${JSON.stringify(role.value)}`);
    }
    const lang = this.optString(v, p, "lang");
    if (lang && lang.value !== "") {
      if (kind !== "subtitle") this.fail(lang.path, "只有字幕轨有语言");
      else if (lang.value.length > smartcutContentLimits.langChars || !LANG_PATTERN.test(lang.value)) {
        this.fail(lang.path, `语言代码格式不对：${JSON.stringify(lang.value)}`);
      }
    }
    this.optBool(v, p, "isHidden");
    this.optBool(v, p, "isMuted");
    this.integer(v, p, "volume", false, 0, 200);
    if (!this.ok()) return;
    const style = own(v, "style");
    if (!isNil(style)) {
      if (!isMap(style)) {
        this.fail(at(p, "style"), "必须是对象");
        return;
      }
      if (kind === "subtitle") this.textStyle(at(p, "style"), style, true);
      else if (kind === "audio") this.duck(at(at(p, "style"), "duck"), own(style, "duck"));
      if (!this.ok()) return;
    }
    const [clips] = this.array(v, p, "clips", false);
    if (!this.ok()) return;
    if (clips.length > 0 && (kind === "subtitle" || kind === "text")) {
      this.fail(at(p, "clips"), `${TRACK_LABELS[kind]}不放素材`);
      return;
    }
    let prevLen = 0;
    for (let j = 0; j < clips.length; j++) {
      const playLen = this.clip(idx(at(p, "clips"), j), clips[j], kind, prevLen, j === clips.length - 1);
      if (!this.ok()) return;
      prevLen = playLen;
    }
    this.clipCount += clips.length;
    // 音频轨按起点放置，同轨不许重叠（SCD-15）：报在按起点排序后重叠的后一段上。
    // 跟随片段的不参与（SC-F-56）：它的落点跟着画面走，裁一下画面就可能挨到别的音效。
    if (kind === "audio") {
      const j = smartcutFirstOverlap(clips.flatMap((cv, index) => {
        if (!isNil(own(cv as JsonMap, "anchor"))) return [];
        const start = own(cv as JsonMap, "startMs") as number;
        return [{ start, end: start + clipPlayLen(cv as JsonMap), index }];
      }));
      if (j >= 0) {
        this.fail(at(idx(at(p, "clips"), j), "startMs"), "与同一条音频轨上的另一段重叠");
        return;
      }
    }
    if (kind === "video") {
      let total = 0;
      clips.forEach((cv, j) => {
        const cm = cv as JsonMap;
        total += clipPlayLen(cm);
        if (j > 0) {
          const t = transitionOf(own(cm, "transitionIn"));
          if (smartcutTransitionActive(t)) total -= t.durationMs;
        }
      });
      if (total > smartcutContentLimits.timelineMs) {
        this.fail(at(p, "clips"), `成片最长 ${smartcutContentLimits.timelineMs / 60000} 分钟，当前 ${(total / 60000).toFixed(1)} 分钟`);
      }
    }
  }

  /** 校验一个片段，返回它在成片里的长度（给下一段的转场判断用）。 */
  private clip(p: string, v: unknown, kind: string, prevLen: number, last: boolean): number {
    if (!isMap(v)) {
      this.fail(p, "片段必须是对象");
      return 0;
    }
    this.id(p, v);
    if (!this.ok()) return 0;
    this.clipIds.add(own(v, "id") as string);
    this.clipKinds.set(own(v, "id") as string, kind);
    const anchor = own(v, "anchor");
    if (!isNil(anchor)) {
      if (kind !== "audio") {
        this.fail(at(p, "anchor"), "只有音频轨上的片段可以跟随画面轨的一段");
        return 0;
      }
      this.clipAnchors.push({ path: at(p, "anchor"), v: anchor });
    }
    this.numericId(v, p, "mediaId", true);
    const [inMs] = this.integer(v, p, "sourceInMs", true, 0, MAX_INT32);
    const [outMs] = this.integer(v, p, "sourceOutMs", true, 0, MAX_INT32);
    if (this.ok() && outMs <= inMs) this.fail(at(p, "sourceOutMs"), "取用终点必须晚于起点");
    // 画面轨的 startMs 传了也忽略：落点由服务端按顺序与转场算。
    let start = 0;
    if (kind !== "video") start = this.integer(v, p, "startMs", true, 0, MAX_INT32)[0];
    this.integer(v, p, "speed", false, 25, 400);
    this.integer(v, p, "volume", false, 0, 200);
    if (!this.ok()) return 0;
    const playLen = clipPlayLen(v);
    if (playLen < MIN_CLIP_MS) {
      this.fail(at(p, "sourceOutMs"), `片段太短（成片里不足 ${MIN_CLIP_MS} 毫秒），渲染时会被丢掉`);
      return 0;
    }
    const [fadeIn] = this.integer(v, p, "fadeInMs", false, 0, MAX_INT32);
    const [fadeOut] = this.integer(v, p, "fadeOutMs", false, 0, MAX_INT32);
    if (this.ok() && fadeIn + fadeOut > playLen) {
      this.fail(at(p, "fadeOutMs"), `淡入淡出合计 ${fadeIn + fadeOut} 毫秒，超过片段本身的 ${playLen} 毫秒`);
    }
    if (!this.ok()) return 0;
    // 转场：画面轨的转入要同时照顾上一段（第一段的转入是从黑场淡入，没有上一段可吃）；
    // 转出只在最后一段有意义，中间段的只校验取值（复制来的片段可能带着它）。
    this.transition(at(p, "transitionIn"), own(v, "transitionIn"), kind === "video" ? prevLen : 0, playLen);
    this.transition(at(p, "transitionOut"), own(v, "transitionOut"), kind === "video" && !last ? -1 : 0, playLen);
    this.transform(at(p, "transform"), own(v, "transform"));
    const origin = this.optString(v, p, "origin");
    if (origin && origin.value !== "" && !CLIP_ORIGINS.has(origin.value)) this.fail(origin.path, `不支持的来源 ${JSON.stringify(origin.value)}`);
    const node = this.optString(v, p, "sourceNodeId");
    if (node && utf8Len(node.value) > smartcutContentLimits.sourceNodeIdBytes) {
      this.fail(node.path, `节点 ID 最长 ${smartcutContentLimits.sourceNodeIdBytes} 个字符`);
    }
    this.numericId(v, p, "sourceShotId", false);
    this.alternates(at(p, "alternates"), own(v, "alternates"));
    if (this.ok() && kind !== "video" && start + playLen > smartcutContentLimits.timelineMs) {
      this.fail(at(p, "startMs"), `超出成片最长 ${smartcutContentLimits.timelineMs / 60000} 分钟`);
    }
    return playLen;
  }

  /** prevLen：-1 = 只校验取值（中间段的转出会被忽略）；0 = 没有上一段；>0 = 上一段的成片长度。 */
  private transition(p: string, v: unknown, prevLen: number, selfLen: number): void {
    if (!this.ok() || isNil(v)) return;
    if (!isMap(v)) {
      this.fail(p, "必须是对象");
      return;
    }
    const kind = own(v, "kind");
    if (typeof kind !== "string" || !TRANSITION_KINDS.has(kind)) {
      this.fail(at(p, "kind"), `不支持的转场类型 ${String(kind)}`);
      return;
    }
    const [d, present] = this.integer(v, p, "durationMs", false, 0, MAX_INT32);
    if (!this.ok() || !present) return;
    if (!smartcutTransitionActive({ kind, durationMs: d })) return;
    if (d < MIN_TRANSITION_MS || d > MAX_TRANSITION_MS) {
      this.fail(at(p, "durationMs"), `转场时长只支持 ${MIN_TRANSITION_MS}~${MAX_TRANSITION_MS} 毫秒，当前 ${d}`);
      return;
    }
    if (prevLen < 0) return;
    if (d * 2 > selfLen) {
      this.fail(at(p, "durationMs"), `转场 ${d} 毫秒超过本段 ${selfLen} 毫秒的一半，会把这一段吃光`);
    } else if (prevLen > 0 && d * 2 > prevLen) {
      this.fail(at(p, "durationMs"), `转场 ${d} 毫秒超过上一段 ${prevLen} 毫秒的一半，会把上一段吃光`);
    }
  }

  private transform(p: string, v: unknown): void {
    if (!this.ok() || isNil(v)) return;
    if (!isMap(v)) {
      this.fail(p, "必须是对象");
      return;
    }
    this.number(v, p, "x", -100, 100);
    this.number(v, p, "y", -100, 100);
    this.number(v, p, "scale", 0.1, 5);
    this.number(v, p, "rotation", -180, 180);
    if (!this.ok()) return;
    const crop = own(v, "crop");
    if (!isNil(crop)) {
      if (!isMap(crop)) {
        this.fail(at(p, "crop"), "必须是对象");
        return;
      }
      for (const side of CROP_SIDES) this.number(crop, at(p, "crop"), side, 0, 45);
    }
  }

  private alternates(p: string, v: unknown): void {
    if (!this.ok() || isNil(v)) return;
    if (!Array.isArray(v)) {
      this.fail(p, "必须是数组");
      return;
    }
    if (v.length > smartcutContentLimits.alternates) {
      this.fail(p, `备选最多 ${smartcutContentLimits.alternates} 个，当前 ${v.length} 个`);
      return;
    }
    for (let i = 0; i < v.length; i++) {
      const ap = idx(p, i);
      const m = v[i];
      if (!isMap(m)) {
        this.fail(ap, "必须是对象");
        return;
      }
      this.numericId(m, ap, "mediaId", true);
      const [inMs] = this.integer(m, ap, "sourceInMs", true, 0, MAX_INT32);
      const [outMs] = this.integer(m, ap, "sourceOutMs", true, 0, MAX_INT32);
      if (this.ok() && outMs <= inMs) this.fail(at(ap, "sourceOutMs"), "取用终点必须晚于起点");
      const kind = own(m, "kind");
      if (this.ok() && (typeof kind !== "string" || !ALTERNATE_KINDS.has(kind))) this.fail(at(ap, "kind"), `不支持的备选来历 ${String(kind)}`);
      this.numericId(m, ap, "generationId", false);
      if (!this.ok()) return;
    }
  }

  /** 字幕与文字层共有的部分：id、所属轨道、起止、文字。 */
  private timedItem(p: string, v: unknown, wantKind: "subtitle" | "text"): void {
    if (!isMap(v)) {
      this.fail(p, "必须是对象");
      return;
    }
    this.id(p, v);
    if (!this.ok()) return;
    const trackId = own(v, "trackId");
    if (typeof trackId !== "string" || trackId === "") this.fail(at(p, "trackId"), "缺少所属轨道");
    else if (!this.trackKind.get(trackId)) this.fail(at(p, "trackId"), `轨道 ${trackId} 不在这份内容里`);
    else if (this.trackKind.get(trackId) !== wantKind) this.fail(at(p, "trackId"), `轨道 ${trackId} 不是${TRACK_LABELS[wantKind]}`);
    const [start] = this.integer(v, p, "startMs", true, 0, MAX_INT32);
    const [end] = this.integer(v, p, "endMs", true, 0, MAX_INT32);
    if (!this.ok()) return;
    if (end <= start) {
      this.fail(at(p, "endMs"), "结束必须晚于开始");
      return;
    }
    if (end > smartcutContentLimits.timelineMs) {
      this.fail(at(p, "endMs"), `超出成片最长 ${smartcutContentLimits.timelineMs / 60000} 分钟`);
      return;
    }
    const text = own(v, "text");
    if (typeof text !== "string" || blank(text)) this.fail(at(p, "text"), "文字不能为空");
    else if (runeCount(text) > smartcutContentLimits.textChars) this.fail(at(p, "text"), `文字最多 ${smartcutContentLimits.textChars} 个字`);
  }

  /** 文字与字幕的样式。`bottomPct` 只对字幕有意义，文字层上出现了按未知字段留着。 */
  private textStyle(p: string, v: unknown, subtitle: boolean): void {
    if (!this.ok() || isNil(v)) return;
    if (!isMap(v)) {
      this.fail(p, "必须是对象");
      return;
    }
    this.number(v, p, "fontSizePct", 2, 20);
    this.color(v, p, "color");
    const stroke = own(v, "stroke");
    if (this.ok() && !isNil(stroke)) {
      if (!isMap(stroke)) {
        this.fail(at(p, "stroke"), "必须是对象");
        return;
      }
      this.color(stroke, at(p, "stroke"), "color");
      this.number(stroke, at(p, "stroke"), "widthPct", 0, 5);
    }
    const bg = own(v, "background");
    if (this.ok() && !isNil(bg)) {
      if (!isMap(bg)) {
        this.fail(at(p, "background"), "必须是对象");
        return;
      }
      this.color(bg, at(p, "background"), "color");
      this.number(bg, at(p, "background"), "opacity", 0, 1);
      this.number(bg, at(p, "background"), "paddingPct", 0, 10);
    }
    const align = this.optString(v, p, "align");
    if (align && !TEXT_ALIGNS.has(align.value)) this.fail(align.path, `不支持的对齐方式 ${JSON.stringify(align.value)}`);
    this.number(v, p, "maxWidthPct", 10, 100);
    if (subtitle) this.number(v, p, "bottomPct", 0, 50);
  }

  private box(p: string, v: unknown): void {
    if (!this.ok() || isNil(v)) return;
    if (!isMap(v)) {
      this.fail(p, "必须是对象");
      return;
    }
    this.number(v, p, "xPct", 0, 100);
    this.number(v, p, "yPct", 0, 100);
    const anchor = this.optString(v, p, "anchor");
    if (anchor && !BOX_ANCHORS.has(anchor.value)) this.fail(anchor.path, `不支持的锚点 ${JSON.stringify(anchor.value)}`);
  }

  /**
   * 跟随片段的锚点：`clipId` 格式同对象 ID；临时 ID 必须指向这份内容里的片段，
   * 正式 ID 可以指向已经不在的片段（片段被删后锚点失效，字幕按存储的起止留在原处 —— 正常状态，不拦）。
   */
  private anchor(p: string, v: unknown): void {
    if (!this.ok() || isNil(v)) return;
    if (!isMap(v)) {
      this.fail(p, "必须是对象");
      return;
    }
    const clipId = own(v, "clipId");
    if (typeof clipId !== "string" || !ID_PATTERN.test(clipId)) {
      this.fail(at(p, "clipId"), `跟随的片段 ID 格式不对：${String(clipId)}`);
      return;
    }
    if (smartcutIsTempId(clipId) && !this.clipIds.has(clipId)) {
      this.fail(at(p, "clipId"), `跟随的片段 ${clipId} 不在这份内容里`);
      return;
    }
    const [start] = this.integer(v, p, "srcStartMs", true, 0, MAX_INT32);
    const [end] = this.integer(v, p, "srcEndMs", true, 0, MAX_INT32);
    if (this.ok() && end <= start) this.fail(at(p, "srcEndMs"), "结束必须晚于开始");
  }

  /** 配乐轨的人声闪避开关（SC-PARAM-28）。 */
  private duck(p: string, v: unknown): void {
    if (!this.ok() || isNil(v)) return;
    if (!isMap(v)) {
      this.fail(p, "必须是对象");
      return;
    }
    this.optBool(v, p, "enabled");
    this.number(v, p, "depthDb", 0, 40);
  }

  // ---------------------------------------------------------------- 取值助手

  /** 对象的 id：格式对、且在整份内容里唯一（四类对象共用 idMap）。 */
  private id(p: string, m: JsonMap): void {
    if (!this.ok()) return;
    const s = own(m, "id");
    if (typeof s !== "string" || !ID_PATTERN.test(s)) {
      this.fail(at(p, "id"), "ID 必须是数字串或 tmp- 开头的临时 ID");
      return;
    }
    if (NUMERIC_ID_PATTERN.test(s) && !positiveInt64(s)) {
      this.fail(at(p, "id"), "ID 超出范围");
      return;
    }
    const prev = this.ids.get(s);
    if (prev !== undefined) {
      this.fail(at(p, "id"), `与 ${prev} 的 ID 重复`);
      return;
    }
    this.ids.set(s, p);
  }

  private numericId(m: JsonMap, p: string, key: string, required: boolean): void {
    if (!this.ok()) return;
    const v = own(m, key);
    if (isNil(v)) {
      if (required) this.fail(at(p, key), "必填");
      return;
    }
    if (typeof v !== "string" || !NUMERIC_ID_PATTERN.test(v)) {
      this.fail(at(p, key), "必须是数字串");
      return;
    }
    if (!positiveInt64(v)) this.fail(at(p, key), "超出范围");
  }

  /** 整数字段。缺省时 required 报错、否则放行（返回 [0, false]）。 */
  private integer(m: JsonMap, p: string, key: string, required: boolean, lo: number, hi: number): [number, boolean] {
    if (!this.ok()) return [0, false];
    const v = own(m, key);
    if (isNil(v)) {
      if (required) this.fail(at(p, key), "必填");
      return [0, false];
    }
    if (typeof v !== "number" || !Number.isInteger(v)) {
      this.fail(at(p, key), "必须是整数");
      return [0, false];
    }
    if (v < lo || v > hi) {
      this.fail(at(p, key), `取值 ${v} 超出范围 ${lo}~${hi}`);
      return [0, false];
    }
    return [v, true];
  }

  /** 可选的数字字段（缺省放行，前端补默认值）。 */
  private number(m: JsonMap, p: string, key: string, lo: number, hi: number): void {
    if (!this.ok()) return;
    const v = own(m, key);
    if (isNil(v)) return;
    if (typeof v !== "number") {
      this.fail(at(p, key), "必须是数字");
      return;
    }
    if (v < lo || v > hi) this.fail(at(p, key), `取值 ${v} 超出范围 ${lo}~${hi}`);
  }

  private color(m: JsonMap, p: string, key: string): void {
    if (!this.ok()) return;
    const v = own(m, key);
    if (isNil(v)) return;
    if (typeof v !== "string" || !COLOR_PATTERN.test(v)) this.fail(at(p, key), "颜色只支持 #RGB / #RRGGBB / #RRGGBBAA");
  }

  private optBool(m: JsonMap, p: string, key: string): void {
    if (!this.ok()) return;
    const v = own(m, key);
    if (!isNil(v) && typeof v !== "boolean") this.fail(at(p, key), "必须是布尔值");
  }

  /** 可选的字符串字段：出现了且是字符串时返回路径与取值。 */
  private optString(m: JsonMap, p: string, key: string): { path: string; value: string } | null {
    if (!this.ok()) return null;
    const v = own(m, key);
    if (isNil(v)) return null;
    if (typeof v !== "string") {
      this.fail(at(p, key), "必须是字符串");
      return null;
    }
    return { path: at(p, key), value: v };
  }

  /** 数组字段：缺省或 null 时 required 报错、否则视为空。 */
  private array(m: JsonMap, p: string, key: string, required: boolean): [unknown[], boolean] {
    if (!this.ok()) return [[], false];
    const v = own(m, key);
    if (isNil(v)) {
      if (required) this.fail(at(p, key), "必填");
      return [[], false];
    }
    if (!Array.isArray(v)) {
      this.fail(at(p, key), "必须是数组");
      return [[], false];
    }
    return [v, true];
  }
}

/** 与 Go 的 `clipPlayLen` 同一个算法（字段已校验过类型）。 */
function clipPlayLen(m: JsonMap): number {
  const speed = own(m, "speed");
  return smartcutPlayLen({
    sourceInMs: own(m, "sourceInMs") as number,
    sourceOutMs: own(m, "sourceOutMs") as number,
    speed: typeof speed === "number" ? Math.trunc(speed) : 100
  });
}

function transitionOf(v: unknown): { kind: string; durationMs: number } {
  if (!isMap(v)) return { kind: "", durationMs: 0 };
  const kind = own(v, "kind");
  const d = own(v, "durationMs");
  return { kind: typeof kind === "string" ? kind : "", durationMs: typeof d === "number" ? Math.trunc(d) : 0 };
}

/**
 * 保存前校验（与 Go 的 `ValidateContent` 同一份规则）。合法时为 null；否则给出第一处错误的字段路径。
 */
export function smartcutValidateContent(content: unknown): SmartcutContentError | null {
  let raw: unknown;
  try {
    const text = JSON.stringify(content);
    if (text === undefined) return { path: "$", reason: "内容必须是 JSON 对象" };
    raw = JSON.parse(text);
  } catch {
    return { path: "$", reason: "内容不能序列化成 JSON" };
  }
  if (!isMap(raw)) return { path: "$", reason: "内容必须是 JSON 对象" };
  const checker = new ContentChecker();
  checker.root(raw);
  return checker.err;
}

// ---------------------------------------------------------------- 片段编辑（SC-F-24）

/** 编辑结果：失败时说清为什么（切点太靠边、片段会太短……），界面原样显示。 */
export type SmartcutEditResult<T> = { ok: true; value: T } | { ok: false; reason: string };

/** 编辑计算用到的片段字段（整份内容的片段与读回的片段都满足；其余字段原样带着走）。 */
export type SmartcutEditableClip = {
  id: string;
  sourceInMs: number;
  sourceOutMs: number;
  speed?: number;
  startMs?: number;
  durationMs?: number;
  fadeInMs?: number;
  fadeOutMs?: number;
  transitionIn?: Transition | null;
  transitionOut?: Transition | null;
  origin?: string;
  sourceShotId?: string | null;
  pendingGenerationId?: string | null;
};

/** 变速档位（SC-F-24）：0.5× / 0.75× / 1× / 1.25× / 1.5× / 2×。 */
export const smartcutSpeedLevels = [50, 75, 100, 125, 150, 200] as const;

function speedOf(clip: Pick<SmartcutEditableClip, "speed">): number {
  return clip.speed && clip.speed > 0 ? clip.speed : 100;
}

/** 成片偏移 → 素材偏移（与 Go 的 `Split` 同一个换算：截断取整）。 */
function sourceOffset(clip: SmartcutEditableClip, atMs: number): number {
  return Math.trunc((atMs * speedOf(clip)) / 100);
}

function without<T extends object>(obj: T, ...keys: string[]): T {
  return Object.fromEntries(Object.entries(obj).filter(([key]) => !keys.includes(key))) as T;
}

/**
 * 用户动过的片段：来源改写为 user（SCD-5：AI 的追加指令不碰用户改过的），带着 `durationMs` 的重算长度，
 * 淡入淡出收进新长度（合计超过片段本身的话保存会被拒，而用户只是裁了一刀）。
 */
function touched<T extends SmartcutEditableClip>(clip: T): T {
  const next: SmartcutEditableClip = { ...clip, origin: "user" };
  const len = smartcutPlayLen(next);
  if (typeof next.durationMs === "number") next.durationMs = len;
  const fadeIn = next.fadeInMs ?? 0;
  const fadeOut = next.fadeOutMs ?? 0;
  if (fadeIn + fadeOut > len) {
    const keepIn = Math.min(fadeIn, Math.floor(len / 2));
    if (next.fadeInMs !== undefined) next.fadeInMs = keepIn;
    if (next.fadeOutMs !== undefined) next.fadeOutMs = Math.min(fadeOut, len - keepIn);
  }
  return next as T;
}

const tooClose = (): { ok: false; reason: string } => ({ ok: false, reason: `切点太靠边：两侧都至少要有 ${MIN_CLIP_MS} 毫秒` });

/**
 * 在片段里成片偏移 `atMs` 处分割（`atMs` = 播放头 − 片段的成片起点）。与 Go 的 `domain.Split` 同一套换算：
 * 两侧都至少 100 毫秒；`cut = sourceIn + atMs × speed / 100`；左半段不带原来的淡出，右半段不带原来的淡入，
 * 也不继承溯源（`sourceShotId`）、备选（记的是原片段换素材前的取用区间）与进行中的片段级生成
 * （那次生成按片段 ID 回填，认的是保留原 ID 的左半段）。
 *
 * 与 Go 不同的两处（已回报主线，由 SCT-04 / SCT-05 统一）：
 * - **右半段不继承转入**：继承的话刀口上凭空多一次叠化，成片还短了一个转场时长，而用户只是切了一刀；
 * - **左半段不带转出**：它已不是最后一段，转出（淡出到黑场）跟着右半段走。
 * 两半的长度变了，相邻转场可能超出上限 —— 调用方随后对整条轨跑一次 {@link smartcutFitTransitions}。
 */
export function smartcutSplitClip<T extends SmartcutEditableClip>(clip: T, atMs: number, newId: string): SmartcutEditResult<[T, T]> {
  const playLen = smartcutPlayLen(clip);
  if (!(atMs >= MIN_CLIP_MS) || playLen - atMs < MIN_CLIP_MS) return tooClose();
  const cut = clip.sourceInMs + sourceOffset(clip, atMs);
  const leftBase: SmartcutEditableClip = without({ ...clip, sourceOutMs: cut }, "transitionOut");
  if (leftBase.fadeOutMs !== undefined) leftBase.fadeOutMs = 0;
  const rightBase: SmartcutEditableClip = without({ ...clip, id: newId, sourceInMs: cut }, "transitionIn", "sourceShotId", "pendingGenerationId", "alternates");
  if (rightBase.fadeInMs !== undefined) rightBase.fadeInMs = 0;
  const left = touched(leftBase);
  const right = touched(rightBase);
  const leftLen = smartcutPlayLen(left);
  if (leftLen < MIN_CLIP_MS || smartcutPlayLen(right) < MIN_CLIP_MS) return tooClose();
  if (typeof clip.startMs === "number") right.startMs = clip.startMs + leftLen;
  return { ok: true, value: [left as T, right as T] };
}

/**
 * 删除播放头左侧（成片偏移 `atMs` 之前的部分）：取用起点后移，剩下的部分至少 100 毫秒。
 * 剩下的部分**留在原处**：带着 `startMs` 的（按起点放置的轨道）起点加上删掉的那段；画面轨由落点重算自动前移。
 */
export function smartcutTrimStartAt<T extends SmartcutEditableClip>(clip: T, atMs: number): SmartcutEditResult<T> {
  const playLen = smartcutPlayLen(clip);
  if (!(atMs > 0) || atMs >= playLen) return { ok: false, reason: "播放头不在这个片段里" };
  const next = touched<SmartcutEditableClip>({ ...clip, sourceInMs: clip.sourceInMs + sourceOffset(clip, atMs) });
  const nextLen = smartcutPlayLen(next);
  if (nextLen < MIN_CLIP_MS) return { ok: false, reason: `剩下的部分不足 ${MIN_CLIP_MS} 毫秒` };
  if (typeof clip.startMs === "number") next.startMs = clip.startMs + playLen - nextLen;
  return { ok: true, value: next as T };
}

/** 删除播放头右侧（成片偏移 `atMs` 之后的部分）：取用终点前移，剩下的部分至少 100 毫秒，起点不动。 */
export function smartcutTrimEndAt<T extends SmartcutEditableClip>(clip: T, atMs: number): SmartcutEditResult<T> {
  const playLen = smartcutPlayLen(clip);
  if (!(atMs > 0) || atMs >= playLen) return { ok: false, reason: "播放头不在这个片段里" };
  const next = touched<SmartcutEditableClip>({ ...clip, sourceOutMs: clip.sourceInMs + sourceOffset(clip, atMs) });
  if (smartcutPlayLen(next) < MIN_CLIP_MS) return { ok: false, reason: `剩下的部分不足 ${MIN_CLIP_MS} 毫秒` };
  return { ok: true, value: next as T };
}

/**
 * 拖动片段一端的把手（`deltaMs` 是成片毫秒，右正左负）。取用区间夹在素材本身之内（时长未知时只夹 0），
 * 成片里不短于 100 毫秒；拖到头时停在边界上而不是报错（拖动是连续的，报错会让把手卡住）。
 * `placed`（按起点放置的轨道）：拖左端时右边缘不动，且左边缘不越过成片 0 点；画面轨由落点重算，不改 `startMs`。
 */
export function smartcutTrimEdge<T extends SmartcutEditableClip>(
  clip: T,
  edge: "start" | "end",
  deltaMs: number,
  options: { sourceDurationMs?: number; placed?: boolean } = {}
): T {
  const speed = speedOf(clip);
  // 成片长度 ≥ 100 ⟺ 取用长度 ≥ speed（floor(len × 100 / speed) ≥ 100）。
  const minSrc = speed;
  if (edge === "start") {
    const placedStart = options.placed && typeof clip.startMs === "number" ? clip.startMs : undefined;
    const delta = placedStart === undefined ? deltaMs : Math.max(deltaMs, -placedStart);
    const target = clip.sourceInMs + Math.round((delta * speed) / 100);
    const sourceInMs = Math.max(0, Math.min(target, clip.sourceOutMs - minSrc));
    if (sourceInMs === clip.sourceInMs) return clip;
    const next = touched<SmartcutEditableClip>({ ...clip, sourceInMs });
    if (placedStart !== undefined) next.startMs = Math.max(0, placedStart + smartcutPlayLen(clip) - smartcutPlayLen(next));
    return next as T;
  }
  const maxOut = options.sourceDurationMs && options.sourceDurationMs > 0 ? options.sourceDurationMs : Number.POSITIVE_INFINITY;
  const target = clip.sourceOutMs + Math.round((deltaMs * speed) / 100);
  const sourceOutMs = Math.max(Math.min(maxOut, target), clip.sourceInMs + minSrc);
  if (sourceOutMs === clip.sourceOutMs) return clip;
  return touched<SmartcutEditableClip>({ ...clip, sourceOutMs }) as T;
}

/** 变速（SC-F-24）：取用区间不变，成片长度跟着变；变速后不足 100 毫秒的拒绝。起点不动。 */
export function smartcutSetSpeed<T extends SmartcutEditableClip>(clip: T, speed: number): SmartcutEditResult<T> {
  if (!Number.isInteger(speed) || speed < 25 || speed > 400) return { ok: false, reason: "变速只支持 0.25~4 倍" };
  const next = touched<SmartcutEditableClip>({ ...clip, speed });
  if (smartcutPlayLen(next) < MIN_CLIP_MS) return { ok: false, reason: `变速后片段不足 ${MIN_CLIP_MS} 毫秒` };
  return { ok: true, value: next as T };
}

/**
 * 片段变短之后把转场收进合法范围（与保存前校验同一套上限）：画面轨的转入不超过本段与上一段各自的一半、
 * 最后一段的转出不超过本段一半；其它轨的转场只看本段。收到 100 毫秒以下的直接去掉（比两帧还短的转场看不出来）。
 * 返回新数组与改动过的片段 ID —— 界面要说出来「转场已按片段长度缩短」，静默改掉用户的设置不行。
 */
export function smartcutFitTransitions<T extends SmartcutEditableClip>(clips: readonly T[], trackKind: string): { clips: T[]; changed: string[] } {
  const changed = new Set<string>();
  const lens = clips.map(clip => smartcutPlayLen(clip));
  const out = clips.map((clip, i) => {
    let next: SmartcutEditableClip = clip;
    const fit = (key: "transitionIn" | "transitionOut", cap: number) => {
      const t = next[key];
      if (!t || !smartcutTransitionActive(t)) return;
      const limit = Math.min(MAX_TRANSITION_MS, cap);
      if (t.durationMs <= limit) return;
      if (limit >= MIN_TRANSITION_MS) {
        const replaced: SmartcutEditableClip = { ...next };
        replaced[key] = { ...t, durationMs: limit };
        next = replaced;
      } else {
        next = without(next, key);
      }
      changed.add(clip.id);
    };
    const half = Math.floor(lens[i]! / 2);
    if (trackKind === "video") {
      fit("transitionIn", i > 0 ? Math.min(half, Math.floor(lens[i - 1]! / 2)) : half);
      if (i === clips.length - 1) fit("transitionOut", half);
    } else {
      fit("transitionIn", half);
      fit("transitionOut", half);
    }
    return next as T;
  });
  return { clips: out, changed: [...changed] };
}

// ---------------------------------------------------------------- 临时 ID（SCD-43）

/** 新建对象的临时 ID：保存后由响应的 `idMap` 换成正式 ID。 */
export function smartcutIsTempId(id: string): boolean {
  return id.startsWith("tmp-");
}

export function smartcutMapId(id: string, idMap: Readonly<Record<string, string>>): string {
  return Object.prototype.hasOwnProperty.call(idMap, id) ? idMap[id]! : id;
}

/** 按 `idMap` 改写对象上的几个 ID 字段；一个都没命中时返回原对象（引用不变）。 */
function mapKeys(obj: JsonMap, keys: readonly string[], idMap: Readonly<Record<string, string>>): JsonMap {
  let next: JsonMap | null = null;
  for (const key of keys) {
    const value = own(obj, key);
    if (typeof value !== "string" || !Object.prototype.hasOwnProperty.call(idMap, value)) continue;
    next ??= { ...obj };
    next[key] = idMap[value];
  }
  return next ?? obj;
}

/** 逐项改写数组；一项都没变时返回原数组。不是数组的原样返回。 */
function mapList(list: unknown, fn: (item: JsonMap) => JsonMap): unknown {
  if (!Array.isArray(list)) return list;
  let changed = false;
  const out = list.map((item: unknown) => {
    if (!isMap(item)) return item;
    const mapped = fn(item);
    if (mapped !== item) changed = true;
    return mapped;
  });
  return changed ? out : list;
}

/**
 * 保存成功后按 `idMap` 把内容里的临时 ID 换成正式 ID：轨道、片段、字幕、文字的 `id`，字幕与文字的 `trackId`，
 * 字幕锚点与片段锚点的 `clipId`。没有命中的部分保持原引用，其余字段原样保留；什么都没换时返回原对象。
 *
 * ⚠️ 只改这几个已知字段，不全文替换：用户在字幕里打一个「tmp-1」不该被改掉。
 * 整份内容（`TimelineContent`）与读回的时间线（`Timeline`）字段名相同，两种都能传。
 */
export function smartcutApplyIdMap<T extends object>(content: T, idMap: Readonly<Record<string, string>>): T {
  if (!isMap(content) || !Object.keys(idMap).length) return content;
  const tracks = mapList(own(content, "tracks"), (track) => {
    const t = mapKeys(track, ["id"], idMap);
    const clips = mapList(own(track, "clips"), (clip) => {
      const c = mapKeys(clip, ["id"], idMap);
      // 跟随片段的音频：锚点里的片段 ID 同样换成正式 ID（同字幕的锚点）。
      const anchor = own(clip, "anchor");
      const mappedAnchor = isMap(anchor) ? mapKeys(anchor, ["clipId"], idMap) : anchor;
      return mappedAnchor === anchor ? c : { ...c, anchor: mappedAnchor };
    });
    return clips === own(track, "clips") ? t : { ...t, clips };
  });
  const subtitles = mapList(own(content, "subtitles"), (sub) => {
    const s = mapKeys(sub, ["id", "trackId"], idMap);
    const anchor = own(sub, "anchor");
    const mappedAnchor = isMap(anchor) ? mapKeys(anchor, ["clipId"], idMap) : anchor;
    return mappedAnchor === anchor ? s : { ...s, anchor: mappedAnchor };
  });
  const texts = mapList(own(content, "texts"), text => mapKeys(text, ["id", "trackId"], idMap));
  if (tracks === own(content, "tracks") && subtitles === own(content, "subtitles") && texts === own(content, "texts")) return content;
  const next: JsonMap = { ...content };
  if (tracks !== own(content, "tracks")) next.tracks = tracks;
  if (subtitles !== own(content, "subtitles")) next.subtitles = subtitles;
  if (texts !== own(content, "texts")) next.texts = texts;
  return next as T;
}

/**
 * 撤销栈里的一份快照（整份 JSON 串）同样改写：不改写的话撤销一步就回到带临时 ID 的内容，
 * 再保存时服务端把它们当成新对象又建一遍（片段翻倍）。解析不了或没有命中的快照原样返回。
 */
export function smartcutRemapSnapshot(snapshot: string, idMap: Readonly<Record<string, string>>): string {
  if (!Object.keys(idMap).length) return snapshot;
  let parsed: unknown;
  try {
    parsed = JSON.parse(snapshot);
  } catch {
    return snapshot;
  }
  if (!isMap(parsed)) return snapshot;
  const mapped = smartcutApplyIdMap(parsed, idMap);
  return mapped === parsed ? snapshot : JSON.stringify(mapped);
}

/** 整个撤销 / 重做栈一起改写（内容与栈必须同时换，SCD-43）。 */
export function smartcutRemapSnapshots(stack: readonly string[], idMap: Readonly<Record<string, string>>): string[] {
  return stack.map(snapshot => smartcutRemapSnapshot(snapshot, idMap));
}
