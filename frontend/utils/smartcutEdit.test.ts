import { describe, expect, it } from "vitest";
import type { components } from "@tabtv/api-client";
import {
  smartcutDeleteLeftAt,
  smartcutDeleteRightAt,
  smartcutDuplicateClip,
  smartcutFitZoom,
  smartcutInRange,
  smartcutInsertMarkMs,
  smartcutMoveClip,
  smartcutMoveSpan,
  smartcutPatchTrack,
  smartcutRemoveClips,
  smartcutReorderClip,
  smartcutReorderTarget,
  smartcutSetClipSpeed,
  smartcutSnapSpan,
  smartcutSplitAt,
  smartcutTrimClip,
  smartcutVideoClipAt,
  smartcutVisibleRange
} from "./smartcutEdit";
import { smartcutRelayout } from "./smartcutModel";
import { smartcutZoomLevels } from "./smartcutTimeline";

type Timeline = components["schemas"]["Timeline"];
type TimelineClip = components["schemas"]["TimelineClip"];

function c(id: string, outMs: number, extra: Partial<TimelineClip> = {}): TimelineClip {
  return { id, idx: 0, mediaId: `m${id}`, sourceInMs: 0, sourceOutMs: outMs, startMs: 0, durationMs: outMs, speed: 100, volume: 100, origin: "ai", ...extra };
}

function tl(): Timeline {
  return smartcutRelayout({
    id: "1", projectId: "2", name: "t", version: 3, aspectRatio: "16:9", fps: 30, width: 1920, height: 1080, status: "editing",
    tracks: [
      { id: "v", kind: "video", idx: 1, isMuted: false, volume: 100, clips: [c("a", 4000), c("b", 3000, { transitionIn: { kind: "fade", durationMs: 1000 } }), c("c", 2000)] },
      { id: "m", kind: "audio", idx: 2, isMuted: false, volume: 100, role: "music", clips: [c("x", 3000, { startMs: 0 }), c("y", 2000, { startMs: 5000 })] },
      { id: "s", kind: "subtitle", idx: 3, isMuted: false, volume: 100, clips: [] },
      { id: "w", kind: "text", idx: 4, isMuted: false, volume: 100, clips: [] }
    ],
    subtitles: [
      { id: "s1", trackId: "s", startMs: 0, endMs: 1000, text: "一" },
      { id: "s2", trackId: "s", startMs: 500, endMs: 1500, text: "二", anchor: { clipId: "a", srcStartMs: 500, srcEndMs: 1500 } }
    ],
    texts: [
      { id: "t1", trackId: "w", startMs: 0, endMs: 1000, text: "标题" },
      { id: "t2", trackId: "w", startMs: 2000, endMs: 3000, text: "副题" }
    ],
    totalMs: 0, effectiveMs: 0
  });
}

const ids = (t: Timeline, track = 0) => t.tracks[track]!.clips.map(x => x.id);
const value = (r: { ok: true; value: Timeline } | { ok: false; reason: string }) => {
  if (!r.ok) throw new Error(r.reason);
  return r.value;
};

describe("smartcutReorderClip", () => {
  it("smartcutReorderClip：画面轨挪位，来源改写为 user；位置没变原样返回", () => {
    const t = tl();
    const next = smartcutReorderClip(t, "a", 3);
    expect(ids(next)).toEqual(["b", "c", "a"]);
    expect(next.tracks[0]!.clips[2]!.origin).toBe("user");
    expect(smartcutReorderClip(t, "a", 1)).toBe(t);
    // 非画面轨不能排序
    expect(smartcutReorderClip(t, "x", 2)).toBe(t);
  });

  it("smartcutReorderClip：挪到第一位的段原来的转入超出新上限时被收短", () => {
    const t = tl();
    // b 原本有 1 秒转入；挪到第一位后它的转入是「从黑场淡入」，上限只看自己（1.5 秒），1 秒仍合法
    const next = smartcutReorderClip(t, "b", 1);
    expect(next.tracks[0]!.clips[0]!.transitionIn).toEqual({ kind: "fade", durationMs: 1000 });
  });
});

