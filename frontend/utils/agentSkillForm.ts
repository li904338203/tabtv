/**
 * Agent 技能配置的纯规则（TT-T-19，移植自来源系统管理端 `utils/agentSkillForm.ts`）。
 *
 * 与服务端 `agent/domain.ValidateSkill` 与保存前的试渲染逐条对应；前端先算一遍是为了**边写边提示**，
 * 服务端保存时还会再验一次（以服务端为准，它的 `details.issues` 一次给出全部原因）。
 *
 * ⚠️ 工具白名单**只能收窄**：状态机不允许的工具，写进白名单也不会下发（规划态永远没有付费工具）。
 *    可选项来自服务端的工具规格表（`GET /admin/agent/tools`），这里不写死工具名单。
 * ⚠️ 「付费步骤前必须经用户确认」写在代码里，技能配置里没有任何字段能关掉它 —— 步骤的「检查点」只是展示。
 * ⚠️ 保存是**整份替换**：scenes / inputSlots / prefillText / isBatch 每次都回传，漏传一项配好的槽位就被清空
 *    而接口照样 200。输入槽在这里按 JSON 编辑（来源系统是逐行表格），形状校验以服务端为准。
 */
import { promptPlaceholdersOf } from "./promptForm";

export interface SkillStep { no: number; name: string; checkpoint: boolean }
export interface SkillBindings { video: string; image: string; reasoning: string }
export type SkillScene = "general" | "marketing";

/** 服务端工具规格（白名单可选项）。 */
export interface ToolOption {
  name: string;
  paid: boolean;
  canvasWrite: boolean;
  action: string;
  planning: boolean;
  executing: boolean;
}

/** 绑定模型的候选。 */
export interface SkillModelOption { code: string; name: string; domain: string; status: string; agentCapable: boolean }

export interface SkillDraft {
  id: string;
  code: string;
  name: string;
  category: string;
  description: string;
  tags: string[];
  coverMediaId: string;
  body: string;
  steps: SkillStep[];
  tools: string[];
  bindings: SkillBindings;
  isQuickEntry: boolean;
  sort: number;
  isEnabled: boolean;
  version: number;
  scenes: SkillScene[];
  /** 输入槽声明的 JSON 文本（编辑态）；保存时解析。 */
  inputSlotsText: string;
  prefillText: string;
  isBatch: boolean;
  isBuiltin: boolean;
  isUserModified: boolean;
}

export const SKILL_CODE_RE = /^[a-z][a-z0-9_]{1,63}$/;
export const SKILL_BODY_MAX = 20000;
export const SKILL_PREFILL_MAX = 200;
export const SKILL_VERSION_CONFLICT = 40902;

export const SKILL_SCENES: Array<{ value: SkillScene; label: string }> = [
  { value: "general", label: "通用" },
  { value: "marketing", label: "营销" }
];

type Row = Record<string, unknown>;

const str = (v: unknown) => (v === undefined || v === null ? "" : String(v));
const strArr = (v: unknown) => (Array.isArray(v) ? v.map(String) : []);

/** 输入槽数组 → 编辑用的 JSON 文本（空数组显示成空串，读起来就是「没有声明」）。 */
export function slotsToText(v: unknown): string {
  return Array.isArray(v) && v.length ? JSON.stringify(v, null, 2) : "";
}

/** 编辑用的 JSON 文本 → 输入槽数组；空串 = 不声明（用场景默认槽位）。 */
export function slotsFromText(text: string): { slots: unknown[]; error: string } {
  if (!text.trim()) return { slots: [], error: "" };
  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch {
    return { slots: [], error: "输入槽不是合法的 JSON" };
  }
  if (!Array.isArray(raw)) return { slots: [], error: "输入槽必须是 JSON 数组" };
  const bad = raw.findIndex(x => typeof x !== "object" || x === null || Array.isArray(x) || typeof (x as Row).key !== "string");
  if (bad >= 0) return { slots: [], error: `输入槽第 ${bad + 1} 项要是带 key 的对象` };
  return { slots: raw, error: "" };
}

