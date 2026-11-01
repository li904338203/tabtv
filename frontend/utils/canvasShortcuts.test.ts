import { describe, expect, it } from "vitest";

import { canvasShortcutsEnabled, canvasViewShortcut, isAgentPanelToggle, isEditableTarget } from "./canvasShortcuts";

/** 测试环境没有 DOM：用最小的替身模拟元素（closest 按选择器里有没有某个标签判断）。 */
function element(tag: string, opts: { contentEditable?: boolean; insideEditable?: boolean } = {}) {
  return {
    tagName: tag.toUpperCase(),
    isContentEditable: Boolean(opts.contentEditable || opts.insideEditable),
    closest: (selector: string) => {
      if (opts.contentEditable || opts.insideEditable) return {};
      return selector.split(",").map(item => item.trim()).includes(tag) ? {} : null;
    }
  };
}

describe("画布快捷键不响应编辑区域里的按键", () => {
  it("contenteditable 里的退格不删节点（工作台的画面描述 / 提示词编辑器就是它）", () => {
    // 2026-10-14 实测：在画面描述里删字，删掉的是选中的脚本节点，画布存成 0 个节点
    expect(isEditableTarget(element("div", { contentEditable: true }))).toBe(true);
    // 光标落在编辑器里的子元素（@提及的高亮块）上也算
    expect(isEditableTarget(element("mark", { insideEditable: true }))).toBe(true);
    expect(canvasShortcutsEnabled({ target: element("div", { contentEditable: true }) as never, defaultPrevented: false }, false)).toBe(false);
  });

  it("输入框、多行文本、下拉照旧排除", () => {
    for (const tag of ["input", "textarea", "select"]) expect(isEditableTarget(element(tag))).toBe(true);
  });

  it("普通区域（画布本身）照常响应", () => {
    expect(isEditableTarget(element("div"))).toBe(false);
    expect(isEditableTarget(null)).toBe(false);
    expect(canvasShortcutsEnabled({ target: element("div") as never, defaultPrevented: false }, false)).toBe(true);
  });

  it("全屏层盖在画布上时一律不响应 —— 用户看不见画布，对它的改动都是静默的", () => {
    expect(canvasShortcutsEnabled({ target: element("div") as never, defaultPrevented: false }, true)).toBe(false);
  });

  it("已被别处处理过的按键不再响应", () => {
    expect(canvasShortcutsEnabled({ target: element("div") as never, defaultPrevented: true }, false)).toBe(false);
  });
});

describe("Agent 面板开关快捷键", () => {
  const key = (init: Partial<KeyboardEvent>) => ({ key: "j", metaKey: false, ctrlKey: false, shiftKey: false, altKey: false, ...init });

  it("⌘J 与 Ctrl+J 识别为面板开关", () => {
    expect(isAgentPanelToggle(key({ metaKey: true }))).toBe(true);
    expect(isAgentPanelToggle(key({ ctrlKey: true, key: "J" }))).toBe(true);
    expect(isAgentPanelToggle(key({}))).toBe(false);
    expect(isAgentPanelToggle(key({ metaKey: true, shiftKey: true }))).toBe(false);
    expect(isAgentPanelToggle(key({ metaKey: true, key: "k" }))).toBe(false);
  });

  it("可编辑元素聚焦时快捷键不生效", () => {
    // CanvasBoard 里先过 canvasShortcutsEnabled 再判 isAgentPanelToggle：输入框 / contenteditable 里的 ⌘J 不开合面板
    const typing = { target: element("div", { contentEditable: true }) as never, defaultPrevented: false };
    expect(canvasShortcutsEnabled(typing, false)).toBe(false);
    expect(canvasShortcutsEnabled({ target: element("textarea") as never, defaultPrevented: false }, false)).toBe(false);
  });
});

describe("画布视图快捷键（2026-11-01，左下角工具条）", () => {
  const key = (init: Partial<KeyboardEvent>) => ({ key: "", code: "", metaKey: false, ctrlKey: false, shiftKey: false, altKey: false, ...init });

  it("⌥⇧F 整理画布：认物理键位（Mac 上按住 Option 时 key 是「Ï」）", () => {
    expect(canvasViewShortcut(key({ key: "Ï", code: "KeyF", altKey: true, shiftKey: true }))).toBe("tidy");
    expect(canvasViewShortcut(key({ key: "F", code: "KeyF", altKey: true, shiftKey: true }))).toBe("tidy");
    // 少一个键、多一个 ⌘ 都不是
    expect(canvasViewShortcut(key({ key: "ƒ", code: "KeyF", altKey: true }))).toBeNull();
    expect(canvasViewShortcut(key({ key: "f", code: "KeyF", altKey: true, shiftKey: true, metaKey: true }))).toBeNull();
  });

  it("⌘+ 放大：⌘= 与 ⌘⇧= 都算，小键盘的 + 也算", () => {
    expect(canvasViewShortcut(key({ key: "=", code: "Equal", metaKey: true }))).toBe("zoomIn");
    expect(canvasViewShortcut(key({ key: "+", code: "Equal", metaKey: true, shiftKey: true }))).toBe("zoomIn");
    expect(canvasViewShortcut(key({ key: "+", code: "NumpadAdd", ctrlKey: true }))).toBe("zoomIn");
  });

  it("⌘- 缩小、⌘0 适合屏幕；Windows 上是 Ctrl", () => {
    expect(canvasViewShortcut(key({ key: "-", code: "Minus", metaKey: true }))).toBe("zoomOut");
    expect(canvasViewShortcut(key({ key: "-", code: "NumpadSubtract", ctrlKey: true }))).toBe("zoomOut");
    expect(canvasViewShortcut(key({ key: "0", code: "Digit0", metaKey: true }))).toBe("fit");
    expect(canvasViewShortcut(key({ key: "0", code: "Numpad0", ctrlKey: true }))).toBe("fit");
  });

  it("不带 ⌘ / Ctrl 的、带 Alt 的、别的键都不是", () => {
    expect(canvasViewShortcut(key({ key: "=", code: "Equal" }))).toBeNull();
    expect(canvasViewShortcut(key({ key: "0", code: "Digit0" }))).toBeNull();
    expect(canvasViewShortcut(key({ key: "≠", code: "Equal", metaKey: true, altKey: true }))).toBeNull();
    expect(canvasViewShortcut(key({ key: ")", code: "Digit0", metaKey: true, shiftKey: true }))).toBeNull();
    expect(canvasViewShortcut(key({ key: "z", code: "KeyZ", metaKey: true }))).toBeNull();
  });
});
