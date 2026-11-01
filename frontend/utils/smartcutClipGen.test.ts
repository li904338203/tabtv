import { describe, expect, it } from "vitest";
import type { components } from "@tabtv/api-client";
import {
  smartcutClaimGenerationFrame,
  smartcutClipQuoteText,
  smartcutClipToolKey,
  smartcutClipToolReason,
  smartcutFailedClipIds,
  smartcutFillAfter,
  smartcutGenerationOutputKind,
  smartcutGenerationStatusText,
  smartcutMusicDuration,
  smartcutPlaceGenerated,
  smartcutSfxRangeReason,
  type SmartcutClipToolContext
} from "./smartcutClipGen";

type Generation = components["schemas"]["SmartcutGeneration"];
type Timeline = components["schemas"]["Timeline"];
type TimelineClip = components["schemas"]["TimelineClip"];

const kling = { name: "可灵视频配音效", minSec: 3, maxSec: 20 };
const base: SmartcutClipToolContext = { editable: true, disabledReason: "", trackKind: "video", mediaKind: "video", clipMs: 8000, models: [kling] };

describe("smartcutClipToolReason：工具能不能用、为什么", () => {
  it("只读排在最前（给最根本的那一条）", () => {
    expect(smartcutClipToolReason("desubtitle", { ...base, editable: false, disabledReason: "你在该工作区是只读" })).toBe("你在该工作区是只读");
    expect(smartcutClipToolReason("regenerate", { ...base, editable: false, disabledReason: "", pending: true })).toBe("现在不能编辑");
  });

  it("上传的素材没有生成信息：重拍置灰并说明，去字幕与补镜照常可用", () => {
    const upload = { ...base, origin: { kind: "upload" as const, canRegenerate: false, reason: "这是上传的素材，没有生成信息：重新生成只能用于生成出来的素材" } };
    expect(smartcutClipToolReason("regenerate", upload)).toContain("没有生成信息");
    expect(smartcutClipToolReason("desubtitle", upload)).toBe("");
    expect(smartcutClipToolReason("fill", { editable: true, disabledReason: "", models: [kling] })).toBe("");
    // 生成信息还没取回来时说在读取，而不是按钮可点、点下去 42217
    expect(smartcutClipToolReason("regenerate", { ...base, origin: undefined })).toContain("正在读取");
    expect(smartcutClipToolReason("regenerate", { ...base, origin: { kind: "generation", canRegenerate: true } })).toBe("");
  });

  it("这一段正在生成时不能再重拍或去字幕；图片与音频片段不能去字幕或配音效", () => {
    expect(smartcutClipToolReason("regenerate", { ...base, pending: true, origin: { kind: "generation", canRegenerate: true } })).toContain("正在生成");
    expect(smartcutClipToolReason("desubtitle", { ...base, mediaKind: "image" })).toContain("图片没有字幕");
    expect(smartcutClipToolReason("desubtitle", { ...base, trackKind: "audio", mediaKind: "audio" })).toContain("画面轨");
    expect(smartcutClipToolReason("sfx", { ...base, mediaKind: "image" })).toContain("只有视频片段");
  });

  it("SC-AC-57：音效只收模型范围里的片段（按毫秒判）：25 秒置灰并说清，8 秒可用", () => {
    expect(smartcutClipToolReason("sfx", { ...base, clipMs: 25000, model: kling })).toBe("「可灵视频配音效」只收 3~20 秒的片段，这一段取用了 25.0 秒：先把它裁到这个范围里");
    expect(smartcutClipToolReason("sfx", { ...base, clipMs: 8000, model: kling })).toBe("");
    // 2.6 秒按秒向上取整是 3，按毫秒判才拦得住
    expect(smartcutSfxRangeReason(2600, kling)).toContain("2.6 秒");
    expect(smartcutSfxRangeReason(20000, kling)).toBe("");
    expect(smartcutSfxRangeReason(25000, null)).toBe("");
  });

  it("没有可用的模型时说清楚", () => {
    expect(smartcutClipToolReason("music", { editable: true, disabledReason: "", models: [] })).toContain("没有可用的模型");
    expect(smartcutClipToolReason("fill", { editable: true, disabledReason: "", models: [] })).toContain("没有可用的视频模型");
  });
});