/** 接口行（列表或详情）→ 表单。新建传 null。 */
export function skillDraft(row: Row | null, nextSort = 10): SkillDraft {
  const b = (row?.modelBindings ?? {}) as Row;
  const steps = Array.isArray(row?.steps)
    ? (row!.steps as Row[]).map((s, i) => ({ no: i + 1, name: str(s.name), checkpoint: s.checkpoint === true }))
    : [];
  return {
    id: str(row?.id),
    code: str(row?.code),
    name: str(row?.name),
    category: str(row?.category),
    description: str(row?.description),
    tags: strArr(row?.tags),
    coverMediaId: str(row?.coverMediaId),
    body: str(row?.body),
    steps,
    tools: strArr(row?.tools),
    bindings: { video: str(b.video), image: str(b.image), reasoning: str(b.reasoning) },
    isQuickEntry: row?.isQuickEntry === true,
    sort: typeof row?.sort === "number" ? (row.sort as number) : nextSort,
    isEnabled: row?.isEnabled === true,
    version: typeof row?.version === "number" ? (row.version as number) : 0,
    scenes: strArr(row?.scenes) as SkillScene[],
    inputSlotsText: slotsToText(row?.inputSlots),
    prefillText: str(row?.prefillText),
    isBatch: row?.isBatch === true,
    isBuiltin: row?.isBuiltin === true,
    isUserModified: row?.isUserModified === true
  };
}

/** 步骤序号按数组顺序从 1 起重编（服务端要求连续，**不会**替你悄悄改）。 */
export function renumberSkillSteps(steps: SkillStep[]): SkillStep[] {
  return steps.map((s, i) => ({ ...s, no: i + 1 }));
}

/** 上移 / 下移一步并重编号；越界时原样返回。 */
export function moveSkillStep(steps: SkillStep[], index: number, delta: -1 | 1): SkillStep[] {
  const to = index + delta;
  if (index < 0 || index >= steps.length || to < 0 || to >= steps.length) return steps;
  const next = [...steps];
  const [it] = next.splice(index, 1);
  next.splice(to, 0, it!);
  return renumberSkillSteps(next);
}

/** 表单 → 请求体。新建带 code；更新带 version（乐观锁）。输入槽文本须先通过 slotsFromText。 */
export function skillPayload(d: SkillDraft, coverMediaId: string, mode: "create" | "update"): Row {
  const body: Row = {
    name: d.name.trim(),
    category: d.category.trim(),
    description: d.description.trim(),
    tags: d.tags.map(t => t.trim()).filter(Boolean),
    coverMediaId: coverMediaId || null,
    body: d.body,
    steps: renumberSkillSteps(d.steps).map(s => ({ no: s.no, name: s.name.trim(), checkpoint: s.checkpoint })),
    tools: d.tools,
    modelBindings: { video: d.bindings.video, image: d.bindings.image, reasoning: d.bindings.reasoning },
    isQuickEntry: d.isQuickEntry,
    sort: d.sort,
    // 原样回传（不 trim、不重排）：回读要与录入的逐字一致
    scenes: [...d.scenes],
    inputSlots: slotsFromText(d.inputSlotsText).slots,
    prefillText: d.prefillText,
    isBatch: d.isBatch
  };
  if (mode === "create") body.code = d.code.trim();
  else body.version = d.version;
  return body;
}

/** 正文（主说明 / 附属文档）的问题：未闭合占位符、用了不存在的变量。没问题返回空串。 */
export function skillBodyIssue(label: string, body: string, vars: string[]): string {
  if (!body.trim()) return `${label}不能为空`;
  if ([...body].length > SKILL_BODY_MAX) return `${label}最多 ${SKILL_BODY_MAX} 字`;
  const { names, error } = promptPlaceholdersOf(body);
  if (error) return `${label}：${error}`;
  const unknown = names.filter(n => !vars.includes(n));
  // ⚠️ 不认识的变量渲染时会报错、技能加载不上；服务端也会拒绝保存
  if (unknown.length) return `${label}用了不存在的变量：${unknown.join("、")}（可用：${vars.join("、") || "无"}）`;
  return "";
}

