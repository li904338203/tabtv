import { describe, expect, it } from "vitest";
import { directorShortcutAction, type DirectorShortcutContext } from "./directorShortcuts";

const ctx = (extra: Partial<DirectorShortcutContext> = {}): DirectorShortcutContext => ({ walking: false, readonly: false, hasSelection: true, proposal: false, ...extra });
const press = (code: string, key: string, extra: Partial<KeyboardEvent> & { target?: unknown } = {}) =>
  ({ code, key, metaKey: false, ctrlKey: false, shiftKey: false, altKey: false, target: null, type: "keydown", ...extra }) as unknown as KeyboardEvent;
const input = { isContentEditable: false, closest: (selector: string) => (selector.includes("input") ? {} : null) };

describe("directorShortcutAction", () => {
  it("directorShortcutAction：输入框里不消费任何键", () => {
    for (const event of [press("Backspace", "Backspace"), press("KeyW", "w"), press("KeyZ", "z", { metaKey: true }), press("Escape", "Escape")]) {
      expect(directorShortcutAction({ ...event, target: input } as unknown as KeyboardEvent, ctx())).toBeNull();
    }
    // contenteditable 也算
    expect(directorShortcutAction({ ...press("Delete", "Delete"), target: { isContentEditable: true } } as unknown as KeyboardEvent, ctx())).toBeNull();
  });

  it("directorShortcutAction：W E R 切换变换模式", () => {
    expect(directorShortcutAction(press("KeyW", "w"), ctx())).toEqual({ type: "mode", mode: "translate" });
    expect(directorShortcutAction(press("KeyE", "e"), ctx())).toEqual({ type: "mode", mode: "rotate" });
    expect(directorShortcutAction(press("KeyR", "r"), ctx())).toEqual({ type: "mode", mode: "scale" });
    // 中文输入法下 key 是 Process：按键位判照样能切
    expect(directorShortcutAction(press("KeyE", "Process"), ctx())).toEqual({ type: "mode", mode: "rotate" });
    // 带修饰键的不是切模式（⌘W 是浏览器关标签页，⇧W 不是快捷键）
    expect(directorShortcutAction(press("KeyW", "w", { metaKey: true }), ctx())).toBeNull();
    expect(directorShortcutAction(press("KeyW", "W", { shiftKey: true }), ctx())).toBeNull();
  });

  it("directorShortcutAction：走位模式下 W 交给走位而不是切模式", () => {
    expect(directorShortcutAction(press("KeyW", "w"), ctx({ walking: true }))).toEqual({ type: "walk", code: "KeyW", down: true });
    expect(directorShortcutAction(press("ShiftLeft", "Shift", { shiftKey: true }), ctx({ walking: true }))).toEqual({ type: "walk", code: "ShiftLeft", down: true });
    // 松开也要交给走位，否则会一直走下去
    expect(directorShortcutAction(press("KeyD", "d", { type: "keyup" }), ctx({ walking: true }))).toEqual({ type: "walk", code: "KeyD", down: false });
    // R 不是走位键：走位中也不切到缩放
    expect(directorShortcutAction(press("KeyR", "r"), ctx({ walking: true }))).toBeNull();
    // 非走位时 keyup 什么都不做
    expect(directorShortcutAction(press("KeyW", "w", { type: "keyup" }), ctx())).toBeNull();
  });

  it("directorShortcutAction：Esc 只取消不关闭", () => {
    expect(directorShortcutAction(press("Escape", "Escape"), ctx())).toEqual({ type: "cancel" });
    expect(directorShortcutAction(press("Escape", "Escape"), ctx({ walking: true, proposal: true, readonly: true }))).toEqual({ type: "cancel" });
    expect(directorShortcutAction(press("Escape", "Escape", { type: "keyup" }), ctx())).toBeNull();
  });

  it("directorShortcutAction：只读时 W E R 与删除无效", () => {
    const readonly = ctx({ readonly: true });
    for (const event of [press("KeyW", "w"), press("KeyE", "e"), press("KeyR", "r"), press("Delete", "Delete"), press("Backspace", "Backspace"),
      press("KeyZ", "z", { metaKey: true }), press("KeyD", "d", { ctrlKey: true })]) {
      expect(directorShortcutAction(event, readonly)).toBeNull();
    }
    // 看还是能看：聚焦选中照常
    expect(directorShortcutAction(press("KeyF", "f"), readonly)).toEqual({ type: "focus" });
  });

  it("directorShortcutAction：有提案时除 Esc 外都无效", () => {
    const proposal = ctx({ proposal: true });
    for (const event of [press("KeyW", "w"), press("Delete", "Delete"), press("KeyZ", "z", { metaKey: true }), press("KeyF", "f")]) {
      expect(directorShortcutAction(event, proposal)).toBeNull();
    }
    expect(directorShortcutAction(press("Escape", "Escape"), proposal)).toEqual({ type: "cancel" });
  });

  it("directorShortcutAction：Backspace 删除选中对象", () => {
    expect(directorShortcutAction(press("Backspace", "Backspace"), ctx())).toEqual({ type: "delete" });
    expect(directorShortcutAction(press("Delete", "Delete"), ctx())).toEqual({ type: "delete" });
    // 没有选中时不删
    expect(directorShortcutAction(press("Backspace", "Backspace"), ctx({ hasSelection: false }))).toBeNull();
  });

  it("撤销 / 重做 / 复制", () => {
    expect(directorShortcutAction(press("KeyZ", "z", { metaKey: true }), ctx())).toEqual({ type: "undo" });
    expect(directorShortcutAction(press("KeyZ", "Z", { metaKey: true, shiftKey: true }), ctx())).toEqual({ type: "redo" });
    expect(directorShortcutAction(press("KeyY", "y", { ctrlKey: true }), ctx())).toEqual({ type: "redo" });
    expect(directorShortcutAction(press("KeyD", "d", { metaKey: true }), ctx())).toEqual({ type: "duplicate" });
    expect(directorShortcutAction(press("KeyD", "d", { metaKey: true }), ctx({ hasSelection: false }))).toBeNull();
  });
  it("directorShortcutAction：关节模式下 Delete 只重置当前关节，不删角色；没选关节时什么都不做", () => {
    const joint = ctx({ jointMode: true, jointSelected: true });
    expect(directorShortcutAction(press("Delete", "Delete"), joint)).toEqual({ type: "reset-joint" });
    expect(directorShortcutAction(press("Backspace", "Backspace"), joint)).toEqual({ type: "reset-joint" });
    expect(directorShortcutAction(press("Delete", "Delete"), ctx({ jointMode: true, jointSelected: false }))).toBeNull();
    expect(directorShortcutAction(press("Delete", "Delete"), ctx({ jointMode: true, jointSelected: true, readonly: true }))).toBeNull();
    expect(directorShortcutAction(press("KeyD", "d", { metaKey: true }), joint)).toBeNull();
    // 关节模式之外照旧删除
    expect(directorShortcutAction(press("Delete", "Delete"), ctx())).toEqual({ type: "delete" });
  });
  it("空格切换播放（只读也行）；焦点在按钮上时空格归按钮；选中关键帧时 ← / → 挪 1 帧", () => {
    expect(directorShortcutAction(press("Space", " "), ctx())).toEqual({ type: "play" });
    expect(directorShortcutAction(press("Space", " "), ctx({ readonly: true }))).toEqual({ type: "play" });
    const button = { isContentEditable: false, closest: (selector: string) => (selector.includes("button") ? {} : null) };
    expect(directorShortcutAction({ ...press("Space", " "), target: button } as unknown as KeyboardEvent, ctx())).toBeNull();
    expect(directorShortcutAction(press("Space", " "), ctx({ proposal: true }))).toBeNull();
    expect(directorShortcutAction(press("ArrowLeft", "ArrowLeft"), ctx({ keyframeSelected: true }))).toEqual({ type: "nudge-key", delta: -1 });
    expect(directorShortcutAction(press("ArrowRight", "ArrowRight"), ctx({ keyframeSelected: true }))).toEqual({ type: "nudge-key", delta: 1 });
    expect(directorShortcutAction(press("ArrowRight", "ArrowRight"), ctx())).toBeNull();
    expect(directorShortcutAction(press("ArrowRight", "ArrowRight"), ctx({ keyframeSelected: true, readonly: true }))).toBeNull();
  });
});
