// @vitest-environment jsdom
import { afterAll, beforeAll, describe, expect, it } from "vitest";
import type { components } from "@tabtv/api-client";
import { PlaybackEngine, type PlaybackEngineOptions, type PlaybackMediaPurpose } from "./PlaybackEngine";
import type { ScheduleMediaInfo } from "./Scheduler";

type Timeline = components["schemas"]["Timeline"];
type TimelineClip = components["schemas"]["TimelineClip"];
type TimelineTrack = components["schemas"]["TimelineTrack"];

// jsdom 没有实现媒体播放：把 play / pause / load / currentTime / paused / playbackRate 换成可观察的假实现。
type FakeMedia = { paused: boolean; time: number; rate: number };
const fake = new WeakMap<HTMLMediaElement, FakeMedia>();
function st(el: HTMLMediaElement): FakeMedia {
  let s = fake.get(el);
  if (!s) {
    s = { paused: true, time: 0, rate: 1 };
    fake.set(el, s);
  }
  return s;
}
const proto = HTMLMediaElement.prototype;
const saved: Array<[string, PropertyDescriptor | undefined]> = ["paused", "currentTime", "playbackRate", "play", "pause", "load"]
  .map(k => [k, Object.getOwnPropertyDescriptor(proto, k)]);

beforeAll(() => {
  Object.defineProperty(proto, "paused", { configurable: true, get(this: HTMLMediaElement) { return st(this).paused; } });
  Object.defineProperty(proto, "currentTime", {
    configurable: true,
    get(this: HTMLMediaElement) { return st(this).time; },
    set(this: HTMLMediaElement, v: number) { st(this).time = v; }
  });
  Object.defineProperty(proto, "playbackRate", {
    configurable: true,
    get(this: HTMLMediaElement) { return st(this).rate; },
    set(this: HTMLMediaElement, v: number) { st(this).rate = v; }
  });
  Object.defineProperty(proto, "play", { configurable: true, writable: true, value(this: HTMLMediaElement) { st(this).paused = false; return Promise.resolve(); } });
  Object.defineProperty(proto, "pause", { configurable: true, writable: true, value(this: HTMLMediaElement) { st(this).paused = true; } });
  Object.defineProperty(proto, "load", { configurable: true, writable: true, value() {} });
});

afterAll(() => {
  for (const [k, d] of saved) {
    // 原型上本来没有的（jsdom 未实现）就把假实现删掉，恢复原样。
    if (d) Object.defineProperty(proto, k, d);
    else Reflect.deleteProperty(proto, k);
  }
});

function clip(id: string, mediaId: string, sourceInMs: number, sourceOutMs: number, extra: Partial<TimelineClip> = {}): TimelineClip {
  return { id, idx: 0, mediaId, sourceInMs, sourceOutMs, startMs: 0, durationMs: 0, speed: 100, volume: 100, ...extra };
}

function track(id: string, kind: TimelineTrack["kind"], clips: TimelineClip[] = [], extra: Partial<TimelineTrack> = {}): TimelineTrack {
  return { id, kind, idx: 0, isMuted: false, volume: 100, clips, ...extra };
}

/** 画面：A（m1，2 秒）→ 叠化 500ms → B（m2，2 秒），成片 3.5 秒；音频轨 S 从 500 响到 1500；字幕 1000~2000。 */
function sample(): Timeline {
  return {
    id: "1", projectId: "2", name: "t", version: 1, aspectRatio: "16:9", fps: 25, width: 1920, height: 1080, status: "editing",
    tracks: [
      track("v", "video", [clip("a", "m1", 1000, 3000), clip("b", "m2", 0, 2000, { transitionIn: { kind: "fade", durationMs: 500 } })]),
      track("a1", "audio", [clip("s", "m9", 0, 1000, { startMs: 500 })]),
      track("t1", "subtitle")
    ],
    subtitles: [{ id: "x", trackId: "t1", startMs: 1000, endMs: 2000, text: "你好" }],
    texts: [],
    totalMs: 0,
    effectiveMs: 0
  };
}

const silent = (id: string): ScheduleMediaInfo | undefined => (id === "m9" ? { kind: "audio" } : { kind: "video", hasAudio: false });

type Loader = (mediaId: string, purpose: PlaybackMediaPurpose) => Promise<Blob | null>;