export function skillPrefillIssue(text: string): string {
  const n = [...text].length;
  return n > SKILL_PREFILL_MAX ? `预填描述最多 ${SKILL_PREFILL_MAX} 字，当前 ${n} 字` : "";
}

/** 已选但不在服务端规格表里的工具名（例如工具改名 / 下线后的存量配置）。 */
export function skillUnknownTools(selected: string[], tools: ToolOption[]): string[] {
  const known = new Set(tools.map(t => t.name));
  return selected.filter(n => !known.has(n));
}

/** 第一条要改的问题；没问题返回空串。 */
export function validateSkillDraft(d: SkillDraft, ctx: { creating: boolean; vars: string[]; tools: ToolOption[]; list: Row[] }): string {
  if (ctx.creating) {
    const code = d.code.trim();
    if (!SKILL_CODE_RE.test(code)) return "code 要以小写字母开头，只含小写字母、数字、下划线，2~64 位";
    if (ctx.list.some(r => r.code === code)) return `code「${code}」已存在`;
  }
  if (!d.name.trim()) return "名称不能为空";
  if (!d.category.trim()) return "分类不能为空";
  if (new Set(d.tags.map(t => t.trim())).size !== d.tags.length) return "标签有重复";
  if (d.tags.length > 10) return "标签最多 10 个";
  const issue = skillBodyIssue("主说明", d.body, ctx.vars);
  if (issue) return issue;
  const blank = d.steps.findIndex(s => !s.name.trim());
  if (blank >= 0) return `第 ${blank + 1} 步缺少名称`;
  const unknown = skillUnknownTools(d.tools, ctx.tools);
  if (unknown.length) return `工具白名单里有未注册的工具：${unknown.join("、")}`;
  const prefill = skillPrefillIssue(d.prefillText);
  if (prefill) return prefill;
  const slots = slotsFromText(d.inputSlotsText);
  if (slots.error) return slots.error;
  return "";
}

/** 一个工具「在什么时候会下发、做什么」—— 白名单只能收窄，这句话决定使用者会不会误以为能放开它。 */
export function skillToolMeaning(t: ToolOption): string {
  const when = t.planning && t.executing ? "规划与执行都可用"
    : t.executing ? "仅执行态可用（用户确认规划之后）"
      : t.planning ? "仅规划态可用" : "当前不会下发";
  const what = [t.paid ? "会产生费用" : "", t.canvasWrite ? "会改画布" : ""].filter(Boolean).join("、");
  return what ? `${when} · ${what}` : when;
}

/** 白名单的配置提醒（不拦截）：空 = 不收窄；漏了技能工具会让模型读不到附属文档 / 加载不了别的技能。 */
export function skillWhitelistNotes(selected: string[], refCount: number): string[] {
  if (!selected.length) return ["白名单为空 = 不收窄：会话按状态机允许的全部工具运行。"];
  const notes = ["加载本技能后，会话的工具收窄为「状态允许的 ∩ 白名单」；状态不允许的工具写进来也不会下发。"];
  if (refCount > 0 && !selected.includes("read_skill_ref")) notes.push("没有 read_skill_ref：加载本技能后模型读不到它的附属文档。");
  if (!selected.includes("load_skill")) notes.push("没有 load_skill：加载本技能后模型不能再加载别的技能。");
  return notes;
}

/** 绑定模型的下拉：推理只给支持 Agent 的文本模型；停用的照样列出并标明（可以先配好再启用模型）。 */
export function skillModelOptionsFor(models: SkillModelOption[], kind: keyof SkillBindings): Array<{ value: string; label: string }> {
  const fit = models.filter(m => (kind === "reasoning" ? m.domain === "text" && m.agentCapable : m.domain === kind));
  return [
    { value: "", label: kind === "reasoning" ? "不绑定（用户偏好 / 默认）" : "不绑定（智能匹配）" },
    ...fit.map(m => ({ value: m.code, label: `${m.name || m.code}（${m.code}）${m.status === "enabled" ? "" : " · 已停用"}` }))
  ];
}

