import { describe, expect, it } from "vitest";
import { smartcutShortcutAction, type SmartcutShortcutContext } from "./smartcutShortcuts";

/** node 环境里没有 DOM：用最小的假元素（`closest` / `isContentEditable`）代替。 */
const fakeElement = (matches: string[], isContentEditable = false) => ({
  isContentEditable,
  closest: (selector: string) => (matches.some(m => selector.includes(m)) ? {} : null)
});
const body = fakeElement([]);
const input = fakeElement(["input"]);
const textarea = fakeElement(["textarea"]);
const editable = fakeElement([], true);
const button = fakeElement(["button"]);

type Init = Partial<Pick<KeyboardEvent, "key" | "code" | "metaKey" | "ctrlKey" | "shiftKey" | "altKey">> & {
  target?: unknown;
  isComposing?: boolean;
};
const press = (init: Init, ctx: SmartcutShortcutContext = { hasSelection: true }) => smartcutShortcutAction({
  key: "", code: "", metaKey: false, ctrlKey: false, shiftKey: false, altKey: false, ...init,
  target: (init.target ?? body) as EventTarget
}, ctx);

describe("剪辑台快捷键（SC-UX-03）", () => {
  it("空格播放 / 暂停；焦点在按钮上时空格归按钮", () => {
    expect(press({ key: " ", code: "Space" })).toBe("play");
    expect(press({ key: " ", code: "Space", target: button })).toBeNull();
  });

  it("← / → 逐帧，Shift 逐秒；焦点在片段列表上时归列表", () => {
    expect(press({ key: "ArrowLeft" })).toBe("frame-back");
    expect(press({ key: "ArrowRight" })).toBe("frame-forward");
    expect(press({ key: "ArrowLeft", shiftKey: true })).toBe("second-back");
    expect(press({ key: "ArrowRight", shiftKey: true })).toBe("second-forward");
    expect(press({ key: "ArrowRight" }, { clipListFocused: true })).toBeNull();
  });

  it("S 分割、Delete / Backspace 删除选中片段（没选中时不响应）", () => {
    expect(press({ key: "s", code: "KeyS" })).toBe("split");
    expect(press({ key: "Delete", code: "Delete" })).toBe("delete");
    expect(press({ key: "Backspace", code: "Backspace" })).toBe("delete");
    expect(press({ key: "Delete", code: "Delete" }, { hasSelection: false })).toBeNull();
    expect(press({ key: "S", code: "KeyS", shiftKey: true })).toBeNull();
    expect(press({ key: "s", code: "KeyS", altKey: true })).toBeNull();
  });

  it("⌘ 与 Ctrl 都认：撤销、重做（⇧⌘Z / Ctrl+Shift+Z / Ctrl+Y）、复制片段", () => {
    for (const mod of [{ metaKey: true }, { ctrlKey: true }]) {
      expect(press({ key: "z", code: "KeyZ", ...mod })).toBe("undo");
      expect(press({ key: "Z", code: "KeyZ", shiftKey: true, ...mod })).toBe("redo");
      expect(press({ key: "d", code: "KeyD", ...mod })).toBe("duplicate");
      expect(press({ key: "d", code: "KeyD", ...mod }, { hasSelection: false })).toBeNull();
    }
    expect(press({ key: "y", code: "KeyY", ctrlKey: true })).toBe("redo");
    // ⌘S 不是剪辑台的动作（浏览器自己的保存，交给它）
    expect(press({ key: "s", code: "KeyS", metaKey: true })).toBeNull();
  });

  // 中文输入法、俄文键盘下 key 不是 s / z：按物理键位判断。
  it("字母键按物理键位", () => {
    expect(press({ key: "ы", code: "KeyS" })).toBe("split");
    expect(press({ key: "я", code: "KeyZ", ctrlKey: true })).toBe("undo");
  });

  // ⚠️ 在字幕编辑框里按退格删字，删掉的却是选中的片段 —— 画布上踩过的坑（CD-78）。
  it("输入框、多行文本、contenteditable 里一律不响应；输入法组字中也不响应", () => {
    for (const target of [input, textarea, editable]) {
      expect(press({ key: "Backspace", code: "Backspace", target })).toBeNull();
      expect(press({ key: " ", code: "Space", target })).toBeNull();
      expect(press({ key: "ArrowLeft", target })).toBeNull();
      expect(press({ key: "s", code: "KeyS", target })).toBeNull();
      expect(press({ key: "z", code: "KeyZ", metaKey: true, target })).toBeNull();
      expect(press({ key: "d", code: "KeyD", ctrlKey: true, target })).toBeNull();
    }
    expect(press({ key: "s", code: "KeyS", isComposing: true })).toBeNull();
  });

  it("只读：能播放、能移动播放头，不能改", () => {
    const readonly = { readonly: true, hasSelection: true };
    expect(press({ key: " ", code: "Space" }, readonly)).toBe("play");
    expect(press({ key: "ArrowRight" }, readonly)).toBe("frame-forward");
    expect(press({ key: "s", code: "KeyS" }, readonly)).toBeNull();
    expect(press({ key: "Delete", code: "Delete" }, readonly)).toBeNull();
    expect(press({ key: "z", code: "KeyZ", metaKey: true }, readonly)).toBeNull();
    expect(press({ key: "d", code: "KeyD", ctrlKey: true }, readonly)).toBeNull();
  });

  it("其余按键没有动作", () => {
    expect(press({ key: "a", code: "KeyA" })).toBeNull();
    expect(press({ key: "Escape", code: "Escape" })).toBeNull();
  });
});
