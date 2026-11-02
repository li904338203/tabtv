import { describe, expect, it } from "vitest";
import {
  smartcutDefaultSpeechModel,
  smartcutGranularityLabel,
  smartcutModelFits,
  smartcutSpeechModelLabel,
  smartcutTranscribeQuoteText,
  smartcutTranscribeResultText,
  smartcutTranslateResultText,
  smartcutTranslateTargets
} from "./smartcutSubtitle";

const models = [
  { code: "oai", name: "OpenAI 转写", granularity: "text" as const, unitPrice: 1, unit: "per_second" },
  { code: "volc", name: "火山极速版", granularity: "char" as const, unitPrice: 2, unit: "per_second" }
];

describe("识别模型能力标签（SCT-36）", () => {
  it("粒度说清楚；不够口播 / 解说用的照样列出并说明", () => {
    expect(smartcutGranularityLabel("char")).toBe("字级时间");
    expect(smartcutGranularityLabel("segment")).toBe("只有整句时间");
    expect(smartcutGranularityLabel(undefined)).toBe("");
    expect(smartcutSpeechModelLabel(models[1]!, "talk")).toBe("火山极速版 · 字级时间");
    expect(smartcutSpeechModelLabel(models[0]!, "talk")).toBe("OpenAI 转写 · 只有整段文字（不能用于口播精剪）");
    expect(smartcutSpeechModelLabel(models[0]!, "narr")).toContain("不能用于解说成片");
    expect(smartcutSpeechModelLabel(models[0]!, "subtitles")).toBe("OpenAI 转写 · 只有整段文字");
    expect(smartcutModelFits("word", "talk")).toBe(true);
    expect(smartcutModelFits("segment", "narr")).toBe(false);
  });

  it("缺省模型：服务端建议的优先，其次是够用途的第一个", () => {
    expect(smartcutDefaultSpeechModel(models, "talk")).toBe("volc");
    expect(smartcutDefaultSpeechModel(models, "subtitles")).toBe("volc");
    expect(smartcutDefaultSpeechModel(models, "talk", "oai")).toBe("oai");
    expect(smartcutDefaultSpeechModel(models, "talk", "gone")).toBe("volc");
    expect(smartcutDefaultSpeechModel([models[0]!], "talk")).toBe("oai");
    expect(smartcutDefaultSpeechModel([], "talk")).toBe("");
  });
});

describe("识别报价与受理文案", () => {
  it("报价：几份、几秒；已识别过的跳过也说出来", () => {
    expect(smartcutTranscribeQuoteText({ credits: 22, modelCode: "volc", blockers: [], items: 2, durationSec: 11,
      skipped: [{ mediaId: "9", reason: "已有识别结果，不再收费" }] })).toBe("识别 2 份素材，共 11 秒（1 份已识别过，跳过）");
    expect(smartcutTranscribeQuoteText({ credits: 0, modelCode: "volc", blockers: [], items: 0,
      skipped: [{ mediaId: "9", reason: "已有识别结果，不再收费" }] })).toBe("没有要识别的素材（1 份已识别过，跳过）");
    expect(smartcutTranscribeQuoteText({ credits: 0, modelCode: "volc", blockers: [{ code: "unavailable", message: "模型暂时不可用" }] }))
      .toBe("模型暂时不可用");
  });

  it("受理：开始几份、等音轨几份、已识别过的字幕已写入；没受理成的与字幕没写成的逐条列出", () => {
    const r = smartcutTranscribeResultText({
      items: [{ generationId: "1", kind: "asr", status: "queued", progress: 0, credits: 10, createdAt: "" }],
      waiting: [{ mediaId: "2", reason: "正在准备识别用音轨" }],
      skipped: [{ mediaId: "3", reason: "已有识别结果，不再收费" }, { mediaId: "4", reason: "没有音轨" }],
      failed: [{ mediaId: "5", reason: "模型暂时不可用，没有识别" }],
      subtitleError: "字幕轨已满"
    }, true);
    expect(r.text).toBe("已开始识别 1 份；1 份在准备音轨，准备好后自动识别；1 份已识别过，字幕已写入；1 份跳过。识别完成后字幕会自动写进时间线");
    expect(r.failed).toEqual(["模型暂时不可用，没有识别", "字幕没有写入：字幕轨已满"]);
    expect(smartcutTranscribeResultText({ items: [], waiting: [], skipped: [], failed: [] }, false).text).toBe("没有要识别的素材");
  });
});

describe("字幕翻译（SC-AC-53）", () => {
  it("目标语言去掉原轨自己的语言", () => {
    expect(smartcutTranslateTargets(["zh-CN", "en", "ja", "ko"], "zh-CN")).toEqual(["en", "ja", "ko"]);
    expect(smartcutTranslateTargets(undefined, null)).toEqual(["zh-CN", "en", "ja", "ko"]);
  });

  it("译了几条、放在哪；失败的按序号说，原文不变", () => {
    expect(smartcutTranslateResultText({ translated: 10, failed: [] }, "en", "newTrack"))
      .toEqual({ text: "已译好 10 条，放在新的英文字幕轨", ok: true });
    expect(smartcutTranslateResultText({ translated: 8, failed: [{ index: 7, reason: "x" }, { index: 3, reason: "x" }] }, "ja", "replace"))
      .toEqual({ text: "已译好 8 条，已替换成日文；第 3、7 条没有译出来，保持原文", ok: true });
    expect(smartcutTranslateResultText({ translated: 0, failed: [{ index: 1, reason: "AI 的译文无法解析：模型没有给出译文" }] }, "ko", "newTrack"))
      .toEqual({ text: "一条都没有译出来，字幕没有改：AI 的译文无法解析：模型没有给出译文", ok: false });
    const many = Array.from({ length: 12 }, (_, i) => ({ index: i + 1, reason: "x" }));
    expect(smartcutTranslateResultText({ translated: 3, failed: many }, "en", "newTrack").text)
      .toContain("第 1、2、3、4、5、6、7、8 等 12 条没有译出来，新轨里没有这几条");
  });
});