describe("smartcutMoveClip", () => {
  it("smartcutMoveClip：按起点放置的轨道改起点；重叠失败；画面轨不支持", () => {
    const t = tl();
    expect(value(smartcutMoveClip(t, "y", 3500)).tracks[1]!.clips[1]!.startMs).toBe(3500);
    expect(smartcutMoveClip(t, "y", 2000)).toEqual({ ok: false, reason: "与同一条轨上的片段重叠，已退回原处" });
    expect(smartcutMoveClip(t, "a", 100).ok).toBe(false);
    expect(value(smartcutMoveClip(t, "y", 5000))).toBe(t);
    // 夹到 0 之后与原起点相同：原样返回
    expect(value(smartcutMoveClip(t, "x", -50))).toBe(t);
  });
});

describe("smartcutTrimClip", () => {
  it("smartcutTrimClip：画面轨裁掉尾巴后后面的段跟着移", () => {
    const next = smartcutRelayout(value(smartcutTrimClip(tl(), "a", "end", -1000, 4000)));
    expect(next.tracks[0]!.clips[0]!.sourceOutMs).toBe(3000);
    // b 起点 = 3000 − 1000（转场）
    expect(next.tracks[0]!.clips[1]!.startMs).toBe(2000);
  });

  it("smartcutTrimClip：不超出素材时长；按起点放置的轨道拖长撞上下一段失败", () => {
    const t = tl();
    expect(value(smartcutTrimClip(t, "a", "end", 5000, 4500)).tracks[0]!.clips[0]!.sourceOutMs).toBe(4500);
    expect(smartcutTrimClip(t, "x", "end", 3000, 10000).ok).toBe(false);
  });
});

describe("分割与删左右", () => {
  it("smartcutSplitAt：以播放头为界一分为二，右半段用新 ID", () => {
    const next = smartcutRelayout(value(smartcutSplitAt(tl(), "a", 1500, "tmp-r")));
    expect(ids(next)).toEqual(["a", "tmp-r", "b", "c"]);
    expect(next.tracks[0]!.clips[0]).toMatchObject({ sourceOutMs: 1500, origin: "user" });
    expect(next.tracks[0]!.clips[1]).toMatchObject({ sourceInMs: 1500, sourceOutMs: 4000, startMs: 1500 });
    expect(smartcutSplitAt(tl(), "a", 4000, "tmp-r").ok).toBe(false);
  });

  it("smartcutDeleteLeftAt / smartcutDeleteRightAt：保留另一侧", () => {
    expect(value(smartcutDeleteLeftAt(tl(), "a", 1000)).tracks[0]!.clips[0]).toMatchObject({ sourceInMs: 1000, sourceOutMs: 4000 });
    expect(value(smartcutDeleteRightAt(tl(), "a", 1000)).tracks[0]!.clips[0]).toMatchObject({ sourceInMs: 0, sourceOutMs: 1000 });
    // 按起点放置的轨道删左侧：剩下的部分留在原处
    expect(value(smartcutDeleteLeftAt(tl(), "y", 6000)).tracks[1]!.clips[1]).toMatchObject({ sourceInMs: 1000, startMs: 6000 });
  });
});

describe("删除、复制、变速", () => {
  it("smartcutRemoveClips：跨轨删除；不存在时原样返回", () => {
    const t = tl();
    const next = smartcutRemoveClips(t, ["b", "y"]);
    expect(ids(next)).toEqual(["a", "c"]);
    expect(ids(next, 1)).toEqual(["x"]);
    expect(smartcutRemoveClips(t, ["nope"])).toBe(t);
  });

  it("smartcutDuplicateClip：画面轨插在后面；放置轨放在结束处，放不下就放到末尾；副本不继承溯源", () => {
    const t = tl();
    t.tracks[0]!.clips[0] = { ...t.tracks[0]!.clips[0]!, sourceShotId: "77", pendingGenerationId: "88" };
    const v = value(smartcutDuplicateClip(t, "a", "tmp-d"));
    expect(ids(v)).toEqual(["a", "tmp-d", "b", "c"]);
    expect(v.tracks[0]!.clips[1]).not.toHaveProperty("sourceShotId");
    expect(v.tracks[0]!.clips[1]).not.toHaveProperty("pendingGenerationId");
    // x 结束在 3000，副本 [3000, 6000) 会撞上 y（5000 起）→ 放到轨道末尾 7000
    expect(value(smartcutDuplicateClip(t, "x", "tmp-e")).tracks[1]!.clips[2]!.startMs).toBe(7000);
    // y 挪远之后放得下，就紧跟在 x 后面
    const roomy = value(smartcutMoveClip(t, "y", 8000));
    expect(value(smartcutDuplicateClip(roomy, "x", "tmp-f")).tracks[1]!.clips[2]!.startMs).toBe(3000);
  });

  it("smartcutSetClipSpeed：变速后放置轨撞上下一段失败；同速原样返回", () => {
    const t = tl();
    expect(value(smartcutSetClipSpeed(t, "a", 100))).toBe(t);
    expect(smartcutRelayout(value(smartcutSetClipSpeed(t, "a", 200))).tracks[0]!.clips[0]!.durationMs).toBe(2000);
    expect(smartcutSetClipSpeed(t, "x", 50)).toEqual({ ok: false, reason: "与同一条轨上的片段重叠，变速后放不下" });
  });
});

