/**
 * 剪辑台的预览引擎（docs/agent/smartcut SCT-21，SCD-42、dev/03 §6）：按时间线**真正连续地播放**画面、声音，
 * 并告诉界面此刻该叠哪些字。独立的 TS 类，不依赖 Vue / Nuxt —— 命令式的媒体调度放进响应式里只会更乱。
 *
 * 结构：
 * - 时钟：播放时以 `AudioContext.currentTime` 为准（第一次 `play()` 在用户手势里建 / resume），
 *   没有声音或建不了时退回 `performance.now()`；换时钟源时重新对齐，不跳。
 * - 画面：两个槽位交替（每个槽位一个 `<video>` + 一个 `<img>`）。当前段在播，下一段提前约 300ms 换好 src、
 *   定位到取用起点并暂停待命，到点互换。每个槽位是一整幅黑底（与服务端「每段叠到黑底上再拼接」一致），
 *   转场重叠区里后一段按透明度盖在前一段上；溶解、滑动、擦除一律按淡入淡出近似（SC-F-32）。
 * - 声音：每个音源一个 `<audio>`（按需建、不响了就释放），接 `MediaElementAudioSourceNode → GainNode`；
 *   **不用 decodeAudioData**（5 分钟的立体声整段解码就上百 MB）。没有 Web Audio 时直接设 `audio.volume`。
 *   画面轨视频的原声与画面共用同一份预览版 blob（预览版带 64k 的音轨）。
 * - 文字与字幕：引擎只算「此刻可见哪些」并在变化时发 `overlay` 事件；排版图（服务端 PNG）与两段式显示由组件负责 ——
 *   排版缓存在 store 里，引擎不该再持有一份。
 * - 素材：只缓存预览版 / 音频 / 图片的 blob（`BlobCache`，LRU 按字节、上限 300 MB），只预取后面 1~2 段。
 *
 * ⚠️ 播放必须由用户手势触发（`play()` 里建 / resume `AudioContext`），否则 Safari 与 Chrome 都可能静音或拒播（SCR-4）。
 * ⚠️ 同一页面只有一个在播的引擎：另一个开始播时，这一个暂停并发 `preempted`（由持有者决定要不要释放）。
 * ⚠️ `dispose()` 释放全部媒体元素、`AudioContext` 与 rAF；缓存是自己建的就整个清掉（revoke 全部 object URL），
 *    注入的缓存只解除 pin（它可能还被别的引擎用着）。
 */
import type { components } from "@tabtv/api-client";
import { smartcutTransformCss } from "~/utils/smartcutTransform";
import { BlobCache, type BlobCacheEntry } from "./BlobCache";
import {
  buildSchedule,
  sameOverlays,
  scheduleAt,
  upcomingAudio,
  upcomingVideo,
  type ScheduleAudioSource,
  type ScheduleFrame,
  type ScheduleMediaInfoFn,
  type ScheduleOverlayItem,
  type SchedulePlan,
  type ScheduleVideoSegment,
  type ScheduleVisible
} from "./Scheduler";

type Timeline = components["schemas"]["Timeline"];

/**
 * 取一份素材的用途：
 * - `video`：画面轨视频的**预览版**（短边 480、带音轨）—— 画面与原声共用；
 * - `audio`：音频轨的音频文件；
 * - `image`：画面轨的图片。
 * loader 回 null 表示「现在没有」（预览版还在生成）：不报错，界面显示占位。
 */
export type PlaybackMediaPurpose = "video" | "audio" | "image";

export type PlaybackMissing = { clipId: string; mediaId: string; reason: "loading" | "not_ready" | "error" };

export interface PlaybackEngineEvents {
  /** 播放中每帧一次（rAF 节流）；seek、换时间线时也发。 */
  timeupdate: { ms: number };
  /** 播到成片末尾（已自动暂停）。 */
  ended: { ms: number };
  /** 播放 / 暂停 / 等待素材的状态变了。 */
  state: { playing: boolean; waiting: boolean };
  /** 此刻在画的某一段缺素材：`loading` 正在取（播放中时钟暂停等待）、`not_ready` 预览版还没好、`error` 取失败（都继续往下播）。 */
  stalled: PlaybackMissing;
  /** 等待素材结束、重新走起来。 */
  playing: { ms: number };
  /** 此刻在画的各段里缺素材的集合变了（空 = 都齐了）：界面据此显示 / 撤掉「预览版生成中」占位。 */
  missing: { items: PlaybackMissing[] };
  /** 此刻可见的文字与字幕变了（含时间线被编辑后内容变了）。 */
  overlay: { items: readonly ScheduleOverlayItem[] };
  /** 别的引擎开始播放，这一个已暂停。 */
  preempted: undefined;
  error: { error: unknown; mediaId: string; purpose: PlaybackMediaPurpose };
}

