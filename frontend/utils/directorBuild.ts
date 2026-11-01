/**
 * AI 搭景的前端纯函数（DRT-20，dev/03 §8）：参考图汇总、禁用理由、错误文案。不碰网络、不碰 store。
 *
 * ⚠️ 错误文案**按业务码分流，不匹配 message**：message 改一个字匹配就失效，失效的表现是所有失败都退回同一句兜底
 *    （同图片工具栏的阻塞原因分类）。只有服务端原因本身就是要给用户看的那两类（42203 不可用、40001 参数）才原样带出。
 * ⚠️ 禁用理由给**最根本的那一条**（CLAUDE.md §5.17 / §5.18）：只读排在最前 —— 否则只读成员会去改描述、
 *    去等配额，而真正的原因从头到尾都是他没有权限。
 */
import { ErrorCode } from "@tabtv/api-client";
import type { components } from "@tabtv/api-client";
import type { WorkspaceRole } from "./agentState";
import { errorCode } from "./asyncState";
import type { DirectorUpstreamImage } from "./directorCanvas";
import { directorCanEdit } from "./directorRole";

type BuildConfig = components["schemas"]["DirectorBuildConfig"];

/** 上传在节点上的参考图（`params.reference` 归一后的形状，见 utils/canvas.referenceResources）。 */
type UploadedLike = { id: string; type: string; name?: string };

export type DirectorBuildRefs = {
  /** 按顺序发给服务端的参考图（图 1、图 2……）。 */
  mediaIds: string[];
  /** 超出上限丢掉的张数（界面要说出来：选了 6 张只发 4 张不解释，用户会以为漏了）。 */
  dropped: number;
  /** 连着、但还没有可用产出的上游（标题）：没算进参考图。 */
  pending: string[];
};

/**
 * 搭景要带的参考图（DR-F-02、DR-F-101）：入边上游按连线顺序（被选为全景背景的那一条除外）→ 节点上上传的参考；
 * 按媒体 ID 去重；超过 `maxImages` 的丢掉并报数。
 */
export function directorBuildReferences(input: {
  incoming: readonly DirectorUpstreamImage[];
  panoramaSourceNodeId?: string | null;
  uploads: readonly UploadedLike[];
  maxImages: number;
}): DirectorBuildRefs {
  const ordered: string[] = [];
  const pending: string[] = [];
  for (const upstream of [...input.incoming].sort((a, b) => a.order - b.order)) {
    if (input.panoramaSourceNodeId && upstream.sourceNodeId === input.panoramaSourceNodeId) continue;
    if (upstream.mediaId) ordered.push(upstream.mediaId);
    else pending.push(upstream.title);
  }
  for (const upload of input.uploads) {
    if (upload.type === "image" && /^\d+$/.test(upload.id)) ordered.push(upload.id);
  }
  const unique = [...new Set(ordered)];
  const limit = Math.max(0, Math.floor(input.maxImages));
  return { mediaIds: unique.slice(0, limit), dropped: Math.max(0, unique.length - limit), pending };
}

/**
 * 编辑器顶栏「AI 搭景」入口为什么点不了（DR-AC-32）；能点时为 null。只看权限与服务端的可用性 ——
 * 描述、参考图这些输入问题在展开的条里再说；配置还没读到时**不拦**入口（条里会说正在读取 / 读不到并给重试）。
 */
export function directorBuildEntryReason(input: {
  config: BuildConfig | null | undefined;
  role: WorkspaceRole | null | undefined;
}): string | null {
  if (!directorCanEdit(input.role)) return "需要编辑权限：你在该工作区是只读成员";
  if (input.config && !input.config.available) return input.config.reason?.trim() || "AI 搭景暂不可用";
  return null;
}

/**
 * 「发送 / AI 搭景」为什么点不了；能点时为 null。优先级：只读 → 服务端说不可用 → 提案预览中 → 输入问题。
 */
