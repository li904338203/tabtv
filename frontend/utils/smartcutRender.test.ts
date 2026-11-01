import type { components } from "@tabtv/api-client";
import { describe, expect, it } from "vitest";
import {
  smartcutDownloadable,
  smartcutIsTerminal,
  smartcutMergeJob,
  smartcutPollDelayMs,
  smartcutPollExhaustedText,
  smartcutPrecheckSummary,
  smartcutProgressText,
  smartcutRenderBlocker,
  smartcutRetryAfterText,
  smartcutWatermarkNote
} from "./smartcutRender";

// 前 25 条移植自 `.backup/web-rewrite/app/features/timeline/render.test.ts`（SCT-18：一条断言都不删，只改函数名）。
// 改动只在工厂与测试数据：类型换成生成物（RenderJob 多了必填的 timelineId / timelineVersion，
// 预检多了 runnable、字幕数叫 subtitles，问题码是枚举）；文案统一称「导出」，见 SCT-18 执行记录。

type RenderJob = components["schemas"]["RenderJob"];
type RenderPrecheck = components["schemas"]["RenderPrecheck"];

function job(over: Partial<RenderJob> = {}): RenderJob {
  return { id: "1", timelineId: "9", timelineVersion: 3, status: "running", progress: 50, creditsCost: 10, ...over };
}

function pre(over: Partial<RenderPrecheck> = {}): RenderPrecheck {
  return { runnable: true, issues: [], durationMs: 10_000, credits: 10, clipCount: 3, subtitles: 0, ...over };
}

describe("smartcutRenderBlocker", () => {
  it("还没预检时不让点 —— 预检免费，导出单独一步", () => {
    expect(smartcutRenderBlocker(null, 100, false)).toContain("预检");
  });

  // ⚠️ 只说"有问题"的话用户不知道去改什么；一条一条报也太长 ——
  // 给第一条原文 + 总数。
  it("有问题时给第一条原文和总数", () => {
    expect(smartcutRenderBlocker(pre({ runnable: false, issues: [{ code: "media_missing", detail: "第 2 段素材已被删除" }] }), 100, false))
      .toBe("第 2 段素材已被删除");
    const many = smartcutRenderBlocker(
      pre({ runnable: false, issues: [{ code: "media_missing", detail: "第 2 段素材已被删除" }, { code: "too_long", detail: "超长" }] }),
      100, false);
    expect(many).toContain("第 2 段素材已被删除");
    expect(many).toContain("共 2 项");
  });

  // ⚠️ 余额不足要在**点之前**说清还差多少，而不是点了收一个 422。
  it("余额不足说清还差多少", () => {
    expect(smartcutRenderBlocker(pre({ credits: 30 }), 10, false)).toBe("积分不足，还差 20 分");
  });

  // ⚠️ 不知道余额时**不猜**：猜"不够"会把一次本来能跑的导出拦掉。
  it("余额未知时不拦", () => {
    expect(smartcutRenderBlocker(pre({ credits: 30 }), null, false)).toBe("");
  });

  it("已有导出在跑时不让再发一次", () => {
    expect(smartcutRenderBlocker(pre(), 100, true)).toContain("进行中");
  });

  it("一切就绪时不拦", () => {
    expect(smartcutRenderBlocker(pre(), 100, false)).toBe("");
  });
});

describe("smartcutProgressText", () => {
  // ⚠️ 只给百分比的话，卡在 40% 十分钟时用户不知道是死了还是在编码 ——
  // 而这两种的处置完全不同。
  it("带阶段说明现在在干什么", () => {
    expect(smartcutProgressText(job({ stage: "encoding", progress: 40 }))).toBe("编码 40%");
    expect(smartcutProgressText(job({ progress: 40 }))).toBe("40%");
  });

  it("失败时给服务端的原话", () => {
    expect(smartcutProgressText(job({ status: "failed", errorMessage: "ffmpeg 挂了" }))).toBe("ffmpeg 挂了");
    // 没有 message 时也要有话说，不能是空白。
    expect(smartcutProgressText(job({ status: "failed" }))).toBe("导出失败");
  });

  it("排队/完成/取消各有说法", () => {
    expect(smartcutProgressText(job({ status: "queued" }))).toBe("排队中");
    expect(smartcutProgressText(job({ status: "success" }))).toBe("已完成");
    expect(smartcutProgressText(job({ status: "cancelled" }))).toBe("已取消");
  });
});

