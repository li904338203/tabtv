import { describe, expect, it } from "vitest";
import {
  canSubmitContent,
  chipRemoveLabel,
  chipText,
  chipTitle,
  chipToRemoveOnBackspace,
  detectTrigger,
  enterAction,
  hasSkillChip,
  moveIndex,
  orderChips,
  shouldSend,
  type ComposerChip
} from "./composerKeys";

const ready = { hasText: true, hasRefs: false, uploading: false };

describe("回车判定", () => {
  it("组字中回车不发送", () => {
    expect(shouldSend({ key: "Enter", isComposing: true }, ready)).toBe(false);
    expect(shouldSend({ key: "Enter", keyCode: 229 }, ready)).toBe(false);
    // 部分浏览器最后一次 keydown 上 isComposing 已是 false：靠组件自己记的状态兜住
    expect(shouldSend({ key: "Enter", isComposing: false, composing: true }, ready)).toBe(false);
    expect(shouldSend({ key: "Enter", keyCode: 13 }, ready)).toBe(true);
  });

  it("Shift 回车换行", () => {
    expect(enterAction({ key: "Enter", shiftKey: true })).toBe("newline");
    expect(shouldSend({ key: "Enter", shiftKey: true }, ready)).toBe(false);
    expect(enterAction({ key: "a" })).toBe("none");
  });

  it("空内容无附件不发送", () => {
    expect(shouldSend({ key: "Enter" }, { hasText: false, hasRefs: false, uploading: false })).toBe(false);
    expect(shouldSend({ key: "Enter" }, { hasText: false, hasRefs: true, uploading: false })).toBe(true);
  });

  it("上传中不发送", () => {
    expect(canSubmitContent({ hasText: true, hasRefs: true, uploading: true })).toBe(false);
    expect(canSubmitContent({ ...ready, disabled: true })).toBe(false);
  });
});

describe("detectTrigger", () => {
  it("开头的 / 打开技能选择，选过技能后不再触发", () => {
    expect(detectTrigger("/故事")).toEqual({ kind: "/", query: "故事", start: 0 });
    expect(detectTrigger("  /")).toEqual({ kind: "/", query: "", start: 2 });
    expect(detectTrigger("/故事", { skillSelected: true })).toBeNull();
    expect(detectTrigger("和/或")).toBeNull();
    expect(detectTrigger("/故事 选题")).toBeNull();
  });

  it("@ 在开头或空白之后触发，邮箱不触发", () => {
    expect(detectTrigger("看看 @sho")).toEqual({ kind: "@", query: "sho", start: 3 });
    expect(detectTrigger("@")).toEqual({ kind: "@", query: "", start: 0 });
    expect(detectTrigger("a@b.com")).toBeNull();
    expect(detectTrigger("@shot 继续")).toBeNull();
  });

  it("moveIndex 循环移动", () => {
    expect(moveIndex(-1, 1, 3)).toBe(0);
    expect(moveIndex(-1, -1, 3)).toBe(2);
    expect(moveIndex(2, 1, 3)).toBe(0);
    expect(moveIndex(0, -1, 3)).toBe(2);
    expect(moveIndex(0, 1, 0)).toBe(-1);
  });
});

describe("胶囊（MKT-T-11）", () => {
  const atStart = { key: "Backspace", selectionStart: 0, selectionEnd: 0, composing: false };

  it("只传一个技能胶囊时行为逐字不变（MKR-9）", () => {
    const chips: ComposerChip[] = [{ dim: "skill", code: "story", name: "故事短片" }];
    // 显示：技能只显示名；悬停与删除按钮的说法沿用单技能时代
    expect(chipText(chips[0]!)).toBe("故事短片");
    expect(chipTitle(chips[0]!)).toBe("技能：故事短片");
    expect(chipRemoveLabel("skill")).toBe("移除技能");
    // `/` 触发：有技能胶囊 ≡ 旧的 skillSelected=true
    expect(hasSkillChip(chips)).toBe(true);
    expect(detectTrigger("/故事", { skillSelected: hasSkillChip(chips) })).toBeNull();
    expect(hasSkillChip([])).toBe(false);
    expect(hasSkillChip(null)).toBe(false);
    expect(detectTrigger("/故事", { skillSelected: hasSkillChip([]) })).toEqual({ kind: "/", query: "故事", start: 0 });
    // 退格：光标在最前面删技能；不在最前面 / 有选区 / 组字中 / 别的键 一律不删
    expect(chipToRemoveOnBackspace(chips, atStart)).toBe("skill");
    expect(chipToRemoveOnBackspace(chips, { ...atStart, selectionStart: 1, selectionEnd: 1 })).toBeNull();
    expect(chipToRemoveOnBackspace(chips, { ...atStart, selectionEnd: 3 })).toBeNull();
    expect(chipToRemoveOnBackspace(chips, { ...atStart, composing: true })).toBeNull();
    expect(chipToRemoveOnBackspace(chips, { ...atStart, key: "Delete" })).toBeNull();
    expect(chipToRemoveOnBackspace([], atStart)).toBeNull();
  });

  it("多个胶囊按 技能 → 创意 → Hook → 风格 排，退格删最后一个", () => {
    const chips: ComposerChip[] = [
      { dim: "style", code: "s", name: "胶片" },
      { dim: "hook", code: "h", name: "反问开场" },
      { dim: "creative", code: "c", name: "开箱" },
      { dim: "skill", code: "k", name: "带货短视频" }
    ];
    expect(orderChips(chips).map(chip => chip.dim)).toEqual(["skill", "creative", "hook", "style"]);
    expect(orderChips(chips).map(chipText)).toEqual(["带货短视频", "创意：开箱", "Hook：反问开场", "风格：胶片"]);
    expect(chipToRemoveOnBackspace(chips, atStart)).toBe("style");
    expect(chipToRemoveOnBackspace(chips.filter(chip => chip.dim !== "style" && chip.dim !== "hook"), atStart)).toBe("creative");
    expect(hasSkillChip(chips.filter(chip => chip.dim !== "skill"))).toBe(false);
  });
});
