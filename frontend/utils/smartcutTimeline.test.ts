import type { components } from "@tabtv/api-client";
import { describe, expect, it } from "vitest";
import {
  smartcutAnchorsOf,
  smartcutAspectPresets,
  smartcutClampPlayhead,
  smartcutClampZoom,
  smartcutClipAt,
  smartcutClipsAt,
  smartcutDescribeDuration,
  smartcutDropPosition,
  smartcutFollowSize,
  smartcutFormatTime,
  smartcutFormatTimeMs,
  smartcutMaxTransitionMs,
  smartcutMsToPx,
  smartcutPresetFor,
  smartcutPreviewBox,
  smartcutPxToMs,
  smartcutSnapMs,
  smartcutSnapPx,
  smartcutSourceOffsetMs,
  smartcutSpecSize,
  smartcutSubtitleIssues,
  smartcutSubtitlesAt,
  smartcutTickStepMs,
  smartcutTicks,
  type SmartcutSubtitleTiming
} from "./smartcutTimeline";

// 前 28 条移植自 `.backup/web-rewrite/app/features/timeline/timeline.test.ts`（SCT-18：一条断言都不删，只改函数名）。

type TimelineClip = components["schemas"]["TimelineClip"];

function clip(id: string, startMs: number, durationMs: number, speed = 100): TimelineClip {
  return {
    id, idx: 1, mediaId: "m" + id, sourceInMs: 0, sourceOutMs: durationMs,
    startMs, durationMs, speed, volume: 100
  };
}

describe("缩放与刻度", () => {
  it("档位夹回范围", () => {
    expect(smartcutClampZoom(-5)).toBe(0);
    expect(smartcutClampZoom(999)).toBe(5);
    expect(smartcutClampZoom(Number.NaN)).toBe(2);
  });

  it("像素与毫秒往返一致", () => {
    const px = smartcutMsToPx(3000, 2);
    expect(smartcutPxToMs(px, 2)).toBe(3000);
  });

  // ⚠️ 用固定的"好看"间隔而不是 `总长/10`：后者会出现"每 3.7 秒一格"，
  // 用户没法据此估时间。
  it("刻度间隔保证标签不叠", () => {
    // 10px/s 时 1 秒只有 10px，必须跳到更大的档。
    expect(smartcutTickStepMs(0)).toBeGreaterThanOrEqual(5000);
    // 320px/s 时 1 秒就有 320px，1 秒一格足够。
    expect(smartcutTickStepMs(5)).toBe(1000);
  });

  it("刻度末尾多给一格", () => {
    const got = smartcutTicks(5000, 5);
    expect(got[0]).toBe(0);
    // 不多给的话最后一段片段右侧是一片没有刻度的空白。
    expect(got[got.length - 1]).toBeGreaterThan(5000);
  });
});

describe("播放头", () => {
  it("夹在 [0, total] 内", () => {
    expect(smartcutClampPlayhead(-100, 5000)).toBe(0);
    expect(smartcutClampPlayhead(99999, 5000)).toBe(5000);
  });

  // ⚠️ 左闭右开：相邻两段在接缝处不能都命中，否则点在接缝上时
  // 选中的是哪一段取决于遍历顺序 —— 时灵时不灵。
  it("接缝处只命中后一段", () => {
    const clips = [clip("a", 0, 1000), clip("b", 1000, 1000)];
    expect(smartcutClipAt(clips, 999)?.id).toBe("a");
    expect(smartcutClipAt(clips, 1000)?.id).toBe("b");
  });

  it("落在末尾时归最后一段", () => {
    const clips = [clip("a", 0, 1000)];
    // 否则拖到最右边会"没有选中任何片段"。
    expect(smartcutClipAt(clips, 1000)?.id).toBe("a");
    expect(smartcutClipAt(clips, 1001)).toBeNull();
    expect(smartcutClipAt([], 0)).toBeNull();
  });

  // ⚠️ 必须带上变速换算：2 倍速时时间线上的 1 秒对应素材里的 2 秒。
  // 不换算的话预览里播的位置和播放头对不上，而两边各自看都是"在播"。
  it("素材内偏移要按变速换算", () => {
    const c = { ...clip("a", 2000, 2000, 200), sourceInMs: 1000 };
    // 播放头在段内 1000ms → 素材里 1000 + 1000*2 = 3000
    expect(smartcutSourceOffsetMs(c, 3000)).toBe(3000);
    // 播放头在段起点 → 素材起点
    expect(smartcutSourceOffsetMs(c, 2000)).toBe(1000);
    // 播放头在段之前 → 夹到起点，不给负数
    expect(smartcutSourceOffsetMs(c, 0)).toBe(1000);
  });

  it("变速为 0 时按原速处理，不除零", () => {
    const c = { ...clip("a", 0, 1000), speed: 0 };
    expect(smartcutSourceOffsetMs(c, 500)).toBe(500);
  });
});

