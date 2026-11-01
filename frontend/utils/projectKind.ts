import type { components } from "@tabtv/api-client";
import { agentCanvasViewPath, agentChatPath } from "./agentChat";
import { sessionRunning, sessionWaitingUser } from "./agentMessages";

/**
 * 项目类型 Agent（AGT-45）的纯函数：类型筛选与记忆、卡片上的状态文案、点卡片去哪。
 *
 * 「是不是 Agent 项目」由服务端在列表查询时算（有没有任何会话），前端**只看 `agent` 字段**，
 * 不自己猜 —— 项目名、画布内容都不是判据。
 *
 * 「营销」（MKT-F-60/61、MK-17）同理**只看服务端 `origin`**：它是建项目时落的列、建后不可改，
 * 与 Agent 的「按会话实时算」口径不同。营销项目建立时必带营销会话，所以同时也是 Agent 项目。
 */

export type ProjectAgentSummary = components["schemas"]["ProjectAgentSummary"];
export type ProjectKind = "all" | "canvas" | "agent" | "marketing";

type StorageLike = Pick<Storage, "getItem" | "setItem">;

// ---------------------------------------------------------------- 类型筛选

export const PROJECT_KIND_OPTIONS: ReadonlyArray<{ code: ProjectKind; name: string }> = [
  { code: "all", name: "全部" },
  { code: "canvas", name: "画布" },
  { code: "agent", name: "Agent" },
  { code: "marketing", name: "营销" }
];

export const PROJECT_KIND_KEY = "seeaitv:project-kind";

export function normalizeProjectKind(raw: unknown): ProjectKind {
  return raw === "canvas" || raw === "agent" || raw === "marketing" ? raw : "all";
}

/** 上次的筛选：存坏了 / 读不到都按「全部」。 */
export function readProjectKind(storage: Pick<Storage, "getItem"> | undefined): ProjectKind {
  try {
    return normalizeProjectKind(storage?.getItem(PROJECT_KIND_KEY));
  } catch {
    return "all";
  }
}

export function writeProjectKind(storage: StorageLike | undefined, kind: ProjectKind) {
  try {
    storage?.setItem(PROJECT_KIND_KEY, kind);
  } catch {
    // 隐私模式下写不进去：只是下次不记得。
  }
}

/** 筛选为空时那句话：「还没有」要说清是哪一类，不然用户以为整个项目库是空的。 */
export function projectKindEmptyText(kind: ProjectKind): string {
  if (kind === "agent") return "还没有 Agent 项目，在创造页用 Agent 模式发一句话就会有";
  if (kind === "canvas") return "还没有画布项目";
  if (kind === "marketing") return "还没有营销项目";
  return "这里还没有项目";
}

// ---------------------------------------------------------------- 卡片角标

export type ProjectBadgeVariant = "agent" | "marketing";

/** 营销项目：**只看服务端 `origin`**，不看项目名、会话场景或画布内容（MK-17：后来打开营销会话不改变类型）。 */
export function isMarketingProject(project: { origin?: string | null }): boolean {
  return project.origin === "marketing";
}

/**
 * 卡片封面上显示哪一个角标；`null` = 不显示。
 *
 * 营销项目同时是 Agent 项目，但**只显示「营销」一个** —— 两个角标挤在封面左上角互相压住。
 */
export function projectBadgeVariant(project: { origin?: string | null; agent?: unknown }): ProjectBadgeVariant | null {
  if (isMarketingProject(project)) return "marketing";
  if (project.agent) return "agent";
  return null;
}

// ---------------------------------------------------------------- 会话状态

export type AgentStateTone = "running" | "waiting" | "idle" | "failed" | "ended";

/**
 * 卡片上的状态只分五档（会话下拉里的细分状态在这里没有意义）：
 * 进行中 / 等你确认 / 空闲 / 失败 / 已结束。未知状态按「空闲」——配置可以比前端新，不能显示空白。
 */
export function agentStateTone(state: string | null | undefined): AgentStateTone {
  if (!state) return "idle";
  if (sessionRunning(state)) return "running";
  if (sessionWaitingUser(state)) return "waiting";
  if (state === "failed") return "failed";
  if (state === "cancelled") return "ended";
  return "idle";
}

const TONE_TEXT: Record<AgentStateTone, string> = {
  running: "进行中",
  waiting: "等你确认",
  idle: "空闲",
  failed: "失败",
  ended: "已结束"
};

export function agentStateText(state: string | null | undefined): string {
  return TONE_TEXT[agentStateTone(state)];
}

/** 会话标题为空（刚建、还没起名）时的说法。 */
export function agentSessionTitle(agent: Pick<ProjectAgentSummary, "title"> | null | undefined): string {
  const title = agent?.title?.trim();
  return title || "未命名会话";
}

// ---------------------------------------------------------------- 点卡片去哪

/**
 * 点卡片：Agent 项目 → 全屏对话页的最近会话；普通项目 → 画布（不变）。
 */
export function projectOpenPath(project: { id?: string | null; agent?: Pick<ProjectAgentSummary, "sessionId"> | null }): string {
  const id = project.id ?? "";
  if (project.agent?.sessionId) return agentChatPath(id, project.agent.sessionId);
  return `/video/canvas/${encodeURIComponent(id)}`;
}

/**
 * 卡片菜单「打开画布」（只对 Agent 项目出现）：带上最近会话，画布右侧面板展开在同一个会话上 ——
 * 与对话页「画布 ↗」同一个地址。
 */
export function projectCanvasPath(project: { id?: string | null; agent?: Pick<ProjectAgentSummary, "sessionId"> | null }): string {
  return agentCanvasViewPath(project.id ?? "", { sessionId: project.agent?.sessionId });
}
