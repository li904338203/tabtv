/**
 * 剪辑台外壳的文案与禁用原因（docs/agent/smartcut SCT-20，SC-UX-01~05）：顶栏、工具条、轨道头都从这里取，
 * 这样「为什么点不动」只有一份说法，也能单独测。
 *
 * ⚠️ 置灰的按钮要说清为什么（title + aria-label），只读成员「点不动并知道为什么」（SC-AC-66）；
 *    禁用理由给最根本的那一条：只读排在「没有选中片段」之前 —— 否则他会去选片段，选完发现还是点不动。
 */
import type { components } from "@tabtv/api-client";
import type { SmartcutReadonlyReason, SmartcutSaveState } from "~/stores/smartcut";

type TimelineTrack = components["schemas"]["TimelineTrack"];

/** 窄于这个宽度只提示「剪辑台需要更宽的屏幕」，仍可只读查看（SC-UX-05）。 */
export const smartcutMinStudioWidth = 1024;

export type SmartcutSaveStateView = {
  label: string;
  /** `quiet` 平常 · `busy` 进行中 · `alert` 醒目（点开提示条）。 */
  tone: "quiet" | "busy" | "alert";
  /** 点一下展开提示条（冲突、离线、失败才有出路可选）。 */
  actionable: boolean;
};

const SAVE_VIEWS: Record<SmartcutSaveState, SmartcutSaveStateView> = {
  idle: { label: "已保存", tone: "quiet", actionable: false },
  dirty: { label: "未保存", tone: "quiet", actionable: false },
  saving: { label: "保存中", tone: "busy", actionable: false },
  error: { label: "保存失败", tone: "alert", actionable: true },
  conflict: { label: "有冲突", tone: "alert", actionable: true },
  offline: { label: "离线", tone: "alert", actionable: true }
};

export function smartcutSaveStateView(state: SmartcutSaveState): SmartcutSaveStateView {
  return SAVE_VIEWS[state];
}

export function smartcutReadonlyText(reason: SmartcutReadonlyReason): string {
  if (reason === "forbidden") return "你没有编辑这条时间线的权限，只能查看";
  if (reason === "role") return "你在该工作区是只读成员，只能查看";
  return "";
}

/** 撤销 / 重做按钮的禁用原因（空串 = 可用）。 */
export function smartcutHistoryDisabledReason(input: { readonlyReason: SmartcutReadonlyReason; available: boolean; kind: "undo" | "redo" }): string {
  if (input.readonlyReason) return smartcutReadonlyText(input.readonlyReason);
  if (!input.available) return input.kind === "undo" ? "没有可以撤销的操作" : "没有可以重做的操作";
  return "";
}

/** 收起 / 关闭之前要先存上；存不上时说清该怎么办（同导演台，SC-F-147）。 */
export function smartcutCloseBlocker(state: SmartcutSaveState): string | null {
  if (state === "conflict") return "时间线有冲突，请先在提示条里选一个出路";
  if (state === "offline") return "已离线，恢复网络、改动存上之后再关闭";
  if (state === "error") return "改动还没存上，请先重试保存";
  return null;
}

export type SmartcutTool = "split" | "delete-left" | "delete-right" | "delete" | "speed" | "duplicate";

/**
 * 工具条按钮的禁用原因（空串 = 可用）。`target` 是这一下要作用的片段：分割与删左右取选中的片段，没有选中时取播放头下
 * 画面轨的那一段；删除、变速、复制只认选中。
 */
export function smartcutToolDisabledReason(
  tool: SmartcutTool,
  ctx: { readonlyReason: SmartcutReadonlyReason; hasSelection: boolean; playheadInTarget: boolean }
): string {
  if (ctx.readonlyReason) return smartcutReadonlyText(ctx.readonlyReason);
  switch (tool) {
    case "split":
    case "delete-left":
    case "delete-right":
      return ctx.playheadInTarget ? "" : "把播放头移到要处理的片段中间";
    default:
      return ctx.hasSelection ? "" : "先选中一个片段";
  }
}

const ROLE_LABELS: Record<string, string> = { music: "配乐", voice: "人声", sfx: "音效" };