describe("吸附", () => {
  // ⚠️ 阈值按像素算再换回毫秒：放大到 320px/s 时 8px 只有 25ms，
  // 缩小到 10px/s 时 8px 是 800ms —— 用固定毫秒的话缩小之后会把片段拽出去老远。
  it("阈值随缩放变化", () => {
    const anchors = [1000];
    // 放得很大时，差 200ms 已经远超 8px，不吸。
    expect(smartcutSnapMs(1200, anchors, 5)).toBe(1200);
    // 缩得很小时，同样的 200ms 在 8px 之内，会吸过去。
    expect(smartcutSnapMs(1200, anchors, 0)).toBe(1000);
  });

  it("吸到最近的锚点", () => {
    const got = smartcutSnapMs(1010, [1000, 1030], 5);
    expect(got).toBe(1000);
  });

  it("没有锚点时原样返回", () => {
    expect(smartcutSnapMs(1234, [], 2)).toBe(1234);
  });

  it("锚点含片段首尾与 0", () => {
    const got = smartcutAnchorsOf([clip("a", 0, 1000), clip("b", 1000, 500)], 1200);
    expect(got).toContain(0);
    expect(got).toContain(1000);
    expect(got).toContain(1500);
    expect(got).toContain(1200);
  });

  it("smartcutSnapPx 是个合理的小数", () => {
    expect(smartcutSnapPx).toBeGreaterThan(0);
    expect(smartcutSnapPx).toBeLessThan(40);
  });
});

describe("拖放排序", () => {
  // ⚠️ 判据是片段的**中点**：按左边界判的话，把 A 往右挪一点点
  // 就会越过 B 的左边界、直接跳到 B 后面，而用户只是想微调。
  it("按中点判插入位置", () => {
    const clips = [clip("a", 0, 1000), clip("b", 1000, 1000), clip("c", 2000, 1000)];
    expect(smartcutDropPosition(clips, 100)).toBe(1); // 还在 a 的左半
    expect(smartcutDropPosition(clips, 600)).toBe(2); // 过了 a 的中点
    expect(smartcutDropPosition(clips, 1600)).toBe(3); // 过了 b 的中点
  });

  it("空轨道也给合法位置", () => {
    expect(smartcutDropPosition([], 0)).toBe(1);
  });
});

describe("展示", () => {
  it("时间格式", () => {
    expect(smartcutFormatTime(0)).toBe("00:00");
    expect(smartcutFormatTime(65000)).toBe("01:05");
    expect(smartcutFormatTimeMs(65123)).toBe("01:05.123");
    expect(smartcutFormatTime(-5)).toBe("00:00");
  });

  // ⚠️ 设了转场之后两个数不一样，此时两个都要显示：用户按"放了多少素材"
  // 理解前者，但配乐与字幕要对齐的是后者。
  it("有转场时两个时长都要说", () => {
    expect(smartcutDescribeDuration(12000, 12000)).toBe("00:12");
    const got = smartcutDescribeDuration(12000, 11000);
    expect(got).toContain("00:11");
    expect(got).toContain("素材合计");
    expect(got).toContain("转场重叠");
  });
});

