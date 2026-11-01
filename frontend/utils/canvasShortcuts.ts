/**
 * 画布快捷键（删除节点 / 撤销重做 / 复制节点）该不该响应这一次按键。
 *
 * ⚠️ 起因（CD-78，2026-10-14 实测）：此前只排除了 `<input>` / `<textarea>`，而脚本工作台的画面描述与
 *    提示词编辑器是 **contenteditable**。在里面按退格删字，删掉的是画布上**选中的脚本节点**（打开工作台时
 *    它被选中）—— 画布随即自动保存成 0 个节点；工作台的脚本 ID 取自那个节点，于是变成空串，
 *    下一步「准备资产」打出 `/scripts//assets/reconcile`，报「scriptID 非法」。整个过程没有任何提示。
 * ⚠️ 撤销也一样：在编辑器里按 Cmd+Z 想撤销一个字，撤销的是画布。
 */

type TargetLike = { isContentEditable?: boolean; closest?: (selector: string) => unknown } | null;

/** 可编辑区域：输入框、多行文本、下拉、contenteditable（含其内部的子元素）、role=textbox。 */
const EDITABLE_SELECTOR = 'input, textarea, select, [contenteditable=""], [contenteditable="true"], [role="textbox"]';

export function isEditableTarget(target: EventTarget | TargetLike): boolean {
  const el = target as TargetLike;
  if (!el) return false;
  if (el.isContentEditable) return true;
  return typeof el.closest === "function" && Boolean(el.closest(EDITABLE_SELECTOR));
}

/**
 * @param overlayOpen 有全屏层盖在画布上（脚本工作台、素材广场……）时，画布一律不响应快捷键 ——
 *   用户看不见画布，任何对它的改动都是「静默发生」的。
 */
export function canvasShortcutsEnabled(event: Pick<KeyboardEvent, "target" | "defaultPrevented">, overlayOpen: boolean): boolean {
  if (overlayOpen || event.defaultPrevented) return false;
  return !isEditableTarget(event.target);
}

/**
 * `⌘J` / `Ctrl+J`：开关画布右侧的 Agent 面板（AGT-28，设计 03 §9）。
 *
 * ⚠️ 只判「是不是这个组合键」；**可编辑元素聚焦时不抢**由调用方先过 {@link canvasShortcutsEnabled} ——
 *    在输入框里按 ⌘J 的人可能是在用输入法或浏览器自己的快捷键，面板突然开合会把他的焦点和光标都弄丢。
 * 带 Shift / Alt 的不算（`⌘⇧J` 在部分浏览器是下载面板）。
 */
export function isAgentPanelToggle(event: Pick<KeyboardEvent, "key" | "metaKey" | "ctrlKey" | "shiftKey" | "altKey">): boolean {
  if (event.shiftKey || event.altKey) return false;
  if (!(event.metaKey || event.ctrlKey)) return false;
  return event.key.toLowerCase() === "j";
}

/** 画布视图快捷键对应的动作（2026-11-01，左下角工具条）。 */
export type CanvasViewShortcut = "tidy" | "zoomIn" | "zoomOut" | "fit";

/**
 * 整理画布 `⌥⇧F` / `Alt+Shift+F`，放大 `⌘+`，缩小 `⌘-`，适合屏幕 `⌘0`（Windows 上是 Ctrl）。
 *
 * ⚠️ 同 {@link isAgentPanelToggle}：只判组合键，可编辑元素聚焦时由调用方先挡掉 —— 那时 ⌘+ / ⌘- 照旧是浏览器自己的缩放。
 * ⚠️ Mac 上按住 Option 时 `event.key` 是变了形的字符（⌥⇧F 是「Ï」），整理画布只能认物理键位 `KeyF`。
 * ⚠️ 放大在多数键盘上是不按 Shift 的 `⌘=`（`+` 和 `=` 同一个键）；小键盘的 + − 0 也认。
 */
export function canvasViewShortcut(
  event: Pick<KeyboardEvent, "key" | "code" | "metaKey" | "ctrlKey" | "shiftKey" | "altKey">
): CanvasViewShortcut | null {
  const modifier = event.metaKey || event.ctrlKey;
  if (event.altKey && event.shiftKey && !modifier && event.code === "KeyF") return "tidy";
  if (!modifier || event.altKey) return null;
  if (event.key === "=" || event.key === "+" || event.code === "NumpadAdd") return "zoomIn";
  if (event.key === "-" || event.key === "_" || event.code === "NumpadSubtract") return "zoomOut";
  if (!event.shiftKey && (event.key === "0" || event.code === "Numpad0")) return "fit";
  return null;
}
