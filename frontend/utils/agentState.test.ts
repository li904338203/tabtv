import { describe, expect, it } from "vitest";
import {
  composerAvailability,
  composerSubmitAction,
  isVisibleMessage,
  normalizeTodo,
  sendButtonMode,
  statusBar,
  type AgentSessionState,
  type StatusBarSession
} from "./agentState";

const session = (state: AgentSessionState, extra: Partial<StatusBarSession> = {}): StatusBarSession => ({
  state,
  approvedBudget: 0,
  spent: 0,
  ...extra
});

const todo = [
  { title: "确认选题", status: "done" },
  { title: "撰写创作规划", status: "in_progress" },
  { title: "生成视频", status: "pending" }
];

describe("statusBar 状态条", () => {
  it("规划中显示步骤进度", () => {
    const view = statusBar(session("planning"), todo);
    expect(view.visible).toBe(true);
    expect(view.icon).toBe("spinner");
    expect(view.text).toBe("规划中 · Step 2/3 · 撰写创作规划");
  });

  it("空闲时不出现", () => {
    expect(statusBar(session("idle"), todo).visible).toBe(false);
    expect(statusBar(null, todo).visible).toBe(false);
  });

  it("等你确认可点击跳卡片", () => {
    for (const state of ["awaiting_input", "awaiting_approval"] as const) {
      const view = statusBar(session(state, { pendingCardId: "9" }), []);
      expect(view.text).toBe("等你确认 · 点击跳到卡片");
      expect(view.jumpToCard).toBe(true);
    }
  });

  it("执行中显示已花与额度", () => {
    const view = statusBar(session("executing", { approvedBudget: 60, spent: 4 }), todo);
    expect(view.text).toBe("Step 2/3 · 撰写创作规划");
    expect(view.spentText).toBe("已花 ✦4 / 额度 ✦60");
    expect(view.warn).toBe(false);
  });

  it("额度达到 80% 警示", () => {
    expect(statusBar(session("executing", { approvedBudget: 60, spent: 48 }), []).warn).toBe(true);
    expect(statusBar(session("executing", { approvedBudget: 60, spent: 47 }), []).warn).toBe(false);
  });

  it("生成超时说比预计慢不说失败", () => {
    const start = Date.parse("2026-10-15T10:00:00Z");
    const on = statusBar(session("waiting_task", { approvedBudget: 60, spent: 60 }), [], {
      waitingSince: "2026-10-15T10:00:00Z", estimatedSeconds: 120, now: start + 60_000
    });
    expect(on.text).toBe("生成中 · 预计约 2 分钟");
    const late = statusBar(session("waiting_task"), [], {
      waitingSince: "2026-10-15T10:00:00Z", estimatedSeconds: 120, now: start + 10 * 60_000
    });
    expect(late.text).toBe("生成中 · 仍在处理，比预计慢");
    expect(late.text).not.toContain("失败");
  });

  it("不知道预计时间就不说预计", () => {
    expect(statusBar(session("waiting_task"), []).text).toBe("生成中");
  });

  it("除 failed 外任何状态的文案都不含「失败」", () => {
    const states: AgentSessionState[] = ["idle", "planning", "awaiting_input", "awaiting_approval", "executing", "waiting_task", "cancelled"];
    for (const state of states) {
      const view = statusBar(session(state, { approvedBudget: 10, spent: 9, failureReason: "上游失败" }), todo, {
        waitingSince: "2020-01-01T00:00:00Z", estimatedSeconds: 1
      });
      expect(`${view.text}${view.spentText}`).not.toContain("失败");
    }
    expect(statusBar(session("failed", { failureReason: "推理模型报价失败" }), []).text).toBe("已停止：推理模型报价失败");
  });

  it("连接断开时如实说正在重连", () => {
    const view = statusBar(session("executing"), [], { connection: "offline" });
    expect(view.text).toBe("连接已断开，正在重连");
    expect(statusBar(session("idle"), [], { connection: "offline" }).visible).toBe(false);
  });
});

