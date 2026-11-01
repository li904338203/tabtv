import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import type { components } from "@tabtv/api-client";
import { describe, expect, it } from "vitest";
import {
  smartcutApplyIdMap,
  smartcutFitTransitions,
  smartcutIsTempId,
  smartcutMapId,
  smartcutRemapSnapshot,
  smartcutRemapSnapshots,
  smartcutSetSpeed,
  smartcutSplitClip,
  smartcutTrimEdge,
  smartcutTrimEndAt,
  smartcutTrimStartAt,
  smartcutValidateContent,
  smartcutValidateFormat,
  type SmartcutEditableClip
} from "./smartcutContent";
import { smartcutPlayLen } from "./smartcutLayout";
import { smartcutAspectPresets } from "./smartcutTimeline";

type TimelineContent = components["schemas"]["TimelineContent"];
type ContentClip = components["schemas"]["TimelineContentClip"];

/**
 * 保存前校验与 Go 的 `ValidateContent` 读**同一批** fixture（SCT-03 产出，SC-N-15）。
 * ⚠️ 直接读仓库里的 `api/fixtures/smartcut/content/`，不复制一份 —— 复制的那份迟早和 Go 侧那份分叉。
 */
const FIXTURE_DIR = fileURLToPath(new URL("../../api/fixtures/smartcut/content/", import.meta.url));
const readFixture = (name: string): unknown => JSON.parse(readFileSync(join(FIXTURE_DIR, name), "utf8"));
const fixtureNames = (prefix: string) => readdirSync(FIXTURE_DIR).filter(name => name.startsWith(prefix) && name.endsWith(".json")).sort();

describe("保存前校验读 content fixture（与 Go 同一份规则）", () => {
  it("valid-*.json 全部通过", () => {
    const names = fixtureNames("valid-");
    expect(names.length).toBeGreaterThanOrEqual(4);
    for (const name of names) {
      expect({ name, error: smartcutValidateContent(readFixture(name)) }).toEqual({ name, error: null });
    }
  });

  it("invalid-*.json 逐个报出与 fixture 相同的字段路径", () => {
    const names = fixtureNames("invalid-");
    expect(names.length).toBeGreaterThanOrEqual(60);
    for (const name of names) {
      const fixture = readFixture(name) as { why: string; content: unknown; path: string };
      const error = smartcutValidateContent(fixture.content);
      expect({ name, why: fixture.why, path: error?.path }).toEqual({ name, why: fixture.why, path: fixture.path });
    }
  });

  // valid-full 的每个原样 JSON 字段都带 future* 未知字段：校验放行，且不改调用方手上的对象。
  it("未知字段放行，校验不改动传入的内容", () => {
    const content = readFixture("valid-full.json");
    const before = JSON.stringify(content);
    expect(smartcutValidateContent(content)).toBeNull();
    expect(JSON.stringify(content)).toBe(before);
    expect(before).toContain("future");
  });

  it("aspects.json：画幅表与 smartcutAspectPresets 逐项相等", () => {
    const { presets } = readFixture("aspects.json") as { presets: Array<{ aspectRatio: string; width: number; height: number }> };
    expect(smartcutAspectPresets.map(p => ({ aspectRatio: p.ratio, width: p.width, height: p.height }))).toEqual(presets);
  });

  it("aspects.json：画幅 + 帧率 + 宽高 → 期望 path（空串 = 合法）", () => {
    const { formats } = readFixture("aspects.json") as {
      formats: Array<{ aspectRatio: string; fps: number; width: number; height: number; path: string }>;
    };
    expect(formats.length).toBeGreaterThan(0);
    for (const f of formats) {
      const error = smartcutValidateFormat(f.aspectRatio, f.fps, f.width, f.height);
      expect({ ...f, path: error?.path ?? "" }).toEqual(f);
    }
  });
});

const baseContent = (): TimelineContent => ({
  version: 3,
  aspectRatio: "9:16",
  fps: 30,
  width: 1080,
  height: 1920,
  tracks: [
    { id: "1001", kind: "video", clips: [
      { id: "2001", mediaId: "77", sourceInMs: 0, sourceOutMs: 4000 },
      { id: "tmp-a", mediaId: "78", sourceInMs: 500, sourceOutMs: 4500, transitionIn: { kind: "dissolve", durationMs: 1000 } }
    ] },
    { id: "1002", kind: "audio", role: "music", clips: [{ id: "2002", mediaId: "88", startMs: 2500, sourceInMs: 0, sourceOutMs: 30000 }] },
    { id: "tmp-s", kind: "subtitle", lang: "zh-CN" }
  ],
  subtitles: [{ id: "tmp-9", trackId: "tmp-s", startMs: 0, endMs: 1800, text: "tmp-1", anchor: { clipId: "tmp-a", srcStartMs: 500, srcEndMs: 2300 } }],
  texts: []
});