function harness(options: { loadMedia?: Loader; mediaInfo?: PlaybackEngineOptions["mediaInfo"] } = {}) {
  let now = 0;
  let nextId = 0;
  const frames = new Map<number, (t: number) => void>();
  const revoked: string[] = [];
  const loads: string[] = [];
  let urls = 0;
  const created: string[] = [];
  const stage = document.createElement("div");
  document.body.append(stage);
  const engine = new PlaybackEngine({
    stage,
    loadMedia: options.loadMedia ?? (async (mediaId, purpose) => {
      loads.push(`${purpose}:${mediaId}`);
      return new Blob([new Uint8Array(16)]);
    }),
    mediaInfo: options.mediaInfo ?? silent,
    createAudioContext: () => null,
    createObjectURL: () => {
      const url = `blob:${++urls}`;
      created.push(url);
      return url;
    },
    revokeObjectURL: url => revoked.push(url),
    now: () => now,
    requestFrame: (cb) => {
      nextId += 1;
      frames.set(nextId, cb);
      return nextId;
    },
    cancelFrame: id => frames.delete(id)
  });
  const events: Array<[string, unknown]> = [];
  for (const type of ["timeupdate", "ended", "state", "stalled", "playing", "missing", "overlay", "preempted", "error"] as const) {
    engine.on(type, payload => events.push([type, payload]));
  }
  /** 让加载的 Promise 走完，再把排着的帧跑一轮（播放中每帧会再排下一帧，所以只跑一轮）。 */
  const step = async (advanceMs = 0) => {
    await new Promise(r => setTimeout(r, 0));
    now += advanceMs;
    const queued = [...frames.entries()];
    frames.clear();
    for (const [, cb] of queued) cb(now);
    await new Promise(r => setTimeout(r, 0));
  };
  const slot = (clipId: string) => engine.inspect().slots.find(s => s.clipId === clipId);
  const videoOf = (clipId: string) => {
    const layers = [...stage.querySelectorAll("video")];
    return layers.find(v => v.getAttribute("src") === slot(clipId)?.src);
  };
  return { engine, stage, events, step, slot, videoOf, revoked, created, loads };
}

