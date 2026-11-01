import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import { smartcutValidateContent } from "./smartcutContent";
import {
  smartcutAnchoredSpan,
  smartcutFirstOverlap,
  smartcutLayout,
  smartcutOverlapIndex,
  smartcutPlayLen,
  smartcutSequence,
  smartcutTrackEnd,
  smartcutTransitionActive,
  type SmartcutLayout,
  type SmartcutLayoutClip,
  type SmartcutLayoutInput,
  type SmartcutPlacedSpan
} from "./smartcutLayout";

/**
 * 成片时间与 Go 的 `timeline/domain.Layout` 读**同一批** fixture（SCT-04 产出，SC-N-15）。
 * ⚠️ 直接读仓库里的 `api/fixtures/smartcut/layout/`，不复制一份。期望值是 fixture 里手算写死的，不拿任一侧的实现去算。
 */
const LAYOUT_DIR = fileURLToPath(new URL("../../api/fixtures/smartcut/layout/", import.meta.url));

/**
 * 换成 Go `LayoutResult` 的 JSON 形状：片段比起点与时长（跟随片段的音频另带 `follow`），`hidden` 只在为 true 时出现
 * （`omitempty`）。
 */
function asGoLayout(layout: SmartcutLayout) {
  const spans = (record: Record<string, SmartcutPlacedSpan>) => Object.fromEntries(Object.entries(record)
    .map(([id, s]) => [id, s.hidden ? { startMs: s.startMs, endMs: s.endMs, hidden: true } : { startMs: s.startMs, endMs: s.endMs }]));
  return {
    clips: Object.fromEntries(Object.entries(layout.clips).map(([id, c]) => [id, c.follow
      ? { startMs: c.startMs, durationMs: c.durationMs, follow: c.follow.hidden
        ? { sourceInMs: c.follow.sourceInMs, sourceOutMs: c.follow.sourceOutMs, speed: c.follow.speed, hidden: true }
        : { sourceInMs: c.follow.sourceInMs, sourceOutMs: c.follow.sourceOutMs, speed: c.follow.speed } }
      : { startMs: c.startMs, durationMs: c.durationMs }])),
    subtitles: spans(layout.subtitles),
    texts: spans(layout.texts),
    totalMs: layout.totalMs,
    effectiveMs: layout.effectiveMs
  };
}

describe("成片时间读 layout fixture（与 Go 同一份规则）", () => {
  const names = readdirSync(LAYOUT_DIR).filter(name => name.endsWith(".json")).sort();

  it("fixture 都在", () => {
    expect(names.length).toBeGreaterThanOrEqual(9);
  });

  for (const name of names) {
    it(name, () => {
      const fixture = JSON.parse(readFileSync(join(LAYOUT_DIR, name), "utf8")) as { why: string; content: SmartcutLayoutInput; expect: unknown };
      // fixture 的内容本身必须合法（与 Go 测试一样先过保存前校验）
      expect(smartcutValidateContent(fixture.content)).toBeNull();
      expect({ why: fixture.why, layout: asGoLayout(smartcutLayout(fixture.content)) }).toEqual({ why: fixture.why, layout: fixture.expect });
    });
  }
});

function clip(id: string, ms: number, extra: Partial<SmartcutLayoutClip> = {}): SmartcutLayoutClip {
  return { id, sourceInMs: 0, sourceOutMs: ms, speed: 100, ...extra };
}
const dissolve = (durationMs: number) => ({ kind: "dissolve" as const, durationMs });