describe("校验的是发出去的那份 JSON", () => {
  it("undefined 的键与缺省同义", () => {
    const content = baseContent();
    content.tracks[0]!.clips![0]!.speed = undefined;
    expect(smartcutValidateContent(content)).toBeNull();
  });

  // NaN 序列化成 null：必填的整数就是「必填」，可选的就是缺省 —— 与服务端看到的一致。
  it("NaN 变成 null：必填字段报必填", () => {
    const content = baseContent();
    content.tracks[0]!.clips![0]!.sourceOutMs = Number.NaN;
    expect(smartcutValidateContent(content)).toMatchObject({ path: "tracks[0].clips[0].sourceOutMs", reason: "必填" });
  });

  it("不是对象报 $", () => {
    expect(smartcutValidateContent(undefined)?.path).toBe("$");
    expect(smartcutValidateContent([baseContent()])?.path).toBe("$");
    expect(smartcutValidateContent("x")?.path).toBe("$");
  });

  it("码点数而不是 UTF-16 长度：64 个 emoji 的轨道名合法", () => {
    const content = baseContent();
    content.tracks[0]!.name = "🎬".repeat(64);
    expect(smartcutValidateContent(content)).toBeNull();
    content.tracks[0]!.name = "🎬".repeat(65);
    expect(smartcutValidateContent(content)?.path).toBe("tracks[0].name");
  });
});

// ---------------------------------------------------------------- 片段编辑

const clip = (over: Partial<SmartcutEditableClip> = {}): SmartcutEditableClip =>
  ({ id: "2001", sourceInMs: 1000, sourceOutMs: 9000, speed: 100, fadeInMs: 300, fadeOutMs: 400, origin: "ai", ...over });

const unwrap = <T,>(result: { ok: true; value: T } | { ok: false; reason: string }): T => {
  if (!result.ok) throw new Error(result.reason);
  return result.value;
};

describe("分割（SC-F-24）", () => {
  it("原速：切点按成片偏移换算，左不带淡出、右不带淡入，两半都标为用户改过", () => {
    const [left, right] = unwrap(smartcutSplitClip(clip(), 3000, "tmp-r"));
    expect(left).toMatchObject({ id: "2001", sourceInMs: 1000, sourceOutMs: 4000, fadeInMs: 300, fadeOutMs: 0, origin: "user" });
    expect(right).toMatchObject({ id: "tmp-r", sourceInMs: 4000, sourceOutMs: 9000, fadeInMs: 0, fadeOutMs: 400, origin: "user" });
  });

  // 2 倍速：成片里的 1 秒是素材里的 2 秒；0.5 倍速反过来。
  it("带变速：2× 与 0.5×", () => {
    const [fastLeft, fastRight] = unwrap(smartcutSplitClip(clip({ speed: 200 }), 1000, "tmp-r"));
    expect(fastLeft.sourceOutMs).toBe(3000);
    expect(fastRight.sourceInMs).toBe(3000);
    expect(smartcutPlayLen(fastLeft) + smartcutPlayLen(fastRight)).toBe(smartcutPlayLen(clip({ speed: 200 })));

    const [slowLeft, slowRight] = unwrap(smartcutSplitClip(clip({ speed: 50 }), 1000, "tmp-r"));
    expect(slowLeft.sourceOutMs).toBe(1500);
    expect(smartcutPlayLen(slowLeft)).toBe(1000);
    expect(slowRight.sourceInMs).toBe(1500);
  });

  it("切点太靠边（任一侧不足 100 毫秒）时拒绝并说明", () => {
    expect(smartcutSplitClip(clip(), 99, "tmp-r")).toEqual({ ok: false, reason: "切点太靠边：两侧都至少要有 100 毫秒" });
    expect(smartcutSplitClip(clip(), 7950, "tmp-r").ok).toBe(false);
    expect(smartcutSplitClip(clip(), 100, "tmp-r").ok).toBe(true);
  });

  // ⚠️ 与 Go 的 domain.Split 不同：右半段不继承转入（否则刀口上凭空多一次叠化），左半段不带转出。
  it("右半段不继承转入、溯源、备选与进行中的生成；左半段不带转出", () => {
    const alternates = [{ mediaId: "66", sourceInMs: 0, sourceOutMs: 8000, kind: "original" as const }];
    const original = { ...clip({
      transitionIn: { kind: "dissolve", durationMs: 500 },
      transitionOut: { kind: "fade", durationMs: 500 },
      sourceShotId: "33",
      pendingGenerationId: "44"
    }), alternates };
    const [left, right] = unwrap(smartcutSplitClip(original, 4000, "tmp-r"));
    expect(left.transitionIn).toEqual({ kind: "dissolve", durationMs: 500 });
    expect(left).not.toHaveProperty("transitionOut");
    expect(left).toMatchObject({ sourceShotId: "33", pendingGenerationId: "44", alternates });
    expect(right).not.toHaveProperty("transitionIn");
    expect(right).not.toHaveProperty("sourceShotId");
    expect(right).not.toHaveProperty("pendingGenerationId");
    expect(right).not.toHaveProperty("alternates");
    expect(right.transitionOut).toEqual({ kind: "fade", durationMs: 500 });
  });

  it("带起点与时长的片段（读回的形状）一起更新", () => {
    const [left, right] = unwrap(smartcutSplitClip(clip({ startMs: 2500, durationMs: 8000 }), 3000, "tmp-r"));
    expect(left).toMatchObject({ startMs: 2500, durationMs: 3000 });
    expect(right).toMatchObject({ startMs: 5500, durationMs: 5000 });
  });

  it("分割后的整份内容仍能通过保存前校验", () => {
    const content = baseContent();
    const video = content.tracks[0]!;
    const [left, right] = unwrap(smartcutSplitClip(video.clips![1]!, 1500, "tmp-b"));
    video.clips = smartcutFitTransitions([video.clips![0]!, left, right], "video").clips as ContentClip[];
    expect(smartcutValidateContent(content)).toBeNull();
  });
});

