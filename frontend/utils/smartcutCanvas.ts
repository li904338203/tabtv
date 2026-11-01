/**
 * 智能剪辑接入画布的纯函数（docs/agent/smartcut SCT-23，dev/03 §8）：打开时的自愈判定、节点上的时间线 ID、
 * 下游取输入时的成片、未就绪提示、连线变化的签名。都不碰网络、不碰 store —— 调用方是 CanvasBoard、BaseNode 与 ParamPanel。
 *
 * ⚠️ 时间线正文、成片、封面、版本都不进 canvasData（SCK-13）：节点 params 里只有 `timelineId` 与用户输入
 *    （要求、模式、规格），这里只读这些；成片与状态从摘要（`useSmartcuts`）取。
 */
import type { components } from "@tabtv/api-client";
import type { CanvasEdge, JsonObject } from "~/types/canvas";
import { smartcutRetryAfterText } from "./smartcutRender";

type Summary = components["schemas"]["SmartcutSummary"];

export const SMARTCUT_NODE_TYPE = "smartcut";

type NodeLike = { id: string; data: { nodeType: string; title?: string; params: JsonObject } };

/** 节点上的时间线 ID（雪花号字符串）；没有或形状不对时为空串。 */
export function smartcutTimelineIdOf(params: JsonObject | null | undefined): string {
  const value = String(params?.timelineId ?? "").trim();
  return /^\d+$/.test(value) ? value : "";
}

/** 打开时在**本项目**的摘要里查到的结果。`missing`：本项目没有这条时间线（多半是从别的项目粘贴来的节点）。 */
export type SmartcutTimelineLookup = { status: "found"; canvasNodeId: string } | { status: "missing" };

export type SmartcutOpenPlan =
  /** 时间线就是本节点的：直接打开。 */
  | { action: "open"; timelineId: string }
  /** 节点上没有 `timelineId`（Agent 建了一半、建节点时请求失败）：新建并写回。 */
  | { action: "create" }
  /** 时间线属于别的节点（复制、粘贴、复制后分叉失败）：从它复制出本节点自己的（SC-F-07）。 */
  | { action: "fork"; sourceTimelineId: string }
  /** 时间线不在本项目：先按它复制（同一工作区可以），复制不了再建空的并提示 `notice`。 */
  | { action: "recreate"; sourceTimelineId: string; notice: string };

export const SMARTCUT_RECREATED_NOTICE = "原时间线不在本项目，已新建";

/**
 * 打开剪辑台前的判定（照导演台的 `directorOpenPlan`）。`lookup` 为 null 表示节点上没有 timelineId、不必查。
 * ⚠️ 节点与时间线按 `canvasNodeId` 一一对应（SCD-20）：两个节点指向同一条的话，改一个另一个跟着变（SC-AC-61）。
 */
export function smartcutOpenPlan(node: NodeLike, lookup: SmartcutTimelineLookup | null): SmartcutOpenPlan {
  const timelineId = smartcutTimelineIdOf(node.data.params);
  if (!timelineId) return { action: "create" };
  if (!lookup || lookup.status === "missing") {
    return { action: "recreate", sourceTimelineId: timelineId, notice: SMARTCUT_RECREATED_NOTICE };
  }
  if (lookup.canvasNodeId !== node.id) return { action: "fork", sourceTimelineId: timelineId };
  return { action: "open", timelineId };
}

/**
 * 下游取输入用的成片（SCD-50）：最近一次**成功**导出的成片媒体 ID。没有（还没导出、导出失败、正在导出）时为空串 ——
 * 下游视为「未就绪」，不受理、不扣费。⚠️ 成片过期（剪辑已改）不影响：下游用的仍是那一版成片，角标另外提示。
 */
export function smartcutOutputMediaIdOf(summary: Pick<Summary, "latestRender"> | null | undefined): string {
  const render = summary?.latestRender;
  if (!render || render.status !== "success") return "";
  const id = String(render.outputMediaId ?? "");
  return /^\d+$/.test(id) ? id : "";
}

/** 下游节点的发送按钮上，上游是没导出的智能剪辑时的提示。 */
export function smartcutUpstreamNotReadyText(title: string | undefined): string {
  return `请先在智能剪辑里导出成片：${title || "智能剪辑"}`;
}

