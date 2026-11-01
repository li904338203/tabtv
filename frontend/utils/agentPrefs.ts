/**
 * Agent 模型偏好的读写（AGT-27/29，设计 02 §4.5、总体设计 §11）。纯函数。
 *
 * 偏好形状（服务端原样保存，`AgentPreferences.prefs`）：
 * `{ video:{model,ratio,resolution,duration}, image:{model}, reasoning:{model}, lastMode, ... }`
 *
 * ⚠️ **未知字段原样保留**（§5.4）：用户偏好是整份 PUT，前端不认识的键（管理端以后加的、
 *    别的客户端写的）丢掉一次就再也回不来。所以一律「按节合并」，不重建对象。
 * ⚠️ 两层：用户默认（`/agent/preferences`）< 会话级（`agent_session.prefs`）。改动默认只影响本会话，
 *    「设为默认」才写用户偏好。
 */
export type PrefKind = "video" | "image" | "reasoning";
export type AgentPrefsValue = Record<string, unknown>;

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/** 取一节（`video` / `image` / `reasoning`）；没有或不是对象时为空对象。 */
export function prefSection(prefs: AgentPrefsValue | null | undefined, kind: PrefKind): Record<string, unknown> {
  const value = prefs?.[kind];
  return isObject(value) ? value : {};
}

/** 所选模型；未自定义（智能匹配）为 undefined。 */
export function prefModel(prefs: AgentPrefsValue | null | undefined, kind: PrefKind): string | undefined {
  const model = prefSection(prefs, kind).model;
  return typeof model === "string" && model ? model : undefined;
}

/** 按节合并：`override` 的每一节浅合并进 `base` 的同一节，其余键原样保留。 */
export function mergePrefs(base: AgentPrefsValue | null | undefined, override: AgentPrefsValue | null | undefined): AgentPrefsValue {
  const out: AgentPrefsValue = { ...(base ?? {}) };
  for (const [key, value] of Object.entries(override ?? {})) {
    out[key] = isObject(value) && isObject(out[key]) ? { ...(out[key] as Record<string, unknown>), ...value } : value;
  }
  return out;
}

/**
 * 生效的偏好。会话级偏好（与新会话草稿）是**完整快照**而不是差异：建会话时前端把当时生效的整份偏好写进去，
 * 之后的改动也整份写回。所以有会话级的就用它，空的才回落到用户默认。
 *
 * ⚠️ 不能「用户默认 + 会话级」按节合并：用户在本会话里把视频模型改回「智能匹配」（去掉 model 字段）时，
 *    合并会把用户默认里的那个模型又带回来 —— 他点了智能匹配，按钮上却还是那个模型。
 */
export function effectivePrefs(userPrefs: AgentPrefsValue | null | undefined, layer: AgentPrefsValue | null | undefined): AgentPrefsValue {
  return layer && Object.keys(layer).length > 0 ? layer : { ...(userPrefs ?? {}) };
}

/** 改一节里的若干字段；值为 undefined 表示去掉该字段（如模型改回「智能匹配」）。 */
export function withPrefFields(prefs: AgentPrefsValue | null | undefined, kind: PrefKind, fields: Record<string, unknown>): AgentPrefsValue {
  const merged = { ...prefSection(prefs, kind), ...fields };
  const section = Object.fromEntries(Object.entries(merged).filter(([, value]) => value !== undefined));
  return { ...(prefs ?? {}), [kind]: section };
}

/**
 * 实际要用的推理模型：所选的若仍在可选列表里且可用就用它，否则用列表里第一个可用的。
 *
 * ⚠️ 服务端在会话偏好没有 `reasoning.model` 时会兜底取「按付费主体会员等级报得出价的第一个」（04 文档 P-13），
 *    但前端仍然显式给出一个：按钮上要显示用的是哪个、价格多少；所选模型被下架 / 变成需要会员时退回第一个可用的，
 *    而不是带着它去发一轮必然失败的请求。
 */
export function resolveReasoningModel(
  prefs: AgentPrefsValue | null | undefined,
  options: Array<{ code: string; available: boolean }>
): string | undefined {
  const chosen = prefModel(prefs, "reasoning");
  if (chosen && options.some(option => option.code === chosen && option.available)) return chosen;
  return options.find(option => option.available)?.code;
}

/** 偏好按钮文案（设计 02 §4.5）：未自定义视频模型时「智能匹配模型｜创作偏好」，否则显示所选视频模型名。 */
export function prefButtonText(prefs: AgentPrefsValue | null | undefined, videoOptions: Array<{ code: string; name: string }>): string {
  const code = prefModel(prefs, "video");
  if (!code) return "智能匹配模型｜创作偏好";
  return videoOptions.find(option => option.code === code)?.name ?? code;
}
