/**
 * 画布 Agent 面板的消息流展示规则（AGT-28，设计 03 §3、§4）。纯函数，不碰 DOM / store / 网络。
 *
 * 组件只负责「放在哪、长什么样」，**说什么、合不合并、显示几项**全在这里 —— 这些规则散在组件里的话，
 * 每一条都只能靠浏览器走查去验，而它们恰恰是最容易悄悄错的：
 * - 相邻只读步骤合并成一行（不合并的话三次读取刷三行，真正的产出被挤出视野）；
 * - 付费步骤**不参与合并**（合并之后用户看不到「这一步花了钱」）；
 * - 页脚的「退回」单列，**不从花费里减**（减掉的话用户对不上账本流水，§5.18 第四次）；
 * - 产出**全部列出**（只给第一张等于把另外几张已经扣过费的丢了，GO-40）。
 *
 * ⚠️ 金额一律来自服务端（卡片 payload / 账本），这里只负责「怎么说」，不做任何推算。
 */
import type { components } from "@tabtv/api-client";

export type AgentMessage = components["schemas"]["AgentMessage"];
export type AgentStep = components["schemas"]["AgentStep"];
export type AgentCard = components["schemas"]["AgentCard"];
export type AgentTurn = components["schemas"]["AgentTurn"];
export type AgentTurnLedger = components["schemas"]["AgentTurnLedger"];

type Json = Record<string, unknown>;

// ---------------------------------------------------------------- 通用小工具

function obj(value: unknown): Json {
  return value && typeof value === "object" && !Array.isArray(value) ? value as Json : {};
}

function str(value: unknown): string {
  return typeof value === "string" ? value : typeof value === "number" ? String(value) : "";
}