/**
 * 指向这个节点的连线的签名（来源 + 顺序）：画布保存成功后签名变了才触发一次同步（SCD-49）——
 * 不比签名的话每次保存（拖动节点、改别的节点的提示词）都会给每个智能剪辑节点发一次同步。
 */
export function smartcutInboundSignature(edges: readonly Pick<CanvasEdge, "source" | "target" | "data">[], nodeId: string): string {
  return edges
    .filter(edge => edge.target === nodeId)
    .map(edge => `${edge.source}:${edge.data?.order ?? 0}`)
    .sort()
    .join("|");
}

/** 打开剪辑台之后要顺手做的事：AI 剪辑（SCT-31）、把没用上的素材追加到末尾（SC-F-14）。 */
export type SmartcutPendingAction =
  | { type: "plan" }
  | { type: "append-unused" };

type RuleLike = { sourceType?: string; source?: string; targetType?: string; target?: string; isAllowed?: boolean; allowed?: boolean };

/**
 * 「+ 参考 → 从画布选」的候选（SCD-48）：连线规则允许连进智能剪辑、还没连上、不是它自己的节点，最近建的在前。
 * 规则来自 `/canvas/config` 的 connectionRules（配置驱动，前端不写死哪些类型能连）。
 */
export function smartcutConnectableNodes<T extends NodeLike>(
  nodes: readonly T[],
  edges: readonly Pick<CanvasEdge, "source" | "target">[],
  nodeId: string,
  rules: readonly RuleLike[]
): T[] {
  const connected = new Set(edges.filter(edge => edge.target === nodeId).map(edge => edge.source));
  const allowed = new Set(rules
    .filter(rule => (rule.targetType ?? rule.target) === SMARTCUT_NODE_TYPE && (rule.isAllowed ?? rule.allowed))
    .map(rule => rule.sourceType ?? rule.source));
  return [...nodes].reverse().filter(node => node.id !== nodeId && !connected.has(node.id) && allowed.has(node.data.nodeType));
}

type ConfigLike = { available: boolean; reason?: string; usedThisHour: number; perHour: number } | null | undefined;

/**
 * AI 剪辑入口的禁用原因（空串 = 可以发送，SC-F-09、SC-UX-05）：指令面板的「发送」与剪辑台里的 AI 按钮共用。
 * 给最根本的那一条：只读 → AI 剪辑进行中 → 没有素材 → 配置读不到 / 不可用 → 被限流（说多久后再试）→ 解说没有配音。
 *
 * ⚠️ 被限流时**要说多久后可以再试**（SC-AC-35）：`retryAfterSeconds` 来自上一次 429 的 `retry_after_seconds`；
 *    没有它而本小时次数已经用完时，只说用完了。
 */
export function smartcutSendDisabledReason(input: {
  canEdit: boolean;
  readyInputs: number;
  config: ConfigLike;
  configError?: string;
  mode?: string;
  hasVoice?: boolean;
  /** 这条时间线上有进行中的 AI 调用（摘要的 `activeCall`，或剪辑台里刚发出的请求）。 */
  running?: boolean;
  /** 上一次被限流时服务端说的「多少秒后再试」还剩多少秒。 */
  retryAfterSeconds?: number;
}): string {
  if (!input.canEdit) return "需要编辑权限：你在该工作区是只读成员";
  if (input.running) return "AI 剪辑进行中，请等它完成";
  if (input.readyInputs <= 0) return "先连入视频、图片或音频素材";
  if (!input.config) return input.configError || "AI 剪辑的可用性还没读到";
  if (!input.config.available) return input.config.reason ? `AI 剪辑暂不可用：${input.config.reason}` : "AI 剪辑暂不可用";
  if (input.retryAfterSeconds && input.retryAfterSeconds > 0) return `AI 剪辑太频繁，${smartcutRetryAfterText(input.retryAfterSeconds)}`;
  if (input.config.perHour > 0 && input.config.usedThisHour >= input.config.perHour) return `本小时的 AI 剪辑次数用完了（${input.config.perHour} 次）`;
  if (input.mode === "narr" && input.hasVoice === false) return "解说成片需要一段配音：连入一个音频节点并标为人声";
  return "";
}