export type PlaybackEngineOptions = {
  /** 舞台：引擎在里面建一个铺满的根元素（舞台本身的尺寸与画幅比例由组件负责）。 */
  stage: HTMLElement;
  loadMedia: (mediaId: string, purpose: PlaybackMediaPurpose) => Promise<Blob | null>;
  /** 素材种类、有没有音轨、宽高（时间线上没有这些字段）。 */
  mediaInfo?: ScheduleMediaInfoFn;
  /** 共享的缓存（节点卡与剪辑台各自建也行）；不给就自己建一个，上限 `maxCacheBytes`。 */
  cache?: BlobCache;
  maxCacheBytes?: number;
  /** 预取后面几段（1~2，SCD-42）。 */
  prefetch?: number;
  /** 下一段提前多久换好 src 并定位（毫秒）。 */
  preloadLeadMs?: number;
  /** 播放中漂移超过多少秒才校正 currentTime（频繁 seek 反而卡）。 */
  driftToleranceSec?: number;
  /** 测试注入。 */
  document?: Document;
  createAudioContext?: () => AudioContext | null;
  createObjectURL?: (blob: Blob) => string;
  revokeObjectURL?: (url: string) => void;
  now?: () => number;
  requestFrame?: (cb: (t: number) => void) => number;
  cancelFrame?: (id: number) => void;
};

const DEFAULT_PRELOAD_LEAD_MS = 300;
const DEFAULT_DRIFT_SEC = 0.15;
/** 暂停时（拖播放头）要精确到这一帧附近：差超过这么多秒就 seek。 */
const PAUSED_DRIFT_SEC = 0.04;
/** 预览版没好 / 取失败之后多久再试一次（期间按缺素材处理，不反复打接口）。 */
const RETRY_MISSING_MS = 5000;
/** 音频预取窗口：起点在这么久之内的音源先把 blob 取回来（最多两个）。 */
const AUDIO_PREFETCH_MS = 3000;

type Handler<T> = (payload: T) => void;

type Slot = {
  layer: HTMLDivElement;
  outer: HTMLDivElement;
  video: HTMLVideoElement;
  img: HTMLImageElement;
  segment: ScheduleVideoSegment | null;
  mediaKey: string | null;
  objectUrl: string | null;
  unpin: (() => void) | null;
  cssKey: string;
};

type Lane = {
  key: string;
  source: ScheduleAudioSource;
  el: HTMLAudioElement;
  node: MediaElementAudioSourceNode | null;
  gain: GainNode | null;
  mediaKey: string;
  objectUrl: string | null;
  unpin: (() => void) | null;
  /** 上一次写进去的增益（没变就不写：每帧排一个自动化事件没有意义）。 */
  lastGain: number;
};

type MediaState = { state: "ready"; entry: BlobCacheEntry } | { state: "loading" | "not_ready" | "error" };

type PitchedMedia = HTMLMediaElement & { preservesPitch?: boolean; webkitPreservesPitch?: boolean; mozPreservesPitch?: boolean };

/** 同一页面只有一个在播的引擎（SCR-4：节点卡与剪辑台不能同时出声）。 */
const playback: { active: PlaybackEngine | null } = { active: null };

function defaultAudioContext(): AudioContext | null {
  const g = globalThis as unknown as { AudioContext?: typeof AudioContext; webkitAudioContext?: typeof AudioContext };
  const Ctor = g.AudioContext ?? g.webkitAudioContext;
  return Ctor ? new Ctor() : null;
}

function mediaKeyOf(purpose: PlaybackMediaPurpose, mediaId: string): string {
  return `${purpose}:${mediaId}`;
}

function segmentPurpose(seg: ScheduleVideoSegment): PlaybackMediaPurpose {
  return seg.kind === "image" ? "image" : "video";
}

/** 画面原声与画面共用预览版；音频轨用音频文件本身。 */
function lanePurpose(src: ScheduleAudioSource): PlaybackMediaPurpose {
  return src.origin === "video" ? "video" : "audio";
}

/** 播放出错（自动播放策略拒绝、元素已被释放）不该冒成未处理的 rejection。 */
function playQuietly(el: HTMLMediaElement): void {
  try {
    const p = el.play() as Promise<void> | undefined;
    if (p && typeof p.catch === "function") p.catch(() => {});
  } catch {
    // 个别环境 play() 同步抛：当作这次没播起来，下一帧再试。
  }
}

export class PlaybackEngine {
  private readonly doc: Document;
  private readonly root: HTMLDivElement;
  private readonly slots: Slot[];
  private readonly lanes = new Map<string, Lane>();
  private readonly cache: BlobCache;
  private readonly ownsCache: boolean;
  private readonly handlers = new Map<keyof PlaybackEngineEvents, Set<Handler<never>>>();
  private readonly loadMedia: PlaybackEngineOptions["loadMedia"];
  private mediaInfo: ScheduleMediaInfoFn | undefined;
  private readonly prefetchCount: number;
  private readonly leadMs: number;
  private readonly driftSec: number;
  private readonly createCtx: () => AudioContext | null;
  private readonly now: () => number;
  private readonly requestFrame: (cb: (t: number) => void) => number;
  private readonly cancelFrame: (id: number) => void;

