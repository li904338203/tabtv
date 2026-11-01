/**
 * 画布节点的来源主体（MCPT-09）：站内 Agent 会话 / 外部 AI 客户端（MCP 授权）。
 *
 * 节点顶层有两种来源字段：
 *  - 旧的 `agent:{sessionId, stepId, …, userTouched}` —— 站内 Agent **继续只写它**（不改已上线行为）；
 *  - 新的 `origin:{kind:"mcp", grantId, clientName, userTouched}` —— MCP 建的节点只写它。
 *
 * ⚠️ 读路径**只有 `nodeOrigin` 这一个函数**（与 Go `canvas/domain.NodeOrigin` 同语义，由共享 fixture
 *    `api/fixtures/canvas-ops/009~015` 钉住）：有 `origin` 以它为准；只有 `agent` 视为 kind=agent；
 *    `userTouched` 两处**任一**为 true 即为 true（倒向保护用户）。
 * ⚠️ 两个字段都只做原样往返：不在打开画布时把 `agent` 迁成 `origin`（打开页面就改数据会在热更新下
 *    反复触发保存，且改的是用户没动过的画布）。
 *
 * 纯函数、不碰 store。
 */
import type { JsonObject, OpActor } from "~/types/canvas";

export type OriginKind = "agent" | "mcp";

export type NodeOrigin = {
  kind: OriginKind | string;
  sessionId?: string;
  grantId?: string;
  clientName?: string;
  userTouched: boolean;
};

/** 带来源字段的东西：存储节点（顶层）或运行时节点的 `data`（同名键）。 */
export type OriginCarrier = { agent?: unknown; origin?: unknown };

function isObject(value: unknown): value is JsonObject {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function idString(value: unknown): string | undefined {
  if (typeof value === "string" && value) return value;
  if (typeof value === "number" && Number.isInteger(value)) return String(value);
  return undefined;
}

/** 解析来源；没有任何（对象形状的）来源字段时返回 null。坏值当没有。 */
export function nodeOrigin(node: OriginCarrier | null | undefined): NodeOrigin | null {
  if (!node) return null;
  const agent = isObject(node.agent) ? node.agent : undefined;
  const origin = isObject(node.origin) ? node.origin : undefined;
  let out: NodeOrigin;
  if (origin) {
    out = {
      kind: typeof origin.kind === "string" ? origin.kind : "",
      sessionId: idString(origin.sessionId),
      grantId: idString(origin.grantId),
      clientName: typeof origin.clientName === "string" && origin.clientName ? origin.clientName : undefined,
      userTouched: false
    };
  } else if (agent) {
    out = { kind: "agent", sessionId: idString(agent.sessionId), userTouched: false };
  } else {
    return null;
  }
  out.userTouched = agent?.userTouched === true || origin?.userTouched === true;
  return out;
}

/** 节点是否由这个操作者创建（agent 比会话、mcp 比授权；不看 userTouched，只给 link_generation 用）。 */
export function createdByActor(node: OriginCarrier, actor: OpActor): boolean {
  const origin = nodeOrigin(node);
  if (!origin || origin.kind !== actor.kind) return false;
  if (actor.kind === "agent") return Boolean(actor.sessionId) && origin.sessionId === String(actor.sessionId);
  if (actor.kind === "mcp") return Boolean(actor.grantId) && origin.grantId === String(actor.grantId);
  return false;
}

/** 节点是否归这个操作者所有：来源主体一致且用户没动过（与 Go OwnedByActor 一致）。 */
export function ownedByActor(node: OriginCarrier, actor: OpActor): boolean {
  const origin = nodeOrigin(node);
  return Boolean(origin && !origin.userTouched) && createdByActor(node, actor);
}

/** 用户改过（任一来源字段标过）。 */
export function isUserTouched(node: OriginCarrier): boolean {
  return nodeOrigin(node)?.userTouched === true;
}

/**
 * 标记「用户改过」：`agent` / `origin` 存在哪个就写哪个（两个都在就两个都写），其余键原样保留。
 * 返回新的字段值；没有任何来源字段、或已经标过时返回 null（调用方据此不做任何写入）。
 */
export function withTouched(node: OriginCarrier): { agent?: JsonObject; origin?: JsonObject } | null {
  const agent = isObject(node.agent) ? node.agent : undefined;
  const origin = isObject(node.origin) ? node.origin : undefined;
  if (!agent && !origin) return null;
  const agentDone = !agent || agent.userTouched === true;
  const originDone = !origin || origin.userTouched === true;
  if (agentDone && originDone) return null;
  return {
    ...(agent ? { agent: { ...agent, userTouched: true } } : {}),
    ...(origin ? { origin: { ...origin, userTouched: true } } : {})
  };
}

/** 外部客户端缺名字时的说法（不编造客户端名）。 */
export const UNKNOWN_CLIENT = "外部 AI 客户端";

/** 角标上的短文字与悬停说明。 */
export function originLabel(origin: NodeOrigin | null): { mark: string; text: string; title: string } | null {
  if (!origin) return null;
  const touched = origin.userTouched ? " · 已被你修改" : "";
  if (origin.kind === "mcp") {
    const client = origin.clientName || UNKNOWN_CLIENT;
    return {
      // 空心靠样式（OriginBadge 的 touched 态）表达：⌘ 没有对应的空心字形
      mark: "⌘",
      text: client,
      title: `由外部 AI 客户端创建（${client}）${touched}`
    };
  }
  return { mark: origin.userTouched ? "✧" : "✦", text: "", title: `由 Agent 创建${touched}` };
}

/** 远端批次提示里「谁」的说法：Agent / 协作者 / 客户端名。 */
export function actorLabel(kind: string, clientName?: string | null): string {
  if (kind === "user") return "协作者";
  if (kind === "mcp") return clientName || UNKNOWN_CLIENT;
  return "Agent";
}