describe("轨道与字幕", () => {
  it("smartcutPatchTrack：改属性；值没变原样返回", () => {
    const t = tl();
    expect(smartcutPatchTrack(t, "m", { isMuted: true }).tracks[1]!.isMuted).toBe(true);
    expect(smartcutPatchTrack(t, "m", { isMuted: false })).toBe(t);
    expect(smartcutPatchTrack(t, "nope", { isMuted: true })).toBe(t);
  });

  it("smartcutMoveSpan：字幕整体平移；跟随片段的不能拖；文字同轨不许重叠", () => {
    const t = tl();
    expect(value(smartcutMoveSpan(t, "subtitle", "s1", 2000)).subtitles[0]).toMatchObject({ startMs: 2000, endMs: 3000 });
    expect(smartcutMoveSpan(t, "subtitle", "s2", 2000).ok).toBe(false);
    expect(smartcutMoveSpan(t, "text", "t1", 2500).ok).toBe(false);
    expect(value(smartcutMoveSpan(t, "text", "t1", 3000)).texts![0]).toMatchObject({ startMs: 3000, endMs: 4000 });
  });
});

describe("视图", () => {
  it("smartcutVideoClipAt：播放头下画面轨的那一段（接缝与两端不算）", () => {
    const t = tl();
    expect(smartcutVideoClipAt(t, 100)?.id).toBe("a");
    expect(smartcutVideoClipAt(t, 0)).toBeNull();
    expect(smartcutVideoClipAt(t, 3500)?.id).toBe("a");
  });

  it("smartcutVisibleRange：前后各多一屏", () => {
    // 40px/s：滚到 400px、视口 800px → [−400 夹到 0, 2000px) = [0, 50s)
    expect(smartcutVisibleRange(400, 800, 2)).toEqual({ fromMs: 0, toMs: 50000 });
    // 滚到 4000px：[3200px, 5600px) = [80s, 140s)
    expect(smartcutVisibleRange(4000, 800, 2)).toEqual({ fromMs: 80000, toMs: 140000 });
    expect(smartcutInRange(79000, 80000, { fromMs: 80000, toMs: 140000 })).toBe(true);
    expect(smartcutInRange(0, 79999, { fromMs: 80000, toMs: 140000 })).toBe(false);
  });

  it("smartcutFitZoom：能放下整条成片的最大档", () => {
    // 60 秒、视口 1000px：40px/s 是 2400px 放不下，10px/s 600px 放得下，20px/s 1200 放不下
    expect(smartcutFitZoom(60000, 1000, smartcutZoomLevels)).toBe(0);
    expect(smartcutFitZoom(10000, 1000, smartcutZoomLevels)).toBe(3);
    expect(smartcutFitZoom(0, 1000, smartcutZoomLevels)).toBe(0);
  });
});