describe("smartcutMusicDuration：时长缺省取从播放头到成片结尾，按模型的单次范围截", () => {
  const volc = { name: "豆包音乐", minSec: 30, maxSec: 60 };
  const eleven = { name: "ElevenLabs Music", minSec: 3, maxSec: 300 };
  it("成片 9 秒、播放头 1.5 秒、豆包最短 30 秒：按 30 秒生成计费，放进时间线时裁到 7.5 秒（说出来）", () => {
    expect(smartcutMusicDuration(9000, 1500, null, volc)).toEqual({ sec: 30, wantMs: 7500, landMs: 7500,
      note: "「豆包音乐」单次最短 30 秒：按 30 秒生成并计费，放进时间线时裁到 7.5 秒" });
  });
  it("播放头在 0 就是成片时长；ElevenLabs 按向上取整的秒数生成、落下去按毫秒收在结尾，不额外说明", () => {
    expect(smartcutMusicDuration(8200, 0, null, eleven)).toEqual({ sec: 9, wantMs: 8200, landMs: 8200, note: "" });
  });
  it("播放头在结尾或之后：取成片时长；离结尾不到 3 秒：夹到 3 秒", () => {
    expect(smartcutMusicDuration(9000, 9000, null, eleven).wantMs).toBe(9000);
    expect(smartcutMusicDuration(9000, 12000, null, eleven).wantMs).toBe(9000);
    expect(smartcutMusicDuration(9000, 8000, null, eleven).wantMs).toBe(3000);
  });
  it("想要 90 秒、豆包最长 60 秒：按 60 秒生成；ElevenLabs 照想要的", () => {
    expect(smartcutMusicDuration(0, 0, 90, volc)).toEqual({ sec: 60, wantMs: 90000, landMs: 60000, note: "「豆包音乐」单次最长 60 秒：按 60 秒生成" });
    expect(smartcutMusicDuration(0, 0, 90, eleven)).toEqual({ sec: 90, wantMs: 90000, landMs: 90000, note: "" });
  });
  it("空时间线按 30 秒；超出 3~300 的夹回去", () => {
    expect(smartcutMusicDuration(0, 0, null, null).sec).toBe(30);
    expect(smartcutMusicDuration(0, 0, 900, null).sec).toBe(300);
    expect(smartcutMusicDuration(1000, 0, null, null).sec).toBe(3);
  });
});

describe("smartcutClipQuoteText：确认框的花费说明", () => {
  it("写明花费、按多少秒计费、发给哪家、失败退款（价格只用服务端的报价）", () => {
    const text = smartcutClipQuoteText({ credits: 6, unitPrice: 2, unit: "per_second", durationSec: 3, modelCode: "erase-m", blockers: [], items: 1 }, "火山 MediaKit 去字幕");
    expect(text).toEqual({ cost: "✦6", detail: "按 3 秒计费（✦2/秒）", provider: "会发给「火山 MediaKit 去字幕」处理",
      refund: "生成失败或取消会按原批次全额退款", blocked: "" });
  });
  it("有 blockers 时确认置灰并原样给出原因", () => {
    const text = smartcutClipQuoteText({ credits: 0, modelCode: "erase-m", items: 0,
      blockers: [{ code: "42204", message: "这个模型只收公网地址的素材……请联系管理员" }] }, "去字幕");
    expect(text.blocked).toBe("这个模型只收公网地址的素材……请联系管理员");
    expect(text.cost).toBe("");
  });
});

describe("smartcutClaimGenerationFrame：实时事件按 generationId 认领", () => {
  const known: Record<string, Generation> = {
    g1: { generationId: "g1", kind: "regen", status: "queued", progress: 0, credits: 15, clipId: "c1", createdAt: "2026-10-28T10:00:00Z" }
  };
  it("不是记录里的任务不认（画布节点上的生成由画布的 store 管）", () => {
    expect(smartcutClaimGenerationFrame(known, { type: "generation.update", generationId: "g9", status: "running", progress: 10 })).toBeNull();
    expect(smartcutClaimGenerationFrame(known, { type: "render.update", generationId: "g1", status: "running" })).toBeNull();
  });
  it("进度与终态；失败带原因；终态之后晚到的非终态事件不改回去", () => {
    expect(smartcutClaimGenerationFrame(known, { type: "generation.update", generationId: "g1", status: "running", progress: 30 })).toMatchObject({ status: "running", progress: 30 });
    const failed = smartcutClaimGenerationFrame(known, { type: "generation.update", generationId: "g1", status: "failed", progress: 30, error: { message: "上游超时" } });
    expect(failed).toMatchObject({ status: "failed", error: "上游超时" });
    expect(smartcutClaimGenerationFrame({ g1: failed! }, { type: "generation.update", generationId: "g1", status: "running", progress: 50 })).toBeNull();
    expect(smartcutClaimGenerationFrame(known, { type: "generation.update", generationId: "g1", status: "queued", progress: 0 })).toBeNull();
  });
});

