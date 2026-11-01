/**
 * 智能剪辑节点卡的状态与角标（docs/agent/smartcut SC-F-02、SCD-47）。输入是摘要接口的 `SmartcutSummary`。
 *
 * 七种状态：空 / 已铺好 / AI 剪辑中（带阶段）/ 已剪好未导出 / 导出中（带百分比）/ 已导出 / 失败（带原因）；
 * 两个角标：「素材有变化」（`inputsStale`）、「剪辑已改，成片未更新」（`renderStale`）。
 *
 * ⚠️ 状态与角标**只从摘要算**（SCK-13）：刷新页面、换一个端打开都一致。前端不比素材指纹、不比版本去自己判过期 ——
 *    指纹只在 Go 里算（dev/03 §11），各算一份的话两个端看到的角标会不一样。
 * 优先级：AI 剪辑中 → 导出中 → 失败 → 已导出 → 已剪好未导出 → 已铺好 → 空。
 * - 「失败」只认**针对当前版本**的那次导出：失败之后又改过时间线，那次失败已经不是最新的消息，卡片回到内容状态。
 * - 「已导出」之后再改了时间线，状态仍是已导出（成片还在、下游还能用），由「剪辑已改，成片未更新」角标说明。
 * - 取消的导出不算失败（是用户自己停的），卡片回到内容状态。
 */
import type { components } from "@tabtv/api-client";

type SmartcutSummary = components["schemas"]["SmartcutSummary"];

export type SmartcutNodeStateKind = "empty" | "laid" | "ai" | "edited" | "exporting" | "exported" | "failed";

export type SmartcutNodeBadge = { key: "inputsStale" | "renderStale"; label: string };

export type SmartcutNodeState = {
  kind: SmartcutNodeStateKind;
  /** 卡片上那行字。 */
  label: string;
  /** AI 剪辑的阶段名，或导出失败的原因。 */
  detail?: string;
  /** 导出进度（0~100），只在导出中时有。 */
  progress?: number;
  badges: SmartcutNodeBadge[];
};

/** AI 剪辑的阶段（`activeCall.stage`）→ 卡片上的说法（SC-F-02：同步素材、理解素材、规划、装配）。 */
const STAGE_LABELS: Record<string, string> = {
  sync: "同步素材",
  understand: "理解素材",
  plan: "规划",
  compile: "装配",
  apply: "装配",
  done: "装配"
};

/** 调用种类 → 卡片上的动作名。翻译也会占住时间线（写入前存快照），说清是在翻译而不是在剪。 */
const CALL_LABELS: Record<string, string> = { plan: "AI 剪辑中", edit: "AI 剪辑中", translate: "翻译字幕中" };

export function smartcutStageLabel(stage: string | undefined): string {
  return stage ? STAGE_LABELS[stage] ?? "" : "";
}

export function smartcutNodeBadges(summary: Pick<SmartcutSummary, "inputsStale" | "renderStale">): SmartcutNodeBadge[] {
  const badges: SmartcutNodeBadge[] = [];
  if (summary.inputsStale) badges.push({ key: "inputsStale", label: "素材有变化" });
  if (summary.renderStale) badges.push({ key: "renderStale", label: "剪辑已改，成片未更新" });
  return badges;
}

/** 节点卡的状态与角标。还没取到摘要（新建、离线）时按空时间线显示，不显示角标。 */
export function smartcutNodeState(summary: SmartcutSummary | null | undefined): SmartcutNodeState {
  if (!summary) return { kind: "empty", label: "空", badges: [] };
  const badges = smartcutNodeBadges(summary);
  const call = summary.activeCall;
  if (call) {
    const stage = smartcutStageLabel(call.stage);
    const action = CALL_LABELS[call.kind ?? ""] ?? "AI 剪辑中";
    return { kind: "ai", label: stage ? `${action} · ${stage}` : action, ...(stage ? { detail: stage } : {}), badges };
  }
  const render = summary.latestRender;
  if (render && (render.status === "queued" || render.status === "running")) {
    const progress = Math.max(0, Math.min(100, Math.round(render.progress)));
    return { kind: "exporting", label: render.status === "queued" ? "导出排队中" : `导出中 ${progress}%`, progress, badges };
  }
  if (render && render.status === "failed" && render.timelineVersion === summary.version) {
    const reason = render.errorMessage?.trim() || "导出失败";
    return { kind: "failed", label: `导出失败：${reason}`, detail: reason, badges };
  }
  if (render && render.status === "success") return { kind: "exported", label: "已导出", badges };
  if (summary.clipCount <= 0) return { kind: "empty", label: "空", badges };
  return summary.isArranged ? { kind: "edited", label: "已剪好，未导出", badges } : { kind: "laid", label: "已铺好", badges };
}
