import { describe, expect, it } from "vitest";
import type { components } from "@tabtv/api-client";
import { smartcutValidateContent } from "./smartcutContent";
import { smartcutContentOf, smartcutRelayout, smartcutSameContent, smartcutTempId } from "./smartcutModel";

type Timeline = components["schemas"]["Timeline"];

function base(): Timeline {
  return {
    id: "1", projectId: "2", name: "剪辑", version: 7, aspectRatio: "9:16", fps: 30, width: 1080, height: 1920, status: "editing",
    tracks: [
      {
        id: "11", kind: "video", idx: 1, isMuted: false, volume: 100, role: null, style: null, clips: [
          { id: "21", idx: 1, mediaId: "77", sourceInMs: 1000, sourceOutMs: 5000, startMs: 0, durationMs: 4000, speed: 100, volume: 100,
            mediaUrl: "/api/v1/media/77", pendingGenerationId: "99", sourceNodeId: null, origin: "sync",
            transform: { scale: 1.2, futureKnob: 3 } as never },
          { id: "22", idx: 2, mediaId: "78", sourceInMs: 0, sourceOutMs: 3000, startMs: 4000, durationMs: 3000, speed: 200, volume: 80,
            transitionIn: { kind: "fade", durationMs: 500 } }
        ]
      },
      { id: "12", kind: "audio", idx: 2, isMuted: true, volume: 60, role: "music", style: { duck: { enabled: true, depthDb: 12 } }, clips: [
        { id: "31", idx: 1, mediaId: "88", sourceInMs: 0, sourceOutMs: 6000, startMs: 500, durationMs: 6000, speed: 100, volume: 100 }
      ] },
      { id: "13", kind: "subtitle", idx: 3, isMuted: false, volume: 100, lang: "zh-CN", clips: [] }
    ],
    subtitles: [
      { id: "41", startMs: 0, endMs: 1500, text: "跟着第一段", anchor: { clipId: "21", srcStartMs: 1000, srcEndMs: 2500 }, trackId: "13" },
      { id: "42", startMs: 2000, endMs: 3000, text: "旧数据没有 trackId" }
    ],
    texts: [{ id: "51", trackId: "61", startMs: 0, endMs: 1000, text: "标题", style: { sizePct: 6, futureGlow: true }, box: { xPct: 50, yPct: 20 } }],
    totalMs: 5500, effectiveMs: 5000
  };
}

describe("smartcutContentOf", () => {
  it("smartcutContentOf：不发服务端算的字段，画面轨不发落点，其它轨发放置位置", () => {
    const content = smartcutContentOf(base());
    const video = content.tracks[0]!;
    expect(video).not.toHaveProperty("role");
    expect(video).not.toHaveProperty("style");
    expect(video.clips![0]).toEqual({
      id: "21", mediaId: "77", sourceInMs: 1000, sourceOutMs: 5000, speed: 100, volume: 100, origin: "sync",
      transform: { scale: 1.2, futureKnob: 3 }
    });
    expect(video.clips![1]).toMatchObject({ transitionIn: { kind: "fade", durationMs: 500 } });
    expect(video.clips![1]).not.toHaveProperty("startMs");
    const audio = content.tracks[1]!;
    expect(audio).toMatchObject({ role: "music", isMuted: true, volume: 60, style: { duck: { enabled: true, depthDb: 12 } } });
    expect(audio.clips![0]).toMatchObject({ startMs: 500 });
  });

  it("smartcutContentOf：旧字幕挂到第一条字幕轨；未知字段原样保留；结果通过保存前校验", () => {
    const content = smartcutContentOf(base());
    expect(content.subtitles![1]).toMatchObject({ id: "42", trackId: "13" });
    expect(content.texts![0]!.style).toEqual({ sizePct: 6, futureGlow: true });
    // 文字层的轨道 61 不存在：校验会报出来 —— 换成存在的文字轨后通过
    const fixed = { ...content, tracks: [...content.tracks, { id: "61", kind: "text" as const, isMuted: false, volume: 100, clips: [] }] };
    expect(smartcutValidateContent(fixed)).toBeNull();
  });
});

describe("smartcutRelayout", () => {
  it("smartcutRelayout：重算画面轨落点、总长与跟随片段的字幕", () => {
    const t = base();
    // 把第一段的取用起点往后挪 500ms：后一段跟着前移，跟随的字幕整体前移 500ms 并被截掉开头
    t.tracks[0]!.clips[0] = { ...t.tracks[0]!.clips[0]!, sourceInMs: 1500 };
    const next = smartcutRelayout(t);
    const [a, b] = next.tracks[0]!.clips;
    expect(a).toMatchObject({ idx: 1, startMs: 0, durationMs: 3500 });
    // 2 倍速的 3 秒素材占 1.5 秒；转场重叠 500ms
    expect(b).toMatchObject({ idx: 2, startMs: 3000, durationMs: 1500 });
    expect(next.totalMs).toBe(5000);
    expect(next.effectiveMs).toBe(4500);
    expect(next.subtitles[0]).toMatchObject({ startMs: 0, endMs: 1000, hidden: false });
    // 音频轨的放置位置不动
    expect(next.tracks[1]!.clips[0]).toMatchObject({ startMs: 500, durationMs: 6000 });
  });

  it("smartcutRelayout：跟随的区间大半被裁掉时隐藏，起止保持现值", () => {
    const t = base();
    t.tracks[0]!.clips[0] = { ...t.tracks[0]!.clips[0]!, sourceInMs: 2200 };
    const next = smartcutRelayout(t);
    expect(next.subtitles[0]).toMatchObject({ startMs: 0, endMs: 1500, hidden: true });
  });
});

describe("smartcutSameContent / smartcutTempId", () => {
  it("smartcutSameContent：只比会发给服务端的内容，不比算出来的落点与版本", () => {
    const a = base();
    const b = { ...base(), version: 9, totalMs: 1, effectiveMs: 1 };
    expect(smartcutSameContent(a, b)).toBe(true);
    const c = base();
    c.tracks[0]!.clips[0] = { ...c.tracks[0]!.clips[0]!, volume: 10 };
    expect(smartcutSameContent(a, c)).toBe(false);
  });

  it("smartcutTempId：符合服务端的临时 ID 规则且不重复", () => {
    const ids = new Set(Array.from({ length: 500 }, () => smartcutTempId()));
    expect(ids.size).toBe(500);
    for (const id of ids) expect(id).toMatch(/^tmp-[A-Za-z0-9_-]{1,40}$/);
  });
});