  private timeline: Timeline | null = null;
  private plan: SchedulePlan = buildSchedule(null);
  private ctx: AudioContext | null = null;
  private ctxFailed = false;
  private positionMs = 0;
  private playingFlag = false;
  private waitingFlag = false;
  private anchor = { src: "perf" as "audio" | "perf", clockMs: 0, positionMs: 0 };
  private rafId: number | null = null;
  private tickQueued: number | null = null;
  private disposed = false;
  /** 预览版没好 / 取失败的素材：到这个时刻之前按缺素材处理。 */
  private readonly missingUntil = new Map<string, { until: number; reason: "not_ready" | "error" }>();
  private missingSig = "";
  private readonly stalledSeen = new Set<string>();
  private overlays: readonly ScheduleOverlayItem[] = [];
  private lastEmittedMs = -1;

  constructor(options: PlaybackEngineOptions) {
    this.doc = options.document ?? options.stage.ownerDocument;
    this.loadMedia = options.loadMedia;
    this.mediaInfo = options.mediaInfo;
    this.prefetchCount = Math.max(1, Math.min(2, options.prefetch ?? 2));
    this.leadMs = options.preloadLeadMs ?? DEFAULT_PRELOAD_LEAD_MS;
    this.driftSec = options.driftToleranceSec ?? DEFAULT_DRIFT_SEC;
    this.createCtx = options.createAudioContext ?? defaultAudioContext;
    this.now = options.now ?? (() => globalThis.performance?.now() ?? Date.now());
    const raf = globalThis.requestAnimationFrame?.bind(globalThis);
    const caf = globalThis.cancelAnimationFrame?.bind(globalThis);
    this.requestFrame = options.requestFrame
      ?? raf
      ?? ((cb: (t: number) => void) => setTimeout(() => cb(this.now()), 16) as unknown as number);
    this.cancelFrame = options.cancelFrame ?? caf ?? ((id: number) => clearTimeout(id));
    this.ownsCache = !options.cache;
    this.cache = options.cache ?? new BlobCache({
      maxBytes: options.maxCacheBytes,
      createObjectURL: options.createObjectURL,
      revokeObjectURL: options.revokeObjectURL
    });

    this.root = this.doc.createElement("div");
    this.root.dataset.smartcutPreview = "";
    // 自己建立定位上下文：不依赖舞台是不是 positioned。
    this.root.style.cssText = "position:relative;width:100%;height:100%;overflow:hidden;background:#000";
    this.slots = [this.createSlot(), this.createSlot()];
    options.stage.appendChild(this.root);
  }

  // ---------------------------------------------------------------- 对外

  get currentMs(): number {
    return this.positionMs;
  }

  get playing(): boolean {
    return this.playingFlag;
  }

  /** 等素材中（播放状态下时钟暂停）。 */
  get waiting(): boolean {
    return this.waitingFlag;
  }

  /** 成片长度（与 `smartcutLayout` 的 effectiveMs 一致）。 */
  get durationMs(): number {
    return this.plan.effectiveMs;
  }

  get overlayItems(): readonly ScheduleOverlayItem[] {
    return this.overlays;
  }

  /** 当前的调度表（组件取成片时间、字幕顺序用）。 */
  get schedule(): SchedulePlan {
    return this.plan;
  }

  on<K extends keyof PlaybackEngineEvents>(type: K, handler: Handler<PlaybackEngineEvents[K]>): () => void {
    let set = this.handlers.get(type);
    if (!set) {
      set = new Set();
      this.handlers.set(type, set);
    }
    set.add(handler as Handler<never>);
    return () => set.delete(handler as Handler<never>);
  }

  /**
   * 换时间线（编辑之后随时可调，播放中也行）：重算调度表，保持当前位置（超出新长度时夹到末尾），立即校正画面。
   */
  setTimeline(timeline: Timeline | null): void {
    if (this.disposed) return;
    const at = this.livePosition();
    this.timeline = timeline;
    this.plan = buildSchedule(timeline, this.mediaInfo);
    this.positionMs = Math.min(at, this.plan.effectiveMs);
    this.reanchor();
    this.stalledSeen.clear();
    for (const slot of this.slots) slot.cssKey = "";
    this.tick(true);
  }

  /**
   * 素材信息或预览版的就绪状态变了（收到 `smartcut.updated` 的分析完成事件）：清掉「没好」的标记并重算。
   * 不给 mediaId 就全部清。
   */
  refreshMedia(mediaId?: string, mediaInfo?: ScheduleMediaInfoFn): void {
    if (this.disposed) return;
    if (mediaInfo) this.mediaInfo = mediaInfo;
    if (mediaId) {
      for (const purpose of ["video", "audio", "image"] as const) this.missingUntil.delete(mediaKeyOf(purpose, mediaId));
    } else {
      this.missingUntil.clear();
    }
    this.setTimeline(this.timeline);
  }

