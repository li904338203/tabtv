import { describe, expect, it } from "vitest";
import type { components } from "@tabtv/api-client";
import { smartcutRelayout } from "./smartcutModel";
import {
  smartcutAddSubtitle,
  smartcutAddSubtitleTrack,
  smartcutAddText,
  smartcutFadeOutLimitMs,
  smartcutMergeStyle,
  smartcutMoveTrack,
  smartcutPatchClip,
  smartcutPatchSubtitle,
  smartcutPatchText,
  smartcutRemoveSubtitles,
  smartcutRemoveText,
  smartcutSwitchAlternate,
  smartcutTransitionLimitMs
} from "./smartcutPanels";

type Timeline = components["schemas"]["Timeline"];
type TimelineClip = components["schemas"]["TimelineClip"];

const c = (id: string, outMs: number, extra: Partial<TimelineClip> = {}): TimelineClip =>
  ({ id, idx: 0, mediaId: `m${id}`, sourceInMs: 0, sourceOutMs: outMs, startMs: 0, durationMs: outMs, speed: 100, volume: 100, ...extra });

function tl(): Timeline {
  return smartcutRelayout({
    id: "1", projectId: "2", name: "t", version: 1, aspectRatio: "16:9", fps: 30, width: 1920, height: 1080, status: "editing",
    tracks: [
      { id: "v", kind: "video", idx: 1, isMuted: false, volume: 100, clips: [c("a", 4000), c("b", 2000), c("c", 6000)] },
      { id: "s1", kind: "subtitle", idx: 2, isMuted: false, volume: 100, lang: "zh-CN", clips: [] },
      { id: "s2", kind: "subtitle", idx: 3, isMuted: false, volume: 100, lang: "en", clips: [] },
      { id: "w", kind: "text", idx: 4, isMuted: false, volume: 100, clips: [] }
    ],
    subtitles: [
      { id: "x1", trackId: "s1", startMs: 0, endMs: 1000, text: "一" },
      { id: "x2", trackId: "s1", startMs: 1000, endMs: 2500, text: "二", anchor: { clipId: "a", srcStartMs: 1000, srcEndMs: 2500 } }
    ],
    texts: [{ id: "t1", trackId: "w", startMs: 0, endMs: 1000, text: "标题" }],
    totalMs: 0, effectiveMs: 0
  });
}

const ok = <T>(r: { ok: true; value: T } | { ok: false; reason: string }): T => {
  if (!r.ok) throw new Error(r.reason);
  return r.value;
};

describe("片段设置", () => {
  it("smartcutPatchClip：音量、淡入淡出；淡入 + 淡出超过片段长度时拦下并说出上限", () => {
    const t = tl();
    const next = ok(smartcutPatchClip(t, "b", { volume: 150, fadeInMs: 500, fadeOutMs: 500 }));
    expect(next.tracks[0]!.clips[1]).toMatchObject({ volume: 150, fadeInMs: 500, fadeOutMs: 500, origin: "user" });
    expect(smartcutPatchClip(t, "b", { fadeInMs: 1500, fadeOutMs: 600 })).toEqual({ ok: false, reason: "声音淡入与淡出合计不能超过片段长度 2 秒" });
    expect(smartcutPatchClip(t, "b", { volume: 201 }).ok).toBe(false);
    expect(ok(smartcutPatchClip(t, "b", { volume: 100 }))).toBe(t);
  });

  it("smartcutPatchClip：转场上限取相邻两段较短者的一半，第一段是「从黑场淡入」只看自己", () => {
    const t = tl();
    expect(smartcutTransitionLimitMs(t, "b")).toBe(1000);
    expect(smartcutTransitionLimitMs(t, "a")).toBe(2000);
    expect(smartcutTransitionLimitMs(t, "c")).toBe(1000);
    expect(smartcutPatchClip(t, "b", { transitionIn: { kind: "dissolve", durationMs: 1200 } })).toEqual({ ok: false, reason: "转场最长 1 秒（不超过相邻两段较短者的一半）" });
    expect(ok(smartcutPatchClip(t, "b", { transitionIn: { kind: "dissolve", durationMs: 800 } })).tracks[0]!.clips[1]!.transitionIn).toEqual({ kind: "dissolve", durationMs: 800 });
    expect(smartcutPatchClip(t, "a", { transitionIn: { kind: "fade", durationMs: 2500 } })).toEqual({ ok: false, reason: "从黑场淡入最长 2 秒（不超过相邻两段较短者的一半）" });
    // none = 去掉
    const withT = ok(smartcutPatchClip(t, "b", { transitionIn: { kind: "fade", durationMs: 500 } }));
    expect(ok(smartcutPatchClip(withT, "b", { transitionIn: { kind: "none", durationMs: 0 } })).tracks[0]!.clips[1]).not.toHaveProperty("transitionIn");
  });

  it("smartcutPatchClip：只有最后一段能淡出到黑场，上限是它的一半", () => {
    const t = tl();
    expect(smartcutFadeOutLimitMs(t, "c")).toBe(3000);
    expect(smartcutFadeOutLimitMs(t, "b")).toBe(0);
    expect(smartcutPatchClip(t, "b", { transitionOut: { kind: "fade", durationMs: 500 } })).toEqual({ ok: false, reason: "只有画面轨的最后一段能淡出到黑场" });
    expect(ok(smartcutPatchClip(t, "c", { transitionOut: { kind: "fade", durationMs: 1500 } })).tracks[0]!.clips[2]!.transitionOut).toEqual({ kind: "fade", durationMs: 1500 });
  });
});