describe("字幕", () => {
  const subs: SmartcutSubtitleTiming[] = [
    { id: "1", startMs: 0, endMs: 2000, text: "甲说" },
    { id: "2", startMs: 1500, endMs: 3000, text: "乙同时说" }
  ];

  // ⚠️ 返回数组不是单条：允许时间重叠（两个人同时说话是正当的），
  // 只取第一条的话第二个人的台词永远不显示。
  it("重叠时都返回", () => {
    expect(smartcutSubtitlesAt(subs, 1700).map(s => s.id)).toEqual(["1", "2"]);
    expect(smartcutSubtitlesAt(subs, 500).map(s => s.id)).toEqual(["1"]);
    expect(smartcutSubtitlesAt(subs, 3000)).toEqual([]);
  });

  // ⚠️ 这些是**质量问题不是错误**，只提示不拦截 —— 拦下来用户就走不下去了。
  it("只提示不拦截", () => {
    const issues = smartcutSubtitleIssues([
      { id: "1", startMs: 0, endMs: 20000, text: "很长的一条" },
      { id: "2", startMs: 0, endMs: 1000, text: "   " }
    ], 5000);
    expect(issues.length).toBeGreaterThanOrEqual(3);
    expect(issues.some(i => i.message.includes("超出片长"))).toBe(true);
    expect(issues.some(i => i.message.includes("考虑拆成两条"))).toBe(true);
    expect(issues.some(i => i.message.includes("没有内容"))).toBe(true);
  });

  it("片长未知时不报超出", () => {
    const issues = smartcutSubtitleIssues([{ id: "1", startMs: 0, endMs: 9000, text: "好" }], 0);
    expect(issues).toEqual([]);
  });
});

describe("画幅与预览", () => {
  it("预设的宽高都是偶数", () => {
    // H.264 的 yuv420p 要求宽高都能被 2 整除，否则 ffmpeg 直接报错。
    for (const p of [smartcutPresetFor("16:9"), smartcutPresetFor("9:16"), smartcutPresetFor("1:1")]) {
      expect(p.width % 2).toBe(0);
      expect(p.height % 2).toBe(0);
    }
    // 认不出的画幅回落到第一个而不是 undefined。
    expect(smartcutPresetFor("unknown").ratio).toBe("16:9");
  });

  // ⚠️ 等比缩放而不是填满：填满的话竖屏片子在横屏容器里会被拉扁，
  // 用户按那个画面调构图，导出来完全不是一回事。
  it("预览框等比缩放", () => {
    const got = smartcutPreviewBox(1080, 1920, { width: 800, height: 600 });
    expect(got.height).toBe(600);
    expect(got.width).toBe(Math.round(1080 * (600 / 1920)));
  });

  it("尺寸为 0 时不崩", () => {
    expect(smartcutPreviewBox(0, 0, { width: 800, height: 600 })).toEqual({ width: 0, height: 0 });
  });
});

describe("转场上限", () => {
  // ⚠️ 转场吃掉两段各一半的时间，上限是**两侧较短那段的一半**。
  it("取两侧较短那段的一半", () => {
    const clips = [clip("a", 0, 4000), clip("b", 4000, 1000)];
    expect(smartcutMaxTransitionMs(clips, "b")).toBe(500);
  });

  // 第一段是"从黑场淡入"，没有上一段可吃。
  it("第一段只受自己长度限制", () => {
    const clips = [clip("a", 0, 4000)];
    expect(smartcutMaxTransitionMs(clips, "a")).toBe(2000);
  });

  it("不超过 3 秒的硬上限", () => {
    const clips = [clip("a", 0, 60000), clip("b", 60000, 60000)];
    expect(smartcutMaxTransitionMs(clips, "b")).toBe(3000);
  });

  it("找不到片段时给 0", () => {
    expect(smartcutMaxTransitionMs([], "x")).toBe(0);
  });
});

// ---------------------------------------------------------------- 以下为智能剪辑新增

describe("转场重叠区间（SCK-15）", () => {
  // 第 2 段带 1 秒转场：成片里 [3000, 4000) 两段同时在播，预览要两段都拿到才画得出叠化。
  it("smartcutClipsAt：重叠区间返回两段，其余只返回一段", () => {
    const clips = [clip("a", 0, 4000), clip("b", 3000, 4000)];
    expect(smartcutClipsAt(clips, 3500).map(c => c.id)).toEqual(["a", "b"]);
    expect(smartcutClipsAt(clips, 2999).map(c => c.id)).toEqual(["a"]);
    expect(smartcutClipsAt(clips, 4000).map(c => c.id)).toEqual(["b"]);
    expect(smartcutClipsAt(clips, 7000).map(c => c.id)).toEqual(["b"]);
    expect(smartcutClipsAt(clips, 7001)).toEqual([]);
    expect(smartcutClipsAt([], 0)).toEqual([]);
  });

  it("smartcutClipAt：重叠区间里归前一段（左闭右开的自然结果）", () => {
    const clips = [clip("a", 0, 4000), clip("b", 3000, 4000)];
    expect(smartcutClipAt(clips, 3500)?.id).toBe("a");
  });
});