  /**
   * 开始播放。**必须在用户手势里调用**：这里建 / resume `AudioContext`。播完再点播放从头开始。
   */
  play(): void {
    if (this.disposed || this.playingFlag) return;
    this.ensureAudioContext();
    if (this.plan.effectiveMs <= 0) return;
    if (this.positionMs >= this.plan.effectiveMs) this.positionMs = 0;
    if (playback.active && playback.active !== this) playback.active.preempt();
    playback.active = this;
    this.playingFlag = true;
    this.waitingFlag = false;
    this.reanchor();
    this.emit("state", { playing: true, waiting: false });
    this.tick(true);
    this.loop();
  }

  pause(): void {
    if (this.disposed || !this.playingFlag) return;
    this.positionMs = this.livePosition();
    this.playingFlag = false;
    this.waitingFlag = false;
    this.stopLoop();
    this.tick(true);
    this.emit("state", { playing: false, waiting: false });
  }

  /** 定位到成片时刻（夹到 [0, 成片长度]）；播放中继续播。 */
  seek(ms: number): void {
    if (this.disposed) return;
    const target = Math.max(0, Math.min(Number.isFinite(ms) ? ms : 0, this.plan.effectiveMs));
    this.positionMs = target;
    this.reanchor();
    this.tick(true);
  }

  /** 释放全部媒体元素、AudioContext、rAF 与（自己建的）缓存。之后的调用一律无效。 */
  dispose(): void {
    if (this.disposed) return;
    this.stopLoop();
    if (this.tickQueued !== null) this.cancelFrame(this.tickQueued);
    this.tickQueued = null;
    this.playingFlag = false;
    this.disposed = true;
    if (playback.active === this) playback.active = null;
    for (const slot of this.slots) this.resetSlot(slot);
    for (const lane of this.lanes.values()) this.releaseLane(lane);
    this.lanes.clear();
    this.root.remove();
    if (this.ownsCache) this.cache.clear();
    const ctx = this.ctx;
    this.ctx = null;
    if (ctx) void ctx.close().catch(() => {});
    this.handlers.clear();
  }

  /** 诊断用（测试与内存排查）：两个槽位在放什么、有几个音源元素。 */
  inspect(): { slots: Array<{ clipId: string | null; src: string | null; opacity: string; zIndex: string }>; lanes: string[] } {
    return {
      slots: this.slots.map(s => ({ clipId: s.segment?.clipId ?? null, src: s.objectUrl, opacity: s.layer.style.opacity, zIndex: s.layer.style.zIndex })),
      lanes: [...this.lanes.keys()]
    };
  }

  // ---------------------------------------------------------------- 事件

  private emit<K extends keyof PlaybackEngineEvents>(type: K, payload: PlaybackEngineEvents[K]): void {
    for (const handler of this.handlers.get(type) ?? []) {
      try {
        (handler as Handler<PlaybackEngineEvents[K]>)(payload);
      } catch (err) {
        console.error(`[smartcut] 预览事件 ${String(type)} 的监听器出错`, err);
      }
    }
  }

  private preempt(): void {
    if (!this.playingFlag) return;
    this.pause();
    this.emit("preempted", undefined);
  }

  // ---------------------------------------------------------------- 时钟

  private ensureAudioContext(): void {
    if (!this.ctx && !this.ctxFailed) {
      try {
        this.ctx = this.createCtx();
      } catch {
        this.ctx = null;
      }
      if (!this.ctx) this.ctxFailed = true;
      else for (const lane of this.lanes.values()) this.attachWebAudio(lane);
    }
    if (this.ctx && this.ctx.state === "suspended") void this.ctx.resume().catch(() => {});
  }

  /** 当前时钟读数（毫秒）：AudioContext 在走时用它，否则用 performance.now。 */
  private clock(): { src: "audio" | "perf"; ms: number } {
    const ctx = this.ctx;
    if (ctx && ctx.state === "running") return { src: "audio", ms: ctx.currentTime * 1000 };
    return { src: "perf", ms: this.now() };
  }

  private reanchor(): void {
    const c = this.clock();
    this.anchor = { src: c.src, clockMs: c.ms, positionMs: this.positionMs };
  }

  /** 此刻的成片位置：播放且不在等素材时按时钟推进；时钟源换了就在当前位置重新对齐（不跳）。 */
  private livePosition(): number {
    if (!this.playingFlag || this.waitingFlag) return this.positionMs;
    const c = this.clock();
    if (c.src !== this.anchor.src) {
      this.anchor = { src: c.src, clockMs: c.ms, positionMs: this.positionMs };
      return this.positionMs;
    }
    return this.anchor.positionMs + (c.ms - this.anchor.clockMs);
  }