describe("拖动落点与吸附", () => {
  const clips = [{ id: "a", startMs: 0, durationMs: 4000 }, { id: "b", startMs: 4000, durationMs: 2000 }, { id: "c", startMs: 6000, durationMs: 2000 }];

  it("smartcutReorderTarget：被拖的段不算在内，能放到最后一位", () => {
    // A 往右拖过 B 的中点（5000）但没过 C 的中点（7000）→ 第 2 位
    expect(smartcutReorderTarget(clips, "a", 5500)).toBe(2);
    // 过了 C 的中点 → 最后一位
    expect(smartcutReorderTarget(clips, "a", 7500)).toBe(3);
    // 往右拖一点点：自己的中点不算「被越过」，仍在第 1 位
    expect(smartcutReorderTarget(clips, "a", 2100)).toBe(1);
    // C 往左拖过 A 的中点 → 第 1 位
    expect(smartcutReorderTarget(clips, "c", 1500)).toBe(1);
  });

  it("smartcutInsertMarkMs：插入线落在那一位原来那段的起点，放到最后时在最后一段的结束", () => {
    expect(smartcutInsertMarkMs(clips, "a", 2)).toBe(6000);
    expect(smartcutInsertMarkMs(clips, "a", 3)).toBe(8000);
    expect(smartcutInsertMarkMs(clips, "c", 1)).toBe(0);
  });

  it("smartcutSnapSpan：起点与终点各找锚点，取移动量小的那端", () => {
    const snap = (anchors: number[]) => (ms: number) => anchors.find(a => Math.abs(a - ms) <= 100) ?? ms;
    expect(smartcutSnapSpan(1050, 500, snap([1000]))).toBe(1000);
    // 终点 1580 贴到 1600 → 起点 1100
    expect(smartcutSnapSpan(1080, 500, snap([1600]))).toBe(1100);
    // 两端都能贴：起点差 50、终点差 20 → 按终点
    expect(smartcutSnapSpan(1050, 500, snap([1000, 1570]))).toBe(1070);
    expect(smartcutSnapSpan(3000, 500, snap([1000]))).toBe(3000);
  });
});

describe("smartcutSetClipTransform", () => {
  it("smartcutSetClipTransform：设变换、重置；没变原样返回", async () => {
    const { smartcutSetClipTransform } = await import("./smartcutEdit");
    const t = tl();
    const moved = smartcutSetClipTransform(t, "a", { x: 10, y: -5, scale: 1.2 });
    expect(moved.tracks[0]!.clips[0]).toMatchObject({ transform: { x: 10, y: -5, scale: 1.2 }, origin: "user" });
    expect(smartcutSetClipTransform(moved, "a", { x: 10, y: -5, scale: 1.2 })).toBe(moved);
    expect(smartcutSetClipTransform(moved, "a", null).tracks[0]!.clips[0]).not.toHaveProperty("transform");
    expect(smartcutSetClipTransform(t, "a", null)).toBe(t);
  });
});

describe("smartcutAppendInputs", () => {
  it("smartcutAppendInputs：视频图片进画面轨（图片 3 秒），音频按用途进轨道末尾，时长未知的不铺", async () => {
    const { smartcutAppendInputs } = await import("./smartcutEdit");
    let n = 0;
    const id = () => `tmp-${++n}`;
    const t = tl();
    const { timeline, appended } = smartcutAppendInputs(t, [
      { kind: "video", status: "ready", used: false, mediaId: "v9", durationMs: 2500, sourceNodeId: "n9" },
      { kind: "image", status: "ready", used: false, mediaId: "i9", sourceNodeId: "n10" },
      { kind: "audio", status: "ready", used: false, mediaId: "a9", durationMs: 4000, audioRole: "music", sourceNodeId: "n11" },
      { kind: "audio", status: "ready", used: false, mediaId: "a10", durationMs: 2000, audioRole: "voice", sourceNodeId: "n12" },
      { kind: "video", status: "ready", used: false, mediaId: "v10", sourceNodeId: "n13" },
      { kind: "video", status: "ready", used: true, mediaId: "v1", durationMs: 1000, sourceNodeId: "n1" },
      { kind: "video", status: "pending", used: false, sourceNodeId: "n14" }
    ], id);
    expect(appended).toBe(4);
    expect(timeline.tracks[0]!.clips.slice(-2).map(c => [c.mediaId, c.sourceOutMs])).toEqual([["v9", 2500], ["i9", 3000]]);
    const music = timeline.tracks.find(tr => tr.role === "music")!;
    expect(music.clips.at(-1)).toMatchObject({ mediaId: "a9", startMs: 7000 });
    const voice = timeline.tracks.find(tr => tr.role === "voice")!;
    expect(voice.clips).toEqual([expect.objectContaining({ mediaId: "a10", startMs: 0 })]);
    expect(smartcutAppendInputs(t, [], id).timeline).toBe(t);
  });
});
