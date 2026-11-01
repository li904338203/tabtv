import type { components } from "@tabtv/api-client";
import { describe, expect, it } from "vitest";
import {
  smartcutAdoptLabel,
  smartcutChangedClips,
  smartcutChangedText,
  smartcutConflictDetails,
  smartcutConflictText,
  smartcutFallbackNote,
  smartcutForceApplyLabel,
  smartcutLimitText,
  smartcutOverwriteLabel,
  smartcutPlanConflictText,
  smartcutPlanErrorText,
  smartcutPlanRequestOf,
  smartcutPlanSteps,
  smartcutPlanView,
  smartcutRemoteNoticeText,
  smartcutReplaceResult,
  smartcutReplanConfirmText,
  smartcutSecondsText,
  smartcutShortage,
  smartcutUnsuitable,
  smartcutUserEditedCount,
  smartcutVariantLabel,
  smartcutVariantSummary
} from "./smartcutPlan";

type SmartcutVariant = components["schemas"]["SmartcutVariant"];
type Timeline = components["schemas"]["Timeline"];
type SmartcutInput = components["schemas"]["SmartcutInput"];

const variant = (over: Partial<SmartcutVariant> = {}): SmartcutVariant => ({
  index: 0, status: "success", summary: "", totalMs: 28_000, usedClips: ["1", "2", "3-1"], warnings: [], usedFallback: false, ...over
});
/** useApi 抛出的错误形状：`createError({statusCode, data:{code, details}})`。 */
const apiError = (code: number, details: Record<string, unknown> = {}, message = "") => ({ message, data: { code, details } });