describe("删左 / 删右（带变速）", () => {
  it("删左：0.5× 下成片 1 秒是素材 0.5 秒；按起点放置的片段右边缘不动", () => {
    const next = unwrap(smartcutTrimStartAt(clip({ speed: 50, startMs: 1000 }), 1000));
    expect(next.sourceInMs).toBe(1500);
    expect(next.startMs! + smartcutPlayLen(next)).toBe(1000 + smartcutPlayLen(clip({ speed: 50 })));
    expect(next.origin).toBe("user");
  });

  it("删左：2× 下成片 1 秒是素材 2 秒", () => {
    expect(unwrap(smartcutTrimStartAt(clip({ speed: 200 }), 1000)).sourceInMs).toBe(3000);
  });

  it("删右：2× 与 0.5×，起点不动", () => {
    expect(unwrap(smartcutTrimEndAt(clip({ speed: 200, startMs: 700 }), 1000))).toMatchObject({ sourceOutMs: 3000, startMs: 700 });
    expect(unwrap(smartcutTrimEndAt(clip({ speed: 50 }), 1000)).sourceOutMs).toBe(1500);
  });

  it("剩下不足 100 毫秒、播放头不在片段里时拒绝", () => {
    expect(smartcutTrimStartAt(clip(), 7950).ok).toBe(false);
    expect(smartcutTrimEndAt(clip(), 50).ok).toBe(false);
    expect(smartcutTrimStartAt(clip(), 0)).toEqual({ ok: false, reason: "播放头不在这个片段里" });
    expect(smartcutTrimEndAt(clip(), 8000).ok).toBe(false);
  });

  it("变短后淡入淡出收进新长度", () => {
    const next = unwrap(smartcutTrimEndAt(clip({ fadeInMs: 800, fadeOutMs: 800 }), 1000));
    expect((next.fadeInMs ?? 0) + (next.fadeOutMs ?? 0)).toBeLessThanOrEqual(smartcutPlayLen(next));
  });
});

describe("裁切把手与变速", () => {
  it("拖右端：夹在素材时长之内，且成片不短于 100 毫秒", () => {
    expect(smartcutTrimEdge(clip(), "end", 5000, { sourceDurationMs: 10000 }).sourceOutMs).toBe(10000);
    expect(smartcutTrimEdge(clip(), "end", -20000).sourceOutMs).toBe(1100);
    expect(smartcutTrimEdge(clip({ speed: 200 }), "end", -20000).sourceOutMs).toBe(1200);
  });

  it("拖左端：按起点放置的片段右边缘不动、左边缘不越过 0 点；画面轨不改起点", () => {
    const placed = smartcutTrimEdge(clip({ startMs: 300 }), "start", -5000, { placed: true });
    expect(placed.sourceInMs).toBe(700);
    expect(placed.startMs).toBe(0);
    expect(placed.startMs! + smartcutPlayLen(placed)).toBe(300 + 8000);
    const video = smartcutTrimEdge(clip({ startMs: 300 }), "start", -5000);
    expect(video).toMatchObject({ sourceInMs: 0, startMs: 300 });
  });

  it("没拖动时原样返回（同一个引用，不压撤销步）", () => {
    const c = clip({ sourceInMs: 0 });
    expect(smartcutTrimEdge(c, "start", -100)).toBe(c);
  });

  it("变速档位：长度跟着变，不足 100 毫秒或越界时拒绝", () => {
    expect(smartcutPlayLen(unwrap(smartcutSetSpeed(clip(), 200)))).toBe(4000);
    expect(smartcutSetSpeed(clip({ sourceInMs: 0, sourceOutMs: 150 }), 200).ok).toBe(false);
    expect(smartcutSetSpeed(clip(), 500)).toEqual({ ok: false, reason: "变速只支持 0.25~4 倍" });
  });
});