export function smartcutRoleLabel(role: string | null | undefined): string {
  return role ? ROLE_LABELS[role] ?? "" : "";
}

const LANG_LABELS: Record<string, string> = { zh: "中文", "zh-CN": "中文", en: "英文", ja: "日文", ko: "韩文" };

export function smartcutLangLabel(lang: string | null | undefined): string {
  if (!lang) return "";
  return LANG_LABELS[lang] ?? LANG_LABELS[lang.split("-")[0]!] ?? lang;
}

/**
 * 轨道头上显示的名字：用户起过名就用名字，否则按类型与顺序给（「画面」「音频 2 · 配乐」「字幕 · 英文」「文字 1」）。
 * `ordinal` 是同类轨道里的第几条（从 1 起）。
 */
export function smartcutTrackLabel(track: Pick<TimelineTrack, "kind" | "name" | "role" | "lang">, ordinal: number): string {
  if (track.name?.trim()) return track.name.trim();
  switch (track.kind) {
    case "video":
      return "画面";
    case "audio": {
      const role = smartcutRoleLabel(track.role);
      return role ? `音频 ${ordinal} · ${role}` : `音频 ${ordinal}`;
    }
    case "subtitle": {
      const lang = smartcutLangLabel(track.lang);
      return lang ? `字幕 · ${lang}` : `字幕 ${ordinal}`;
    }
    default:
      return `文字 ${ordinal}`;
  }
}

/** 首次打开剪辑台的三步引导（SC-UX-14）。 */
export const smartcutGuideSteps = [
  { title: "时间线怎么用", text: "拖动片段调整顺序，拖两端裁切；按 S 在播放头处分割，Delete 删除，⌘Z 撤销。" },
  { title: "AI 剪辑", text: "在画布节点的指令面板里说一句要求，AI 按你的素材剪一版，结果可以整轮撤销。" },
  { title: "导出", text: "顶栏「导出」先免费预检，再导出成片；成片可以下载、发布到画布或提交作品。" }
] as const;

/** 首次打开剪辑台的引导：AI 关着时（utils/smartcutFeatures）不讲 AI 剪辑那一步。 */
export function smartcutGuideStepsFor(aiEnabled: boolean) {
  return aiEnabled ? [...smartcutGuideSteps] : smartcutGuideSteps.filter(step => step.title !== "AI 剪辑");
}

const SNAPSHOT_REASONS: Record<string, string> = {
  ai_plan: "AI 剪辑之前",
  ai_edit: "追加指令之前",
  ai_variant: "切换方案之前",
  clip_fill: "片段生成回填之前",
  replace_changed: "替换变化的素材之前",
  subtitles: "识别字幕之前",
  translate: "翻译字幕之前",
  restore: "恢复历史版本之前"
};

/** 快照是「那一步之前」存下的：标签说清是哪一步之前（恢复它 = 撤销那一步及之后的改动）。 */
export function smartcutSnapshotLabel(reason: string): string {
  return SNAPSHOT_REASONS[reason] ?? "自动存档";
}

/**
 * 恢复之后缺素材的片段（快照引用、素材已被删除，SC-F-146）：说成「第 N 段缺素材」——
 * 用户在时间线上数得出第几段，数不出片段 ID。画面轨按顺序数，其它轨说轨道名。
 */
export function smartcutMissingText(
  timeline: { tracks: ReadonlyArray<Pick<TimelineTrack, "kind" | "name" | "role" | "lang"> & { clips: ReadonlyArray<{ id: string }> }> } | null,
  missing: ReadonlyArray<{ clipId: string }>
): string {
  if (!missing.length) return "";
  const parts = missing.map(({ clipId }) => {
    for (const [i, track] of (timeline?.tracks ?? []).entries()) {
      const index = track.clips.findIndex(clip => clip.id === clipId);
      if (index < 0) continue;
      if (track.kind === "video") return `第 ${index + 1} 段`;
      const ordinal = (timeline?.tracks ?? []).slice(0, i + 1).filter(t => t.kind === track.kind).length;
      return `${smartcutTrackLabel(track, ordinal)}第 ${index + 1} 段`;
    }
    return "一段";
  });
  return `${[...new Set(parts)].join("、")}缺素材（素材已被删除），导出前请替换或删除`;
}
