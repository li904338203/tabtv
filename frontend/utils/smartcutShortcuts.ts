/**
 * 剪辑台的快捷键分发（docs/agent/smartcut SC-UX-03）：只返回动作名，不直接执行 —— 执行在剪辑台里，
 * 这里只回答「这一下按键是什么意思」，于是每条规则都能单独测。
 *
 * - 空格：播放 / 暂停；← / →：逐帧，加 Shift 逐秒；S：分割；Delete / Backspace：删除选中片段；
 *   ⌘ / Ctrl+Z：撤销，⇧⌘Z / Ctrl+Shift+Z / Ctrl+Y：重做；⌘ / Ctrl+D：复制片段。
 * - ⚠️ **焦点在可编辑区域时一律不响应**（输入框、多行文本、contenteditable……，判定复用 `isEditableTarget`）：
 *   在字幕编辑框里按退格删字，删掉的却是选中的片段 —— 这是画布上踩过的坑（CD-78）。输入法组字中同理。
 * - 字母键按物理键位（`code`）判断：中文输入法、俄文键盘下 `key` 不是 s / z / d。
 * - 只读时只放行播放与移动播放头（看得到、改不了）。
 */
import { isEditableTarget } from "./canvasShortcuts";

export type SmartcutShortcutAction =
  | "play"
  | "frame-back"
  | "frame-forward"
  | "second-back"
  | "second-forward"
  | "split"
  | "delete"
  | "undo"
  | "redo"
  | "duplicate";

export type SmartcutShortcutContext = {
  /** 只读（角色只读、被拒、结构比前端新）：编辑类快捷键一律不响应。 */
  readonly?: boolean;
  /** 有选中的片段：删除与复制要它。 */
  hasSelection?: boolean;
  /** 焦点在片段列表上（roving tabindex，SCD-53）：← / → 归它在片段之间移动，不移动播放头。 */
  clipListFocused?: boolean;
};

type KeyLike = Pick<KeyboardEvent, "key" | "code" | "metaKey" | "ctrlKey" | "shiftKey" | "altKey" | "target"> & { isComposing?: boolean };

/** 焦点在可点的控件上（按钮、单选、选项、链接）：空格是在「按」它（同导演台）。 */
function isPressable(target: EventTarget | null): boolean {
  const el = target as { closest?: (selector: string) => unknown } | null;
  return Boolean(el?.closest?.("button, a[href], summary, [role='button'], [role='radio'], [role='option'], [role='checkbox'], [role='tab']"));
}

/** 这次按键在剪辑台里的动作；没有动作时为 null。 */
export function smartcutShortcutAction(event: KeyLike, ctx: SmartcutShortcutContext = {}): SmartcutShortcutAction | null {
  if (event.isComposing || isEditableTarget(event.target)) return null;

  const modifier = event.metaKey || event.ctrlKey;
  if (modifier && !event.altKey) {
    if (ctx.readonly) return null;
    if (event.code === "KeyZ") return event.shiftKey ? "redo" : "undo";
    if (event.code === "KeyY" && !event.shiftKey) return "redo";
    if (event.code === "KeyD" && !event.shiftKey) return ctx.hasSelection ? "duplicate" : null;
    return null;
  }
  if (modifier || event.altKey) return null;

  if (event.code === "Space") return isPressable(event.target) ? null : "play";
  if (event.key === "ArrowLeft" || event.key === "ArrowRight") {
    if (ctx.clipListFocused) return null;
    const back = event.key === "ArrowLeft";
    if (event.shiftKey) return back ? "second-back" : "second-forward";
    return back ? "frame-back" : "frame-forward";
  }
  if (event.key === "Delete" || event.key === "Backspace") return !ctx.readonly && ctx.hasSelection ? "delete" : null;
  if (event.shiftKey) return null;
  if (event.code === "KeyS") return ctx.readonly ? null : "split";
  return null;
}