describe("PlaybackEngine（jsdom）", () => {
  it("暂停时定位：画面取回来后挂到槽位，定位到素材内时刻；转场重叠区里两段叠放", async () => {
    const h = harness();
    h.engine.setTimeline(sample());
    await h.step();
    expect(h.slot("a")).toMatchObject({ src: "blob:1", opacity: "1", zIndex: "1" });
    expect(h.videoOf("a")!.currentTime).toBe(1);

    h.engine.seek(1750);
    await h.step();
    expect(h.slot("a")).toMatchObject({ opacity: "1", zIndex: "1" });
    expect(h.slot("b")).toMatchObject({ opacity: "0.5", zIndex: "2" });
    expect(h.videoOf("a")!.currentTime).toBeCloseTo(2.75, 6);
    expect(h.videoOf("b")!.currentTime).toBeCloseTo(0.25, 6);
    expect(h.videoOf("a")!.paused).toBe(true);
    h.engine.dispose();
  });

  it("播放按时钟推进，下一段提前预加载（透明、暂停在取用起点），播到末尾自动暂停并发 ended", async () => {
    const h = harness();
    h.engine.setTimeline(sample());
    await h.step();
    h.engine.play();
    await h.step(1000);
    expect(h.engine.currentMs).toBe(1000);
    expect(h.videoOf("a")!.paused).toBe(false);

    await h.step(300); // 1300：B 在 1500 开始，进入 300ms 的预加载窗口
    await h.step();
    expect(h.slot("b")).toMatchObject({ opacity: "0", zIndex: "0" });
    expect(h.videoOf("b")!.currentTime).toBe(0);
    expect(h.videoOf("b")!.paused).toBe(true);

    await h.step(5000);
    expect(h.engine.playing).toBe(false);
    expect(h.engine.currentMs).toBe(3500);
    expect(h.events.filter(([t]) => t === "ended")).toEqual([["ended", { ms: 3500 }]]);
    h.engine.dispose();
  });

  it("在画的一段还在取素材：播放中时钟停下来等，取回来后接着走并发 playing", async () => {
    // 每份素材各自一个闸门：预取也会调 loader，只放行 m1 才能证明「等的是在画的那一段」。
    const gates = new Map<string, (b: Blob) => void>();
    const release = (mediaId: string) => gates.get(mediaId)!(new Blob([new Uint8Array(4)]));
    const h = harness({ loadMedia: mediaId => new Promise<Blob | null>(r => gates.set(mediaId, r)) });
    h.engine.setTimeline(sample());
    h.engine.play();
    expect(h.engine.waiting).toBe(true);
    expect(h.events).toContainEqual(["stalled", { clipId: "a", mediaId: "m1", reason: "loading" }]);
    await h.step(500);
    expect(h.engine.currentMs).toBe(0);
    release("m1");
    await h.step();
    expect(h.engine.waiting).toBe(false);
    expect(h.events.some(([t]) => t === "playing")).toBe(true);
    await h.step(200);
    expect(h.engine.currentMs).toBe(200);
    h.engine.dispose();
  });

  it("预览版还没好（loader 回 null）：不报错、不干等，发 not_ready 并继续往下播", async () => {
    const h = harness({ loadMedia: async (id) => (id === "m1" ? null : new Blob([new Uint8Array(4)])) });
    h.engine.setTimeline(sample());
    h.engine.play();
    await h.step();
    await h.step();
    expect(h.engine.waiting).toBe(false);
    expect(h.events).toContainEqual(["stalled", { clipId: "a", mediaId: "m1", reason: "not_ready" }]);
    expect(h.events).toContainEqual(["missing", { items: [{ clipId: "a", mediaId: "m1", reason: "not_ready" }] }]);
    expect(h.slot("a")?.opacity).toBe("0");
    const before = h.engine.currentMs;
    await h.step(1000);
    expect(h.engine.currentMs).toBe(before + 1000);
    expect(h.events.some(([t]) => t === "error")).toBe(false);
    h.engine.dispose();
  });

  it("音频：即将开始的提前建元素，在响的按增益设音量（没有 Web Audio），播完的释放", async () => {
    const h = harness();
    h.engine.setTimeline(sample());
    await h.step();
    expect(h.engine.inspect().lanes).toEqual([]);
    h.engine.seek(300); // S 在 500 开始，进入预加载窗口
    await h.step();
    expect(h.engine.inspect().lanes).toEqual(["a1:s"]);
    h.engine.seek(1000);
    await h.step();
    expect(h.loads).toContain("audio:m9");
    h.engine.seek(2000);
    await h.step();
    expect(h.engine.inspect().lanes).toEqual([]);
    h.engine.dispose();
  });

  it("可见的字幕变化时发 overlay（进入与离开各一次）", async () => {
    const h = harness();
    h.engine.setTimeline(sample());
    await h.step();
    h.engine.seek(1500);
    h.engine.seek(1600);
    h.engine.seek(2500);
    const overlays = h.events.filter(([t]) => t === "overlay").map(([, p]) => (p as { items: Array<{ id: string }> }).items.map(i => i.id));
    expect(overlays).toEqual([["x"], []]);
    h.engine.dispose();
  });

  it("编辑后换时间线：播放中保持当前位置，超出新长度时夹到末尾", async () => {
    const h = harness();
    h.engine.setTimeline(sample());
    await h.step();
    h.engine.play();
    await h.step(1200);
    const shorter = sample();
    shorter.tracks[0]!.clips = [clip("a", "m1", 1000, 2000)];
    h.engine.setTimeline(shorter);
    expect(h.engine.currentMs).toBe(1000);
    expect(h.engine.durationMs).toBe(1000);
    h.engine.dispose();
  });

  it("同一页面只有一个在播：另一个开始播放时，前一个暂停并发 preempted", async () => {
    const a = harness();
    const b = harness();
    a.engine.setTimeline(sample());
    b.engine.setTimeline(sample());
    await a.step();
    await b.step();
    a.engine.play();
    b.engine.play();
    expect(a.engine.playing).toBe(false);
    expect(b.engine.playing).toBe(true);
    expect(a.events.some(([t]) => t === "preempted")).toBe(true);
    a.engine.dispose();
    b.engine.dispose();
  });

  it("dispose：自己建的缓存全部 revoke，根元素移除，之后的调用无效", async () => {
    const h = harness();
    h.engine.setTimeline(sample());
    await h.step();
    h.engine.seek(1750);
    await h.step();
    expect(h.stage.children.length).toBe(1);
    h.engine.dispose();
    expect(h.stage.children.length).toBe(0);
    // 在画的两段 + 预取的音频：建过的 object URL 一个不落地 revoke。
    expect(h.created.length).toBe(3);
    expect([...h.revoked].sort()).toEqual([...h.created].sort());
    h.engine.play();
    h.engine.seek(100);
    expect(h.engine.playing).toBe(false);
  });
});
