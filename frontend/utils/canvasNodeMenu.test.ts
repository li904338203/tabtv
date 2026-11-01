import { describe, expect, it } from "vitest";
import { markdownPlainText, nodeMenuItems, textNodeMarkdown } from "./canvasNodeMenu";

const labels = (items: ReturnType<typeof nodeMenuItems>) => items.map(i => (i.kind === "separator" ? "—" : i.label));

describe("节点右键菜单", () => {
  const disabledOf = (items: ReturnType<typeof nodeMenuItems>) => items.filter(i => i.kind === "item" && i.disabled).map(i => i.kind === "item" && i.label);
  it("文本节点照参考产品（没有创建主体）；有生成结果时评级与保存可用", () => {
    const items = nodeMenuItems({ nodeType: "text", hasOutput: true, hasText: true, canPaste: true, marks: { generationId: "9" } });
    expect(labels(items)).toEqual(["评级", "保存到我的资产", "—", "下载", "复制节点", "复制文本", "创建副本", "粘贴", "删除", "—", "复制到剪贴板"]);
    expect(disabledOf(items)).toEqual([]);
  });
  it("已保存的显示「已保存到我的资产」且灰着；手写文本（没有生成结果）不能评级、保存", () => {
    expect(labels(nodeMenuItems({ nodeType: "text", hasOutput: true, hasText: true, canPaste: true, marks: { generationId: "9", saved: true } }))[1]).toBe("已保存到我的资产");
    const manual = nodeMenuItems({ nodeType: "text", hasOutput: false, hasText: true, canPaste: true, marks: {} });
    expect(disabledOf(manual)).toEqual(["评级", "保存到我的资产"]);
  });
  it("没有内容时下载与复制文字灰着；没复制过节点时粘贴灰着", () => {
    const items = nodeMenuItems({ nodeType: "text", hasOutput: false, hasText: false, canPaste: false });
    expect(disabledOf(items)).toEqual(["评级", "保存到我的资产", "下载", "复制文本", "粘贴", "复制到剪贴板"]);
  });
  it("媒体节点：评级与保存、下载、通用项；风格等节点只有通用项；没有问 Agent", () => {
    expect(labels(nodeMenuItems({ nodeType: "image", hasOutput: true, hasText: false, canPaste: true, marks: { generationId: "1" } })))
      .toEqual(["评级", "保存到我的资产", "—", "下载", "—", "复制节点", "创建副本", "粘贴", "删除"]);
    expect(labels(nodeMenuItems({ nodeType: "style", hasOutput: false, hasText: false, canPaste: true }))).toEqual(["复制节点", "创建副本", "粘贴", "删除"]);
    expect(labels(nodeMenuItems({ nodeType: "script", hasOutput: false, hasText: false, canPaste: true })).join()).not.toContain("Agent");
  });
});

describe("文本内容", () => {
  it("改过的生成结果优先，其次生成结果，再次手写内容", () => {
    expect(textNodeMarkdown({ params: { text: "改", textEditedFor: "1" }, generation: { id: "1", outputText: "原" } })).toBe("改");
    expect(textNodeMarkdown({ params: { text: "旧改动", textEditedFor: "1" }, generation: { id: "2", outputText: "新" } })).toBe("新");
    expect(textNodeMarkdown({ params: { text: "手写" } })).toBe("手写");
  });
  it("Markdown 转纯文字", () => {
    expect(markdownPlainText("# 标题\n\n**粗体**与*斜体*、`代码`、[链接](http://x)\n\n- 一\n- 二")).toBe("标题\n\n粗体与斜体、代码、链接\n\n• 一\n• 二");
  });
});
