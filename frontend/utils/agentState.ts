/**
 * 画布 Agent 的「会话状态 → 界面怎么说 / 能不能点」（AGT-27，设计 03 §2.2 / §2.3 / §8）。纯函数。
 *
 * 面板（AGT-28）与输入框（AGT-29）只从这里取文案与可用性：
 *  - 状态条五种状态的说法；
 *  - 输入框能不能发、不能发的**最根本的那一条**理由；
 *  - 发送键是「发送」还是「停止」。
 *
 * ⚠️ 两条「错了不报错」的规则钉在测试里（`agentState.test.ts`）：
 *  1. 除 `failed` 外**任何状态的文案都不出现「失败」二字** —— 生成比预计慢不等于失败，
 *     用户是按文案行动的（会去重试，而重试要再花一次钱，GAP-14）；
 *  2. 禁用理由给**最根本的那一条**（§5.17/§5.18：运行、渲染、共享三处都踩过）：
 *     只读成员不该被引导去充值。
 */
import type { components } from "@tabtv/api-client";

export type AgentSessionState = components["schemas"]["AgentSessionState"];
export type AgentSession = components["schemas"]["AgentSession"];
export type AgentMessage = components["schemas"]["AgentMessage"];
export type AgentStep = components["schemas"]["AgentStep"];
export type AgentCard = components["schemas"]["AgentCard"];
export type AgentTurn = components["schemas"]["AgentTurn"];
export type WorkspaceRole = "owner" | "admin" | "editor" | "viewer";
export type ConnectionState = "online" | "connecting" | "offline";

/** 模型正在输出（发送键变停止）的状态。挂起等待（等卡片 / 等生成）不在其中：那时可以继续发消息。 */
const RUNNING_STATES: ReadonlySet<AgentSessionState> = new Set(["planning", "executing"]);

/** 会话在后台干活（折叠竖条转圈用）：模型在跑，或在等生成任务。 */
export function isWorking(state: AgentSessionState | undefined): boolean {
  return state === "planning" || state === "executing" || state === "waiting_task";
}

/** 会话在等用户（折叠竖条红点用）。 */
export function isAwaitingUser(state: AgentSessionState | undefined): boolean {
  return state === "awaiting_input" || state === "awaiting_approval";
}

/**
 * 消息流里**给用户看**的角色。
 *
 * - `tool` 是工具调用的原始返回（展示走步骤行，不重复显示）；
 * - `system_note` 是**只给用户看**的说明（截断提示、规划作废等；后端 loop.go 语义），不进模型上下文但要显示；
 * - `system_event` **不显示**（2026-10-25 按用户要求，design/03 R-8）：唤醒时写给模型的任务结局
 *   （「所等的 1 个任务已全部结束……」），模型靠它接着往下做；任务成败与产出在步骤行与产出卡片里已经看得到。
 */
export const VISIBLE_MESSAGE_ROLES: ReadonlySet<AgentMessage["role"]> = new Set(["user", "assistant", "system_note"]);

export function isVisibleMessage(message: Pick<AgentMessage, "role">): boolean {
  return VISIBLE_MESSAGE_ROLES.has(message.role);
}

// ---------------------------------------------------------------- 任务清单

export type TodoStatus = "done" | "doing" | "todo";
export type TodoItem = { title: string; status: TodoStatus };

const DONE_WORDS = new Set(["done", "completed", "complete", "success", "succeeded", "finished"]);
const DOING_WORDS = new Set(["doing", "in_progress", "running", "active", "current"]);

/**
 * `agent_session.todo` 是 jsonb 数组，契约里没有固定形状（写入方是 Agent 的工具）。
 * 这里只认几个常见键，认不出的项**保留并当作待做**，不丢 —— 丢掉的话「Step a/b」的分母会变小。
 */
export function normalizeTodo(raw: unknown): TodoItem[] {
  if (!Array.isArray(raw)) return [];
  return raw.map((entry, index) => {
    if (typeof entry === "string") return { title: entry, status: "todo" as const };
    const item = entry && typeof entry === "object" ? entry as Record<string, unknown> : {};
    const titleSource = item.title ?? item.text ?? item.name ?? item.content;
    const title = typeof titleSource === "string" && titleSource.trim() ? titleSource.trim() : `步骤 ${index + 1}`;
    const statusSource = typeof item.status === "string" ? item.status.toLowerCase() : "";
    const status: TodoStatus = item.done === true || DONE_WORDS.has(statusSource)
      ? "done"
      : DOING_WORDS.has(statusSource) ? "doing" : "todo";
    return { title, status };
  });
}