describe("失败标记与生成记录", () => {
  const g = (id: string, kind: Generation["kind"], status: Generation["status"], at: string, clipId = "c1"): Generation =>
    ({ generationId: id, kind, status, progress: 0, credits: 1, clipId, createdAt: at });
  it("按每段最近一次判：失败之后又成功了的不再标失败；音效失败不让画面标失败", () => {
    expect([...smartcutFailedClipIds({ a: g("a", "regen", "failed", "2026-10-28T10:00:00Z") })]).toEqual(["c1"]);
    expect([...smartcutFailedClipIds({
      a: g("a", "regen", "failed", "2026-10-28T10:00:00Z"), b: g("b", "regen", "success", "2026-10-28T10:05:00Z")
    })]).toEqual([]);
    expect([...smartcutFailedClipIds({ a: g("a", "sfx", "failed", "2026-10-28T10:00:00Z") })]).toEqual([]);
  });
  it("状态说明：落下去了 / 位置不在了可以拖进来 / 失败原因", () => {
    expect(smartcutGenerationStatusText({ kind: "regen", status: "running", progress: 40 })).toBe("生成中 40%");
    expect(smartcutGenerationStatusText({ kind: "fill", status: "success", progress: 100, landed: true })).toBe("已放进时间线");
    expect(smartcutGenerationStatusText({ kind: "fill", status: "success", progress: 100, landed: false, outputMediaId: "m1" })).toContain("可以拖进时间线");
    expect(smartcutGenerationStatusText({ kind: "sfx", status: "failed", progress: 0, error: "片段太长" })).toBe("失败：片段太长");
    expect(smartcutGenerationOutputKind("music")).toEqual({ kind: "audio", role: "music" });
    expect(smartcutGenerationOutputKind("asr")).toBeNull();
  });
  it("每次确认一个新键", () => {
    expect(smartcutClipToolKey("sfx")).not.toBe(smartcutClipToolKey("sfx"));
    expect(smartcutClipToolKey("music")).toMatch(/^smartcut-music-/);
  });
});

function clip(id: string, mediaId: string, startMs: number, outMs: number, extra: Partial<TimelineClip> = {}): TimelineClip {
  return { id, idx: 0, mediaId, sourceInMs: 0, sourceOutMs: outMs, startMs, durationMs: outMs, speed: 100, volume: 100, origin: "user", ...extra };
}
function tl(audio: TimelineClip[] = [], role: "music" | "sfx" = "music"): Timeline {
  return {
    id: "t1", projectId: "p1", name: "剪辑", version: 1, aspectRatio: "16:9", fps: 30, width: 1920, height: 1080, status: "editing",
    tracks: [
      { id: "1", kind: "video", idx: 1, isMuted: false, volume: 100, clips: [clip("a", "m1", 0, 4000), clip("b", "m2", 4000, 3000)] },
      { id: "2", kind: "audio", idx: 1, isMuted: false, volume: 100, role, clips: audio }
    ],
    subtitles: [], texts: [], totalMs: 7000, effectiveMs: 7000
  } as Timeline;
}

describe("smartcutPlaceGenerated：生成记录里的产物放进时间线", () => {
  it("视频插在最近的接缝（播放头在第一段后半 → 插在第一段之后）", () => {
    const r = smartcutPlaceGenerated(tl(), { mediaId: "m9", kind: "video", durationMs: 5000 }, 3000, { clip: "tmp-x", track: "tmp-t" });
    expect(r.ok && r.value.tracks[0]!.clips.map(c => c.id)).toEqual(["a", "tmp-x", "b"]);
  });
  it("配乐放在那段时间空着的配乐轨；占着时新建一条；时长未知不放", () => {
    const r = smartcutPlaceGenerated(tl(), { mediaId: "m9", kind: "audio", durationMs: 5000, role: "music" }, 1000, { clip: "tmp-x", track: "tmp-t" });
    expect(r.ok && r.value.tracks[1]!.clips[0]).toMatchObject({ id: "tmp-x", startMs: 1000, sourceOutMs: 5000 });
    const busy = smartcutPlaceGenerated(tl([clip("s", "m3", 0, 8000)]), { mediaId: "m9", kind: "audio", durationMs: 5000, role: "music" }, 1000, { clip: "tmp-x", track: "tmp-t" });
    expect(busy.ok && busy.value.tracks.map(t => t.id)).toEqual(["1", "2", "tmp-t"]);
    expect(busy.ok && busy.value.tracks[2]).toMatchObject({ role: "music", name: "配乐", clips: [{ id: "tmp-x", startMs: 1000 }] });
    expect(smartcutPlaceGenerated(tl(), { mediaId: "m9", kind: "audio", durationMs: 0, role: "music" }, 0, { clip: "x", track: "y" }))
      .toEqual({ ok: false, reason: "这份产物的时长还不知道，稍后再试" });
  });
  it("跟随片段的音效不挡别的音频", () => {
    const anchored = clip("s", "m3", 0, 8000, { anchor: { clipId: "a", srcStartMs: 0, srcEndMs: 4000 } });
    const r = smartcutPlaceGenerated(tl([anchored], "sfx"), { mediaId: "m9", kind: "audio", durationMs: 2000, role: "sfx" }, 1000, { clip: "tmp-x", track: "tmp-t" });
    expect(r.ok && r.value.tracks[1]!.clips.map(c => c.id)).toEqual(["s", "tmp-x"]);
  });
});

describe("smartcutFillAfter：补镜插在播放头所在的那一段之后", () => {
  it("播放头在第二段里 → 第二段之后；在最前面 → 最前面", () => {
    expect(smartcutFillAfter(tl(), 5000)).toBe("b");
    expect(smartcutFillAfter(tl(), 1000)).toBe("a");
    expect(smartcutFillAfter(tl(), 0)).toBe("");
    expect(smartcutFillAfter(null, 1000)).toBe("");
  });
});