describe("结果摘要", () => {
  it("一套方案：用了哪几段、成片多长", () => {
    expect(smartcutVariantSummary(variant())).toBe("用了 3 段素材（1、2、3-1） · 成片 00:28");
    expect(smartcutVariantSummary(variant({ usedClips: [] }))).toBe("没有用到素材 · 成片 00:28");
    expect(smartcutVariantSummary(variant({ index: 1, status: "failed", error: "模型输出无法解析" }))).toBe("方案 B 没有生成：模型输出无法解析");
  });

  it("结果卡：写进去的那一套、警告、兜底说明、追加指令的改动", () => {
    const view = smartcutPlanView({
      callId: "1", appliedVariant: 0, changes: ["第 3 段换成了 5 号素材"],
      variants: [variant({ warnings: ["素材不够，成片比目标短 4 秒"], usedFallback: true })]
    }, "mix");
    expect(view.title).toBe("AI 剪辑完成（兜底方案）");
    expect(view.summary).toContain("用了 3 段素材");
    expect(view.warnings).toEqual(["素材不够，成片比目标短 4 秒"]);
    expect(view.fallbackNote).toContain("按素材编号顺序拼接");
    expect(view.changes).toEqual(["第 3 段换成了 5 号素材"]);
    expect(view.variants).toEqual([]);
  });

  it("多方案：给切换列表，标出写进去的那一套与失败的", () => {
    const view = smartcutPlanView({
      callId: "1", appliedVariant: 1,
      variants: [variant(), variant({ index: 1, usedClips: ["2"] }), variant({ index: 2, status: "failed", error: "超时" })]
    });
    expect(view.title).toBe("AI 剪辑完成");
    expect(view.variants.map(v => [v.label, v.applied, v.failed])).toEqual([
      ["方案 A", false, false], ["方案 B", true, false], ["方案 C", false, true]
    ]);
    expect(view.summary).toBe("用了 1 段素材（2） · 成片 00:28");
  });

  it("一套都没写进去时说清楚", () => {
    expect(smartcutPlanView({ callId: "1", appliedVariant: null, variants: [variant({ status: "failed" })] }).title).toBe("AI 剪辑没有可用的方案");
    expect(smartcutPlanView({ callId: "1", variants: [variant()] }).title).toBe("AI 剪辑出了方案，还没写进时间线");
  });

  it("结果卡：理由、说明（不重复服务端的兜底句）、素材不够的出路按 shortage 字段给", () => {
    const view = smartcutPlanView({
      callId: "1", appliedVariant: 0,
      variants: [variant({
        usedFallback: true, reasons: [{ clip: "3", reason: "产品特写最抓人" }, { clip: "4", reason: "  " }],
        notes: ["AI 没能给出方案，已按规则剪好", "素材 2 与素材 1 重复，没有用"], shortage: { totalMs: 14_280, targetMs: 20_000 },
        warnings: ["随便写的一句，不靠它判断"]
      })]
    }, "mix");
    expect(view.reasons).toEqual([{ clip: "3", reason: "产品特写最抓人" }]);
    expect(view.notes).toEqual(["素材 2 与素材 1 重复，没有用"]);
    expect(view.shortage?.text).toBe("素材共 14.3 秒，不足目标 20 秒");
    expect(view.shortage?.options.map(o => o.action)).toEqual(["allowSlow", "allowRepeat", "fill"]);
    // 没有 shortage 字段时，警告里写着「素材共……不足」也不给出路
    expect(smartcutPlanView({ callId: "1", appliedVariant: 0, variants: [variant({ warnings: ["素材共 14.3 秒，不足目标 20 秒，已按 14.3 秒剪"] })] }).shortage).toBeNull();
  });

  it("素材不够：解说说的是不足配音；秒数四舍五入到 0.1、整秒不写 .0", () => {
    expect(smartcutShortage(variant({ shortage: { totalMs: 5000, targetMs: 8000 } }), "narr")?.text).toBe("画面共 5 秒，不足配音 8 秒");
    expect(smartcutShortage(variant({ shortage: null }))).toBeNull();
    expect([smartcutSecondsText(14_280), smartcutSecondsText(20_000), smartcutSecondsText(950), smartcutSecondsText(-1)]).toEqual(["14.3", "20", "1", "0"]);
  });

  it("追加指令的结果卡标题说「追加修改」；方案标签是 A / B / C", () => {
    expect(smartcutPlanView({ callId: "1", appliedVariant: 0, variants: [variant()], changes: ["第 3 段：只取前 60%"] }, undefined, "edit").title).toBe("追加修改完成");
    expect([0, 1, 2].map(smartcutVariantLabel)).toEqual(["方案 A", "方案 B", "方案 C"]);
  });

  it("兜底说明按模式说", () => {
    expect(smartcutFallbackNote("talk")).toContain("停顿");
    expect(smartcutFallbackNote("narr")).toContain("按句均分");
    expect(smartcutFallbackNote("board")).toContain("按素材编号顺序拼接");
  });
});