/** 当前绑定在候选里找不到（模型被删、改了能力）时给出说明。 */
export function skillBindingIssue(models: SkillModelOption[], kind: keyof SkillBindings, code: string): string {
  if (!code) return "";
  const m = models.find(x => x.code === code);
  if (!m) return `「${code}」在模型目录里不存在`;
  if (kind === "reasoning" && !(m.domain === "text" && m.agentCapable)) return `「${code}」不支持 Agent，不能作推理模型`;
  if (kind !== "reasoning" && m.domain !== kind) return `「${code}」不是${kind === "video" ? "视频" : "图片"}模型`;
  return "";
}

/** 营销技能必须配封面（服务端在**启用**时强制，草稿可以先存 —— 这里只提醒）。 */
export function skillCoverNote(scenes: string[], hasCover: boolean): string {
  if (!scenes.includes("marketing") || hasCover) return "";
  return "营销场景的技能必须配封面：现在可以先保存，但启用时会被拒绝（营销页按封面展示技能）。";
}

/** 从保存失败的 `details` 里取字段级问题（`details.issues[]` 或 `details.problems[]`）。 */
export function skillServerIssues(details: Record<string, unknown> | undefined): string[] {
  const out: string[] = [];
  for (const key of ["issues", "problems"]) {
    const raw = details?.[key];
    if (!Array.isArray(raw)) continue;
    for (const x of raw) {
      if (typeof x === "string") out.push(x);
      else if (x && typeof x === "object") {
        const r = x as Row;
        out.push(r.field ? `${str(r.field)}：${str(r.message)}` : str(r.message));
      }
    }
  }
  return out;
}

/** 保存响应里不拦截的提示（`warnings[]`）。 */
export function skillWarningsOf(row: Row | null | undefined): string[] {
  const raw = row?.warnings;
  if (!Array.isArray(raw)) return [];
  return (raw as Row[]).map(x => (x.field ? `${str(x.field)}：${str(x.message)}` : str(x.message)));
}

/**
 * 导出一个技能（含附属文档）：只留内容字段，不带 ID / 版本 / 时间 / 封面媒体 ——
 * 那些在另一台机器上没有意义，带过去还会让人以为封面也跟着过去了。
 */
export function skillExportItem(row: Row, refs: Row[]): Row {
  return {
    code: str(row.code), name: str(row.name), category: str(row.category), description: str(row.description),
    tags: strArr(row.tags), body: str(row.body),
    steps: Array.isArray(row.steps) ? row.steps : [], tools: strArr(row.tools),
    modelBindings: row.modelBindings ?? {}, isQuickEntry: row.isQuickEntry === true,
    sort: typeof row.sort === "number" ? row.sort : 0,
    scenes: strArr(row.scenes), inputSlots: Array.isArray(row.inputSlots) ? row.inputSlots : [],
    prefillText: str(row.prefillText), isBatch: row.isBatch === true,
    references: refs.map(f => ({ name: str(f.name), body: str(f.body), sort: typeof f.sort === "number" ? f.sort : 0 }))
  };
}

/** 导入项 → 保存请求体（新建带 code，覆盖带当前 version）；封面保持现状（覆盖时）或留空（新建时）。 */
export function skillImportPayload(item: Row, current: Row | null): Row {
  const d = skillDraft({ ...item, inputSlots: item.inputSlots, coverMediaId: current?.coverMediaId ?? "", version: current?.version });
  return skillPayload(d, d.coverMediaId, current ? "update" : "create");
}

/** 导入项里的附属文档。 */
export function skillImportRefs(item: Row): Array<{ name: string; body: string; sort: number }> {
  return (Array.isArray(item.references) ? (item.references as Row[]) : [])
    .map(f => ({ name: str(f.name).trim(), body: str(f.body), sort: typeof f.sort === "number" ? f.sort : 0 }))
    .filter(f => f.name);
}
