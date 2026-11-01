/**
 * 3D 导演台编辑器的快捷键（DRT-15，DR-UX-03 / 04，dev/03 §6.3）：一次按键该做什么。纯函数，外壳只负责分发。
 *
 * 外壳在 window **捕获阶段**监听、对编辑器打开期间的**全部** keydown 调 `stopPropagation`（画布永远收不到），
 * 默认行为照常；这里只回答「这个键在编辑器里是什么动作」，不认识的返回 null（照样被拦在编辑器里）。
 *
 * ⚠️ 字母键按 `code`（物理键位）判，不按 `key`：中文输入法下 `key` 是 "Process"，W 就切不了 gizmo；
 *    走位的 WASD 本来就是按键位排的。
 * ⚠️ 焦点在可编辑区（输入框、contenteditable 等）时一律不消费：在名称框里打字、在描述里按退格，
 *    删掉的不能是场景里选中的对象（与画布 CD-78 同一个坑）。
 * ⚠️ Esc 只取消当前操作（退出走位、退出提案预览、取消拖动），**不关闭编辑器**（DR-UX-02）：
 *    关闭要先存盘，只能走「返回画布」按钮。
 */
import { isEditableTarget } from "./canvasShortcuts";
import type { DirectorTransformMode } from "~/director/engine/events";

export type DirectorShortcutContext = {
  /** 自由走位中：WASD / E / Q / Shift 交给走位，W / E / R 不切 gizmo。 */
  walking: boolean;
  /** 只读成员：改场景的键（变换模式、删除、复制、撤销重做）一律无效。 */
  readonly: boolean;
  hasSelection: boolean;
  /** AI 提案预览中：除 Esc 外都无效（预览期间场景是锁着的）。 */
  proposal: boolean;
  /**
   * 关节模式（DRT-21）：Delete / Backspace 只把当前关节恢复为预设值、**不删角色**；没选关节时什么都不做。
   * 复制也不响应（复制出来的角色不在关节模式里，用户会以为复制失败）。
   */
  jointMode?: boolean;
  jointSelected?: boolean;
  /** 时间轴上选中了一个关键帧（DRT-24）：← / → 把它挪 1 帧。 */
  keyframeSelected?: boolean;
};

export type DirectorShortcutAction =
  | { type: "walk"; code: string; down: boolean }
  | { type: "mode"; mode: DirectorTransformMode }
  | { type: "focus" }
  | { type: "delete" }
  | { type: "reset-joint" }
  | { type: "play" }
  | { type: "nudge-key"; delta: -1 | 1 }
  | { type: "duplicate" }
  | { type: "undo" }
  | { type: "redo" }
  | { type: "cancel" };

type KeyLike = Pick<KeyboardEvent, "key" | "code" | "metaKey" | "ctrlKey" | "shiftKey" | "altKey" | "target" | "type">;

/** 焦点在可点的控件上（按钮、单选、选项、链接）：空格是在「按」它。 */
function isPressable(target: EventTarget | null): boolean {
  const el = target as { closest?: (selector: string) => unknown } | null;
  return Boolean(el?.closest?.("button, a[href], summary, [role='button'], [role='radio'], [role='option'], [role='checkbox'], [role='tab']"));
}

/** 走位用的键（物理键位）。Shift 是加速。 */
export const DIRECTOR_WALK_CODES: ReadonlySet<string> = new Set([
  "KeyW", "KeyA", "KeyS", "KeyD", "KeyE", "KeyQ", "ShiftLeft", "ShiftRight"
]);

const MODE_CODES: Readonly<Record<string, DirectorTransformMode>> = { KeyW: "translate", KeyE: "rotate", KeyR: "scale" };

/**
 * 这次按键在编辑器里的动作；没有动作时为 null。keydown 与 keyup 都交给它：keyup 只对走位键有意义（松开）。
 */
export function directorShortcutAction(event: KeyLike, ctx: DirectorShortcutContext): DirectorShortcutAction | null {
  if (isEditableTarget(event.target)) return null;
  const down = event.type !== "keyup";

  if (event.key === "Escape") return down ? { type: "cancel" } : null;
  if (ctx.proposal) return null;

  if (ctx.walking && DIRECTOR_WALK_CODES.has(event.code) && !event.metaKey && !event.ctrlKey && !event.altKey) {
    return { type: "walk", code: event.code, down };
  }
  if (!down) return null;

  const modifier = event.metaKey || event.ctrlKey;
  if (modifier && !event.altKey) {
    if (ctx.readonly) return null;
    if (event.code === "KeyZ") return event.shiftKey ? { type: "redo" } : { type: "undo" };
    if (event.code === "KeyY" && !event.shiftKey) return { type: "redo" };
    if (event.code === "KeyD" && !event.shiftKey && ctx.hasSelection && !ctx.jointMode) return { type: "duplicate" };
    return null;
  }
  if (modifier || event.altKey) return null;

  // 空格切换播放（DR-UX-03）：只读成员也能看动画。焦点在按钮、选项上时空格归它（键盘用户按空格是在点那个按钮）
  if (event.code === "Space") return isPressable(event.target) ? null : { type: "play" };
  if ((event.key === "ArrowLeft" || event.key === "ArrowRight") && ctx.keyframeSelected && !ctx.readonly && !event.shiftKey) {
    return { type: "nudge-key", delta: event.key === "ArrowLeft" ? -1 : 1 };
  }

  if (event.key === "Delete" || event.key === "Backspace") {
    if (ctx.readonly) return null;
    if (ctx.jointMode) return ctx.jointSelected ? { type: "reset-joint" } : null;
    return ctx.hasSelection ? { type: "delete" } : null;
  }
  if (event.shiftKey) return null;
  const mode = MODE_CODES[event.code];
  if (mode) return ctx.readonly || ctx.walking ? null : { type: "mode", mode };
  if (event.code === "KeyF") return ctx.hasSelection ? { type: "focus" } : null;
  return null;
}
