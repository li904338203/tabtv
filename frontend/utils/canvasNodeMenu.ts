/**
 * 画布节点的右键菜单（2026-10-30 用户要求：节点右上角的复制 / 删除图标去掉，挪进右键菜单；文本节点照参考产品）。
 *
 * - 所有节点：复制节点（⌘C）、创建副本（⌘D）、粘贴（⌘V，复制过节点才可用）、删除（⌘⌫）；有产出的图片 / 视频 / 音频加「下载」。
 * - 有生成结果的文本 / 图片 / 视频 / 音频：评级（1~5 星，整个工作区共用）与「保存到我的资产」（已存过显示「已保存到我的资产」）；
 *   没有生成结果（手写文本、还没生成）时这两项灰着并说明。「创建主体」不做（用户决定 2026-10-30）。
 * - 文本节点另有：下载（.md）、复制文本（纯文字）、复制到剪贴板（Markdown 原文）。
 * - 「问 Agent」这一版不出现（用户要求隐藏）。
 */
import type { CanvasNodeData } from "~/types/canvas";
import { editedGenerationText } from "./canvas";

export type NodeMenuAction =
  | "rate" | "save-asset"
  | "download" | "copy-node" | "copy-text" | "duplicate" | "paste" | "delete" | "copy-markdown";

export type NodeMenuItem =
  | { kind: "item"; action: NodeMenuAction; label: string; shortcut?: string; disabled?: boolean; hint?: string }
  | { kind: "separator" };

const MEDIA_TYPES = new Set(["image", "video", "audio"]);
const RATABLE_TYPES = new Set(["text", "image", "video", "audio"]);

/** 文本节点卡片上此刻显示的 Markdown 原文：改过的生成结果 → 生成结果 → 手写内容。 */
export function textNodeMarkdown(data: Pick<CanvasNodeData, "params" | "generation">): string {
  const generation = data.generation;
  if (generation?.outputText !== undefined) return editedGenerationText(data.params, generation.id) ?? generation.outputText;
  return typeof data.params.text === "string" ? data.params.text : "";
}

/** Markdown → 纯文字（复制文本用）：去掉标记符号，保留换行与列表的层次感。 */
export function markdownPlainText(markdown: string): string {
  return markdown
    .replace(/```[^\n]*\n([\s\S]*?)```/g, "$1")
    .replace(/^#{1,6}\s+/gm, "")
    .replace(/^\s*>\s?/gm, "")
    .replace(/^(\s*)[-*+]\s+/gm, "$1• ")
    .replace(/!\[([^\]]*)\]\([^)]*\)/g, "$1")
    .replace(/\[([^\]]+)\]\([^)]*\)/g, "$1")
    .replace(/(\*\*|__)(.+?)\1/g, "$2")
    .replace(/(\*|_)(.+?)\1/g, "$2")
    .replace(/~~(.+?)~~/g, "$1")
    .replace(/`([^`]+)`/g, "$1")
    .replace(/^\s*([-*_]\s*){3,}$/gm, "")
    .replace(/\n{3,}/g, "\n\n")
    .trim();
}

/** 评级与保存：只对**生成结果**（本项目里成功的那次生成）成立；手写文本、还没生成的、复制项目带来的（生成 ID 已解绑）都不行。 */
export type NodeMenuMarks = {
  /** 这次生成的 ID；没有 = 不能评级、不能保存。 */
  generationId?: string;
  saved?: boolean;
  /** 还在取评级与保存情况。 */
  loading?: boolean;
};

function marksItems(nodeType: string, marks: NodeMenuMarks | undefined): NodeMenuItem[] {
  if (!RATABLE_TYPES.has(nodeType)) return [];
  const why = marks?.generationId ? "" : "只有生成结果可以评级、保存";
  return [
    { kind: "item", action: "rate", label: "评级", disabled: Boolean(why) || marks?.loading, hint: why || "给这次生成的结果打分（再点同一颗星清除）" },
    marks?.saved
      ? { kind: "item", action: "save-asset", label: "已保存到我的资产", disabled: true, hint: "在左侧「资产」里可以看到它" }
      : { kind: "item", action: "save-asset", label: "保存到我的资产", disabled: Boolean(why) || marks?.loading, hint: why || "保存到左侧「资产」，创作资产页也会展示" },
    { kind: "separator" }
  ];
}

export function nodeMenuItems(input: { nodeType: string; hasOutput: boolean; hasText: boolean; canPaste: boolean; mac?: boolean; marks?: NodeMenuMarks }): NodeMenuItem[] {
  const mod = input.mac === false ? "Ctrl+" : "⌘";
  const del = input.mac === false ? "Delete" : "⌘⌫";
  const common: NodeMenuItem[] = [
    { kind: "item", action: "copy-node", label: "复制节点", shortcut: `${mod}C`, hint: "复制这个节点，之后可以在画布上粘贴" },
    ...(input.nodeType === "text"
      ? [{ kind: "item", action: "copy-text", label: "复制文本", disabled: !input.hasText, hint: input.hasText ? "复制去掉格式的纯文字" : "还没有内容" } as NodeMenuItem]
      : []),
    { kind: "item", action: "duplicate", label: "创建副本", shortcut: `${mod}D`, hint: "在旁边建一个一样的节点" },
    { kind: "item", action: "paste", label: "粘贴", shortcut: `${mod}V`, disabled: !input.canPaste, hint: input.canPaste ? "粘贴复制的节点" : "还没有复制节点" },
    { kind: "item", action: "delete", label: "删除", shortcut: del }
  ];
  if (input.nodeType === "text") {
    return [
      ...marksItems("text", input.marks),
      { kind: "item", action: "download", label: "下载", disabled: !input.hasText, hint: input.hasText ? "下载为 Markdown 文件" : "还没有内容" },
      ...common,
      { kind: "separator" },
      { kind: "item", action: "copy-markdown", label: "复制到剪贴板", disabled: !input.hasText, hint: input.hasText ? "复制 Markdown 原文" : "还没有内容" }
    ];
  }
  const download: NodeMenuItem[] = MEDIA_TYPES.has(input.nodeType) && input.hasOutput
    ? [{ kind: "item", action: "download", label: "下载" }, { kind: "separator" }]
    : [];
  return [...marksItems(input.nodeType, input.marks), ...download, ...common];
}
