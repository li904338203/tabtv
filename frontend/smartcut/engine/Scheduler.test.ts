import { describe, expect, it } from "vitest";
import type { components } from "@tabtv/api-client";
import {
  buildSchedule,
  mergeOverlayStyle,
  overlaysAt,
  sameOverlays,
  scheduleAt,
  stackSubtitleShifts,
  subtitleBottomPct,
  upcomingAudio,
  upcomingVideo,
  type ScheduleMediaInfo
} from "./Scheduler";

type Timeline = components["schemas"]["Timeline"];
type TimelineClip = components["schemas"]["TimelineClip"];
type TimelineTrack = components["schemas"]["TimelineTrack"];
type TimelineSubtitle = components["schemas"]["TimelineSubtitle"];
type TimelineText = components["schemas"]["TimelineText"];

function clip(id: string, mediaId: string, sourceInMs: number, sourceOutMs: number, extra: Partial<TimelineClip> = {}): TimelineClip {
  return { id, idx: 0, mediaId, sourceInMs, sourceOutMs, startMs: 0, durationMs: 0, speed: 100, volume: 100, ...extra };
}

function track(id: string, kind: TimelineTrack["kind"], clips: TimelineClip[] = [], extra: Partial<TimelineTrack> = {}): TimelineTrack {
  return { id, kind, idx: 0, isMuted: false, volume: 100, clips, ...extra };
}

function timeline(tracks: TimelineTrack[], subtitles: TimelineSubtitle[] = [], texts: TimelineText[] = []): Timeline {
  return {
    id: "1", projectId: "2", name: "t", version: 1, aspectRatio: "16:9", fps: 25, width: 1920, height: 1080,
    status: "editing", tracks, subtitles, texts, totalMs: 0, effectiveMs: 0
  };
}

const fade = (durationMs: number) => ({ kind: "fade" as const, durationMs });

/** 画面轨：A 2 秒、B 2 秒且与 A 叠化 500ms（B 从 1500 开始）、C 1 秒硬切（从 3500 开始）。成片 4.5 秒。 */
function threeClips(extra: { a?: Partial<TimelineClip>; b?: Partial<TimelineClip>; c?: Partial<TimelineClip> } = {}) {
  return track("v", "video", [
    clip("a", "m1", 1000, 3000, extra.a),
    clip("b", "m2", 0, 2000, { transitionIn: fade(500), ...extra.b }),
    clip("c", "m3", 500, 1500, extra.c)
  ]);
}