  private loop(): void {
    this.stopLoop();
    const step = () => {
      this.rafId = null;
      if (this.disposed || !this.playingFlag) return;
      this.tick(false);
      if (this.playingFlag) this.rafId = this.requestFrame(step);
    };
    this.rafId = this.requestFrame(step);
  }

  private stopLoop(): void {
    if (this.rafId !== null) this.cancelFrame(this.rafId);
    this.rafId = null;
  }

  /** 暂停状态下异步完成的事（素材取回来、元数据到了）：下一帧补一次 tick（合并多次请求）。 */
  private requestTick(): void {
    if (this.disposed || this.playingFlag || this.tickQueued !== null) return;
    this.tickQueued = this.requestFrame(() => {
      this.tickQueued = null;
      this.tick(true);
    });
  }

  // ---------------------------------------------------------------- 每帧

  private tick(force: boolean): void {
    if (this.disposed) return;
    let ms = this.livePosition();
    let endedNow = false;
    if (this.playingFlag && ms >= this.plan.effectiveMs) {
      ms = this.plan.effectiveMs;
      endedNow = true;
    }
    this.positionMs = ms;
    const frame = scheduleAt(this.plan, ms);

    // 1) 分配槽位、取素材；看此刻在画的各段缺不缺素材
    const shown = this.assignSlots(frame, ms);
    const missing = this.collectMissing(shown);
    this.updateWaiting(missing);
    // 2) 按「在播 / 暂停 / 等待」同步画面与声音
    const live = this.playingFlag && !this.waitingFlag && !endedNow;
    this.applySlots(shown, live);
    this.syncAudio(frame, ms, live);
    this.prefetch(ms);
    this.syncOverlay(frame);

    if (force || ms !== this.lastEmittedMs) {
      this.lastEmittedMs = ms;
      this.emit("timeupdate", { ms });
    }
    if (endedNow) {
      this.playingFlag = false;
      this.waitingFlag = false;
      this.stopLoop();
      this.applySlots(shown, false);
      this.syncAudio(frame, ms, false);
      this.emit("state", { playing: false, waiting: false });
      this.emit("ended", { ms });
    }
  }

  // ---------------------------------------------------------------- 素材

  /** 一份素材的状态；没有也没在取时发起加载（单飞由缓存保证）。 */
  private media(mediaId: string, purpose: PlaybackMediaPurpose): MediaState {
    const key = mediaKeyOf(purpose, mediaId);
    const entry = this.cache.get(key);
    if (entry) return { state: "ready", entry };
    if (this.cache.isLoading(key)) return { state: "loading" };
    const miss = this.missingUntil.get(key);
    if (miss && miss.until > this.now()) return { state: miss.reason };
    this.missingUntil.delete(key);
    this.cache.load(key, () => this.loadMedia(mediaId, purpose)).then(
      (loaded) => {
        if (this.disposed) return;
        if (!loaded) this.missingUntil.set(key, { until: this.now() + RETRY_MISSING_MS, reason: "not_ready" });
        this.requestTick();
      },
      (error: unknown) => {
        if (this.disposed) return;
        this.missingUntil.set(key, { until: this.now() + RETRY_MISSING_MS, reason: "error" });
        this.emit("error", { error, mediaId, purpose });
        this.requestTick();
      }
    );
    // 缓存可能同步就有了结果（加载器同步回 null 也要等一个微任务），一律按「在取」处理。
    return { state: "loading" };
  }

  private prefetch(ms: number): void {
    for (const seg of upcomingVideo(this.plan, ms, this.prefetchCount)) this.media(seg.mediaId, segmentPurpose(seg));
    for (const src of upcomingAudio(this.plan, ms, AUDIO_PREFETCH_MS).slice(0, 2)) this.media(src.mediaId, lanePurpose(src));
  }

  // ---------------------------------------------------------------- 画面

  private createSlot(): Slot {
    const layer = this.doc.createElement("div");
    // 每个槽位是一整幅黑底：转场时盖上去的是「整帧」（与服务端每段叠到黑底上再 xfade 一致），透明度设在这一层。
    layer.style.cssText = "position:absolute;left:0;top:0;width:100%;height:100%;background:#000;overflow:hidden;display:none;opacity:0";
    const outer = this.doc.createElement("div");
    outer.style.cssText = "position:absolute;overflow:hidden";
    const video = this.doc.createElement("video");
    // 画面元素一律静音：声音全部走音频通道（含画面原声），音量、淡入淡出与静音才能统一处理。
    video.muted = true;
    video.defaultMuted = true;
    video.playsInline = true;
    video.preload = "auto";
    video.setAttribute("playsinline", "");
    video.setAttribute("muted", "");
    video.style.cssText = "position:absolute;display:none;max-width:none;max-height:none;object-fit:fill";
    const img = this.doc.createElement("img");
    img.alt = "";
    img.draggable = false;
    img.decoding = "async";
    img.style.cssText = "position:absolute;display:none;max-width:none;max-height:none;object-fit:fill";
    // 素材的真实宽高要等元数据到了才知道：到了之后补一帧，按真实比例重新摆。
    const refit = () => {
      slot.cssKey = "";
      this.requestTick();
    };
    video.addEventListener("loadedmetadata", refit);
    img.addEventListener("load", refit);
    outer.append(video, img);
    layer.append(outer);
    this.root.append(layer);
    const slot: Slot = { layer, outer, video, img, segment: null, mediaKey: null, objectUrl: null, unpin: null, cssKey: "" };
    return slot;
  }

