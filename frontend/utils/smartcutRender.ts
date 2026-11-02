/**
 * 剪辑台导出面板的纯逻辑（docs/agent/smartcut SCT-18；移植自 `.backup/web-rewrite/app/features/timeline/render.ts`）。
 *
 * ⚠️ 与整图运行面板同一套规矩：**预检与导出是两个按钮**。抽成纯函数是为了能表驱动测 ——
 *    这些判断错了的表现都是"用户在不该点的时候点了"或"该拦的没拦住"，而界面上一切正常。
 * ⚠️ 导出按用户限流（SC-F-73）。
 * ⚠️ 所有导出带 `smartcut` 前缀并显式 import（Nuxt 自动导入重名会悄悄顶替，CLAUDE.md §5.28）。
 */
import type { components } from "@tabtv/api-client";

type RenderJob = components["schemas"]["RenderJob"];
type RenderPrecheck = components["schemas"]["RenderPrecheck"];

/** 终态：轮询到这里就停。 */
const TERMINAL: ReadonlySet<string> = new Set(["success", "failed", "cancelled"]);

export function smartcutIsTerminal(status: string): boolean {
  return TERMINAL.has(status);
}

/**
 * 多久之后可以再试的说法（429 的 `retry_after_seconds` / `Retry-After`）。
 *
 * ⚠️ 分钟向上取整：「约 0 分钟后」等于没说，用户会立刻再点一次、再收一个 429。
 */
export function smartcutRetryAfterText(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return "请稍后再试";
  if (seconds < 60) return `请 ${Math.ceil(seconds)} 秒后再试`;
  if (seconds < 3600) return `请约 ${Math.ceil(seconds / 60)} 分钟后再试`;
  return `请约 ${Math.ceil(seconds / 3600)} 小时后再试`;
}

/** 阻塞原因的附加条件（SCT-18 在备份版之上加的三条）。 */
export type SmartcutRenderBlockerExtra = {
  /** 只读的原因（剪辑台的只读状态：角色只读、被拒、结构比前端新），已是给人看的话；空串 = 可编辑。 */
  readonly?: string;
  /** 被限流：多少秒后可以再试（42901 每小时上限）。 */
  retryAfterSeconds?: number;
  /** 还在生成的片段数（片段带 `pendingGenerationId`，SC-F-138）。 */
  generatingClips?: number;
};

/**
 * 能不能点「导出」，不能的话说清为什么。
 *
 * ⚠️ **禁用了就要说为什么**：一个灰掉的按钮不解释原因，用户只会反复点它。
 * ⚠️ 顺序 = 最根本的排前面：权限 → 只读 → 运行中 → 限流 → 片段生成中 → 没预检 → 问题。
 *    限流与生成中排在「先预检」之前：预检真能点，用户点完看到「能导出」再点导出，
 *    才收到一句「太频繁」或「还在生成」—— 原因从头到尾都在那里。
 */
export function smartcutRenderBlocker(
  pre: RenderPrecheck | null,
  running: boolean,
  /**
   * 没有导出权限时的原因；空串表示有权限（P12-04）。
   *
   * ⚠️ **排在所有其他判定之前**：一个只读成员看到"先点预检"会去点预检，
   *    预检还真能点（它只读），然后又去改别的 —— 而真正的原因从头到尾都是他没有导出权限。
   */
  denied = "",
  extra: SmartcutRenderBlockerExtra = {}
): string {
  if (denied) return denied;
  if (extra.readonly) return extra.readonly;
  if (running) return "已有一次导出在进行中，先等它跑完或取消";
  if (extra.retryAfterSeconds && extra.retryAfterSeconds > 0) {
    return `导出太频繁，${smartcutRetryAfterText(extra.retryAfterSeconds)}`;
  }
  if (extra.generatingClips && extra.generatingClips > 0) {
    return `还有 ${extra.generatingClips} 段片段在生成，生成完再导出`;
  }
  if (!pre) return "先点「预检」看看这条时间线能不能导出";
  if (pre.issues.length > 0) {
    // 给数量 + 第一条原文：只说"有问题"的话用户不知道去改什么。
    const first = pre.issues[0]!.detail;
    return pre.issues.length === 1 ? first : `${first}（共 ${pre.issues.length} 项待修）`;
  }
  if (!pre.runnable) return "这条时间线现在不能导出";
  return "";
}

/** 阶段的中文说法（含音频预渲 `mixing` 与导出自检 `checking`，SCD-27 / SCD-29）。 */
const STAGE_TEXT: Record<string, string> = {
  probing: "读取素材",
  assembling: "拼接画面",
  mixing: "混音",
  encoding: "编码",
  archiving: "归档",
  checking: "检查成片"
};

/** 进度条上那行字用到的字段（`RenderJob` 与摘要里的 `SmartcutRenderBrief` 都能传）。 */
export type SmartcutRenderProgress = Pick<RenderJob, "status" | "progress" | "errorMessage"> & { stage?: string | null };

/**
 * 进度条上那行字。
 *
 * ⚠️ 只给百分比的话，卡在 40% 十分钟时用户不知道是死了还是在编码 —— 而这两种的处置完全不同（等 vs 取消重来）。
 */