describe("buildSchedule / scheduleAt：画面", () => {
  it("空时间线：成片 0，任何时刻都是播完", () => {
    const plan = buildSchedule(timeline([track("v", "video")]));
    expect(plan.effectiveMs).toBe(0);
    expect(scheduleAt(plan, 0)).toEqual({ ms: 0, ended: true, video: [], audio: [], overlays: [] });
    expect(buildSchedule(null).video).toEqual([]);
  });

  it("成片时间由 smartcutLayout 算（不读片段上存的 startMs）", () => {
    const plan = buildSchedule(timeline([threeClips({ b: { startMs: 99999 } })]));
    expect(plan.video.map(s => [s.clipId, s.startMs, s.endMs])).toEqual([["a", 0, 2000], ["b", 1500, 3500], ["c", 3500, 4500]]);
    expect(plan.effectiveMs).toBe(4500);
  });

  it("转场重叠区里两段同时在：前一段在下、不透明，后一段按进度 0→1", () => {
    const plan = buildSchedule(timeline([threeClips()]));
    const f = scheduleAt(plan, 1750);
    expect(f.video.map(v => [v.segment.clipId, v.opacity])).toEqual([["a", 1], ["b", 0.5]]);
    expect(f.video[0]!.sourceMs).toBe(2750);
    expect(f.video[1]!.sourceMs).toBe(250);
  });

  it("左闭右开：A 在 2000 已结束；B 的重叠结束后满不透明；末尾即播完", () => {
    const plan = buildSchedule(timeline([threeClips()]));
    expect(scheduleAt(plan, 1999).video.map(v => v.segment.clipId)).toEqual(["a", "b"]);
    expect(scheduleAt(plan, 2000).video.map(v => [v.segment.clipId, v.opacity])).toEqual([["b", 1]]);
    expect(scheduleAt(plan, 3500).video.map(v => v.segment.clipId)).toEqual(["c"]);
    expect(scheduleAt(plan, 4499).ended).toBe(false);
    expect(scheduleAt(plan, 4500).ended).toBe(true);
    expect(scheduleAt(plan, -5).ms).toBe(0);
  });

  it("变速片段的素材内时刻：2 倍速，成片走 500ms 素材走 1000ms", () => {
    const plan = buildSchedule(timeline([track("v", "video", [clip("a", "m1", 1000, 5000, { speed: 200 })])]));
    expect(plan.effectiveMs).toBe(2000);
    expect(scheduleAt(plan, 500).video[0]!.sourceMs).toBe(2000);
  });

  it("第一段的转入 = 从黑场淡入；最后一段的转出 = 淡出到黑场", () => {
    const plan = buildSchedule(timeline([track("v", "video", [
      clip("a", "m1", 0, 2000, { transitionIn: fade(400) }),
      clip("b", "m2", 0, 2000, { transitionOut: fade(1000) })
    ])]));
    expect(scheduleAt(plan, 100).video[0]!.opacity).toBe(0.25);
    expect(scheduleAt(plan, 1000).video[0]!.opacity).toBe(1);
    expect(scheduleAt(plan, 4000 - 250).video[0]!.opacity).toBe(0.25);
    expect(scheduleAt(plan, 2500).video[0]!.opacity).toBe(1);
  });

  it("中间段的转出不生效（接缝的转场只记在后一段上）；kind none 不算转场", () => {
    const plan = buildSchedule(timeline([track("v", "video", [
      clip("a", "m1", 0, 2000, { transitionOut: fade(1000) }),
      clip("b", "m2", 0, 2000, { transitionIn: { kind: "none", durationMs: 500 } })
    ])]));
    expect(plan.video[0]!.fadeToBlackMs).toBe(0);
    expect(plan.video[1]!.startMs).toBe(2000);
    expect(scheduleAt(plan, 1900).video[0]!.opacity).toBe(1);
  });

  it("图片段：种类来自素材信息，素材内时刻为 0；时长为 0 的段不进表", () => {
    const info = (id: string): ScheduleMediaInfo | undefined => (id === "img" ? { kind: "image" } : undefined);
    const plan = buildSchedule(timeline([track("v", "video", [clip("a", "img", 0, 3000), clip("z", "m2", 500, 500)])]), info);
    expect(plan.video.map(s => [s.clipId, s.kind])).toEqual([["a", "image"]]);
    expect(scheduleAt(plan, 1200).video[0]!.sourceMs).toBe(0);
  });

  it("后面几段（预加载）：只给起点在此刻之后的", () => {
    const plan = buildSchedule(timeline([threeClips()]));
    expect(upcomingVideo(plan, 0).map(s => s.clipId)).toEqual(["b", "c"]);
    expect(upcomingVideo(plan, 1600, 2).map(s => s.clipId)).toEqual(["c"]);
    expect(upcomingVideo(plan, 0, 1).map(s => s.clipId)).toEqual(["b"]);
  });
});