function num(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

/** 截取前 n 个字（按码点，不切半个汉字 / emoji），超出加省略号。 */
export function clip(text: string, n: number): string {
  const chars = Array.from(text.replace(/\s+/g, " ").trim());
  return chars.length > n ? `${chars.slice(0, n).join("")}…` : chars.join("");
}

// ---------------------------------------------------------------- 步骤行

/**
 * 只读步骤：相邻的这几类合并成一行「读取了 N 份资料」。
 * ⚠️ 付费（generate）、写入（write_doc / canvas_apply）、交互（ask_user / submit_plan）一律不合并 ——
 *    用户要能一眼看到「这一步花了钱 / 改了画布 / 在等我」。
 */
export const READONLY_TOOLS: ReadonlySet<string> = new Set(["read_canvas", "list_assets", "read_doc", "read_skill_ref", "list_marketing_options", "view_media"]);
/** 付费工具（与服务端 domain 规格表一致；第二期新增的付费工具在这里补）。 */
export const PAID_TOOLS: ReadonlySet<string> = new Set(["generate"]);

export type StepIcon = "running" | "done" | "failed" | "cancelled" | "waiting";

export function stepIcon(status: AgentStep["status"]): StepIcon {
  if (status === "success") return "done";
  if (status === "failed") return "failed";
  if (status === "cancelled") return "cancelled";
  if (status === "waiting") return "waiting";
  return "running";
}

const KIND_LABELS: Record<string, string> = { video: "视频", image: "图片", audio: "音频", text: "文本" };
const MARKETING_KIND_LABELS: Record<string, string> = { creative: "创意", hook: "Hook", style: "风格" };
const OPERATION_KINDS: Array<[RegExp, string]> = [
  [/video|frame/i, "video"],
  [/img|image/i, "image"],
  [/audio|tts|voice/i, "audio"]
];

/** 生成步骤产出的是什么：优先看调用参数里的节点类型，其次按操作码猜一个大类（只用于标题措辞）。 */
export function generateKind(args: Json, display: Json): string {
  const nodeType = str(args.nodeType);
  if (KIND_LABELS[nodeType]) return nodeType;
  const op = str(args.operationCode) || str(display.operationCode);
  for (const [pattern, kind] of OPERATION_KINDS) if (pattern.test(op)) return kind;
  return "";
}

export type StepTitle = {
  /** 「技能学习」「生成视频」… */
  label: string;
  /** 行标题后半段：技能名 / 节点数 / 提示词前 20 字 / 失败原因。 */
  detail: string;
};

/**
 * 步骤行标题（设计 03 §3.3 行标题表）。
 * @param args 这一步对应的工具调用参数（从助手消息的 toolCalls 里按顺序配对；配不上时为空对象）。
 */
export function stepTitle(step: Pick<AgentStep, "tool" | "status" | "display" | "errorMessage">, args: Json = {}): StepTitle {
  const d = obj(step.display);
  const fileName = str(d.fileName) || str(d.path).split("/").pop() || "";
  let title: StepTitle;
  switch (step.tool) {
    case "load_skill":
      title = { label: "技能学习", detail: str(d.name) || str(args.code) };
      break;
    case "read_skill_ref":
      title = { label: "读取技能资料", detail: str(d.name) || str(d.skillName) };
      break;
    case "read_canvas": {
      const n = num(d.nodeCount);
      title = { label: "读取画布", detail: n === undefined ? "" : `${n} 个节点` };
      break;
    }
    case "list_assets": {
      const n = num(d.count);
      title = { label: "浏览素材", detail: n === undefined ? "" : `${n} 项` };
      break;
    }
    case "read_doc":
      title = { label: "读取文档", detail: fileName };
      break;
    // 营销会话（MKT-T-18 / 19）：浏览营销选项、查看第 N 张商品图。
    case "list_marketing_options": {
      const kind = MARKETING_KIND_LABELS[str(d.kind) || str(args.kind)] ?? "营销选项";
      const n = num(d.count);
      title = { label: `浏览${kind}`, detail: n === undefined ? "" : `${n} 项` };
      break;
    }
    case "view_media": {
      const index = num(d.index) ?? num(args.index);
      title = { label: "查看商品图", detail: index === undefined ? "" : `第 ${index} 张` };
      break;
    }
    // tabtv 不计价（TT-T-14）：报价工具只用来确认模型能不能用。
    case "quote":
      title = d.available === false
        ? { label: "检查模型可用性", detail: str(d.blockedReason) || "暂不可用" }
        : { label: "检查模型可用性", detail: "" };
      break;
    case "ask_user":
      title = { label: "向你提问", detail: str(d.title) || str(args.title) };
      break;
    case "write_doc":
      title = str(d.path) === MEMORY_DOC_PATH || str(args.path) === MEMORY_DOC_PATH
        ? { label: "更新项目记忆", detail: "" }
        : { label: "写入文档", detail: fileName || str(args.path).split("/").pop() || "" };
      break;
    case "submit_plan":
      title = { label: "提交创作规划", detail: "" };
      break;
    case "canvas_apply": {
      const added = Array.isArray(d.nodeIds) ? d.nodeIds.length : 0;
      const ops = num(d.ops);
      title = { label: "更新画布", detail: added ? `新增 ${added} 个节点` : ops ? `${ops} 项改动` : "" };
      break;
    }
    case "generate": {
      const kind = KIND_LABELS[generateKind(args, d)] ?? "";
      const prompt = str(args.prompt) || str(d.prompt);
      title = d.overBudget
        ? { label: `生成${kind}`, detail: "超出额度，等你决定是否追加" }
        : { label: `生成${kind}`, detail: prompt ? clip(prompt, 20) : str(args.title) || str(d.modelCode) };
      break;
    }
    case "cancel_task":
      title = { label: "取消任务", detail: "" };
      break;
    default:
      title = { label: step.tool, detail: "" };
  }
  if (step.status === "failed") return { label: title.label, detail: step.errorMessage || "失败" };
  return title;
}

// ---------------------------------------------------------------- 产出

export type OutputItem = { idx: number; type: string; mediaId?: string; text?: string };

export type StepOutputs = {
  /** generating = 任务还没结束（排队 / 生成中）；done / failed / cancelled 是任务结局。 */
  state: "generating" | "done" | "failed" | "cancelled";
  generationId?: string;
  nodeId?: string;
  modelCode?: string;
  items: OutputItem[];
  error?: string;
};

/**
 * 生成步骤的产出（设计 03 §3.5）。不是生成步骤或还没受理（没有 generationId）返回 undefined。
 * ⚠️ **全部列出**：`outputs` 是数组，只给 `[0]` 等于把另外几张已经扣过费的产出丢了（GO-40）。
 */
export function stepOutputs(step: Pick<AgentStep, "tool" | "status" | "display" | "generationId" | "errorMessage">): StepOutputs | undefined {
  if (!PAID_TOOLS.has(step.tool)) return undefined;
  const d = obj(step.display);
  const generationId = step.generationId || str(d.generationId) || undefined;
  if (!generationId) return undefined;
  const result = obj(d.result);
  const raw = Array.isArray(result.outputs) ? result.outputs : [];
  const items: OutputItem[] = raw.map((entry, index) => {
    const o = obj(entry);
    const item: OutputItem = { idx: num(o.idx) ?? index, type: str(o.type) || "image" };
    if (str(o.mediaId)) item.mediaId = str(o.mediaId);
    if (str(o.text)) item.text = str(o.text);
    return item;
  });
  const state: StepOutputs["state"] = step.status === "success"
    ? "done"
    : step.status === "failed"
      ? "failed"
      : step.status === "cancelled" ? "cancelled" : "generating";
  return {
    state,
    generationId,
    nodeId: str(d.nodeId) || undefined,
    modelCode: str(d.modelCode) || undefined,
    items,
    error: state === "failed" || state === "cancelled" ? step.errorMessage || str(result.error) || undefined : undefined
  };
}

/**
 * 「预计约 N 分钟」。超过预计时间说「仍在处理，比预计慢」，**绝不说失败**（GAP-14：
 * 前端不能本地判失败，任务可能下一秒就出片）。
 */
export function etaText(startedAt: string | undefined, estimateSeconds: number | undefined, now: number): string {
  if (!estimateSeconds || estimateSeconds <= 0) return "生成中";
  const started = startedAt ? Date.parse(startedAt) : Number.NaN;
  const elapsed = Number.isFinite(started) ? Math.max(0, (now - started) / 1000) : 0;
  const left = estimateSeconds - elapsed;
  if (left <= 0) return "仍在处理，比预计慢";
  return `生成中 · 预计约 ${Math.max(1, Math.ceil(left / 60))} 分钟`;
}

// ---------------------------------------------------------------- 时间线组装

export type ToolCallInfo = { id?: string; name: string; args: Json };

export type TimelineItem =
  | { kind: "user"; key: string; message: AgentMessage }
  | { kind: "assistant"; key: string; message: AgentMessage; text: string; streaming: boolean; stopped: boolean }
  | { kind: "system"; key: string; message: AgentMessage }
  | { kind: "step"; key: string; step: AgentStep; args: Json; card?: AgentCard }
  | { kind: "steps"; key: string; steps: AgentStep[] }
  | { kind: "card"; key: string; card: AgentCard }
  | { kind: "footer"; key: string; turn: AgentTurn; finishedAt: string }
  | { kind: "suggestions"; key: string; turnId: string; items: string[] };

export type TimelineInput = {
  messages: AgentMessage[];
  steps?: AgentStep[];
  cards?: AgentCard[];
  turns?: AgentTurn[];
  /** 还在流式输出、尚未落库的助手文本（messageId → 已收到的文本）。 */
  streaming?: Record<string, string>;
};

const TERMINAL_TURN = new Set(["success", "failed", "cancelled"]);

function toolCallsOf(message: AgentMessage): ToolCallInfo[] {
  if (!Array.isArray(message.toolCalls)) return [];
  return message.toolCalls.map((raw) => {
    const call = obj(raw);
    let args = obj(call.arguments);
    if (!Object.keys(args).length && typeof call.rawArguments === "string") {
      try { args = obj(JSON.parse(call.rawArguments)); } catch { args = {}; }
    }
    return { id: str(call.id) || undefined, name: str(call.name), args };
  });
}

function time(value: string | null | undefined): number {
  const t = value ? Date.parse(value) : Number.NaN;
  return Number.isFinite(t) ? t : 0;
}

/**
 * 把一页消息 / 步骤 / 卡片 / 轮次排成面板上自上而下的一列。
 *
 * - `tool` 消息是给模型看的工具结果，不显示；只有工具调用、没有文字的助手消息也不显示。
 * - `system_note`（只给用户看的说明）显示成系统通知样式；`system_event`（唤醒时写给模型的任务结局）**不显示**
 *   —— 任务成败与产出在步骤行与产出卡片里已经看得到（design/03 R-8）。
 * - 步骤按开始时间插在消息之间；步骤与助手消息里的工具调用按**同名顺序**配对，取回调用参数
 *   （生成步骤的提示词只在参数里）。
 * - 卡片挂在发出它的步骤下面；找不到那一步（不在本页）时按时间单独出现。
 * - 相邻只读步骤（同一轮、都已成功）合并成一行。
 * - 轮次结束后出一行页脚；最后一轮成功结束时，助手末尾的「接下来你可以」变成建议胶囊。
 */
export function buildTimeline(input: TimelineInput): TimelineItem[] {
  const steps = [...(input.steps ?? [])].sort((a, b) => time(a.startedAt) - time(b.startedAt));
  const cards = input.cards ?? [];
  const turns = new Map((input.turns ?? []).map(turn => [turn.id, turn]));
  const messages = [...input.messages].sort((a, b) => a.seq - b.seq);

  // 工具调用参数：同一轮内按出现顺序，与同名步骤一一配对
  const callsByTurn = new Map<string, ToolCallInfo[]>();
  for (const message of messages) {
    if (message.role !== "assistant" || !message.turnId) continue;
    const list = callsByTurn.get(message.turnId) ?? [];
    list.push(...toolCallsOf(message));
    callsByTurn.set(message.turnId, list);
  }
  const argsByStep = new Map<string, Json>();
  for (const step of steps) {
    const calls = callsByTurn.get(step.turnId);
    const index = calls?.findIndex(call => call.name === step.tool) ?? -1;
    if (calls && index >= 0) {
      argsByStep.set(step.id, calls[index]!.args);
      calls.splice(index, 1);
    }
  }

  const cardByStep = new Map<string, AgentCard>();
  const stepIds = new Set(steps.map(step => step.id));
  const looseCards: AgentCard[] = [];
  for (const card of cards) {
    if (stepIds.has(card.stepId) && !cardByStep.has(card.stepId)) cardByStep.set(card.stepId, card);
    else looseCards.push(card);
  }

  // 每一轮最后一条助手消息（停止标记、建议胶囊都挂在它上面）
  const lastAssistant = new Map<string, string>();
  for (const message of messages) {
    if (message.role === "assistant" && message.turnId && message.content.trim()) lastAssistant.set(message.turnId, message.id);
  }

  type Entry = { at: number; order: number; turnId: string | null; item: TimelineItem };
  const entries: Entry[] = [];
  let order = 0;
  const known = new Set(messages.map(message => message.id));
  const latestTurnId = [...turns.values()].sort((a, b) => time(a.createdAt) - time(b.createdAt)).at(-1)?.id;

  for (const message of messages) {
    const at = time(message.createdAt);
    if (message.role === "tool") continue;
    if (message.role === "user") {
      entries.push({ at, order: order++, turnId: message.turnId, item: { kind: "user", key: `m${message.id}`, message } });
      continue;
    }
    // 唤醒时的任务结局（「所等的 1 个任务已全部结束……」）是写给模型的：模型靠它接着往下做，不能从后端删；
    // 面板上不显示（2026-10-25 按用户要求，design/03 R-8）—— 成败与产出在步骤行与产出卡片里已经看得到。
    if (message.role === "system_event") continue;
    if (message.role === "system_note") {
      entries.push({ at, order: order++, turnId: message.turnId, item: { kind: "system", key: `m${message.id}`, message } });
      continue;
    }
    // assistant
    if (!message.content.trim()) continue;
    const turn = message.turnId ? turns.get(message.turnId) : undefined;
    const isLast = message.turnId ? lastAssistant.get(message.turnId) === message.id : false;
    let text = message.content;
    let suggestions: string[] = [];
    if (isLast && turn?.status === "success" && turn.id === latestTurnId) {
      const split = extractSuggestions(text);
      text = split.body;
      suggestions = split.suggestions;
    }
    entries.push({
      at,
      order: order++,
      turnId: message.turnId,
      item: { kind: "assistant", key: `m${message.id}`, message, text, streaming: false, stopped: isLast && turn?.errorCode === "stopped" }
    });
    if (suggestions.length && turn) {
      entries.push({ at: time(turn.finishedAt) || at, order: Number.MAX_SAFE_INTEGER - 1, turnId: turn.id, item: { kind: "suggestions", key: `s${turn.id}`, turnId: turn.id, items: suggestions } });
    }
  }

  for (const step of steps) {
    const card = cardByStep.get(step.id);
    entries.push({ at: time(step.startedAt), order: order++, turnId: step.turnId, item: { kind: "step", key: `st${step.id}`, step, args: argsByStep.get(step.id) ?? {}, ...(card ? { card } : {}) } });
  }
  for (const card of looseCards) {
    entries.push({ at: time(card.createdAt), order: order++, turnId: null, item: { kind: "card", key: `c${card.id}`, card } });
  }

  // 流式中的文本：还没落库的那条接在最后（落库之后由正式消息替换，不重复显示）
  for (const [messageId, text] of Object.entries(input.streaming ?? {})) {
    if (known.has(messageId) || !text) continue;
    entries.push({ at: Number.MAX_SAFE_INTEGER, order: order++, turnId: null, item: { kind: "assistant", key: `m${messageId}`, message: streamingMessage(messageId, text), text, streaming: true, stopped: false } });
  }

  // 页脚：已结束的轮次，排在该轮最后一项之后
  for (const turn of turns.values()) {
    // 系统轮（唤醒、作废规划）没有调用模型时不出页脚，免得刷一排空页脚
    if (!TERMINAL_TURN.has(turn.status) || (turn.trigger !== "user" && turn.creditsCost === 0 && turn.callCount === 0)) continue;
    const own = entries.filter(entry => entry.turnId === turn.id);
    if (!own.length) continue;
    const at = Math.max(...own.map(entry => entry.at), time(turn.finishedAt));
    entries.push({ at, order: Number.MAX_SAFE_INTEGER, turnId: turn.id, item: { kind: "footer", key: `f${turn.id}`, turn, finishedAt: turn.finishedAt ?? turn.createdAt } });
  }

  entries.sort((a, b) => a.at - b.at || a.order - b.order);
  return mergeReadonlySteps(entries.map(entry => entry.item));
}

function streamingMessage(id: string, text: string): AgentMessage {
  return { id, seq: Number.MAX_SAFE_INTEGER, role: "assistant", content: text, refs: [], toolCalls: null, toolCallId: null, turnId: null, cardId: null, createdAt: "" };
}

/** 相邻（同一轮、都已成功、都只读、都没挂卡片）的步骤合并成一行；单独一步保持原样。 */
export function mergeReadonlySteps(items: TimelineItem[]): TimelineItem[] {
  const out: TimelineItem[] = [];
  let run: Array<Extract<TimelineItem, { kind: "step" }>> = [];
  const flush = () => {
    if (run.length >= 2) out.push({ kind: "steps", key: `g${run[0]!.step.id}`, steps: run.map(item => item.step) });
    else out.push(...run);
    run = [];
  };
  for (const item of items) {
    const mergeable = item.kind === "step" && !item.card && item.step.status === "success" && READONLY_TOOLS.has(item.step.tool);
    if (mergeable && (!run.length || run[0]!.step.turnId === item.step.turnId)) {
      run.push(item);
      continue;
    }
    flush();
    if (mergeable) run.push(item as Extract<TimelineItem, { kind: "step" }>);
    else out.push(item);
  }
  flush();
  return out;
}

export function mergedStepsTitle(steps: AgentStep[]): string {
  return `读取了 ${steps.length} 份资料`;
}

// ---------------------------------------------------------------- 下一步建议

/**
 * 从助手末尾的「接下来你可以：」段落里取建议胶囊（设计 03 §3.8）。
 *
 * 认的形状：一行「接下来你可以」（可带冒号 / 加粗），其后连续的列表项（`- xxx`、`1. xxx`、`[xxx]`）。
 * 取到时把这一段从正文里摘掉（胶囊已经显示了，正文里再列一遍是重复）；取不到原样返回。
 */
export function extractSuggestions(text: string): { body: string; suggestions: string[] } {
  const lines = text.replace(/\r\n/g, "\n").split("\n");
  let head = -1;
  for (let i = lines.length - 1; i >= 0; i -= 1) {
    if (/^\s*(?:#+\s*)?\**\s*接下来你可以/.test(lines[i]!)) { head = i; break; }
  }
  if (head < 0) return { body: text, suggestions: [] };
  const suggestions: string[] = [];
  let end = head + 1;
  for (; end < lines.length; end += 1) {
    const line = lines[end]!.trim();
    if (!line) continue;
    const bracketed = [...line.matchAll(/\[([^\]]+)\]/g)].map(match => match[1]!.trim()).filter(Boolean);
    const listed = /^(?:[-*•]|\d+[.、)])\s*(.+)$/.exec(line)?.[1];
    if (bracketed.length && !listed) { suggestions.push(...bracketed); continue; }
    if (listed) { suggestions.push(listed.replace(/^\[(.*)\]$/, "$1").replace(/\*\*/g, "").trim()); continue; }
    break;
  }
  const trailing = lines.slice(end).join("\n").trim();
  if (!suggestions.length || trailing) return { body: text, suggestions: [] };
  return { body: lines.slice(0, head).join("\n").trimEnd(), suggestions: suggestions.filter(Boolean).slice(0, 6) };
}

// ---------------------------------------------------------------- 页脚

export type FooterView = {
  time: string;
  /** 页脚显示的完整日期时间「2026/10/25 18:22:38」（design/03 R-6）。 */
  dateTime: string;
  /** 「本轮调用模型 2 次」（悬浮提示）；没有调用时为空串。tabtv 不计价，不显示金额（TT-T-14）。 */
  calls: string;
};

function clock(value: string | null | undefined): string {
  const t = value ? new Date(value) : undefined;
  if (!t || Number.isNaN(t.getTime())) return "";
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${pad(t.getHours())}:${pad(t.getMinutes())}:${pad(t.getSeconds())}`;
}

/** 每轮页脚：完成时间与调用次数。 */
export function turnFooter(turn: Pick<AgentTurn, "callCount" | "finishedAt" | "createdAt">): FooterView {
  const at = turn.finishedAt ?? turn.createdAt;
  return { time: clock(at), dateTime: dateClock(at), calls: turn.callCount > 0 ? `本轮调用模型 ${turn.callCount} 次` : "" };
}

/** 「2026/10/25 18:22:38」；取不到时间返回空串。 */
function dateClock(value: string | null | undefined): string {
  const t = value ? new Date(value) : undefined;
  if (!t || Number.isNaN(t.getTime())) return "";
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${t.getFullYear()}/${pad(t.getMonth() + 1)}/${pad(t.getDate())} ${clock(value)}`;
}


// ---------------------------------------------------------------- 卡片

export type PlanItemView = { label: string; meta: string; blocked?: string; userFixable?: boolean };

export type PlanCardView = {
  title: string;
  docPath?: string;
  items: PlanItemView[];
  canApprove: boolean;
  /** 「确认并开始」 */
  approveText: string;
  /** 有不可用项时按钮旁边那句话 */
  blockedText?: string;
};

const UNIT_LABELS: Record<string, string> = { second: "秒", image: "张", time: "次" };

/**
 * 规划确认卡（设计 03 §3.4 C）。tabtv 不计价（TT-T-14）：只列每一项用哪个模型、做多少，不显示金额与余额；
 * 能不能确认取服务端的 `canApprove`（它同时管不可用项）。
 */
export function planCardView(payload: unknown): PlanCardView {
  const p = obj(payload);
  const items = (Array.isArray(p.items) ? p.items : []).map((raw): PlanItemView => {
    const it = obj(raw);
    const kind = KIND_LABELS[str(it.kind)] ?? str(it.kind);
    const quantity = num(it.quantity);
    const unit = UNIT_LABELS[str(it.unit)] ?? "";
    const meta = [kind, str(it.modelCode), quantity ? `${quantity}${unit ? ` ${unit}` : ""}` : ""].filter(Boolean).join(" · ");
    const blocked = str(it.blockedReason) || (str(it.blockedCode) ? "暂不可用" : "");
    return {
      label: str(it.label) || kind || "计划项",
      meta,
      ...(blocked ? { blocked, userFixable: Boolean(it.userFixable) } : {})
    };
  });
  const anyBlocked = items.some(item => item.blocked);
  const canApprove = p.canApprove === true;
  let blockedText: string | undefined;
  if (anyBlocked) blockedText = "有计划项暂不可用，请调整规划";
  else if (!canApprove) blockedText = "暂时无法确认";
  return {
    title: str(p.title) || "请确认创作规划",
    ...(str(p.docPath) ? { docPath: str(p.docPath) } : {}),
    items,
    canApprove,
    approveText: "确认并开始",
    ...(blockedText ? { blockedText } : {})
  };
}

export type BudgetCardView = { reason: string; approveText: string };

/**
 * 追加额度卡（设计 03 §3.4 D）。tabtv 全部 0 价，后端不会再发这张卡（额度永远够）；
 * 保留解析是为了历史会话里已有的卡片还能显示，金额不再展示。
 */
export function budgetCardView(payload: unknown): BudgetCardView {
  const p = obj(payload);
  return { reason: str(p.reason) || "需要追加额度才能继续", approveText: "同意继续" };
}

export const CARD_STATUS_LABELS: Record<string, string> = { submitted: "已提交", voided: "已作废", expired: "已过期" };

/** 问答卡：哪些题还没答（分页点标红用）。 */
export type QuestionAnswer = { selected: string[]; other?: string; text?: string };

export function unansweredQuestions(questions: Array<{ id: string; type: string }>, answers: Record<string, QuestionAnswer | undefined>): string[] {
  return questions.filter((q) => {
    const a = answers[q.id];
    if (!a) return true;
    if (q.type === "text") return !(a.text ?? "").trim();
    const otherFilled = a.other !== undefined && a.other.trim() !== "";
    const count = a.selected.length + (otherFilled ? 1 : 0);
    if (a.other !== undefined && !otherFilled) return true;
    return q.type === "single" ? count !== 1 : count < 1;
  }).map(q => q.id);
}

/** 问答卡的提交体（与服务端 QuestionsSubmission 逐字一致）。 */
export function questionsSubmission(questions: Array<{ id: string; type: string }>, answers: Record<string, QuestionAnswer | undefined>) {
  return {
    answers: questions.map((q) => {
      const a = answers[q.id] ?? { selected: [] };
      if (q.type === "text") return { questionId: q.id, text: (a.text ?? "").trim() };
      return { questionId: q.id, selected: a.selected, ...(a.other !== undefined ? { other: a.other.trim() } : {}) };
    })
  };
}

// ---------------------------------------------------------------- 空状态

export type EmptyNode = { id: string; nodeType: string; title?: string; hasOutput?: boolean };

/**
 * 空状态的建议胶囊（设计 03 §4）：随画布内容变化。
 * 优先级：有选中节点 → 针对它；画布上有图片 → 图生视频类；空画布 / 只有文字 → 写故事类。
 */
export function emptySuggestions(nodes: EmptyNode[], selectedIds: string[]): string[] {
  const selected = nodes.filter(node => selectedIds.includes(node.id));
  if (selected.length === 1) {
    const node = selected[0]!;
    const name = node.title ? `「${clip(node.title, 12)}」` : "选中的节点";
    if (node.nodeType === "image") return [`给${name}生成 3 个运镜版本`, `用${name}做一条 15 秒视频`, `把${name}换成另一种画风`];
    if (node.nodeType === "video") return [`给${name}配音乐和字幕`, `基于${name}续写下一段`, `把${name}换一种运镜再来一版`];
    if (node.nodeType === "text" || node.nodeType === "document") return [`把${name}扩写成分镜`, `根据${name}做一条 15 秒视频`, `润色${name}`];
    return [`说说${name}能怎么用`, "根据画布上的内容做一条 15 秒视频", "帮我写一个故事并分镜"];
  }
  if (selected.length > 1) return [`把选中的 ${selected.length} 个节点串成一条视频`, "比较选中的节点，挑一个继续做", "帮我写一个故事并分镜"];
  if (nodes.some(node => node.nodeType === "image")) return ["根据画布上的图片做一条 15 秒视频", "给画布上的图片生成 3 个运镜版本", "帮我写一个故事并分镜"];
  if (nodes.length) return ["根据画布上的内容做一条 15 秒视频", "帮我把画布上的内容整理成分镜", "帮我写一个故事并分镜"];
  return ["帮我写一个故事并分镜", "做一条 15 秒的产品宣传短片", "给我几个短视频创意方向"];
}

/** 空状态推荐词前面的小图标（键；组件按键取 lucide 图标）。 */
export type SuggestionIcon =
  "music" | "camera" | "continue" | "palette" | "link" | "compare" | "polish" | "story" | "storyboard" | "idea" | "help" | "video" | "spark";

/** 从上往下认，先中先得：「帮我写一个故事并分镜」是故事不是分镜，「给我几个短视频创意方向」是创意不是视频。 */
const SUGGESTION_ICON_RULES: Array<[RegExp, SuggestionIcon]> = [
  [/音乐|字幕/, "music"],
  [/运镜/, "camera"],
  [/续写/, "continue"],
  [/画风/, "palette"],
  [/串成/, "link"],
  [/比较/, "compare"],
  [/润色/, "polish"],
  [/故事/, "story"],
  [/分镜/, "storyboard"],
  [/创意/, "idea"],
  [/怎么用/, "help"],
  [/视频|短片/, "video"]
];

/**
 * 推荐词前面的小图标（2026-11-01 按参考图：每条一行，左图标、右箭头）。按推荐词里的关键词认，认不出给通用的 ✦。
 * 先去掉「」里的节点名再认：节点叫「运镜测试」时，「用「运镜测试」做一条 15 秒视频」仍是视频。
 * 推荐词都出自上面的 emptySuggestions，改那里的文案时顺手看一眼这张表（单测把每一条都钉住了）。
 */
export function suggestionIcon(text: string): SuggestionIcon {
  const plain = text.replace(/「[^」]*」/g, "");
  return SUGGESTION_ICON_RULES.find(([pattern]) => pattern.test(plain))?.[1] ?? "spark";
}

// ---------------------------------------------------------------- 文件

export const MEMORY_DOC_PATH = "/.memory.md";

/** 文件卡要显示的信息；不是文档步骤或没有路径返回 undefined。 */
export function stepFile(step: Pick<AgentStep, "tool" | "status" | "display">): { path: string; fileName: string; docId?: string; nodeId?: string; created: boolean } | undefined {
  if (step.tool !== "write_doc" || step.status !== "success") return undefined;
  const d = obj(step.display);
  const path = str(d.path);
  if (!path) return undefined;
  return {
    path,
    fileName: str(d.fileName) || path.split("/").pop() || path,
    ...(str(d.docId) ? { docId: str(d.docId) } : {}),
    ...(str(d.nodeId) ? { nodeId: str(d.nodeId) } : {}),
    created: d.created === true
  };
}

/** 画布更新步骤新增的节点（展开后逐个可定位）。 */
export function stepNodeIds(step: Pick<AgentStep, "display">): string[] {
  const ids = obj(step.display).nodeIds;
  return Array.isArray(ids) ? ids.map(str).filter(Boolean) : [];
}

// ---------------------------------------------------------------- 会话列表

/** 会话下拉一行的副标题：状态 + 发起人（别人发起的才写）+ 最后活动时间。 */
export const SESSION_STATE_LABELS: Record<string, string> = {
  idle: "空闲",
  planning: "规划中",
  awaiting_input: "等你回答",
  awaiting_approval: "等你确认",
  executing: "执行中",
  waiting_task: "生成中",
  failed: "已停止",
  cancelled: "已结束"
};

export function sessionRunning(state: string): boolean {
  return state === "planning" || state === "executing" || state === "waiting_task";
}

export function sessionWaitingUser(state: string): boolean {
  return state === "awaiting_input" || state === "awaiting_approval";
}

/** 相对时间：刚刚 / N 分钟前 / 今天 15:44 / 10/15 15:44。 */
export function relativeTime(value: string | null | undefined, now: number): string {
  const t = value ? Date.parse(value) : Number.NaN;
  if (!Number.isFinite(t)) return "";
  const diff = Math.max(0, now - t) / 1000;
  if (diff < 60) return "刚刚";
  if (diff < 3600) return `${Math.floor(diff / 60)} 分钟前`;
  const d = new Date(t);
  const n = new Date(now);
  const pad = (x: number) => String(x).padStart(2, "0");
  const hm = `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  if (d.toDateString() === n.toDateString()) return `今天 ${hm}`;
  return `${d.getMonth() + 1}/${d.getDate()} ${hm}`;
}
