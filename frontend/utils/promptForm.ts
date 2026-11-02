/**
 * 系统提示词编辑的纯规则（TT-T-19，移植自来源系统管理端 `utils/promptForm.ts`）。
 *
 * 与服务端 `catalog/domain.ExtractPlaceholders` / `ValidatePromptContent` 逐条对应，
 * 前端先算一遍是为了**边写边提示**（写错一个变量名，运行时发给模型的就是字面量 `{{shotCount}}`，
 * 而这不报错 —— 模型照着那段乱码生成）。服务端保存与发布时还会再验一次。
 */
export interface PromptSpec {
  code: string;
  name: string;
  required: string[];
  optional: string[];
  maxChars: number;
  /** 代码内置的兜底原文：没有覆盖行（或覆盖行已归档）时实际生效的就是它。 */
  builtin: string;
  hasBuiltin: boolean;
}

/** 提示词覆盖行（`GET /admin/prompts`）。 */
export interface PromptRow {
  code: string;
  modelCode?: string;
  status: string;
  version: number;
  content: string;
  draftContent?: string;
  hasUnpublishedDraft?: boolean;
  note?: string;
}

/**
 * 把接口给的规格归一成数组齐全的形状。
 * ⚠️ 服务端曾对没有必需变量的用途回 `required: null`（已修），按数组展开会直接抛错、
 *    整个编辑表单渲染失败 —— 这里兜一道，别让一个字段的形状把整页拖垮。
 */
export function normalizePromptSpec(raw: Record<string, unknown>): PromptSpec {
  const list = (v: unknown) => (Array.isArray(v) ? v.map(String) : []);
  return {
    code: String(raw.code ?? ""),
    name: String(raw.name ?? raw.code ?? ""),
    required: list(raw.required),
    optional: list(raw.optional),
    maxChars: typeof raw.maxChars === "number" ? raw.maxChars : 0,
    builtin: typeof raw.builtin === "string" ? raw.builtin : "",
    hasBuiltin: raw.hasBuiltin === true
  };
}

/** 取出 `{{name}}` 占位符（去重、排序）；格式有问题时给出原因。 */
export function promptPlaceholdersOf(content: string): { names: string[]; error: string } {
  const names = new Set<string>();
  let rest = content;
  for (;;) {
    const i = rest.indexOf("{{");
    if (i < 0) break;
    const j = rest.indexOf("}}", i);
    if (j < 0) return { names: [], error: `占位符未闭合：「${rest.slice(i, i + 20)}」附近缺少 }}` };
    const name = rest.slice(i + 2, j).trim();
    if (!name) return { names: [], error: "存在空占位符 {{}}" };
    if (/[{}]/.test(name)) return { names: [], error: `占位符「${name}」含非法字符` };
    names.add(name);
    rest = rest.slice(j + 2);
  }
  if (rest.includes("}}")) return { names: [], error: "存在多余的 }}" };
  return { names: [...names].sort(), error: "" };
}

/** 第一条要改的问题；没问题返回空串。 */
export function promptIssues(content: string, spec: PromptSpec | undefined): string {
  if (!spec) return "未知的提示词用途";
  if (!content.trim()) return "内容不能为空";
  const n = [...content].length;
  // ⚠️ 过长会把模型输出撑短甚至撑坏：旧系统因此让模型在 JSON 里写出裸引号，整批作废
  if (spec.maxChars > 0 && n > spec.maxChars) return `内容 ${n} 字，超过上限 ${spec.maxChars}`;
  const { names, error } = promptPlaceholdersOf(content);
  if (error) return error;
  const allowed = new Set([...spec.required, ...spec.optional]);
  const unknown = names.filter(x => !allowed.has(x));
  if (unknown.length) return `未知占位符：${unknown.join("、")}（它们不会被替换，会原样发给模型）`;
  const missing = spec.required.filter(x => !names.includes(x));
  if (missing.length) return `缺少必需占位符：${missing.join("、")}`;
  return "";
}

/** 在光标处插入 `{{name}}`，返回新内容与新光标位置。 */
export function insertPromptPlaceholder(content: string, cursor: number, name: string): { content: string; cursor: number } {
  const at = Math.max(0, Math.min(cursor, content.length));
  const token = `{{${name}}}`;
  return { content: content.slice(0, at) + token + content.slice(at), cursor: at + token.length };
}

/** 预览时的样例变量：**给全**（含可选的）—— 少给一个整条渲染就失败，线上会悄悄回落到内置兜底。 */
export function promptSampleVars(spec: PromptSpec | undefined, given: Record<string, string>): Record<string, string> {
  const out: Record<string, string> = {};
  for (const k of [...(spec?.required ?? []), ...(spec?.optional ?? [])]) out[k] = given[k] ?? "";
  return out;
}

/** 某用途的生效覆盖行（未归档的）。通用版 modelCode 为空。 */
export function activePromptRows(rows: readonly PromptRow[], code: string): PromptRow[] {
  return rows.filter(r => r.code === code && r.status !== "archived");
}

/**
 * 编辑器打开时的初始内容：已有覆盖行取它的草稿（没有草稿取线上内容）；
 * 没有覆盖行时从**内置原文**起步 —— 空白的编辑框会让人以为这个用途没有提示词，
 * 而实际上线上一直在用内置那份。
 */
export function promptInitialContent(row: PromptRow | null, spec: PromptSpec | undefined): string {
  if (row && row.status !== "archived") return row.draftContent || row.content || "";
  return spec?.builtin ?? "";
}