describe("播放长度与转场是否生效（与 Go 的 PlayLen / IsActive 一致）", () => {
  it("变速换算取整同 Go：截断", () => {
    expect(smartcutPlayLen({ sourceInMs: 0, sourceOutMs: 10000, speed: 200 })).toBe(5000);
    expect(smartcutPlayLen({ sourceInMs: 0, sourceOutMs: 1000, speed: 75 })).toBe(1333);
    expect(smartcutPlayLen({ sourceInMs: 500, sourceOutMs: 1500, speed: 50 })).toBe(2000);
    // 变速缺省或非正数按原速，不除零
    expect(smartcutPlayLen({ sourceInMs: 0, sourceOutMs: 1000 })).toBe(1000);
    expect(smartcutPlayLen({ sourceInMs: 0, sourceOutMs: 1000, speed: 0 })).toBe(1000);
  });

  it("none 或 0 毫秒都是没有转场", () => {
    expect(smartcutTransitionActive({ kind: "dissolve", durationMs: 500 })).toBe(true);
    expect(smartcutTransitionActive({ kind: "none", durationMs: 500 })).toBe(false);
    expect(smartcutTransitionActive({ kind: "fade", durationMs: 0 })).toBe(false);
    expect(smartcutTransitionActive(undefined)).toBe(false);
  });
});

describe("画面轨的成片时间（SCK-15）", () => {
  it("无转场：首尾相接", () => {
    const seq = smartcutSequence([clip("a", 4000), clip("b", 2500), clip("c", 1000)]);
    expect(seq.map(s => s.startMs)).toEqual([0, 4000, 6500]);
    expect(seq.map(s => s.overlapMs)).toEqual([0, 0, 0]);
  });

  // SC-AC-11：三段各 4 秒，第 2、3 段各 1 秒叠化 → 第 3 段成片起点 6 秒、总长 10 秒。
  it("SC-AC-11：相邻转场扣掉重叠", () => {
    const layout = smartcutLayout({
      tracks: [{ id: "v", kind: "video", clips: [
        clip("a", 4000),
        clip("b", 4000, { transitionIn: dissolve(1000) }),
        clip("c", 4000, { transitionIn: dissolve(1000) })
      ] }]
    });
    expect(layout.clips.a!.startMs).toBe(0);
    expect(layout.clips.b!.startMs).toBe(3000);
    expect(layout.clips.c!.startMs).toBe(6000);
    expect(layout.totalMs).toBe(12000);
    expect(layout.effectiveMs).toBe(10000);
  });

  // ⚠️ 第一段的转入是「从黑场淡入」，不与任何东西重叠 —— 扣掉的话整条片子往前错 0.5 秒。
  it("首段转场不扣", () => {
    const layout = smartcutLayout({
      tracks: [{ id: "v", kind: "video", clips: [clip("a", 4000, { transitionIn: { kind: "fade", durationMs: 500 } }), clip("b", 4000)] }]
    });
    expect(layout.clips.a!.startMs).toBe(0);
    expect(layout.clips.b!.startMs).toBe(4000);
    expect(layout.effectiveMs).toBe(8000);
    expect(layout.clips.a!.overlapMs).toBe(0);
  });

  it("变速后的片段按播放长度排", () => {
    const layout = smartcutLayout({
      tracks: [{ id: "v", kind: "video", clips: [clip("a", 4000, { speed: 200 }), clip("b", 1000, { speed: 50 })] }]
    });
    expect(layout.clips.a!.durationMs).toBe(2000);
    expect(layout.clips.b).toMatchObject({ startMs: 2000, durationMs: 2000 });
    expect(layout.totalMs).toBe(4000);
  });

  // 画面轨片段在整份保存里不传 startMs（传了也忽略），这里也一样不采信。
  it("画面轨不采信存储的起点", () => {
    const layout = smartcutLayout({ tracks: [{ id: "v", kind: "video", clips: [clip("a", 1000, { startMs: 9999 })] }] });
    expect(layout.clips.a!.startMs).toBe(0);
  });

  it("空时间线：总长 0", () => {
    expect(smartcutLayout({ tracks: [{ id: "v", kind: "video", clips: [] }] })).toMatchObject({ totalMs: 0, effectiveMs: 0 });
    expect(smartcutLayout({ tracks: [] })).toMatchObject({ totalMs: 0, effectiveMs: 0 });
  });
});