describe("声音", () => {
  it("音频轨片段：起止左闭右开，增益 = trunc(片段 × 轨道 / 100) / 100 × 线性包络", () => {
    const plan = buildSchedule(timeline([
      threeClips(),
      track("music", "audio", [clip("s", "m9", 0, 3000, { startMs: 1000, volume: 50, fadeInMs: 1000, fadeOutMs: 1000 })], { volume: 80 })
    ]));
    const src = plan.audio.find(a => a.origin === "audio")!;
    expect(src).toMatchObject({ key: "music:s", startMs: 1000, endMs: 4000, baseGain: 0.4 });
    const gainAt = (ms: number) => scheduleAt(plan, ms).audio.find(a => a.source.key === "music:s")?.gain;
    expect(gainAt(999)).toBeUndefined();
    expect(gainAt(1500)).toBeCloseTo(0.2, 10);
    expect(gainAt(2500)).toBeCloseTo(0.4, 10);
    expect(gainAt(3500)).toBeCloseTo(0.2, 10);
    expect(gainAt(4000)).toBeUndefined();
  });

  it("音量按整数换算（与服务端一致）：33% × 50% = 16%", () => {
    const plan = buildSchedule(timeline([track("v", "video", [clip("a", "m1", 0, 1000, { volume: 33 })], { volume: 50 })]));
    expect(plan.audio[0]!.baseGain).toBe(0.16);
  });

  it("画面原声在转场处前一段淡出、后一段淡入（与用户设的取较大者）", () => {
    const plan = buildSchedule(timeline([threeClips({ a: { fadeOutMs: 200 }, b: { fadeInMs: 800 } })]));
    const byClip = Object.fromEntries(plan.audio.map(a => [a.clipId, a]));
    expect(byClip.a).toMatchObject({ origin: "video", fadeInMs: 0, fadeOutMs: 500 });
    expect(byClip.b).toMatchObject({ fadeInMs: 800, fadeOutMs: 0 });
    expect(byClip.c).toMatchObject({ fadeInMs: 0, fadeOutMs: 0 });
  });

  it("淡入 + 淡出夹在片段长度之内", () => {
    const plan = buildSchedule(timeline([track("m", "audio", [clip("s", "x", 0, 1000, { fadeInMs: 800, fadeOutMs: 800 })])]));
    expect(plan.audio[0]).toMatchObject({ fadeInMs: 800, fadeOutMs: 200 });
  });

  it("静音轨、音量 0 的轨与片段、图片、确知没有音轨的视频都不出声", () => {
    const info = (id: string): ScheduleMediaInfo | undefined =>
      id === "img" ? { kind: "image" } : id === "silent" ? { kind: "video", hasAudio: false } : undefined;
    const plan = buildSchedule(timeline([
      track("v", "video", [clip("a", "img", 0, 1000), clip("b", "silent", 0, 1000), clip("c", "m", 0, 1000, { volume: 0 }), clip("d", "m", 0, 1000)]),
      track("mute", "audio", [clip("s", "x", 0, 1000)], { isMuted: true }),
      track("zero", "audio", [clip("t", "x", 0, 1000)], { volume: 0 })
    ]), info);
    expect(plan.audio.map(a => a.key)).toEqual(["v:d"]);
    const muted = buildSchedule(timeline([track("v", "video", [clip("d", "m", 0, 1000)], { isMuted: true })]));
    expect(muted.audio).toEqual([]);
  });

  it("隐藏的音频轨照样出声（服务端渲染只看静音）", () => {
    const plan = buildSchedule(timeline([threeClips(), track("m", "audio", [clip("s", "x", 0, 1000)], { isHidden: true })]));
    expect(plan.audio.some(a => a.key === "m:s")).toBe(true);
  });

  it("即将开始的音源（提前建元素用）", () => {
    const plan = buildSchedule(timeline([
      threeClips(),
      track("m", "audio", [clip("s1", "x", 0, 500, { startMs: 1000 }), clip("s2", "x", 0, 500, { startMs: 3000 })])
    ]));
    expect(upcomingAudio(plan, 800, 300).map(a => a.key)).toEqual(["m:s1"]);
    expect(upcomingAudio(plan, 1000, 300)).toEqual([]);
  });
});