describe("smartcutMergeJob", () => {
  // ⚠️ 两个请求并发时后发的可能先回 —— 不挡的话界面会从"已完成"跳回"编码 80%"。
  it("晚到的旧响应不能把终态打回 running", () => {
    const done = job({ status: "success", progress: 100 });
    expect(smartcutMergeJob(done, job({ status: "running", progress: 80 })).status).toBe("success");
  });

  it("同状态下进度不许回退", () => {
    expect(smartcutMergeJob(job({ progress: 80 }), job({ progress: 50 })).progress).toBe(80);
  });

  it("换了一条任务就整条替换", () => {
    const next = job({ id: "2", status: "queued", progress: 0 });
    expect(smartcutMergeJob(job({ status: "success", progress: 100 }), next).id).toBe("2");
  });

  it("正常推进照常更新", () => {
    expect(smartcutMergeJob(job({ progress: 50 }), job({ progress: 70 })).progress).toBe(70);
  });
});

describe("轮询", () => {
  it("终态就停", () => {
    expect(smartcutIsTerminal("success")).toBe(true);
    expect(smartcutIsTerminal("failed")).toBe(true);
    expect(smartcutIsTerminal("cancelled")).toBe(true);
    expect(smartcutIsTerminal("running")).toBe(false);
    expect(smartcutIsTerminal("queued")).toBe(false);
  });

  // ⚠️ 全程 1 秒的话一次 10 分钟的导出要打 600 次请求。
  it("间隔前期密后期疏", () => {
    expect(smartcutPollDelayMs(0)).toBe(1000);
    expect(smartcutPollDelayMs(10)).toBe(2000);
    expect(smartcutPollDelayMs(100)).toBe(5000);
  });

  // ⚠️ **耗尽不等于失败**（GAP-14）：措辞里不能有"失败"二字 ——
  // 用户会去重新导一次，于是同一条片子排两次队。
  it("耗尽的措辞不说失败", () => {
    expect(smartcutPollExhaustedText).not.toContain("失败");
    expect(smartcutPollExhaustedText).toContain("继续");
  });
});

describe("smartcutDownloadable", () => {
  // ⚠️ 只看 status 不够：产出 ID 缺失时给一个下载按钮，点下去是 404。
  it("缺产出 ID 时不给下载", () => {
    expect(smartcutDownloadable(job({ status: "success" }))).toBe(false);
    expect(smartcutDownloadable(job({ status: "success", outputMediaId: "9" }))).toBe(true);
    expect(smartcutDownloadable(job({ status: "running", outputMediaId: "9" }))).toBe(false);
    expect(smartcutDownloadable(null)).toBe(false);
  });
});

describe("smartcutPrecheckSummary", () => {
  // ⚠️ 要把**时长**说出来：只给一个数字用户没法判断导的是不是他以为的那一版。
  it("说清段数、时长与预计积分", () => {
    const s = smartcutPrecheckSummary(pre({ subtitles: 4 }), "10 秒");
    expect(s).toContain("3 段");
    expect(s).toContain("10 秒");
    expect(s).toContain("4 条字幕");
    expect(s).toContain("预计 10 积分");
  });

  // 有问题时不报价：那个价是按"能跑"算出来的，报出来会让用户以为能点。
  it("有问题时不报价", () => {
    expect(smartcutPrecheckSummary(pre({ runnable: false, issues: [{ code: "too_long", detail: "y" }] }), "10 秒"))
      .not.toContain("积分");
  });
});

describe("smartcutWatermarkNote", () => {
  // ⚠️ 不说的话用户以为平台漏了；说成"平台未配置"的话他会去提工单。
  it("自己关掉的要说是账号设置", () => {
    expect(smartcutWatermarkNote(job({ status: "success" }), true)).toContain("账号设置");
    expect(smartcutWatermarkNote(job({ status: "success" }), false)).toContain("平台");
  });

  it("加上了就不啰嗦", () => {
    expect(smartcutWatermarkNote(job({ status: "success", watermarkApplied: true }), false)).toBe("");
  });

  it("没跑完时不提", () => {
    expect(smartcutWatermarkNote(job({ status: "running" }), true)).toBe("");
  });
});

// ⚠️ 没有导出权限时，禁用的理由要给**最根本**的那一条（P12-04）。
//
// 一个只读成员看到"先点预检"会去点预检（它免费且只读，服务端特意放宽了），
// 然后看到"余额不足"又去充值 —— 而真正的原因从头到尾都是他没有导出权限。
describe("smartcutRenderBlocker 的权限原因优先", () => {
  const denied = "需要导出权限，你在该工作区是只读";

  it("排在「先点预检」之前", () => {
    expect(smartcutRenderBlocker(null, 100, false, denied)).toBe(denied);
  });

  it("排在「已有导出在进行中」之前", () => {
    expect(smartcutRenderBlocker(null, 100, true, denied)).toBe(denied);
  });

  it("有权限时不影响原有判定", () => {
    expect(smartcutRenderBlocker(null, 100, false, "")).toContain("预检");
    expect(smartcutRenderBlocker(null, 100, true, "")).toContain("进行中");
  });
});