describe("字幕质量提示补上的三条（SC-F-54）", () => {
  it("过短、与同一轨的前一条重叠、单行字数过多；不同轨的重叠不提示", () => {
    const issues = smartcutSubtitleIssues([
      { id: "1", startMs: 0, endMs: 500, text: "嗯", trackId: "zh" },
      { id: "2", startMs: 300, endMs: 2300, text: "我们今天要讲的是一个关于剪辑节点的很长很长的故事", trackId: "zh" },
      { id: "3", startMs: 400, endMs: 2400, text: "Hello there", trackId: "en" }
    ], 10000);
    expect(issues.map(i => i.message)).toEqual([
      "第 1 条显示不到 0.7 秒，观众可能来不及看完",
      "第 2 条与前一条重叠（两个人同时说话时可以不管）",
      "第 2 条有一行超过约 16 个字，会自动换行"
    ]);
  });

  it("按宽度算字数：半角算半个；手动换行后每行分别算", () => {
    const at = (text: string) => smartcutSubtitleIssues([{ id: "1", startMs: 0, endMs: 2000, text }], 0);
    expect(at("a".repeat(32))).toEqual([]);
    expect(at("a".repeat(33))).toHaveLength(1);
    expect(at("一二三四五六七八九十一二三四五六\n一二三四五六七八九十一二三四五六")).toEqual([]);
  });
});

describe("吸附锚点含节拍（SC-F-24）", () => {
  it("节拍只是额外的锚点，仍按最近点吸", () => {
    const anchors = smartcutAnchorsOf([clip("a", 0, 1000)], undefined, [480, 960]);
    expect(anchors).toEqual([0, 0, 1000, 480, 960]);
    // 40px/s 时 8px = 200ms：990 离片段尾 1000 更近，不会因为开了节拍就被拽到 960。
    expect(smartcutSnapMs(990, anchors, 2)).toBe(1000);
    expect(smartcutSnapMs(500, anchors, 2)).toBe(480);
  });
});

describe("导出规格尺寸（SCK-11）", () => {
  it("按短边缩放到 720 / 1080，宽高都是偶数", () => {
    expect(smartcutSpecSize("16:9", "1080p")).toEqual({ width: 1920, height: 1080 });
    expect(smartcutSpecSize("16:9", "720p")).toEqual({ width: 1280, height: 720 });
    expect(smartcutSpecSize("9:16", "720p")).toEqual({ width: 720, height: 1280 });
    expect(smartcutSpecSize("3:4", "720p")).toEqual({ width: 720, height: 960 });
    expect(smartcutSpecSize("21:9", "720p")).toEqual({ width: 1706, height: 720 });
    for (const p of smartcutAspectPresets) {
      for (const r of ["720p", "1080p"] as const) {
        const s = smartcutSpecSize(p.ratio, r);
        expect(s.width % 2).toBe(0);
        expect(s.height % 2).toBe(0);
        expect(Math.min(s.width, s.height)).toBe(r === "720p" ? 720 : 1080);
      }
    }
  });

  it("认不出的画幅按 16:9", () => {
    expect(smartcutSpecSize("5:4", "720p")).toEqual({ width: 1280, height: 720 });
  });

  it("跟随第一段素材：短边取 1080、宽高取偶数，尺寸未知回落 16:9", () => {
    expect(smartcutFollowSize(720, 1280)).toEqual({ width: 1080, height: 1920 });
    expect(smartcutFollowSize(1001, 1001)).toEqual({ width: 1080, height: 1080 });
    expect(smartcutFollowSize(1366, 768)).toEqual({ width: 1920, height: 1080 });
    expect(smartcutFollowSize(0, 0)).toEqual({ width: 1920, height: 1080 });
  });

  it("3:4 竖版预设存在（SC-F-21）", () => {
    expect(smartcutPresetFor("3:4")).toMatchObject({ width: 1080, height: 1440 });
  });
});