describe("按起点放置的轨道（SCD-15）", () => {
  // SC-AC-12：配乐放在 2.5 秒，画面轨怎么改都不动它。
  it("音频轨取存储的成片起点，不首尾相接", () => {
    const input: SmartcutLayoutInput = {
      tracks: [
        { id: "v", kind: "video", clips: [clip("a", 4000), clip("b", 4000)] },
        { id: "m", kind: "audio", clips: [clip("music", 30000, { startMs: 2500 }), clip("sfx", 1000, { startMs: 500, speed: 200 })] }
      ]
    };
    const layout = smartcutLayout(input);
    expect(layout.clips.music).toEqual({ trackId: "m", startMs: 2500, durationMs: 30000, overlapMs: 0 });
    expect(layout.clips.sfx).toEqual({ trackId: "m", startMs: 500, durationMs: 500, overlapMs: 0 });
    // 总长只看画面轨：配乐比画面长也不拉长成片（渲染侧 apad + atrim 到画面总长）。
    expect(layout.effectiveMs).toBe(8000);
  });

  it("手工字幕与文字层按存储值，不跟随", () => {
    const layout = smartcutLayout({
      tracks: [{ id: "v", kind: "video", clips: [clip("a", 4000)] }],
      subtitles: [{ id: "s1", startMs: 100, endMs: 900 }],
      texts: [{ id: "t1", startMs: 0, endMs: 3000 }]
    });
    expect(layout.subtitles.s1).toEqual({ startMs: 100, endMs: 900, hidden: false });
    expect(layout.texts.t1).toEqual({ startMs: 0, endMs: 3000, hidden: false });
  });
});

describe("跟随片段的字幕（SC-F-56、SC-AC-19）", () => {
  const anchored = (id: string, srcStartMs: number, srcEndMs: number, clipId = "b") =>
    ({ id, startMs: 0, endMs: 1, anchor: { clipId, srcStartMs, srcEndMs } });

  const timeline = (b: Partial<SmartcutLayoutClip>): SmartcutLayoutInput => ({
    tracks: [{ id: "v", kind: "video", clips: [clip("a", 2000), { ...clip("b", 6000), ...b }] }],
    subtitles: [anchored("s1", 0, 800), anchored("s2", 2000, 3000), anchored("s3", 500, 1500)]
  });

  it("没裁时按源时间落在片段上", () => {
    const layout = smartcutLayout(timeline({}));
    expect(layout.subtitles.s1).toEqual({ startMs: 2000, endMs: 2800, hidden: false });
    expect(layout.subtitles.s2).toEqual({ startMs: 4000, endMs: 5000, hidden: false });
  });

  // 裁掉开头 1 秒：落在裁掉区间里的隐藏（不删），其余整体前移 1 秒；跨在切口上的只显示剩下那截。
  it("被裁掉开头 1 秒：整体前移，裁掉区间里的隐藏", () => {
    const layout = smartcutLayout(timeline({ sourceInMs: 1000 }));
    expect(layout.subtitles.s1!.hidden).toBe(true);
    expect(layout.subtitles.s2).toEqual({ startMs: 3000, endMs: 4000, hidden: false });
    expect(layout.subtitles.s3).toEqual({ startMs: 2000, endMs: 2500, hidden: false });
  });

  it("区间恢复后重新出现", () => {
    const trimmed = smartcutLayout(timeline({ sourceInMs: 1000 }));
    const restored = smartcutLayout(timeline({ sourceInMs: 0 }));
    expect(trimmed.subtitles.s1!.hidden).toBe(true);
    expect(restored.subtitles.s1).toEqual({ startMs: 2000, endMs: 2800, hidden: false });
  });

  it("变速换算：2 倍速时源里的 1 秒只占 0.5 秒", () => {
    const layout = smartcutLayout(timeline({ speed: 200 }));
    expect(layout.subtitles.s2).toEqual({ startMs: 3000, endMs: 3500, hidden: false });
  });

  it("裁掉结尾也同样隐藏", () => {
    const layout = smartcutLayout(timeline({ sourceOutMs: 1800 }));
    expect(layout.subtitles.s2!.hidden).toBe(true);
    expect(layout.subtitles.s1!.hidden).toBe(false);
  });

  it("前面有转场时跟着片段的成片起点走", () => {
    const layout = smartcutLayout(timeline({ transitionIn: dissolve(500) }));
    expect(layout.subtitles.s2).toEqual({ startMs: 3500, endMs: 4500, hidden: false });
  });

  // 锚定的片段被删了：按存储值，不隐藏（字幕还在，只是不再跟随）。锚点本身不成立同理。
  it("锚定片段不在了、锚点不成立：按存储值", () => {
    const layout = smartcutLayout({
      tracks: [{ id: "v", kind: "video", clips: [clip("a", 2000)] }],
      subtitles: [
        { id: "gone", startMs: 300, endMs: 700, anchor: { clipId: "deleted", srcStartMs: 0, srcEndMs: 400 } },
        { id: "bad", startMs: 100, endMs: 200, anchor: { clipId: "a", srcStartMs: 500, srcEndMs: 500 } }
      ]
    });
    expect(layout.subtitles.gone).toEqual({ startMs: 300, endMs: 700, hidden: false });
    expect(layout.subtitles.bad).toEqual({ startMs: 100, endMs: 200, hidden: false });
  });

  // 口播配音在音频轨上，识别出来的字幕跟着音频片段走。
  it("也能跟随音频轨上的片段", () => {
    const layout = smartcutLayout({
      tracks: [{ id: "voice", kind: "audio", clips: [clip("vo", 10000, { startMs: 1500, sourceInMs: 2000 })] }],
      subtitles: [anchored("s", 3000, 4000, "vo")]
    });
    expect(layout.subtitles.s).toEqual({ startMs: 2500, endMs: 3500, hidden: false });
  });

  it("smartcutAnchoredSpan 单独可用：没有交集返回 null", () => {
    expect(smartcutAnchoredSpan({ clipId: "x", srcStartMs: 0, srcEndMs: 500 }, { sourceInMs: 500, sourceOutMs: 900 }, 0)).toBeNull();
    expect(smartcutAnchoredSpan({ clipId: "x", srcStartMs: 400, srcEndMs: 800 }, { sourceInMs: 500, sourceOutMs: 900 }, 100))
      .toEqual({ startMs: 100, endMs: 400 });
  });

  // 大半句已经被裁掉：把整句文字留在剩下的一小截上只会一闪而过，所以不到一半就隐藏；恰好一半还显示。
  it("交集不到原区间的一半也隐藏，恰好一半照常显示", () => {
    const clipRange = { sourceInMs: 1000, sourceOutMs: 6000 };
    expect(smartcutAnchoredSpan({ clipId: "x", srcStartMs: 400, srcEndMs: 1400 }, clipRange, 0)).toBeNull();
    expect(smartcutAnchoredSpan({ clipId: "x", srcStartMs: 500, srcEndMs: 1500 }, clipRange, 0)).toEqual({ startMs: 0, endMs: 500 });
  });
});