/** 「Step a/b 当前步骤名」：当前 = 第一个进行中的，没有则第一个未完成的。清单为空时返回 undefined。 */
export function todoProgress(todo: TodoItem[]): { index: number; total: number; title: string } | undefined {
  if (todo.length === 0) return undefined;
  let current = todo.findIndex(item => item.status === "doing");
  if (current < 0) current = todo.findIndex(item => item.status !== "done");
  if (current < 0) current = todo.length - 1;
  return { index: current + 1, total: todo.length, title: todo[current]!.title };
}

// ---------------------------------------------------------------- 状态条

export type StatusBarSession = {
  state: AgentSessionState;
  approvedBudget: number;
  spent: number;
  pendingCardId?: string | null;
  /** 最近一轮失败时服务端的原话（`AgentTurn.errorMessage`）。 */
  failureReason?: string;
};

export type StatusBarOptions = {
  /** 实时连接状态：`offline` 时状态条如实说「连接已断开，正在重连」（设计 03 §8）。 */
  connection?: ConnectionState;
  /** 正在等的生成任务从何时开始等（ISO 时间）；不知道就不说预计时间。 */
  waitingSince?: string | null;
  /** 预计耗时（秒）；不知道（服务端没给）就不说「预计约 N 分钟」—— **不猜**。 */
  estimatedSeconds?: number | null;
  now?: number;
};

export type StatusBarIcon = "spinner" | "dot" | "error" | "offline";

export type StatusBarView = {
  visible: boolean;
  icon: StatusBarIcon;
  text: string;
  /** 「已花 ✦x / 额度 ✦y」；没有已批准额度时为空串。 */
  spentText: string;
  /** 额度用到 80% 及以上：数字变警示色。 */
  warn: boolean;
  /** 点击状态条应跳到卡片（等你确认）。 */
  jumpToCard: boolean;
};

const HIDDEN: StatusBarView = { visible: false, icon: "dot", text: "", spentText: "", warn: false, jumpToCard: false };

/** 已花 / 额度。额度为 0（还没批准过规划）时不显示 —— 「已花 ✦0 / 额度 ✦0」没有信息量。 */
export function budgetText(approved: number, spent: number): { spentText: string; warn: boolean } {
  if (!(approved > 0)) return { spentText: "", warn: false };
  return { spentText: `已花 ✦${spent} / 额度 ✦${approved}`, warn: spent * 100 >= approved * 80 };
}

/** 秒 → 「约 N 分钟」（不足一分钟按 1 分钟说，避免「约 0 分钟」）。 */
function minutesText(seconds: number): string {
  return `约 ${Math.max(1, Math.round(seconds / 60))} 分钟`;
}

/**
 * 状态条（设计 03 §2.2）。只在会话非空闲时出现。
 *
 * ⚠️ `waiting_task` 超出预计时间后说「仍在处理，比预计慢」，**不说失败**：浏览器看到的超时
 *    不等于任务失败（GAP-14），说成失败的话用户会去重试、再扣一次费。
 */
export function statusBar(session: StatusBarSession | null | undefined, todoRaw: unknown, options: StatusBarOptions = {}): StatusBarView {
  if (!session) return HIDDEN;
  if (options.connection === "offline" && session.state !== "idle" && session.state !== "cancelled") {
    return { ...HIDDEN, visible: true, icon: "offline", text: "连接已断开，正在重连" };
  }
  const todo = normalizeTodo(todoRaw);
  const progress = todoProgress(todo);
  const step = progress ? `Step ${progress.index}/${progress.total} · ${progress.title}` : "";
  const budget = budgetText(session.approvedBudget, session.spent);

  switch (session.state) {
    case "planning":
      return { ...HIDDEN, visible: true, icon: "spinner", text: step ? `规划中 · ${step}` : "规划中" };
    case "awaiting_input":
    case "awaiting_approval":
      return { ...HIDDEN, visible: true, icon: "dot", text: "等你确认 · 点击跳到卡片", jumpToCard: true };
    case "executing":
      return { ...HIDDEN, visible: true, icon: "spinner", text: step || "执行中", ...budget };
    case "waiting_task": {
      let text = "生成中";
      const estimate = options.estimatedSeconds;
      if (estimate && estimate > 0) {
        const since = options.waitingSince ? Date.parse(options.waitingSince) : Number.NaN;
        const now = options.now ?? Date.now();
        const overdue = Number.isFinite(since) && now - since > estimate * 1000;
        text = overdue ? "生成中 · 仍在处理，比预计慢" : `生成中 · 预计${minutesText(estimate)}`;
      }
      return { ...HIDDEN, visible: true, icon: "spinner", text, ...budget };
    }
    case "failed": {
      const reason = (session.failureReason ?? "").trim();
      return { ...HIDDEN, visible: true, icon: "error", text: reason ? `已停止：${reason}` : "已停止：这一步失败了", ...budget };
    }
    default:
      return HIDDEN;
  }
}