describe("文字与字幕", () => {
  const tracks = () => [
    threeClips(),
    track("s1", "subtitle", [], { style: { fontSizePct: 5, stroke: { color: "#000000", widthPct: 1 } } }),
    track("s2", "subtitle", [], { isHidden: true }),
    track("t1", "text")
  ];

  it("跳过隐藏轨道；字幕在前（合并轨道样式，条目优先），再到文字层", () => {
    const plan = buildSchedule(timeline(tracks(), [
      { id: "x1", trackId: "s1", startMs: 0, endMs: 2000, text: "你好", style: { stroke: { widthPct: 2 } } as never },
      { id: "x2", trackId: "s2", startMs: 0, endMs: 2000, text: "hidden track" }
    ], [
      { id: "tt", trackId: "t1", startMs: 500, endMs: 1500, text: "标题", style: { color: "#ff0000" }, box: { yPct: 20, xPct: 50 } }
    ]));
    const items = overlaysAt(plan, 1000);
    expect(items.map(i => i.key)).toEqual(["subtitle:x1", "text:tt"]);
    expect(items[0]!.style).toEqual({ fontSizePct: 5, stroke: { color: "#000000", widthPct: 2 } });
    expect(items[0]!.trackOrder).toBe(1);
    expect(items[1]).toMatchObject({ kind: "text", style: { color: "#ff0000" }, box: { xPct: 50, yPct: 20 } });
    expect(Object.keys(items[1]!.box!)).toEqual(["xPct", "yPct"]);
    expect(scheduleAt(plan, 1000).overlays).toEqual(items);
  });

  it("多条可见字幕轨按轨道顺序（第一条在最下），轨道序号只数可见的", () => {
    const plan = buildSchedule(timeline([threeClips(), track("s0", "subtitle", [], { isHidden: true }), track("s1", "subtitle"), track("s2", "subtitle")], [
      { id: "b", trackId: "s2", startMs: 0, endMs: 1000, text: "second" },
      { id: "a", trackId: "s1", startMs: 100, endMs: 1000, text: "first" }
    ]));
    expect(overlaysAt(plan, 500).map(i => [i.id, i.trackOrder])).toEqual([["a", 1], ["b", 2]]);
  });

  it("空白文字不画；越过成片末尾的夹到末尾；跟随片段被裁掉的隐藏", () => {
    const plan = buildSchedule(timeline([threeClips({ a: { sourceInMs: 1800 } }), track("s1", "subtitle")], [
      { id: "blank", trackId: "s1", startMs: 0, endMs: 1000, text: "  　 " },
      { id: "long", trackId: "s1", startMs: 3000, endMs: 9000, text: "到最后" },
      { id: "cut", trackId: "s1", startMs: 0, endMs: 500, text: "被裁掉", anchor: { clipId: "a", srcStartMs: 1000, srcEndMs: 1500 } }
    ]));
    expect(plan.overlays.map(i => i.id)).toEqual(["long"]);
    expect(plan.overlays[0]).toMatchObject({ startMs: 3000, endMs: plan.effectiveMs });
    expect(overlaysAt(plan, plan.effectiveMs)).toEqual([]);
  });

  it("内容签名：同一条改了字就不是同一组", () => {
    const mk = (text: string) => buildSchedule(timeline([threeClips(), track("s1", "subtitle")], [{ id: "a", trackId: "s1", startMs: 0, endMs: 1000, text }]));
    const a = overlaysAt(mk("一"), 10);
    expect(sameOverlays(a, overlaysAt(mk("一"), 10))).toBe(true);
    expect(sameOverlays(a, overlaysAt(mk("二"), 10))).toBe(false);
    expect(sameOverlays(a, [])).toBe(false);
  });
});

describe("样式合并与字幕叠放（与服务端同一算法）", () => {
  it("逐层覆盖、嵌套对象递归、null 不覆盖、非对象层跳过、键按字典序", () => {
    const merged = mergeOverlayStyle({ b: 1, stroke: { color: "#000", widthPct: 1 } }, "bad", { stroke: { widthPct: 2 }, a: null, c: 3 });
    expect(merged).toEqual({ b: 1, c: 3, stroke: { color: "#000", widthPct: 2 } });
    expect(Object.keys(merged!)).toEqual(["b", "c", "stroke"]);
    expect(mergeOverlayStyle(null, undefined, [1])).toBeNull();
  });

  it("下边距：缺省 8，夹到 0~50", () => {
    expect(subtitleBottomPct(undefined)).toBe(8);
    expect(subtitleBottomPct({ bottomPct: 60 })).toBe(50);
    expect(subtitleBottomPct({ bottomPct: "x" })).toBe(8);
    expect(subtitleBottomPct({ bottomPct: 12 })).toBe(12);
  });

  it("叠放：第一条不动，后面每条让出前面各条的高度加 1% 间距，只往上不往下", () => {
    expect(stackSubtitleShifts([8], [5])).toEqual([0]);
    expect(stackSubtitleShifts([8, 8, 8], [5, 4, 6])).toEqual([0, 6, 11]);
    expect(stackSubtitleShifts([8, 8, 8], [5, 0, 6])).toEqual([0, 6, 6]);
    expect(stackSubtitleShifts([8, 30], [5, 5])).toEqual([0, 0]);
  });
});