describe("按起点放置的轨道：重叠与末尾（SCD-15）", () => {
  it("相邻比较，报按（起点，下标）排序后的后一个；首尾相接不算", () => {
    expect(smartcutFirstOverlap([{ start: 0, end: 1000, index: 0 }, { start: 1000, end: 2000, index: 1 }])).toBe(-1);
    expect(smartcutFirstOverlap([{ start: 500, end: 1500, index: 0 }, { start: 0, end: 1000, index: 1 }])).toBe(0);
    expect(smartcutFirstOverlap([{ start: 0, end: 10000, index: 0 }, { start: 3000, end: 3500, index: 1 }, { start: 1000, end: 2000, index: 2 }])).toBe(2);
    expect(smartcutFirstOverlap([])).toBe(-1);
  });

  it("音频片段按变速后的长度判重叠；追加默认放在轨道末尾", () => {
    const clips = [clip("m1", 4000, { startMs: 0, speed: 200 }), clip("m2", 1000, { startMs: 2000 })];
    expect(smartcutOverlapIndex(clips)).toBe(-1);
    expect(smartcutOverlapIndex([clips[0]!, { ...clips[1]!, startMs: 1999 }])).toBe(1);
    expect(smartcutTrackEnd(clips)).toBe(3000);
    expect(smartcutTrackEnd([])).toBe(0);
  });
});