  /**
   * 给此刻在画的各段（最多上面两段）与即将开始的下一段分配槽位。已经放着这一段的槽位留着用（预加载好的那个直接接上），
   * 其余从空闲槽位里拿；用不上的槽位清空。返回在画的（下层在前）与各自的槽位。
   */
  private assignSlots(frame: ScheduleFrame, ms: number): Array<{ visible: ScheduleVisible; slot: Slot }> {
    const shownVisible = frame.video.slice(-this.slots.length);
    const used = new Set<Slot>();
    const pairs: Array<{ visible: ScheduleVisible; slot: Slot | null }> = shownVisible.map(visible => {
      const slot = this.slots.find(s => !used.has(s) && s.segment?.clipId === visible.segment.clipId) ?? null;
      if (slot) used.add(slot);
      return { visible, slot };
    });
    for (const pair of pairs) {
      if (pair.slot) continue;
      const slot = this.slots.find(s => !used.has(s))!;
      used.add(slot);
      pair.slot = slot;
    }
    const shown = pairs.map(p => ({ visible: p.visible, slot: p.slot! }));
    for (const { visible, slot } of shown) this.bindSlot(slot, visible.segment);

    // 下一段：提前换好 src 并定位到取用起点，暂停待命（透明，压在最下面）。
    const next = upcomingVideo(this.plan, ms, 1)[0];
    const free = this.slots.find(s => !used.has(s));
    if (next && free && next.startMs - ms <= this.leadMs) {
      used.add(free);
      this.bindSlot(free, next);
      this.standby(free, next);
    }
    for (const slot of this.slots) {
      if (!used.has(slot)) this.resetSlot(slot);
    }
    return shown;
  }

  /** 槽位换到这一段：素材变了就先解绑旧的；素材已在缓存里就挂上 src 并 pin 住。 */
  private bindSlot(slot: Slot, seg: ScheduleVideoSegment): void {
    const key = mediaKeyOf(segmentPurpose(seg), seg.mediaId);
    if (slot.mediaKey !== key) {
      this.detachSource(slot);
      slot.mediaKey = key;
    }
    if (slot.segment?.clipId !== seg.clipId) slot.cssKey = "";
    slot.segment = seg;
    if (slot.objectUrl) return;
    const m = this.media(seg.mediaId, segmentPurpose(seg));
    if (m.state !== "ready") return;
    slot.objectUrl = m.entry.objectUrl;
    slot.unpin = this.cache.pin(m.entry.key);
    if (seg.kind === "image") {
      slot.img.src = m.entry.objectUrl;
    } else {
      slot.video.src = m.entry.objectUrl;
    }
  }

  private detachSource(slot: Slot): void {
    slot.unpin?.();
    slot.unpin = null;
    if (slot.objectUrl) {
      if (slot.video.getAttribute("src")) {
        slot.video.pause();
        slot.video.removeAttribute("src");
        // 不调 load() 的话解码器与缓冲区一直占着（换段很频繁，几十段之后内存就上去了）。
        slot.video.load();
      }
      if (slot.img.getAttribute("src")) slot.img.removeAttribute("src");
    }
    slot.objectUrl = null;
    slot.mediaKey = null;
  }

  private resetSlot(slot: Slot): void {
    // 已经是空闲的就不再写 DOM（空闲槽位每帧都会走到这里）。
    if (!slot.segment && !slot.objectUrl && slot.layer.style.display === "none") return;
    this.detachSource(slot);
    slot.segment = null;
    slot.cssKey = "";
    slot.layer.style.display = "none";
    slot.layer.style.opacity = "0";
    slot.layer.style.zIndex = "0";
    slot.video.style.display = "none";
    slot.img.style.display = "none";
  }

  /** 预加载的槽位：透明、压在最下、暂停在取用起点。 */
  private standby(slot: Slot, seg: ScheduleVideoSegment): void {
    this.layoutSlot(slot, seg);
    slot.layer.style.display = "block";
    slot.layer.style.opacity = "0";
    slot.layer.style.zIndex = "0";
    if (seg.kind !== "image" && slot.objectUrl) {
      const v = slot.video;
      if (!v.paused) v.pause();
      v.playbackRate = seg.speed / 100;
      const want = seg.sourceInMs / 1000;
      if (Math.abs(v.currentTime - want) > PAUSED_DRIFT_SEC) v.currentTime = want;
    }
  }