// ---------------------------------------------------------------- 输入框可用性

export type ComposerAvailabilityInput = {
  /** 当前工作区里的角色；不知道（还没取到）按可写处理 —— 服务端仍会拦，前端只是体验层。 */
  role?: WorkspaceRole | null;
  /** 当前会话是不是本人发起的；还没有会话（新会话草稿）时传 true。 */
  isInitiator: boolean;
  /** 当前会话状态；还没有会话时为 undefined。 */
  state?: AgentSessionState;
  pendingCardId?: string | null;
  /** 余额；不知道时为 undefined —— **不猜**。 */
  balance?: number;
  /** 所选推理模型的每轮单价；不知道时为 undefined（不据此禁用）。 */
  turnPrice?: number;
  connection?: ConnectionState;
};

export type ComposerAvailability = {
  canSend: boolean;
  /** 不能发时为什么（悬停 / 输入框下方）；能发时为空串。 */
  reason: string;
  /** 能发但要提醒的事（如「有一张卡片等你确认」）。 */
  hint: string;
  /** 余额不足时还差多少（给「去充值」用）。 */
  shortfall?: number;
};

/**
 * 输入框能不能发（设计 03 §2.3 / §8）。理由的优先级：
 * **权限（只读 / 非发起人）> 会话已取消 > 卡片待处理（只提示不禁用）> 余额不足（写差额）**。
 *
 * ⚠️ 卡片待处理时**照样能发**：直接发文字视为对卡片的补充说明，由 Agent 决定怎么处理（设计 03 §2.3）。
 * ⚠️ 断网不禁用：发送走 HTTP，与实时连接无关；连接状态由状态条说。
 */
export function composerAvailability(input: ComposerAvailabilityInput): ComposerAvailability {
  if (input.role === "viewer") {
    return { canSend: false, reason: "你在该工作区是只读，不能与 Agent 对话", hint: "" };
  }
  if (!input.isInitiator) {
    return { canSend: false, reason: "只有发起人能继续对话，你可以新建一个会话", hint: "" };
  }
  if (input.state === "cancelled") {
    return { canSend: false, reason: "会话已结束，新建一个会话继续", hint: "" };
  }
  const hint = input.pendingCardId ? "有一张卡片等你确认" : "";
  if (input.balance !== undefined && input.turnPrice !== undefined && input.balance < input.turnPrice) {
    const shortfall = input.turnPrice - input.balance;
    return { canSend: false, reason: `积分不足，还差 ✦${shortfall}`, hint, shortfall };
  }
  return { canSend: true, reason: "", hint };
}

/**
 * 发送键是「发送」还是「停止」：模型正在输出（规划 / 执行中，或流式文本还在来）时是停止。
 * ⚠️ 挂起等待（`waiting_task`、等卡片）时是**发送**：等生成时可以追问别的事（设计 03 §2.3）。
 */
export function sendButtonMode(state: AgentSessionState | undefined, streaming: boolean): "send" | "stop" {
  if (state && RUNNING_STATES.has(state)) return "stop";
  if (streaming && state !== "cancelled" && state !== "failed" && state !== "idle") return "stop";
  return "send";
}

/**
 * 输入框「提交」（回车）该做什么（04 文档 P-07）。
 *
 * ⚠️ 运行中按回车**不是停止**：用户在输入框里敲回车，十有八九是想追加一句话，而原先会把正在跑的
 * 这一轮停掉。现在有内容就照常发送（服务端把它排队为下一轮），没内容什么都不做。停止只认停止键。
 */
export function composerSubmitAction(mode: "send" | "stop", hasContent: boolean): "send" | "none" {
  if (mode === "stop" && !hasContent) return "none";
  return "send";
}