describe("normalizeTodo", () => {
  it("认不出的项保留为待做，不缩小分母", () => {
    expect(normalizeTodo(["a", { weird: 1 }, { text: "c", done: true }])).toEqual([
      { title: "a", status: "todo" },
      { title: "步骤 2", status: "todo" },
      { title: "c", status: "done" }
    ]);
    expect(normalizeTodo(null)).toEqual([]);
  });
});

describe("composerAvailability 输入框可用性", () => {
  const base = { role: "editor" as const, isInitiator: true, state: "idle" as const, balance: 100, turnPrice: 2 };

  it("可以发送", () => {
    expect(composerAvailability(base)).toEqual({ canSend: true, reason: "", hint: "" });
  });

  it("禁用理由权限优先于余额", () => {
    const view = composerAvailability({ ...base, role: "viewer", balance: 0 });
    expect(view.canSend).toBe(false);
    expect(view.reason).toContain("只读");
    expect(view.shortfall).toBeUndefined();
  });

  it("非发起人不能发送", () => {
    const view = composerAvailability({ ...base, isInitiator: false, balance: 0 });
    expect(view.canSend).toBe(false);
    expect(view.reason).toContain("只有发起人能继续对话");
  });

  it("会话已取消不能发送", () => {
    expect(composerAvailability({ ...base, state: "cancelled", balance: 0 }).reason).toContain("会话已结束");
  });

  it("卡片待处理只提示不禁用", () => {
    const view = composerAvailability({ ...base, state: "awaiting_approval", pendingCardId: "7" });
    expect(view.canSend).toBe(true);
    expect(view.hint).toBe("有一张卡片等你确认");
  });

  it("余额不足写明差额", () => {
    const view = composerAvailability({ ...base, balance: 1, turnPrice: 4 });
    expect(view).toMatchObject({ canSend: false, reason: "积分不足，还差 ✦3", shortfall: 3 });
  });

  it("不知道余额时不猜", () => {
    expect(composerAvailability({ ...base, balance: undefined }).canSend).toBe(true);
  });
});

describe("sendButtonMode", () => {
  it("运行中发送键变停止", () => {
    expect(sendButtonMode("planning", false)).toBe("stop");
    expect(sendButtonMode("executing", false)).toBe("stop");
  });

  it("挂起等待时仍是发送", () => {
    expect(sendButtonMode("waiting_task", false)).toBe("send");
    expect(sendButtonMode("awaiting_approval", false)).toBe("send");
    expect(sendButtonMode(undefined, false)).toBe("send");
  });

  it("流式文本还在来时是停止", () => {
    expect(sendButtonMode("waiting_task", true)).toBe("stop");
    expect(sendButtonMode("idle", true)).toBe("send");
  });
});

describe("composerSubmitAction", () => {
  // 04 文档 P-07：运行中按回车原先会把正在跑的这一轮停掉。
  it("运行中按回车有内容就发送（排队为下一轮），不是停止", () => {
    expect(composerSubmitAction("stop", true)).toBe("send");
  });

  it("运行中按回车没内容什么都不做", () => {
    expect(composerSubmitAction("stop", false)).toBe("none");
  });

  it("平时照常发送（内容校验另有一道）", () => {
    expect(composerSubmitAction("send", true)).toBe("send");
    expect(composerSubmitAction("send", false)).toBe("send");
  });
});

describe("isVisibleMessage", () => {
  it("system_note 给用户看；system_event（写给模型的任务结局，design/03 R-8）与 tool 不显示", () => {
    expect(isVisibleMessage({ role: "system_event" })).toBe(false);
    expect(isVisibleMessage({ role: "user" })).toBe(true);
    expect(isVisibleMessage({ role: "tool" })).toBe(false);
    expect(isVisibleMessage({ role: "system_note" })).toBe(true);
  });
});