describe("42217 的出路（SC-UX-09）", () => {
  it("素材不够：允许放慢 / 允许重复 / 去补镜", () => {
    const u = smartcutUnsuitable({ reason: "not_enough", missing: "目标 30 秒，素材合计 18 秒" });
    expect(u.text).toBe("素材不够目标时长（缺：目标 30 秒，素材合计 18 秒）");
    expect(u.options.map(o => o.action)).toEqual(["allowSlow", "allowRepeat", "fill"]);
  });

  it("要卡点没配乐、解说没配音：去连音频节点", () => {
    expect(smartcutUnsuitable({ reason: "no_music" }).options.map(o => o.action)).toEqual(["connectAudio", "noBeat"]);
    expect(smartcutUnsuitable({ reason: "no_voice" }).options).toEqual([{ action: "connectAudio", label: "去连音频节点（配音）" }]);
  });

  it("缺识别结果：先识别，带报价", () => {
    const u = smartcutUnsuitable({ reason: "asr_missing", missing: ["2", "5"], quote: { credits: 12, modelCode: "volc_asr", blockers: [] } });
    expect(u.text).toBe("需要先识别语音（缺：2、5）");
    expect(u.options).toEqual([{ action: "transcribe", label: "先识别语音（✦12）" }]);
    expect(smartcutUnsuitable({ reason: "asr_missing" }).options[0]!.label).toBe("先识别语音");
  });

  it("缺识别结果：服务端逐份说清的原因照原样显示；识别框的缺省值取自报价（SCT-36）", () => {
    const u = smartcutUnsuitable({ reason: "asr_missing", missing: ["1"], quote: { credits: 22, modelCode: "volc", keepFillers: true } },
      "口播精剪要先识别语音（编号 1：识别时去掉了口癖，要选「保留口癖」重新识别）");
    expect(u.text).toBe("口播精剪要先识别语音（编号 1：识别时去掉了口癖，要选「保留口癖」重新识别）");
    expect(u.transcribe).toEqual({ modelCode: "volc", keepFillers: true, credits: 22 });
    expect(smartcutUnsuitable({ reason: "no_voice" }, "解说成片需要一条配音").text).toBe("解说成片需要配音");
    expect(smartcutUnsuitable({ reason: "asr_missing" }).transcribe).toEqual({});
  });

  it("出路以服务端的 options 为准；认不出的码丢掉；没给时按原因给默认的", () => {
    expect(smartcutUnsuitable({ reason: "no_music", options: ["ignore_beat", "connect_audio", "teleport"] }).options.map(o => o.action))
      .toEqual(["noBeat", "connectAudio"]);
    expect(smartcutUnsuitable({ reason: "no_picture" })).toEqual({ reason: "no_picture", text: "没有可用的画面素材", options: [{ action: "connectPicture", label: "去连视频或图片节点" }] });
    expect(smartcutUnsuitable({ reason: "no_shots" }).options.map(o => o.action)).toEqual(["fill"]);
    expect(smartcutUnsuitable({ reason: "no_voice", options: ["connect_audio"] }).options[0]!.label).toBe("去连音频节点（配音）");
    // 认不出的原因：服务端给了出路就照给（码表是契约），没给就只说明
    expect(smartcutUnsuitable({ reason: "brand_new", options: ["allow_slow"] }).options.map(o => o.action)).toEqual(["allowSlow"]);
  });

  it("认不出的原因只给说明、不给按钮", () => {
    expect(smartcutUnsuitable({ reason: "something_new" })).toEqual({ reason: "something_new", text: "当前素材不满足这个剪辑模式", options: [] });
    expect(smartcutUnsuitable(null).options).toEqual([]);
  });
});