export function directorBuildDisabledReason(input: {
  config: BuildConfig | null | undefined;
  /** build-config 没取到时的说明（`useDirectorBuildConfig().error`）：取失败不是「不可用」，要说成读不到、可以重试。 */
  configError?: string;
  role: WorkspaceRole | null | undefined;
  prompt: string;
  refs: number;
  proposalOpen: boolean;
}): string | null {
  if (!directorCanEdit(input.role)) return "需要编辑权限：你在该工作区是只读成员";
  const config = input.config;
  if (!config) return input.configError?.trim() || "正在读取 AI 搭景的配置";
  if (!config.available) return config.reason?.trim() || "AI 搭景暂不可用";
  if (input.proposalOpen) return "请先应用或放弃当前的 AI 提案";
  const prompt = input.prompt.trim();
  if (!prompt) return "请先描述想要搭建的场景";
  if (Array.from(prompt).length > config.maxPromptChars) return `描述不能超过 ${config.maxPromptChars} 字`;
  if (input.refs > config.maxImages) {
    return config.maxImages > 0 ? `参考图最多 ${config.maxImages} 张` : "当前模型不收参考图，请先移除参考图";
  }
  if (config.perHour > 0 && config.usedThisHour >= config.perHour) return `本小时已用完 ${config.perHour} 次，请稍后再试`;
  return null;
}

export type DirectorBuildFailure = {
  text: string;
  /** 40902：场景刚被别人改过 —— 调用方要转进冲突流程，而不是只弹一句话。 */
  conflict: boolean;
  /** 42901 时多少秒后可以再试。 */
  retryAfterSeconds?: number;
};

function detailsOf(cause: unknown): Record<string, unknown> {
  const details = (cause as { data?: { details?: unknown } } | undefined)?.data?.details;
  return details && typeof details === "object" && !Array.isArray(details) ? details as Record<string, unknown> : {};
}

function serverMessage(cause: unknown): string {
  const message = (cause as { message?: unknown } | undefined)?.message;
  return typeof message === "string" ? message.trim() : "";
}

/** 搭景失败给用户看的话（DRT-20）：按业务码分流。认不出的码与断网给兜底，不把英文原文给用户看。 */
export function directorBuildErrorText(cause: unknown): DirectorBuildFailure {
  const code = errorCode(cause);
  switch (code) {
    case ErrorCode.RATE_LIMITED: {
      const seconds = Number(detailsOf(cause).retry_after_seconds);
      if (Number.isFinite(seconds) && seconds > 0) {
        const rounded = Math.ceil(seconds);
        return { text: `请 ${rounded} 秒后再试`, conflict: false, retryAfterSeconds: rounded };
      }
      return { text: "请求过于频繁，请稍后再试", conflict: false };
    }
    case ErrorCode.CONCURRENCY_LIMITED:
      return { text: "上一次搭景还没结束", conflict: false };
    case ErrorCode.CAPABILITY_UNSUPPORTED: {
      const reason = serverMessage(cause);
      return { text: reason ? `AI 搭景暂不可用：${reason}` : "AI 搭景暂不可用", conflict: false };
    }
    case ErrorCode.DIRECTOR_DRAFT_INVALID:
      return { text: "没能理解这段描述，换个说法再试", conflict: false };
    case ErrorCode.PROVIDER_ERROR:
    case ErrorCode.PROVIDER_REJECTED:
      return { text: "模型暂时不可用，请稍后再试", conflict: false };
    case ErrorCode.PROVIDER_TIMEOUT:
      return { text: "模型响应超时，请稍后再试", conflict: false };
    case ErrorCode.VERSION_CONFLICT:
      return { text: "场景刚被修改", conflict: true };
    case ErrorCode.INVALID_ARGUMENT:
      return { text: serverMessage(cause) || "参数有误", conflict: false };
    default:
      return { text: "AI 搭景失败，请检查网络后重试", conflict: false };
  }
}