export function smartcutProgressText(job: SmartcutRenderProgress): string {
  if (job.status === "queued") return "排队中";
  if (job.status === "success") return "已完成";
  if (job.status === "cancelled") return "已取消";
  if (job.status === "failed") return job.errorMessage || "导出失败";
  const stage = job.stage ? STAGE_TEXT[job.stage] ?? job.stage : "";
  return stage ? `${stage} ${job.progress}%` : `${job.progress}%`;
}

/**
 * 轮询耗尽时的措辞。
 *
 * ⚠️ **耗尽不等于失败**（GAP-14）：浏览器关掉不会中止服务端的导出。措辞里不能有"失败"二字 ——
 *    用户会去重新导一次，于是同一条片子排两次队。
 */
export const smartcutPollExhaustedText =
  "仍在处理中。导出在服务端继续跑，关掉页面也不会中断 —— 稍后回来刷新即可。";

/**
 * 轮询间隔：前期密后期疏（`render.update` 事件为主，轮询只是兜底）。
 *
 * ⚠️ 全程 1 秒的话一次 10 分钟的导出要打 600 次请求；全程 10 秒的话一条 5 秒的片子导完了用户还在等界面更新。
 */
export function smartcutPollDelayMs(attempt: number): number {
  if (attempt < 5) return 1000;
  if (attempt < 20) return 2000;
  return 5000;
}

/**
 * 合并轮询 / 事件回来的任务。
 *
 * ⚠️ 挡的是**晚到的旧响应**把终态打回 running —— 两个请求并发时后发的可能先回。
 *    用 `progress` 单调与"终态不可逆"两条判断。
 * ⚠️ 但**取消 / 重新导出之后要直接替换**，不走这里：那时服务端是真的把状态改了，走合并的话界面永远停在旧状态，
 *    用户以为没生效、再点一次。
 */
export function smartcutMergeJob<T extends Pick<RenderJob, "id" | "status" | "progress">>(prev: T | null, next: T): T {
  if (!prev || prev.id !== next.id) return next;
  if (smartcutIsTerminal(prev.status) && !smartcutIsTerminal(next.status)) return prev;
  if (prev.status === next.status && next.progress < prev.progress) return prev;
  return next;
}

/**
 * 预检结论那行摘要。
 *
 * ⚠️ 要把**时长**说出来：用户看到"3 分 20 秒 / 3 段"才知道导出的是不是他以为的那一版。
 */
export function smartcutPrecheckSummary(pre: RenderPrecheck, durationText: string): string {
  const parts = [`${pre.clipCount} 段`, durationText];
  const subtitles = pre.subtitles ?? 0;
  if (subtitles > 0) parts.push(`${subtitles} 条字幕`);
  return parts.join(" · ");
}

/**
 * 成片能不能下载。
 *
 * ⚠️ 只看 `status === 'success'` 不够：产出 ID 缺失时给一个下载按钮，点下去是 404 —— 而用户会以为是他的网络问题反复点。
 */
export function smartcutDownloadable(job: Pick<RenderJob, "status" | "outputMediaId"> | null): boolean {
  return !!job && job.status === "success" && !!job.outputMediaId;
}

/**
 * 水印那行提示。
 *
 * ⚠️ 只在**没加**的时候说，且说清是"你自己关了"：不说的话用户以为平台漏了；说成"平台未配置"的话他会去提工单。
 */
export function smartcutWatermarkNote(job: Pick<RenderJob, "status" | "watermarkApplied"> | null, userOff: boolean): string {
  if (!job || job.status !== "success") return "";
  if (job.watermarkApplied) return "";
  return userOff
    ? "按你的账号设置，这条成片未添加 AI 水印。"
    : "这条成片未添加 AI 水印（平台当前未配置水印图）。";
}

/** 预检的一条问题给人看的样子：跟具体片段有关时带上「第 N 段」（SC-F-73：用户要知道改哪一段）。 */
export function smartcutIssueText(issue: Pick<RenderPrecheck["issues"][number], "detail" | "clipIdx">): string {
  return issue.clipIdx && issue.clipIdx > 0 ? `第 ${issue.clipIdx} 段：${issue.detail}` : issue.detail;
}

/**
 * 导出的幂等键（SCB-3）：一次点击一个键，失败后的「重试」复用同一个键 —— 服务端按（工作区，键）只建一个任务，
 * 网络抖了一下重发不会多导出一份。
 */
export function smartcutIdempotencyKey(): string {
  const rand = globalThis.crypto?.randomUUID?.() ?? `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 12)}`;
  return `smartcut-render-${rand}`;
}

/** 限流错误（42901 / 42902）里的「多少秒后再试」；不是限流时为 undefined。 */
export function smartcutRetryAfterOf(cause: unknown): number | undefined {
  const details = (cause as { data?: { details?: Record<string, unknown> } } | undefined)?.data?.details;
  const secs = Number(details?.retry_after_seconds);
  return Number.isFinite(secs) && secs > 0 ? Math.ceil(secs) : undefined;
}