describe("冲突与远端更新按来源分流", () => {
  it("冲突：人 / 你在另一个窗口 / Agent / 外部 AI 客户端 / AI 剪辑 / 素材同步", () => {
    expect(smartcutConflictText({ currentEditorName: "林澈", editorKind: "user" })).toBe("林澈 修改了这条时间线");
    expect(smartcutConflictText({ bySelf: true, currentEditorName: "我", editorKind: "user" })).toBe("你在另一个窗口修改过这条时间线");
    expect(smartcutConflictText({ currentEditorName: "林澈", editorKind: "agent" })).toBe("Agent 刚修改了这条时间线");
    expect(smartcutConflictText({ currentEditorName: "林澈", editorKind: "mcp" })).toBe("外部 AI 客户端刚修改了这条时间线");
    expect(smartcutConflictText({ currentEditorName: "林澈", editorKind: "ai" })).toBe("AI 剪辑刚修改了这条时间线");
    expect(smartcutConflictText({ currentEditorName: "林澈", editorKind: "sync" })).toBe("素材同步刚修改了这条时间线");
    expect(smartcutConflictText({})).toBe("时间线已被修改");
  });

  // ⚠️ 机器写入带的是用户本人的名字：先认来源，否则会说成「林澈 修改了…」让他以为是自己改的。
  it("按钮写明覆盖的是谁；机器来源不报人名", () => {
    expect(smartcutOverwriteLabel({ bySelf: true })).toBe("以我为准（覆盖我在另一个窗口的修改）");
    expect(smartcutOverwriteLabel({ currentEditorName: "林澈", editorKind: "ai" })).toBe("以我为准（覆盖 AI 剪辑的修改）");
    expect(smartcutOverwriteLabel({ currentEditorName: "林澈", editorKind: "user" })).toBe("以我为准（覆盖 林澈 的修改）");
    expect(smartcutOverwriteLabel({ editorKind: "agent" })).toBe("以我为准（覆盖 Agent 的修改）");
    expect(smartcutOverwriteLabel({ editorKind: "sync" })).toBe("以我为准（覆盖素材同步的修改）");
    expect(smartcutOverwriteLabel(null)).toBe("以我为准（覆盖服务器上的版本）");
    expect(smartcutAdoptLabel({ bySelf: true })).toBe("采纳另一个窗口的版本");
    expect(smartcutAdoptLabel({})).toBe("采纳对方版本");
  });

  it("远端更新的提示：同样按来源分流；系统写入按原因说", () => {
    expect(smartcutRemoteNoticeText({ editorKind: "user", editorName: "林澈" })).toBe("时间线被 林澈 更新");
    expect(smartcutRemoteNoticeText({ editorKind: "user", editorName: "我", bySelf: true })).toBe("你在另一个窗口更新了时间线");
    expect(smartcutRemoteNoticeText({ editorKind: "agent", editorName: "林澈" })).toBe("时间线被 Agent 更新");
    expect(smartcutRemoteNoticeText({ editorKind: "mcp" })).toBe("时间线被外部 AI 客户端更新");
    expect(smartcutRemoteNoticeText({ editorKind: "ai", reason: "plan" })).toBe("时间线被 AI 剪辑更新");
    expect(smartcutRemoteNoticeText({ editorKind: "sync", reason: "sync" })).toBe("时间线被素材同步更新");
    expect(smartcutRemoteNoticeText({ editorKind: "system", reason: "clip_fill" })).toBe("片段生成的结果已填进时间线");
    expect(smartcutRemoteNoticeText({ editorKind: "system", reason: "restore" })).toBe("时间线已恢复到历史版本");
    expect(smartcutRemoteNoticeText({ editorKind: "system", reason: "subtitles" })).toBe("识别出的字幕已写进时间线");
    expect(smartcutRemoteNoticeText({})).toBe("时间线已在别处更新");
    expect(smartcutRemoteNoticeText(null)).toBe("");
  });

  it("从错误里取冲突详情：bySelf 只认 true，认不出的来源丢掉", () => {
    expect(smartcutConflictDetails(apiError(40902, { currentVersion: 8, currentEditorName: " 林澈 ", bySelf: "true", editorKind: "ai" })))
      .toEqual({ currentVersion: 8, currentUpdatedAt: "", currentEditorName: "林澈", bySelf: false, editorKind: "ai" });
    expect(smartcutConflictDetails(apiError(40902, { editorKind: "robot" }))).not.toHaveProperty("editorKind");
    expect(smartcutConflictDetails(undefined)).toEqual({ currentUpdatedAt: "", currentEditorName: "", bySelf: false });
  });

  it("AI 剪辑期间被改：说清谁改的，按钮是「仍然应用」", () => {
    expect(smartcutPlanConflictText({ currentEditorName: "林澈", editorKind: "user" })).toBe("剪辑期间时间线有改动：林澈 修改了这条时间线");
    expect(smartcutForceApplyLabel).toContain("仍然应用");
  });
});