// ---------------------------------------------------------------- 以下为智能剪辑新增

describe("smartcutRenderBlocker 的新增原因（只读 / 限流 / 片段生成中）", () => {
  it("只读排在运行中之前，权限之后", () => {
    expect(smartcutRenderBlocker(pre(), 100, true, "", { readonly: "这条时间线现在只能查看" })).toBe("这条时间线现在只能查看");
    expect(smartcutRenderBlocker(pre(), 100, true, "没有导出权限", { readonly: "只能查看" })).toBe("没有导出权限");
  });

  // ⚠️ 限流与生成中排在「先预检」之前：预检免费、真能点，点完才发现原因一直都在。
  it("被限流时说清多久后可以再试，排在预检之前", () => {
    expect(smartcutRenderBlocker(null, 100, false, "", { retryAfterSeconds: 125 })).toBe("导出太频繁，请约 3 分钟后再试");
    expect(smartcutRenderBlocker(null, 100, true, "", { retryAfterSeconds: 125 })).toContain("进行中");
  });

  it("还有片段在生成时不让导出，并说出段数", () => {
    expect(smartcutRenderBlocker(null, 100, false, "", { generatingClips: 2 })).toBe("还有 2 段片段在生成，生成完再导出");
    expect(smartcutRenderBlocker(pre(), 100, false, "", { generatingClips: 0 })).toBe("");
  });

  it("预检不可运行但没给问题时也要有话说", () => {
    expect(smartcutRenderBlocker(pre({ runnable: false }), 100, false)).toBe("这条时间线现在不能导出");
  });

  // 导出免费（SCK-3）：0 积分时不拿余额拦，摘要说「免费」而不是「预计 0 积分」。
  it("免费导出不受余额影响，摘要写免费", () => {
    expect(smartcutRenderBlocker(pre({ credits: 0 }), 0, false)).toBe("");
    expect(smartcutPrecheckSummary(pre({ credits: 0 }), "10 秒")).toBe("3 段 · 10 秒 · 免费");
  });
});

describe("smartcutRetryAfterText", () => {
  it("秒、分钟、小时三档，分钟向上取整", () => {
    expect(smartcutRetryAfterText(12.2)).toBe("请 13 秒后再试");
    expect(smartcutRetryAfterText(60)).toBe("请约 1 分钟后再试");
    expect(smartcutRetryAfterText(61)).toBe("请约 2 分钟后再试");
    expect(smartcutRetryAfterText(7200)).toBe("请约 2 小时后再试");
    expect(smartcutRetryAfterText(0)).toBe("请稍后再试");
    expect(smartcutRetryAfterText(Number.NaN)).toBe("请稍后再试");
  });
});

describe("新增阶段与摘要里的导出简报", () => {
  it("音频预渲与导出自检有中文说法", () => {
    expect(smartcutProgressText(job({ stage: "mixing", progress: 30 }))).toBe("混音 30%");
    expect(smartcutProgressText(job({ stage: "checking", progress: 99 }))).toBe("检查成片 99%");
  });

  it("摘要里的 SmartcutRenderBrief 也能直接用（stage 可为 null）", () => {
    const brief: components["schemas"]["SmartcutRenderBrief"] = { id: "7", status: "running", progress: 12, stage: null, timelineVersion: 4 };
    expect(smartcutProgressText(brief)).toBe("12%");
    expect(smartcutMergeJob(brief, { ...brief, progress: 5 }).progress).toBe(12);
  });
});

describe("导出面板的小工具（SCT-24）", () => {
  it("smartcutIssueText：跟片段有关的带上第几段", async () => {
    const { smartcutIssueText } = await import("./smartcutRender");
    expect(smartcutIssueText({ detail: "素材已被删除", clipIdx: 3 })).toBe("第 3 段：素材已被删除");
    expect(smartcutIssueText({ detail: "时间线上没有画面", clipIdx: 0 })).toBe("时间线上没有画面");
  });

  it("smartcutIdempotencyKey：每次不同", async () => {
    const { smartcutIdempotencyKey } = await import("./smartcutRender");
    expect(smartcutIdempotencyKey()).not.toBe(smartcutIdempotencyKey());
    expect(smartcutIdempotencyKey()).toMatch(/^smartcut-render-/);
  });

  it("smartcutRetryAfterOf：读限流详情里的秒数", async () => {
    const { smartcutRetryAfterOf } = await import("./smartcutRender");
    expect(smartcutRetryAfterOf({ data: { code: 42901, details: { retry_after_seconds: 125 } } })).toBe(125);
    expect(smartcutRetryAfterOf({ data: { code: 40001, details: {} } })).toBeUndefined();
    expect(smartcutRetryAfterOf(new Error("x"))).toBeUndefined();
  });
});