  private collectMissing(shown: Array<{ visible: ScheduleVisible; slot: Slot }>): PlaybackMissing[] {
    const out: PlaybackMissing[] = [];
    for (const { visible, slot } of shown) {
      if (slot.objectUrl) continue;
      const seg = visible.segment;
      const m = this.media(seg.mediaId, segmentPurpose(seg));
      out.push({ clipId: seg.clipId, mediaId: seg.mediaId, reason: m.state === "ready" ? "loading" : m.state });
    }
    return out;
  }

  /**
   * 缺素材时的处置：正在取的，播放中时钟停下来等（真播放器也是缓冲时停住）；预览版没好或取失败的不等 ——
   * 等也等不来，显示占位继续往下播。集合变化时发 `missing`，新出现的缺口各发一次 `stalled`。
   */
  private updateWaiting(missing: PlaybackMissing[]): void {
    const sig = missing.map(m => `${m.clipId}|${m.mediaId}|${m.reason}`).join(",");
    if (sig !== this.missingSig) {
      this.missingSig = sig;
      this.emit("missing", { items: missing });
    }
    for (const m of missing) {
      const k = `${m.clipId}|${m.mediaId}|${m.reason}`;
      if (this.stalledSeen.has(k)) continue;
      this.stalledSeen.add(k);
      this.emit("stalled", m);
    }
    if (!this.playingFlag) {
      this.waitingFlag = false;
      return;
    }
    const loading = missing.some(m => m.reason === "loading");
    if (loading && !this.waitingFlag) {
      this.waitingFlag = true;
      this.emit("state", { playing: true, waiting: true });
    } else if (!loading && this.waitingFlag) {
      this.waitingFlag = false;
      this.reanchor();
      this.emit("state", { playing: true, waiting: false });
      this.emit("playing", { ms: this.positionMs });
    }
  }

  /** 按变换摆放（样式没变就不写 DOM）。 */
  private layoutSlot(slot: Slot, seg: ScheduleVideoSegment): void {
    const plan = this.plan;
    const info = this.mediaInfo?.(seg.mediaId);
    const el = seg.kind === "image" ? slot.img : slot.video;
    const intrinsic = seg.kind === "image"
      ? { width: slot.img.naturalWidth, height: slot.img.naturalHeight }
      : { width: slot.video.videoWidth, height: slot.video.videoHeight };
    const media = info?.width && info?.height ? { width: info.width, height: info.height } : intrinsic;
    const css = smartcutTransformCss(seg.transform, { width: plan.width, height: plan.height }, media);
    const key = `${seg.kind}|${css.outer.left}|${css.outer.top}|${css.outer.width}|${css.outer.height}|${css.outer.transform}|${css.inner.left}|${css.inner.top}|${css.inner.width}|${css.inner.height}`;
    if (key === slot.cssKey) return;
    slot.cssKey = key;
    Object.assign(slot.outer.style, css.outer);
    Object.assign(el.style, css.inner);
    el.style.display = "block";
    (seg.kind === "image" ? slot.video : slot.img).style.display = "none";
  }

  private applySlots(shown: Array<{ visible: ScheduleVisible; slot: Slot }>, live: boolean): void {
    shown.forEach(({ visible, slot }, i) => {
      const seg = visible.segment;
      this.layoutSlot(slot, seg);
      slot.layer.style.display = "block";
      slot.layer.style.zIndex = String(i + 1);
      // 缺素材的段不画（黑底），由界面的占位说明原因。
      slot.layer.style.opacity = slot.objectUrl ? String(Math.round(visible.opacity * 1000) / 1000) : "0";
      if (seg.kind === "image" || !slot.objectUrl) return;
      const v = slot.video;
      const rate = seg.speed / 100;
      if (v.playbackRate !== rate) v.playbackRate = rate;
      const want = visible.sourceMs / 1000;
      if (live) {
        if (Math.abs(v.currentTime - want) > this.driftSec) v.currentTime = want;
        if (v.paused) playQuietly(v);
      } else {
        if (!v.paused) v.pause();
        if (Math.abs(v.currentTime - want) > PAUSED_DRIFT_SEC) v.currentTime = want;
      }
    });
  }

  // ---------------------------------------------------------------- 声音

  private attachWebAudio(lane: Lane): void {
    const ctx = this.ctx;
    if (!ctx || lane.node) return;
    try {
      lane.node = ctx.createMediaElementSource(lane.el);
      lane.gain = ctx.createGain();
      lane.node.connect(lane.gain).connect(ctx.destination);
      // 音量交给 GainNode：元素本身保持满音量（两处都乘就变成平方）；之前按元素音量写过的增益要重写一遍。
      lane.el.volume = 1;
      lane.lastGain = -1;
    } catch {
      // 个别浏览器对某些元素建不了源节点：退回元素音量。
      lane.node = null;
      lane.gain = null;
    }
  }