describe("请求失败的说法（按业务码分流）", () => {
  it("40902 进冲突流程；42217 带出路；42218 说不可用", () => {
    expect(smartcutPlanErrorText(apiError(40902, { currentEditorName: "林澈", editorKind: "user" })))
      .toEqual({ text: "剪辑期间时间线有改动：林澈 修改了这条时间线", conflict: true });
    const unsuitable = smartcutPlanErrorText(apiError(42217, { reason: "no_voice" }));
    expect(unsuitable.unsuitable?.options[0]!.action).toBe("connectAudio");
    expect(smartcutPlanErrorText(apiError(42218, { reason: "没有绑定可用的模型" })).text).toBe("AI 剪辑暂不可用：没有绑定可用的模型");
  });

  it("被限流时说明多久后可以再试", () => {
    expect(smartcutPlanErrorText(apiError(42901, { retry_after_seconds: 600 })))
      .toEqual({ text: "本小时的 AI 剪辑次数用完了，请约 10 分钟后再试", conflict: false, limit: "hourly", retryAfterSeconds: 600 });
    expect(smartcutPlanErrorText(apiError(42902)).text).toBe("上一次 AI 剪辑还没结束");
    expect(smartcutPlanErrorText(apiError(42902, { retry_after_seconds: 5 }))).toMatchObject({ text: "上一次 AI 剪辑还没结束，请 5 秒后再试", limit: "concurrency" });
    expect(smartcutPlanErrorText(apiError(42901)).text).toBe("本小时的 AI 剪辑次数用完了，请稍后再试");
    expect(smartcutLimitText("hourly", 30)).toBe("本小时的 AI 剪辑次数用完了，请 30 秒后再试");
  });

  it("40902 带回 callId 时可以「仍然应用」；不是数字串的不认", () => {
    expect(smartcutPlanErrorText(apiError(40902, { callId: "123", editorKind: "user", currentEditorName: "林澈" })))
      .toEqual({ text: "剪辑期间时间线有改动：林澈 修改了这条时间线", conflict: true, callId: "123" });
    expect(smartcutPlanErrorText(apiError(40902, { callId: "abc" }))).not.toHaveProperty("callId");
  });

  it("上游失败、参数错与认不出的码", () => {
    expect(smartcutPlanErrorText(apiError(50202)).text).toBe("模型响应超时，请稍后再试");
    expect(smartcutPlanErrorText(apiError(40001, {}, "instruction 太长")).text).toBe("instruction 太长");
    expect(smartcutPlanErrorText(new Error("fetch failed")).text).toBe("AI 剪辑失败，请检查网络后重试");
  });
});

describe("请求：节点参数 → AI 剪辑请求", () => {
  it("按配置驱动的参数组请求：跟随素材叫 follow，自动 = 0，方案数夹在 1~3", () => {
    expect(smartcutPlanRequestOf({ smartcut_mode: "board", prompt: "  剪一条 15 秒的卡点  ", aspect_ratio: "custom", resolution: "720p", target_duration: "15", plan_variants: "3" }))
      .toEqual({ mode: "board", instruction: "剪一条 15 秒的卡点", spec: { targetSec: 15, aspectRatio: "follow", resolution: "720p" }, variants: 3 });
    expect(smartcutPlanRequestOf({ target_duration: "auto", plan_variants: "9", aspect_ratio: "9:16" }))
      .toEqual({ mode: "mix", spec: { targetSec: 0, aspectRatio: "9:16" }, variants: 3 });
  });

  it("认不出的值回落默认，不把坏值发给服务端；自定义时长可能存成 {value}", () => {
    expect(smartcutPlanRequestOf({ smartcut_mode: "dance", aspect_ratio: "5:4", resolution: "4k", target_duration: { value: 4000 }, plan_variants: "x" }))
      .toEqual({ mode: "mix", spec: { targetSec: 1800 }, variants: 1 });
    expect(smartcutPlanRequestOf(null)).toEqual({ mode: "mix", spec: { targetSec: 0 }, variants: 1 });
    expect(smartcutPlanRequestOf({ prompt: "x".repeat(2100) }).instruction).toHaveLength(2000);
  });
});