describe("转场收进合法范围", () => {
  it("画面轨：按本段与上一段各自的一半收；收到 100 以下就去掉；说出改了哪几段", () => {
    const clips = [
      clip({ id: "a", sourceInMs: 0, sourceOutMs: 600 }),
      clip({ id: "b", sourceInMs: 0, sourceOutMs: 4000, transitionIn: { kind: "dissolve", durationMs: 1000 } }),
      clip({ id: "c", sourceInMs: 0, sourceOutMs: 150, transitionIn: { kind: "fade", durationMs: 500 }, transitionOut: { kind: "fade", durationMs: 500 } })
    ];
    const { clips: fitted, changed } = smartcutFitTransitions(clips, "video");
    expect(fitted[1]!.transitionIn).toEqual({ kind: "dissolve", durationMs: 300 });
    expect(fitted[2]).not.toHaveProperty("transitionIn");
    expect(fitted[2]).not.toHaveProperty("transitionOut");
    expect(changed).toEqual(["b", "c"]);
    expect(fitted[0]).toBe(clips[0]);
  });

  it("第一段的转入只看本段；合法的不动", () => {
    const first = clip({ id: "a", sourceInMs: 0, sourceOutMs: 1000, transitionIn: { kind: "fade", durationMs: 800 } });
    expect(smartcutFitTransitions([first], "video").clips[0]!.transitionIn).toEqual({ kind: "fade", durationMs: 500 });
    const ok = clip({ id: "b", transitionIn: { kind: "fade", durationMs: 300 } });
    expect(smartcutFitTransitions([ok], "video")).toEqual({ clips: [ok], changed: [] });
  });
});

// ---------------------------------------------------------------- 临时 ID

describe("idMap 改写（SCD-43）", () => {
  // 「tmp-1」也在表里：只按已知字段改的话，字幕文字里的「tmp-1」不会被换掉；全文替换就会。
  const idMap = { "tmp-a": "5001", "tmp-s": "5002", "tmp-9": "5003", "tmp-1": "5004" };

  it("轨道、片段、字幕、字幕所属轨与锚点一起换；字幕文字里的 tmp-1 不动", () => {
    const content = baseContent();
    const mapped = smartcutApplyIdMap(content, idMap);
    expect(mapped.tracks[0]!.clips![1]!.id).toBe("5001");
    expect(mapped.tracks[2]!.id).toBe("5002");
    expect(mapped.subtitles![0]).toMatchObject({ id: "5003", trackId: "5002", text: "tmp-1", anchor: { clipId: "5001", srcStartMs: 500 } });
    // 没命中的部分保持原引用，传入的对象不被改动。
    expect(mapped.tracks[1]).toBe(content.tracks[1]);
    expect(content.tracks[0]!.clips![1]!.id).toBe("tmp-a");
  });

  it("什么都没换时返回原对象", () => {
    const content = baseContent();
    expect(smartcutApplyIdMap(content, { "tmp-zzz": "1" })).toBe(content);
    expect(smartcutApplyIdMap(content, {})).toBe(content);
  });

  it("撤销栈里的快照一起换；解析不了的、没命中的原样返回", () => {
    const snapshot = JSON.stringify(baseContent());
    const [mapped, broken, untouched] = smartcutRemapSnapshots([snapshot, "{oops", JSON.stringify({ tracks: [] })], idMap);
    const parsed = JSON.parse(mapped!) as TimelineContent;
    expect(parsed.tracks[0]!.clips![1]!.id).toBe("5001");
    expect(parsed.subtitles![0]!.anchor!.clipId).toBe("5001");
    expect(broken).toBe("{oops");
    expect(untouched).toBe(JSON.stringify({ tracks: [] }));
    expect(smartcutRemapSnapshot(snapshot, {})).toBe(snapshot);
  });

  it("单个 ID 与临时 ID 判断", () => {
    expect(smartcutMapId("tmp-a", idMap)).toBe("5001");
    expect(smartcutMapId("2001", idMap)).toBe("2001");
    expect(smartcutMapId("toString", idMap)).toBe("toString");
    expect(smartcutIsTempId("tmp-a")).toBe(true);
    expect(smartcutIsTempId("2001")).toBe(false);
  });
});