  private createLane(src: ScheduleAudioSource): Lane {
    const el = this.doc.createElement("audio");
    el.preload = "auto";
    const pitched = el as PitchedMedia;
    // 变速不变调（服务端 atempo 也不变调）。
    pitched.preservesPitch = true;
    pitched.webkitPreservesPitch = true;
    pitched.mozPreservesPitch = true;
    const lane: Lane = { key: src.key, source: src, el, node: null, gain: null, mediaKey: mediaKeyOf(lanePurpose(src), src.mediaId), objectUrl: null, unpin: null, lastGain: -1 };
    this.attachWebAudio(lane);
    return lane;
  }

  private releaseLane(lane: Lane): void {
    lane.unpin?.();
    lane.unpin = null;
    try {
      lane.el.pause();
      lane.el.removeAttribute("src");
      lane.el.load();
    } catch {
      // 已经释放过。
    }
    try {
      lane.node?.disconnect();
      lane.gain?.disconnect();
    } catch {
      // 节点已经断开。
    }
    lane.node = null;
    lane.gain = null;
    lane.objectUrl = null;
  }

  private bindLane(lane: Lane, src: ScheduleAudioSource): void {
    const key = mediaKeyOf(lanePurpose(src), src.mediaId);
    lane.source = src;
    if (lane.mediaKey !== key) {
      lane.unpin?.();
      lane.unpin = null;
      if (lane.objectUrl) {
        lane.el.pause();
        lane.el.removeAttribute("src");
        lane.el.load();
      }
      lane.objectUrl = null;
      lane.mediaKey = key;
    }
    if (lane.objectUrl) return;
    const m = this.media(src.mediaId, lanePurpose(src));
    if (m.state !== "ready") return;
    lane.objectUrl = m.entry.objectUrl;
    lane.unpin = this.cache.pin(m.entry.key);
    lane.el.src = m.entry.objectUrl;
  }

  private setGain(lane: Lane, gain: number, live: boolean): void {
    if (Math.abs(gain - lane.lastGain) < 0.002) return;
    lane.lastGain = gain;
    if (lane.gain && this.ctx) {
      if (live && this.ctx.state === "running") {
        // 播放中每次直接改 value 会有拉链声：用很短的时间常数平滑过去。
        lane.gain.gain.setTargetAtTime(gain, this.ctx.currentTime, 0.015);
      } else {
        lane.gain.gain.value = gain;
      }
    } else {
      // 没有 Web Audio：元素音量只能到 1（200% 的片段在预览里就是 100%，界面已说明预览是近似）。
      lane.el.volume = Math.max(0, Math.min(1, gain));
    }
  }

  /** 此刻在响的音源播起来，即将开始的提前建好并定位，其余释放。 */
  private syncAudio(frame: ScheduleFrame, ms: number, live: boolean): void {
    const want = new Map<string, { source: ScheduleAudioSource; sourceMs: number; gain: number; active: boolean }>();
    for (const a of frame.audio) want.set(a.source.key, { source: a.source, sourceMs: a.sourceMs, gain: a.gain, active: true });
    for (const src of upcomingAudio(this.plan, ms, this.leadMs)) {
      if (!want.has(src.key)) want.set(src.key, { source: src, sourceMs: src.sourceInMs, gain: 0, active: false });
    }
    for (const [key, lane] of this.lanes) {
      if (want.has(key)) continue;
      this.releaseLane(lane);
      this.lanes.delete(key);
    }
    for (const [key, w] of want) {
      let lane = this.lanes.get(key);
      if (!lane) {
        lane = this.createLane(w.source);
        this.lanes.set(key, lane);
      }
      this.bindLane(lane, w.source);
      if (!lane.objectUrl) continue;
      const el = lane.el;
      const rate = w.source.speed / 100;
      if (el.playbackRate !== rate) el.playbackRate = rate;
      this.setGain(lane, w.active ? w.gain : 0, live);
      const at = w.sourceMs / 1000;
      if (live && w.active) {
        if (Math.abs(el.currentTime - at) > this.driftSec) el.currentTime = at;
        if (el.paused) playQuietly(el);
      } else {
        if (!el.paused) el.pause();
        if (Math.abs(el.currentTime - at) > PAUSED_DRIFT_SEC) el.currentTime = at;
      }
    }
  }

  // ---------------------------------------------------------------- 文字与字幕

  private syncOverlay(frame: ScheduleFrame): void {
    if (sameOverlays(this.overlays, frame.overlays)) return;
    this.overlays = frame.overlays;
    this.emit("overlay", { items: frame.overlays });
  }
}