describe("阶段条", () => {
  it("同步素材 → 理解素材 → 规划 → 装配；compile 与 apply 都算装配，done 全部完成，认不出的按刚开始", () => {
    const states = (stage: string | undefined) => smartcutPlanSteps(stage).map(step => step.state);
    expect(smartcutPlanSteps("plan").map(step => step.label)).toEqual(["同步素材", "理解素材", "规划", "装配"]);
    expect(states("sync")).toEqual(["current", "todo", "todo", "todo"]);
    expect(states("plan")).toEqual(["done", "done", "current", "todo"]);
    expect(states("apply")).toEqual(["done", "done", "done", "current"]);
    expect(states("compile")).toEqual(["done", "done", "done", "current"]);
    expect(states("done")).toEqual(["done", "done", "done", "done"]);
    expect(states("whatever")).toEqual(["current", "todo", "todo", "todo"]);
    expect(states(undefined)).toEqual(["current", "todo", "todo", "todo"]);
  });
});

describe("素材变化与手动改动", () => {
  const timeline = (origins: string[], sources: string[]): Timeline => ({
    id: "t1", projectId: "p1", name: "", version: 3, aspectRatio: "16:9", fps: 30, width: 1920, height: 1080, status: "editing",
    tracks: [{ id: "11", kind: "video", idx: 1, isMuted: false, volume: 100, clips: origins.map((origin, i) => ({
      id: `c${i + 1}`, idx: i + 1, mediaId: "77", sourceInMs: 0, sourceOutMs: 2000, startMs: i * 2000, durationMs: 2000, speed: 100, volume: 100,
      origin: origin as "ai", sourceNodeId: sources[i]
    })) }],
    subtitles: [], texts: [], totalMs: 0, effectiveMs: 0
  }) as Timeline;
  const input = (sourceNodeId: string, changed: boolean): SmartcutInput => ({ no: "1", sourceNodeId, sourceNodeType: "video", kind: "video", status: "ready", used: true, changed });

  it("变了的段号按来源节点对上；段号多时只列前 6 个", () => {
    const t = timeline(["ai", "user", "ai", "sync"], ["n1", "n2", "n1", "n3"]);
    expect(smartcutChangedClips(t, [input("n1", true), input("n2", false)])).toEqual([1, 3]);
    expect(smartcutChangedClips(t, [input("n9", true)])).toEqual([]);
    expect(smartcutChangedText([1, 3])).toBe("素材有变化：第 1、3 段");
    expect(smartcutChangedText([])).toBe("素材有变化");
    expect(smartcutChangedText([1, 2, 3, 4, 5, 6, 7, 8])).toBe("素材有变化：第 1、2、3、4、5、6 等 8 段");
  });

  it("重新剪之前：数出用户改过的片段，提示会被替换（可撤销）", () => {
    const t = timeline(["ai", "user", "user", "sync"], ["n1", "n2", "n3", "n4"]);
    expect(smartcutUserEditedCount(t)).toBe(2);
    expect(smartcutReplanConfirmText(2)).toBe("你手动改过 2 处，重新剪会替换它们（可撤销）。");
    expect(smartcutReplanConfirmText(0)).toBe("");
    expect(smartcutUserEditedCount(null)).toBe(0);
  });

  it("只替换变了的那几段：说换了几段，被截短的逐段说出来", () => {
    const t = timeline(["ai", "ai", "ai"], ["n1", "n2", "n1"]);
    const base = { inputs: [], appendedClipIds: [], disconnectedClipIds: [], unusedCount: 0, fingerprint: "f", timelineVersion: 4 };
    expect(smartcutReplaceResult({ ...base, replacedClipIds: ["c1", "c3"], shortened: [{ clipId: "c3", fromMs: 2000, toMs: 1450 }, { clipId: "gone", fromMs: 3000, toMs: 2000 }] }, t))
      .toEqual({ text: "已把 2 段换成新素材（可撤销）", details: ["第 3 段的新素材比原来短，从 2 秒截到了 1.5 秒", "有一段的新素材比原来短，从 3 秒截到了 2 秒"] });
    expect(smartcutReplaceResult({ ...base, replacedClipIds: [] }, t)).toEqual({ text: "没有需要替换的片段", details: [] });
  });
});