describe("备选", () => {
  it("smartcutSwitchAlternate：切回原片，当前的进备选（种类取最近一次生成的）；再切回来", () => {
    const t = tl();
    t.tracks[0]!.clips[0] = { ...t.tracks[0]!.clips[0]!, mediaId: "regen2", alternates: [
      { mediaId: "orig", sourceInMs: 500, sourceOutMs: 3500, kind: "original" },
      { mediaId: "regen1", sourceInMs: 0, sourceOutMs: 4000, kind: "regen" }
    ] };
    const back = ok(smartcutSwitchAlternate(t, "a", 0));
    expect(back.tracks[0]!.clips[0]).toMatchObject({ mediaId: "orig", sourceInMs: 500, sourceOutMs: 3500 });
    expect(back.tracks[0]!.clips[0]!.alternates).toEqual([
      { mediaId: "regen1", sourceInMs: 0, sourceOutMs: 4000, kind: "regen" },
      { mediaId: "regen2", sourceInMs: 0, sourceOutMs: 4000, kind: "regen" }
    ]);
    const again = ok(smartcutSwitchAlternate(back, "a", 1));
    expect(again.tracks[0]!.clips[0]!.mediaId).toBe("regen2");
    expect(again.tracks[0]!.clips[0]!.alternates!.at(-1)).toMatchObject({ mediaId: "orig", kind: "original" });
    expect(smartcutSwitchAlternate(t, "a", 9).ok).toBe(false);
  });
});

describe("文字层", () => {
  it("smartcutAddText：放在第一条有空的文字轨；都满了新建一条", () => {
    const t = tl();
    const one = ok(smartcutAddText(t, 2000, { text: "tmp-t2", track: "tmp-w2" }));
    expect(one.timeline.texts!.at(-1)).toMatchObject({ id: "tmp-t2", trackId: "w", startMs: 2000, endMs: 4000 });
    const two = ok(smartcutAddText(t, 500, { text: "tmp-t3", track: "tmp-w3" }));
    expect(two.timeline.texts!.at(-1)).toMatchObject({ trackId: "tmp-w3" });
    expect(two.timeline.tracks.at(-1)).toMatchObject({ id: "tmp-w3", kind: "text" });
  });

  it("smartcutPatchText：空内容、过短、同轨重叠拦下", () => {
    const t = ok(smartcutAddText(tl(), 2000, { text: "t2", track: "w2" })).timeline;
    expect(ok(smartcutPatchText(t, "t1", { text: "新标题", style: { fontSizePct: 9 } })).texts![0]).toMatchObject({ text: "新标题", style: { fontSizePct: 9 } });
    expect(smartcutPatchText(t, "t1", { text: "  " }).ok).toBe(false);
    expect(smartcutPatchText(t, "t1", { endMs: 50 }).ok).toBe(false);
    expect(smartcutPatchText(t, "t1", { startMs: 1500, endMs: 2500 })).toEqual({ ok: false, reason: "与同一条文字轨上的文字重叠" });
    expect(smartcutRemoveText(t, "t1").texts).toHaveLength(1);
  });
});

describe("字幕", () => {
  it("smartcutAddSubtitle / smartcutPatchSubtitle：加一条、改文字照旧跟随、改起止就不再跟随", () => {
    const t = tl();
    const added = ok(smartcutAddSubtitle(t, "s2", 3000, "tmp-x3"));
    expect(added.subtitles.at(-1)).toMatchObject({ id: "tmp-x3", trackId: "s2", startMs: 3000, endMs: 5000, text: "新字幕" });
    const text = ok(smartcutPatchSubtitle(t, "x2", { text: "改了" }));
    expect(text.subtitles[1]).toMatchObject({ text: "改了", anchor: { clipId: "a" } });
    const time = ok(smartcutPatchSubtitle(t, "x2", { startMs: 1200 }));
    expect(time.subtitles[1]).not.toHaveProperty("anchor");
    expect(smartcutPatchSubtitle(t, "x1", { endMs: 0 }).ok).toBe(false);
    expect(smartcutPatchSubtitle(t, "x1", { text: "" }).ok).toBe(false);
    expect(smartcutRemoveSubtitles(t, ["x1"]).subtitles).toHaveLength(1);
  });

  it("smartcutAddSubtitleTrack / smartcutMoveTrack：新轨放在字幕轨之后；同类之间换位", () => {
    const t = tl();
    const added = ok(smartcutAddSubtitleTrack(t, "tmp-s3", "ja"));
    expect(added.tracks.map(x => x.id)).toEqual(["v", "s1", "s2", "tmp-s3", "w"]);
    const moved = smartcutMoveTrack(t, "s2", -1);
    expect(moved.tracks.map(x => x.id)).toEqual(["v", "s2", "s1", "w"]);
    expect(smartcutMoveTrack(t, "s1", -1)).toBe(t);
  });

  it("smartcutMergeStyle：描边 / 底框按键合并，null 删掉，未知字段保留", () => {
    expect(smartcutMergeStyle({ fontSizePct: 4.5, stroke: { color: "#000000", widthPct: 0.3 }, future: 1 }, { stroke: { widthPct: 0.5 }, color: "#ffcc00" }))
      .toEqual({ fontSizePct: 4.5, stroke: { color: "#000000", widthPct: 0.5 }, future: 1, color: "#ffcc00" });
    expect(smartcutMergeStyle({ stroke: { widthPct: 1 } }, { stroke: null })).toEqual({});
  });
});
